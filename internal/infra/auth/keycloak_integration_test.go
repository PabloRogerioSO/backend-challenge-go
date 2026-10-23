//go:build integration

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	kcBaseURL string
	kcIssuer  string
	kcJWKSURL string
)

func TestMain(m *testing.M) {
	os.Exit(runKeycloak(m))
}

func runKeycloak(m *testing.M) int {
	ctx := context.Background()

	realmPath, err := filepath.Abs(filepath.Join("..", "..", "..", "deploy", "keycloak", "realm-export.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve realm path: %v\n", err)
		return 1
	}
	if _, err := os.Stat(realmPath); err != nil {
		fmt.Fprintf(os.Stderr, "realm-export.json not found at %s: %v\n", realmPath, err)
		return 1
	}

	kc, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "quay.io/keycloak/keycloak:26.0",
			ExposedPorts: []string{"8080/tcp", "9000/tcp"},
			Cmd:          []string{"start-dev", "--import-realm"},
			Env: map[string]string{
				"KC_BOOTSTRAP_ADMIN_USERNAME": "admin",
				"KC_BOOTSTRAP_ADMIN_PASSWORD": "admin",
				"KC_HEALTH_ENABLED":           "true",
				"KC_HOSTNAME_STRICT":          "false",
			},
			Mounts: testcontainers.ContainerMounts{
				testcontainers.BindMount(realmPath, "/opt/keycloak/data/import/realm-export.json"),
			},
			WaitingFor: wait.ForHTTP("/health/ready").
				WithPort("9000/tcp").
				WithStartupTimeout(180 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "start keycloak: %v\n", err)
		return 1
	}
	defer func() { _ = kc.Terminate(context.Background()) }()

	host, err := kc.Host(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "container host: %v\n", err)
		return 1
	}
	port, err := kc.MappedPort(ctx, "8080/tcp")
	if err != nil {
		fmt.Fprintf(os.Stderr, "container port: %v\n", err)
		return 1
	}
	kcBaseURL = fmt.Sprintf("http://%s:%s", host, port.Port())
	kcIssuer = kcBaseURL + "/realms/backend"
	kcJWKSURL = kcIssuer + "/protocol/openid-connect/certs"

	return m.Run()
}

// ---------------------------------------------------------------- helpers

func getToken(t *testing.T, clientID string) string {
	t.Helper()
	endpoint := kcIssuer + "/protocol/openid-connect/token"
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientID+"-secret")

	resp, err := http.PostForm(endpoint, form)
	if err != nil {
		t.Fatalf("token request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token request failed: %d %s", resp.StatusCode, body)
	}
	var payload struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if payload.AccessToken == "" {
		t.Fatalf("empty access_token: %s", body)
	}
	return payload.AccessToken
}

func newIntegrationValidator(t *testing.T) *Validator {
	t.Helper()
	v, err := NewValidator(kcIssuer, kcJWKSURL, "provider-service")
	if err != nil {
		t.Fatalf("NewValidator: %v", err)
	}
	return v
}

func hasScope(scopes []string, target string) bool {
	for _, s := range scopes {
		if s == target {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- tests

func TestKeycloak_ProviderA_TokenIsValid(t *testing.T) {
	token := getToken(t, "provider-a")
	v := newIntegrationValidator(t)

	claims, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if claims.ProviderID != "provider-a" {
		t.Fatalf("ProviderID = %q, want provider-a", claims.ProviderID)
	}
	if !hasScope(claims.Scopes, "wager.write") {
		t.Fatalf("provider-a must have wager.write scope, got %v", claims.Scopes)
	}
	if hasScope(claims.Scopes, "internal.wallet") {
		t.Fatalf("provider-a must NOT have internal.wallet scope, got %v", claims.Scopes)
	}
}

func TestKeycloak_ProviderB_TokenIsValid(t *testing.T) {
	token := getToken(t, "provider-b")
	v := newIntegrationValidator(t)

	claims, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if claims.ProviderID != "provider-b" {
		t.Fatalf("ProviderID = %q, want provider-b", claims.ProviderID)
	}
}

func TestKeycloak_InternalAdmin_HasInternalWalletScope(t *testing.T) {
	token := getToken(t, "internal-admin")
	v := newIntegrationValidator(t)

	claims, err := v.Validate(context.Background(), token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if claims.ProviderID != "internal" {
		t.Fatalf("ProviderID = %q, want internal", claims.ProviderID)
	}
	if !hasScope(claims.Scopes, "internal.wallet") {
		t.Fatalf("internal-admin must have internal.wallet scope, got %v", claims.Scopes)
	}
}

func TestKeycloak_RejectsTamperedToken(t *testing.T) {
	token := getToken(t, "provider-a")
	v := newIntegrationValidator(t)

	// Change a byte in the middle of the token (inside the payload), which is
	// guaranteed to alter the decoded payload and therefore invalidate the
	// signature. Changing the last byte could hit Base64URL padding bits and
	// leave the decoded payload identical.
	tampered := tamperTokenMiddle(token)

	_, err := v.Validate(context.Background(), tampered)
	if !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("error = %v, want ErrInvalidToken", err)
	}
}

func TestKeycloak_Middleware_ProviderAOnWagerRoute(t *testing.T) {
	token := getToken(t, "provider-a")
	v := newIntegrationValidator(t)

	handler := RequireAuth(v)(RequireScope("wager.write")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, ProviderIDFromContext(r.Context()))
	})))

	req := httptest.NewRequest(http.MethodPost, "/wagering/transactions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "provider-a" {
		t.Fatalf("provider_id = %q, want provider-a", rec.Body.String())
	}
}

func TestKeycloak_Middleware_ProviderAOnInternalRoute(t *testing.T) {
	token := getToken(t, "provider-a")
	v := newIntegrationValidator(t)

	handler := RequireAuth(v)(RequireScope("internal.wallet")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))

	req := httptest.NewRequest(http.MethodPost, "/wallets", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestKeycloak_Middleware_InternalAdminOnInternalRoute(t *testing.T) {
	token := getToken(t, "internal-admin")
	v := newIntegrationValidator(t)

	handler := RequireAuth(v)(RequireScope("internal.wallet")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, ProviderIDFromContext(r.Context()))
	})))

	req := httptest.NewRequest(http.MethodPost, "/wallets", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "internal" {
		t.Fatalf("provider_id = %q, want internal", rec.Body.String())
	}
}

func TestKeycloak_Middleware_MissingToken(t *testing.T) {
	v := newIntegrationValidator(t)

	handler := RequireAuth(v)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestKeycloak_Middleware_TamperedToken(t *testing.T) {
	token := getToken(t, "provider-a")
	v := newIntegrationValidator(t)

	tampered := tamperTokenMiddle(token)
	handler := RequireAuth(v)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("Authorization", "Bearer "+tampered)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// tamperTokenMiddle flips the case of a character in the middle of the token
// payload. Unlike the last character (which may carry Base64URL padding bits),
// a middle character is guaranteed to change the decoded payload and, thus,
// invalidate the RS256 signature.
func tamperTokenMiddle(token string) string {
	mid := len(token) / 2
	c := token[mid]
	var replacement byte
	if c >= 'a' && c <= 'z' {
		replacement = c - 32 // lowercase → uppercase
	} else {
		replacement = c + 32 // uppercase or digit → lowercase/digit
	}
	return token[:mid] + string(replacement) + token[mid+1:]
}
