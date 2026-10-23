package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

type ctxKey int

const (
	providerIDKey ctxKey = iota
	scopesKey
)

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// RequireAuth validates the Bearer token and adds provider_id and scopes to the context.
func RequireAuth(v *Validator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := bearerToken(r)
			if !ok {
				writeError(w, http.StatusUnauthorized, "unauthorized", "missing or malformed bearer token")
				return
			}

			claims, err := v.Validate(r.Context(), raw)
			if err != nil {
				// Log the exact reason the token failed for debugging
				slog.ErrorContext(r.Context(), "auth: token validation failed", slog.Any("error", err))

				msg := "invalid token"
				if errors.Is(err, ErrMissingProviderID) {
					msg = "token is missing provider_id claim"
				}
				writeError(w, http.StatusUnauthorized, "unauthorized", msg)
				return
			}

			ctx := context.WithValue(r.Context(), providerIDKey, claims.ProviderID)
			ctx = context.WithValue(ctx, scopesKey, claims.Scopes)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireScope ensures the authenticated token possesses the required scope.
func RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ProviderIDFromContext(r.Context()) == "" {
				writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
				return
			}

			for _, s := range ScopesFromContext(r.Context()) {
				if s == scope {
					next.ServeHTTP(w, r)
					return
				}
			}
			writeError(w, http.StatusForbidden, "forbidden", "insufficient scope")
		})
	}
}

func ProviderIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(providerIDKey).(string)
	return id
}

func ScopesFromContext(ctx context.Context) []string {
	scopes, _ := ctx.Value(scopesKey).([]string)
	if scopes == nil {
		return nil
	}
	out := make([]string, len(scopes))
	copy(out, scopes)
	return out
}

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(h[len(prefix):])
	if token == "" {
		return "", false
	}
	return token, true
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="api"`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: code, Message: msg})
}
