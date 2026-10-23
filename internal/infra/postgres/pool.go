package postgres

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// EnvMaxConns is the environment variable that sets the pool size.
	EnvMaxConns     = "POSTGRES_MAX_CONNS"
	defaultMaxConns = 10
)

// NewPool creates a pgx pool, sized by POSTGRES_MAX_CONNS (default 10), and pings the database.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse dsn: %w", err)
	}
	maxConns, err := maxConnsFromEnv()
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = maxConns
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return pool, nil
}

func maxConnsFromEnv() (int32, error) {
	raw := strings.TrimSpace(os.Getenv(EnvMaxConns))
	if raw == "" {
		return defaultMaxConns, nil
	}
	n, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%w: %s must be a positive integer, got %q", ErrInvalidArgument, EnvMaxConns, raw)
	}
	return int32(n), nil
}
