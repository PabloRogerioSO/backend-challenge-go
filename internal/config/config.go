// Package config loads the application configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHTTPPort        = "8080"
	defaultLogLevel        = "info"
	defaultShutdownTimeout = 15 * time.Second
)

// Config holds the settings read from the environment.
type Config struct {
	HTTPPort          string
	LogLevel          string
	DatabaseURL       string
	KeycloakIssuerURL string
	KeycloakJWKSURL   string
	KeycloakAudience  string
	ShutdownTimeout   time.Duration
}

// Load reads and validates the configuration from the process environment.
func Load() (*Config, error) {
	return load(os.Getenv)
}

func load(getenv func(string) string) (*Config, error) {
	cfg := &Config{
		HTTPPort:          valueOr(getenv("HTTP_PORT"), defaultHTTPPort),
		LogLevel:          valueOr(getenv("LOG_LEVEL"), defaultLogLevel),
		DatabaseURL:       strings.TrimSpace(getenv("DATABASE_URL")),
		KeycloakIssuerURL: strings.TrimSpace(getenv("KEYCLOAK_ISSUER_URL")),
		KeycloakJWKSURL:   strings.TrimSpace(getenv("KEYCLOAK_JWKS_URL")),
		KeycloakAudience:  strings.TrimSpace(getenv("KEYCLOAK_AUDIENCE")),
		ShutdownTimeout:   defaultShutdownTimeout,
	}

	var errs []error
	if port, err := strconv.Atoi(cfg.HTTPPort); err != nil || port < 1 || port > 65535 {
		errs = append(errs, fmt.Errorf("HTTP_PORT must be a port between 1 and 65535, got %q", cfg.HTTPPort))
	}
	for _, r := range []struct{ name, value string }{
		{"DATABASE_URL", cfg.DatabaseURL},
		{"KEYCLOAK_ISSUER_URL", cfg.KeycloakIssuerURL},
		{"KEYCLOAK_JWKS_URL", cfg.KeycloakJWKSURL},
		{"KEYCLOAK_AUDIENCE", cfg.KeycloakAudience},
	} {
		if r.value == "" {
			errs = append(errs, fmt.Errorf("%s is required", r.name))
		}
	}
	if raw := strings.TrimSpace(getenv("SHUTDOWN_TIMEOUT")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT must be a positive duration, got %q", raw))
		} else {
			cfg.ShutdownTimeout = d
		}
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("config: %w", errors.Join(errs...))
	}
	return cfg, nil
}

func valueOr(v, fallback string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	return fallback
}
