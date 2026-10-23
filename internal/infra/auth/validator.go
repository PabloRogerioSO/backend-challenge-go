package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	defaultKeyTTL        = 10 * time.Minute
	defaultMinRefresh    = 30 * time.Second
	defaultFetchTimeout  = 10 * time.Second
	defaultClockLeeway   = 5 * time.Second
	maxJWKSBodyBytes     = 1 << 20
	initialFetchTimeout  = 15 * time.Second
	allowedSigningMethod = "RS256"
)

var (
	// ErrInvalidToken indica token malformado, com assinatura inválida, expirado,
	// com issuer/audience incorretos ou qualquer outra falha de validação.
	ErrInvalidToken = errors.New("auth: invalid token")
	// ErrMissingProviderID indica token válido porém sem a claim "provider_id".
	ErrMissingProviderID = errors.New("auth: missing provider_id claim")
)

// Claims são os dados extraídos de um token validado.
type Claims struct {
	Subject    string
	ProviderID string
	Scopes     []string
}

type tokenClaims struct {
	jwt.RegisteredClaims
	ProviderID string `json:"provider_id"`
	Scope      string `json:"scope"`
}

// Validator valida JWTs emitidos pelo Keycloak usando o JWKS publicado.
type Validator struct {
	issuer   string
	jwksURL  string
	audience string

	client     *http.Client
	ttl        time.Duration
	minRefresh time.Duration

	mu          sync.RWMutex
	keys        map[string]*rsa.PublicKey
	fetchedAt   time.Time
	lastAttempt time.Time

	refreshMu sync.Mutex
}

// NewValidator cria o validador e já baixa o JWKS inicial.
func NewValidator(issuerURL, jwksURL, audience string) (*Validator, error) {
	if strings.TrimSpace(issuerURL) == "" {
		return nil, errors.New("auth: issuerURL is required")
	}
	if strings.TrimSpace(jwksURL) == "" {
		return nil, errors.New("auth: jwksURL is required")
	}
	if strings.TrimSpace(audience) == "" {
		return nil, errors.New("auth: audience is required")
	}

	v := &Validator{
		issuer:     issuerURL,
		jwksURL:    jwksURL,
		audience:   audience,
		client:     &http.Client{Timeout: defaultFetchTimeout},
		ttl:        defaultKeyTTL,
		minRefresh: defaultMinRefresh,
		keys:       map[string]*rsa.PublicKey{},
	}

	ctx, cancel := context.WithTimeout(context.Background(), initialFetchTimeout)
	defer cancel()
	if err := v.refresh(ctx); err != nil {
		return nil, fmt.Errorf("auth: initial JWKS fetch: %w", err)
	}
	return v, nil
}

// Validate valida assinatura, exp, nbf, iss e aud e extrai provider_id e scope.
func (v *Validator) Validate(ctx context.Context, rawToken string) (*Claims, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{allowedSigningMethod}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(defaultClockLeeway),
	)

	claims := &tokenClaims{}
	_, err := parser.ParseWithClaims(rawToken, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("missing kid header")
		}
		return v.key(ctx, kid)
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}

	if strings.TrimSpace(claims.ProviderID) == "" {
		return nil, ErrMissingProviderID
	}

	return &Claims{
		Subject:    claims.Subject,
		ProviderID: claims.ProviderID,
		Scopes:     strings.Fields(claims.Scope),
	}, nil
}

func (v *Validator) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	v.mu.RLock()
	k, ok := v.keys[kid]
	fresh := time.Since(v.fetchedAt) < v.ttl
	v.mu.RUnlock()

	if ok && fresh {
		return k, nil
	}

	if err := v.refresh(ctx); err != nil {
		if ok {
			// Mantém a chave em cache se o Keycloak estiver temporariamente fora.
			return k, nil
		}
		return nil, err
	}

	v.mu.RLock()
	k, ok = v.keys[kid]
	v.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown kid %q", kid)
	}
	return k, nil
}

func (v *Validator) refresh(ctx context.Context) error {
	v.refreshMu.Lock()
	defer v.refreshMu.Unlock()

	v.mu.RLock()
	last := v.lastAttempt
	v.mu.RUnlock()
	if !last.IsZero() && time.Since(last) < v.minRefresh {
		return nil
	}

	v.mu.Lock()
	v.lastAttempt = time.Now()
	v.mu.Unlock()

	keys, err := v.fetchKeys(ctx)
	if err != nil {
		return err
	}

	v.mu.Lock()
	v.keys = keys
	v.fetchedAt = time.Now()
	v.mu.Unlock()
	return nil
}

type jwksDocument struct {
	Keys []jwkEntry `json:"keys"`
}

type jwkEntry struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (v *Validator) fetchKeys(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build JWKS request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := v.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch JWKS: unexpected status %d", resp.StatusCode)
	}

	var doc jwksDocument
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJWKSBodyBytes)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode JWKS: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub, err := parseRSAKey(k)
		if err != nil {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return nil, errors.New("JWKS contains no usable RSA signing keys")
	}
	return keys, nil
}

func parseRSAKey(k jwkEntry) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode exponent: %w", err)
	}
	if len(nBytes) == 0 || len(eBytes) == 0 {
		return nil, errors.New("empty key material")
	}

	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() || e.Int64() <= 0 || e.Int64() > 1<<31-1 {
		return nil, errors.New("invalid exponent")
	}

	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(e.Int64()),
	}, nil
}
