package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// DB is the database handle required by PendingRepo; *pgxpool.Pool implements it.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// PendingTransaction is a wager transaction waiting for its reference.
type PendingTransaction struct {
	ID                             string
	CreatedAt                      time.Time
	ProviderID                     string
	WalletID                       string
	IdempotencyKey                 string
	ExternalTransactionID          string
	Kind                           string
	AmountCents                    int64
	Currency                       string
	RoundID                        string
	GameID                         string
	ReferenceExternalTransactionID string
}

// PendingRepo persists pending-reference lookups and status transitions.
type PendingRepo struct{ db DB }

// NewPendingRepo builds a repository bound to the given database handle.
func NewPendingRepo(db DB) *PendingRepo { return &PendingRepo{db: db} }

// ListPending returns the oldest PENDING_REFERENCE transactions, up to limit.
func (r *PendingRepo) ListPending(ctx context.Context, limit int) ([]PendingTransaction, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, provider_id, wallet_id, idempotency_key, external_transaction_id, kind,
		       amount_cents, currency, COALESCE(round_id, ''), COALESCE(game_id, ''),
		       COALESCE(reference_external_transaction_id, ''), created_at
		FROM wager_transactions
		WHERE status = 'PENDING_REFERENCE'
		ORDER BY created_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending: %w", err)
	}
	defer rows.Close()

	var out []PendingTransaction
	for rows.Next() {
		var p PendingTransaction
		if err := rows.Scan(&p.ID, &p.ProviderID, &p.WalletID, &p.IdempotencyKey, &p.ExternalTransactionID,
			&p.Kind, &p.AmountCents, &p.Currency, &p.RoundID, &p.GameID, &p.ReferenceExternalTransactionID, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan pending: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ReferenceProcessed reports whether the referenced transaction exists and is PROCESSED.
func (r *PendingRepo) ReferenceProcessed(ctx context.Context, providerID, referenceExternalTransactionID string) (bool, error) {
	var ok bool
	err := r.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM wager_transactions
			WHERE provider_id = $1 AND external_transaction_id = $2 AND status = 'PROCESSED')`,
		providerID, referenceExternalTransactionID).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("reference lookup: %w", err)
	}
	return ok, nil
}

// MarkProcessed moves the row to PROCESSED inside tx. Returns false if it already left PENDING_REFERENCE.
func (r *PendingRepo) MarkProcessed(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE wager_transactions SET status = 'PROCESSED', updated_at = now()
		WHERE id = $1 AND status = 'PENDING_REFERENCE'`, id)
	if err != nil {
		return false, fmt.Errorf("mark processed: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkRejected moves the row to REJECTED inside tx. Returns false if it already left PENDING_REFERENCE.
func (r *PendingRepo) MarkRejected(ctx context.Context, tx pgx.Tx, id, failureCode string) (bool, error) {
	tag, err := tx.Exec(ctx, `
		UPDATE wager_transactions SET status = 'REJECTED', failure_code = $2, updated_at = now()
		WHERE id = $1 AND status = 'PENDING_REFERENCE'`, id, failureCode)
	if err != nil {
		return false, fmt.Errorf("mark rejected: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
