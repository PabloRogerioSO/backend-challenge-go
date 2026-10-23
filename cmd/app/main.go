package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/app/usecase"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/config"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/auth"
	httpapi "github.com/PabloRogerioSO/backend-challenge-go/internal/infra/http"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
)

func main() {
	os.Exit(run())
}

func run() int {
	bootstrap := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		bootstrap.Error("load configuration", slog.Any("error", err))
		return 1
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parseLevel(cfg.LogLevel)}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.ErrorContext(ctx, "connect to postgres", slog.Any("error", err))
		return 1
	}
	defer pool.Close()

	validator, err := newAuthValidator(ctx, cfg)
	if err != nil {
		logger.ErrorContext(ctx, "create auth validator", slog.Any("error", err))
		return 1
	}

	wallets := postgres.NewWalletRepo()
	wagers := postgres.NewWageringRepo()
	ledgers := postgres.NewLedgerRepo()
	outbox := postgres.NewOutboxRepo()

	opener := usecase.NewWalletOpener(pool, wallets, wagers, ledgers, outbox)
	processor := usecase.NewWagerProcessor(pool, wallets, wagers, ledgers, outbox)
	reconciler := usecase.NewReconciler(pool, ledgers)

	srv := httpapi.NewServer(cfg, validator, opener, processor, reconciler, wallets, ledgers, wagers, pool)

	serverErr := make(chan error, 1)
	go func() {
		logger.InfoContext(ctx, "http server listening", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.InfoContext(context.Background(), "shutdown signal received, stopping")
	case err := <-serverErr:
		logger.ErrorContext(context.Background(), "http server failed", slog.Any("error", err))
		return 1
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.ErrorContext(context.Background(), "graceful shutdown failed", slog.Any("error", err))
		return 1
	}

	logger.InfoContext(context.Background(), "application stopped")
	return 0
}

func newAuthValidator(ctx context.Context, cfg *config.Config) (*auth.Validator, error) {
	_ = ctx // The current validator creates its own context internally
	return auth.NewValidator(cfg.KeycloakIssuerURL, cfg.KeycloakJWKSURL, cfg.KeycloakAudience)
}

func parseLevel(v string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
