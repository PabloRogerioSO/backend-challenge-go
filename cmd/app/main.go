package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	nethttp "net/http"
	"os"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/app/refworker"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/app/usecase"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/config"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/auth"
	apphttp "github.com/PabloRogerioSO/backend-challenge-go/internal/infra/http"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/metrics"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/outbox"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
	infrasqs "github.com/PabloRogerioSO/backend-challenge-go/internal/infra/sqs"
)

// SqsConfig groups the configuration required for AWS SQS and background workers.
type SqsConfig struct {
	AWSRegion        string
	SQSEndpoint      string
	InboundQueueURL  string
	OutboundQueueURL string
	ConsumerPollers  int
	RefTTL           time.Duration
}

// loadSqsConfig reads and validates SQS-related environment variables.
func loadSqsConfig() (SqsConfig, error) {
	cfg := SqsConfig{
		AWSRegion:        envStr("AWS_REGION", "us-east-1"),
		SQSEndpoint:      os.Getenv("SQS_ENDPOINT"),
		InboundQueueURL:  os.Getenv("SQS_INBOUND_QUEUE_URL"),
		OutboundQueueURL: os.Getenv("SQS_OUTBOUND_QUEUE_URL"),
		ConsumerPollers:  envInt("SQS_CONSUMER_POLLERS", 2),
		RefTTL:           time.Duration(envInt("REFERENCE_TTL_HOURS", 24)) * time.Hour,
	}
	for name, v := range map[string]string{
		"SQS_INBOUND_QUEUE_URL": cfg.InboundQueueURL, "SQS_OUTBOUND_QUEUE_URL": cfg.OutboundQueueURL,
	} {
		if v == "" {
			return SqsConfig{}, fmt.Errorf("missing env %s", name)
		}
	}
	return cfg, nil
}

// newPool initializes the PostgreSQL connection pool and manages its lifecycle.
func newPool(lc fx.Lifecycle, cfg *config.Config) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	pc.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		return nil, err
	}
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error { return pool.Ping(ctx) },
		OnStop:  func(context.Context) error { pool.Close(); return nil },
	})
	return pool, nil
}

// newHTTPDatabase binds the concrete *pgxpool.Pool to the Database interface required by the HTTP server.
func newHTTPDatabase(pool *pgxpool.Pool) apphttp.Database {
	return pool
}

// newUsecaseDB binds the concrete *pgxpool.Pool to the DB interface required by the use cases.
func newUsecaseDB(pool *pgxpool.Pool) usecase.DB {
	return pool
}

// newPostgresDB binds the concrete *pgxpool.Pool to the DB interface required by the PostgreSQL repositories.
func newPostgresDB(pool *pgxpool.Pool) postgres.DB {
	return pool
}

// newSQSClient initializes the AWS SQS client, configured to work with LocalStack.
func newSQSClient(cfg SqsConfig) (*awssqs.Client, error) {
	opts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(cfg.AWSRegion)}
	if cfg.SQSEndpoint != "" && os.Getenv("AWS_ACCESS_KEY_ID") == "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, err
	}
	return awssqs.NewFromConfig(awsCfg, func(o *awssqs.Options) {
		if cfg.SQSEndpoint != "" {
			o.BaseEndpoint = aws.String(cfg.SQSEndpoint)
		}
	}), nil
}

// startHTTP launches the HTTP server in a goroutine and sets up graceful shutdown.
func startHTTP(lc fx.Lifecycle, srv *nethttp.Server, log *slog.Logger) {
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go func() {
				log.Info("http server listening", "addr", srv.Addr)
				if err := srv.ListenAndServe(); err != nil && !errors.Is(err, nethttp.ErrServerClosed) {
					log.Error("http server failed", "err", err)
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			log.Info("http server shutting down")
			return srv.Shutdown(ctx)
		},
	})
}

// runWorker starts a background worker and ensures it drains before the application stops.
func runWorker(lc fx.Lifecycle, log *slog.Logger, name string, run func(context.Context) error) {
	var (
		cancel context.CancelFunc
		done   chan struct{}
	)
	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			ctx, c := context.WithCancel(context.Background())
			cancel, done = c, make(chan struct{})
			go func() {
				defer close(done)
				if err := run(ctx); err != nil {
					log.Error("worker exited with error", "worker", name, "err", err)
				}
			}()
			log.Info("worker started", "worker", name)
			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			cancel()
			select {
			case <-done:
				log.Info("worker stopped", "worker", name)
				return nil
			case <-stopCtx.Done():
				return fmt.Errorf("worker %s did not stop in time: %w", name, stopCtx.Err())
			}
		},
	})
}

// startWorkers initializes and starts the SQS consumer, outbox publisher, and reference worker.
func startWorkers(
	lc fx.Lifecycle, cfg SqsConfig, log *slog.Logger, client *awssqs.Client, tx postgres.TxRunner,
	inbox *postgres.InboxRepo, outboxRepo *postgres.OutboxRepo, pending *postgres.PendingRepo, proc *usecase.WagerProcessor,
) {
	consumer := infrasqs.NewConsumer(infrasqs.Config{
		QueueURL: cfg.InboundQueueURL, Pollers: cfg.ConsumerPollers,
	}, client, tx, inbox, proc, log)
	publisher := outbox.NewPublisher(outbox.Config{QueueURL: cfg.OutboundQueueURL}, outboxRepo, client, tx, log)
	refWorker := refworker.NewWorker(refworker.Config{TTL: cfg.RefTTL}, pending, outboxRepo, proc, tx, log)

	runWorker(lc, log, "sqs-consumer", consumer.Run)
	runWorker(lc, log, "outbox-publisher", publisher.Run)
	runWorker(lc, log, "reference-worker", refWorker.Run)
}

func main() {
	// Initialize Prometheus metrics registry.
	metrics.Register()

	app := fx.New(
		fx.StopTimeout(45*time.Second),
		fx.Provide(
			config.Load,
			loadSqsConfig,
			func() *slog.Logger { return slog.New(slog.NewJSONHandler(os.Stdout, nil)) },
			newPool,
			newHTTPDatabase,
			newUsecaseDB,
			newPostgresDB, // <-- Added the missing interface binding
			newSQSClient,

			// Auth
			func(cfg *config.Config) (*auth.Validator, error) {
				return auth.NewValidator(cfg.KeycloakIssuerURL, cfg.KeycloakJWKSURL, cfg.KeycloakAudience)
			},

			// Repositories
			postgres.NewWalletRepo,
			postgres.NewWageringRepo,
			postgres.NewLedgerRepo,
			postgres.NewInboxRepo,
			postgres.NewOutboxRepo,
			postgres.NewPendingRepo,
			func(pool *pgxpool.Pool) postgres.TxRunner { return postgres.NewTxRunner(pool) },

			// Use Cases
			usecase.NewWalletOpener,
			usecase.NewWagerProcessor,
			usecase.NewReconciler,

			// HTTP Server
			apphttp.NewServer,
		),
		fx.Invoke(startHTTP, startWorkers),
	)
	app.Run()
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
