package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testKID      = "test-key-1"
	testIssuer   = "http://localhost:8080/realms/backend-challenge"
	testAudience = "backend-challenge-api"
	testScope    = "wallet:write"
)

type testEnv struct {
	key       *rsa.PrivateKey
	validator *Validator
	handler   http.Handler
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	jwks := map[string]any{
		"keys": []map[string]string{{
			"kty": "RSA",
			"kid": testKID,
			"use": "sig",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
		}},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	t.Cleanup(srv.Close)

	v, err := NewValidator(testIssuer, srv.URL, testAudience)
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}

	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(ProviderIDFromContext(r.Context())))
	})
	chain := RequireAuth(v)(RequireScope(testScope)(final))

	return &testEnv{key: key, validator: v, handler: chain}
}

func (e *testEnv) sign(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = testKID
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return s
}

func validClaims() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss":         testIssuer,
		"aud":         testAudience,
		"sub":         "service-account-provider-a",
		"iat":         now.Unix(),
		"nbf":         now.Add(-1 * time.Minute).Unix(),
		"exp":         now.Add(10 * time.Minute).Unix(),
		"provider_id": "provider-a",
		"scope":       "profile " + testScope,
	}
}

func (e *testEnv) do(token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/wallets", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func TestMiddleware_NoToken_401(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do("")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	if rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatal("expected WWW-Authenticate header")
	}
}

func TestMiddleware_MalformedAuthorizationHeader_401(t *testing.T) {
	env := newTestEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/wallets", nil)
	req.Header.Set("Authorization", "Basic abc123")
	rec := httptest.NewRecorder()
	env.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestMiddleware_GarbageToken_401(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do("not-a-jwt")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestMiddleware_WrongSignature_401(t *testing.T) {
	env := newTestEnv(t)
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}
	rec := env.do(env.sign(t, otherKey, validClaims()))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestMiddleware_ExpiredToken_401(t *testing.T) {
	env := newTestEnv(t)
	c := validClaims()
	c["exp"] = time.Now().Add(-1 * time.Hour).Unix()
	rec := env.do(env.sign(t, env.key, c))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestMiddleware_NotYetValidToken_401(t *testing.T) {
	env := newTestEnv(t)
	c := validClaims()
	c["nbf"] = time.Now().Add(1 * time.Hour).Unix()
	rec := env.do(env.sign(t, env.key, c))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestMiddleware_WrongIssuer_401(t *testing.T) {
	env := newTestEnv(t)
	c := validClaims()
	c["iss"] = "http://evil.example/realms/other"
	rec := env.do(env.sign(t, env.key, c))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestMiddleware_WrongAudience_401(t *testing.T) {
	env := newTestEnv(t)
	c := validClaims()
	c["aud"] = "another-api"
	rec := env.do(env.sign(t, env.key, c))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestMiddleware_MissingProviderID_401(t *testing.T) {
	env := newTestEnv(t)
	c := validClaims()
	delete(c, "provider_id")
	rec := env.do(env.sign(t, env.key, c))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestMiddleware_WrongScope_403(t *testing.T) {
	env := newTestEnv(t)
	c := validClaims()
	c["scope"] = "profile wallet:read"
	rec := env.do(env.sign(t, env.key, c))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

func TestMiddleware_CorrectScope_200(t *testing.T) {
	env := newTestEnv(t)
	rec := env.do(env.sign(t, env.key, validClaims()))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestMiddleware_ExtractsProviderID(t *testing.T) {
	env := newTestEnv(t)

	for _, providerID := range []string{"provider-a", "provider-b"} {
		c := validClaims()
		c["provider_id"] = providerID
		rec := env.do(env.sign(t, env.key, c))
		if rec.Code != http.StatusOK {
			t.Fatalf("provider %s: expected 200, got %d", providerID, rec.Code)
		}
		if got := rec.Body.String(); got != providerID {
			t.Fatalf("expected provider_id %q, got %q", providerID, got)
		}
	}
}

func TestMiddleware_ExtractsScopes(t *testing.T) {
	env := newTestEnv(t)

	var got []string
	h := RequireAuth(env.validator)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = ScopesFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+env.sign(t, env.key, validClaims()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if len(got) != 2 || got[0] != "profile" || got[1] != testScope {
		t.Fatalf("unexpected scopes: %v", got)
	}
}

func TestRequireScope_WithoutAuth_401(t *testing.T) {
	h := RequireScope(testScope)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestNewValidator_RejectsEmptyArgs(t *testing.T) {
	cases := []struct{ issuer, jwks, aud string }{
		{"", "http://x", "a"},
		{"http://x", "", "a"},
		{"http://x", "http://x", ""},
	}
	for _, c := range cases {
		if _, err := NewValidator(c.issuer, c.jwks, c.aud); err == nil {
			t.Fatalf("expected error for %+v", c)
		}
	}
}
