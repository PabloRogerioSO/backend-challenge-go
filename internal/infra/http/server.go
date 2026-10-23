// Package httpapi is the HTTP layer: routes, middlewares, DTOs and the mapping
// of use case errors to status codes. (The directory is internal/infra/http.)
package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/app/usecase"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/config"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/ledger"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wagering"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wallet"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/auth"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/metrics"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
)

const (
	scopeInternalWallet = "internal.wallet"
	scopeWagerWrite     = "wager.write"

	readyTimeout = 2 * time.Second
)

// Database is what the HTTP layer needs from the database: read access for the
// repositories and a liveness check; *pgxpool.Pool implements it.
type Database interface {
	postgres.Querier
	Ping(ctx context.Context) error
}

type walletOpener interface {
	OpenWallet(ctx context.Context, playerID string, initial money.Money) (*wallet.Wallet, error)
}

type wagerProcessor interface {
	ProcessWager(ctx context.Context, in usecase.ProcessWagerInput) (usecase.ProcessWagerOutput, error)
}

type walletReconciler interface {
	Reconcile(ctx context.Context, walletID string) (usecase.ReconciliationReport, error)
}

type walletReader interface {
	GetByID(ctx context.Context, q postgres.Querier, id string) (*wallet.Wallet, error)
}

type ledgerReader interface {
	ListByWalletCursor(ctx context.Context, q postgres.Querier, walletID, cursor string, limit int) ([]ledger.Entry, string, error)
}

type wagerReader interface {
	GetByID(ctx context.Context, q postgres.Querier, id string) (*wagering.WagerTransaction, error)
	GetByProviderAndExternal(ctx context.Context, q postgres.Querier, providerID, externalID string) (*wagering.WagerTransaction, error)
}

// deps groups everything the router needs. Authentication enters as functions so
// tests can replace it; in production they are the auth package functions.
type deps struct {
	opener     walletOpener
	processor  wagerProcessor
	reconciler walletReconciler
	wallets    walletReader
	ledgers    ledgerReader
	wagers     wagerReader
	db         Database

	authenticate func(http.Handler) http.Handler
	requireScope func(scope string) func(http.Handler) http.Handler
	providerID   func(ctx context.Context) string

	logger *slog.Logger
}

// NewServer builds the HTTP server with all routes. Everything but the health
// endpoints and metrics requires a valid token.
func NewServer(
	cfg *config.Config,
	authValidator *auth.Validator,
	opener *usecase.WalletOpener,
	processor *usecase.WagerProcessor,
	reconciler *usecase.Reconciler,
	wallets *postgres.WalletRepo,
	ledgers *postgres.LedgerRepo,
	wagers *postgres.WageringRepo,
	db Database,
) *http.Server {
	d := deps{
		opener:       opener,
		processor:    processor,
		reconciler:   reconciler,
		wallets:      wallets,
		ledgers:      ledgers,
		wagers:       wagers,
		db:           db,
		authenticate: auth.RequireAuth(authValidator),
		requireScope: auth.RequireScope,
		providerID:   auth.ProviderIDFromContext,
		logger:       slog.Default(),
	}
	return &http.Server{
		Addr:              ":" + cfg.HTTPPort,
		Handler:           newRouter(d),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

func newRouter(d deps) http.Handler {
	h := &handlers{d: d}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(accessLog(d.logger))
	r.Use(metricsMiddleware) // Prometheus Metrics
	r.Use(middleware.Recoverer)

	r.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeAPIError(w, http.StatusNotFound, "not_found", "route not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	})

	// Public routes
	r.Get("/health/live", h.live)
	r.Get("/health/ready", h.ready)
	r.Get("/metrics", promhttp.Handler().ServeHTTP)

	// Protected routes
	r.Group(func(r chi.Router) {
		r.Use(d.authenticate)

		r.With(d.requireScope(scopeInternalWallet)).Post("/wallets", h.createWallet)
		r.Get("/wallets/{walletId}", h.getWallet)
		r.Get("/wallets/{walletId}/ledger", h.listLedger)
		r.Post("/wallets/{walletId}/reconciliation", h.reconcile)

		r.With(d.requireScope(scopeWagerWrite)).Post("/wagering/transactions", h.processWager)
		r.Get("/wagering/transactions/{transactionId}", h.getTransaction)
		r.Get("/providers/{providerId}/wagering/transactions/{externalTransactionId}", h.getTransactionByExternal)
	})
	return r
}

// accessLog logs one line per request. It logs the path without the query string
// (cursors) and never the Authorization header.
func accessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()
			next.ServeHTTP(ww, r)
			status := ww.Status()
			if status == 0 {
				status = http.StatusOK
			}
			logger.LogAttrs(r.Context(), slog.LevelInfo, "http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Duration("duration", time.Since(start)),
				slog.String("request_id", middleware.GetReqID(r.Context())),
			)
		})
	}
}

// metricsMiddleware records the HTTP request count and duration for Prometheus.
func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)

		duration := time.Since(start).Seconds()
		status := strconv.Itoa(ww.Status())
		if ww.Status() == 0 {
			status = "200"
		}

		// Get the route pattern (e.g., /wallets/{walletId}) to avoid high cardinality
		path := chi.RouteContext(r.Context()).RoutePattern()
		if path == "" {
			path = "unknown"
		}

		metrics.HTTPRequestsTotal.WithLabelValues(r.Method, path, status).Inc()
		metrics.HTTPRequestDuration.WithLabelValues(r.Method, path).Observe(duration)
	})
}
