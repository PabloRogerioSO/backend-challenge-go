package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// TxRunner runs fn inside a transaction (commit on nil, rollback on error).
type TxRunner func(ctx context.Context, fn func(pgx.Tx) error) error

// NewTxRunner adapts WithTx to a TxRunner bound to a pool.
func NewTxRunner(pool TxBeginner) TxRunner {
	return func(ctx context.Context, fn func(pgx.Tx) error) error {
		return WithTx(ctx, pool, fn)
	}
}
