package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const rollbackTimeout = 5 * time.Second

// Querier is the read/write subset shared by *pgxpool.Pool and pgx.Tx.
type Querier interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TxBeginner starts transactions; *pgxpool.Pool implements it.
type TxBeginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// WithTx runs fn inside a transaction. It commits when fn returns nil, rolls
// back when fn returns an error (the error is returned unchanged so errors.Is
// keeps working) and rolls back before re-panicking when fn panics. Rollback
// uses a context that survives cancellation of ctx.
func WithTx(ctx context.Context, pool TxBeginner, fn func(pgx.Tx) error) (err error) {
	if fn == nil {
		return fmt.Errorf("%w: nil transaction function", ErrInvalidArgument)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: begin: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			_ = rollback(ctx, tx)
			panic(p)
		}
	}()

	if err = fn(tx); err != nil {
		if rbErr := rollback(ctx, tx); rbErr != nil {
			return errors.Join(err, rbErr)
		}
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: commit: %w", err)
	}
	return nil
}

func rollback(ctx context.Context, tx pgx.Tx) error {
	rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	if err := tx.Rollback(rbCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return fmt.Errorf("postgres: rollback: %w", err)
	}
	return nil
}
