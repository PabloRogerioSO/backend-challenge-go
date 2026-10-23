package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// LedgerTotals is a consistent snapshot of a wallet balance and of its ledger sums.
type LedgerTotals struct {
	Currency           string
	WalletBalanceCents int64
	CreditsCents       int64
	DebitsCents        int64
	Entries            int64
}

// TotalsByWallet reads the wallet balance and the ledger sums in a single
// statement, so both come from the same snapshot. It returns ErrNotFound when
// the wallet does not exist.
func (r *LedgerRepo) TotalsByWallet(ctx context.Context, q Querier, walletID string) (LedgerTotals, error) {
	var t LedgerTotals
	err := q.QueryRow(ctx,
		`SELECT w.currency,
		        w.balance_cents,
		        COALESCE(SUM(e.amount_cents) FILTER (WHERE e.direction = 'CREDIT'), 0)::bigint,
		        COALESCE(SUM(e.amount_cents) FILTER (WHERE e.direction = 'DEBIT'), 0)::bigint,
		        COUNT(e.id)
		   FROM wallets w
		   LEFT JOIN wallet_ledger_entries e ON e.wallet_id = w.id
		  WHERE w.id = $1
		  GROUP BY w.id`,
		walletID,
	).Scan(&t.Currency, &t.WalletBalanceCents, &t.CreditsCents, &t.DebitsCents, &t.Entries)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return LedgerTotals{}, fmt.Errorf("%w: wallet %s", ErrNotFound, walletID)
	case err != nil:
		return LedgerTotals{}, fmt.Errorf("postgres: ledger totals of wallet %s: %w", walletID, err)
	}
	return t, nil
}
