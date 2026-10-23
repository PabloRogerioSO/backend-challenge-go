package config

import (
	"strings"
	"testing"
	"time"
)

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func validEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":        "postgres://u:p@localhost:5432/db",
		"KEYCLOAK_ISSUER_URL": "http://localhost:8081/realms/backend",
		"KEYCLOAK_JWKS_URL":   "http://localhost:8081/realms/backend/protocol/openid-connect/certs",
		"KEYCLOAK_AUDIENCE":   "provider-service",
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := load(env(validEnv()))
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	if cfg.HTTPPort != "8080" || cfg.LogLevel != "info" || cfg.ShutdownTimeout != 15*time.Second {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.KeycloakAudience != "provider-service" || cfg.DatabaseURL == "" {
		t.Fatalf("unexpected values: %+v", cfg)
	}
}

func TestLoadOverrides(t *testing.T) {
	values := validEnv()
	values["HTTP_PORT"] = "9090"
	values["LOG_LEVEL"] = "debug"
	values["SHUTDOWN_TIMEOUT"] = "3s"
	cfg, err := load(env(values))
	if err != nil {
		t.Fatalf("load error: %v", err)
	}
	if cfg.HTTPPort != "9090" || cfg.LogLevel != "debug" || cfg.ShutdownTimeout != 3*time.Second {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
		want   string
	}{
		{"missing database url", func(m map[string]string) { delete(m, "DATABASE_URL") }, "DATABASE_URL is required"},
		{"missing issuer", func(m map[string]string) { delete(m, "KEYCLOAK_ISSUER_URL") }, "KEYCLOAK_ISSUER_URL is required"},
		{"missing jwks", func(m map[string]string) { delete(m, "KEYCLOAK_JWKS_URL") }, "KEYCLOAK_JWKS_URL is required"},
		{"missing audience", func(m map[string]string) { delete(m, "KEYCLOAK_AUDIENCE") }, "KEYCLOAK_AUDIENCE is required"},
		{"port not a number", func(m map[string]string) { m["HTTP_PORT"] = "abc" }, "HTTP_PORT"},
		{"port out of range", func(m map[string]string) { m["HTTP_PORT"] = "70000" }, "HTTP_PORT"},
		{"bad shutdown timeout", func(m map[string]string) { m["SHUTDOWN_TIMEOUT"] = "soon" }, "SHUTDOWN_TIMEOUT"},
		{"negative shutdown timeout", func(m map[string]string) { m["SHUTDOWN_TIMEOUT"] = "-1s" }, "SHUTDOWN_TIMEOUT"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			values := validEnv()
			tc.mutate(values)
			cfg, err := load(env(values))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
			if cfg != nil {
				t.Fatal("config must be nil on error")
			}
		})
	}
}
