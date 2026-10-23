package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wagering"
)

const wagerColumns = `id::text, wallet_id::text, provider_id, external_transaction_id, idempotency_key, request_hash,
	kind, status, amount_cents, currency, round_id, game_id, reference_external_transaction_id,
	resolved_reference_transaction_id::text, failure_code, result_balance_cents, created_at, updated_at`

// WageringRepo persists wager transactions.
type WageringRepo struct{}

// NewWageringRepo returns a stateless wager transaction repository.
func NewWageringRepo() *WageringRepo { return &WageringRepo{} }

// Create inserts a wager transaction. Unique violations on
// ConstraintWagerProviderExternal / ConstraintWagerIdempotencyKey are returned
// wrapped; use IsUniqueViolation to detect them.
func (r *WageringRepo) Create(ctx context.Context, tx pgx.Tx, t *wagering.WagerTransaction) error {
	if t == nil {
		return fmt.Errorf("%w: nil wager transaction", ErrInvalidArgument)
	}
	if err := t.Validate(); err != nil {
		return fmt.Errorf("postgres: create wager transaction %s: %w", t.ID, err)
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO wager_transactions (
		     id, wallet_id, provider_id, external_transaction_id, idempotency_key, request_hash,
		     kind, status, amount_cents, currency, round_id, game_id,
		     reference_external_transaction_id, resolved_reference_transaction_id,
		     failure_code, result_balance_cents, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		t.ID, t.WalletID,
		nullIfEmpty(t.ProviderID), nullIfEmpty(t.ExternalTransactionID),
		nullIfEmpty(t.IdempotencyKey), nullIfEmpty(t.RequestHash),
		string(t.Kind), string(t.Status), t.Amount.Cents(), t.Amount.Currency(),
		nullIfEmpty(t.RoundID), nullIfEmpty(t.GameID),
		nullIfEmpty(t.ReferenceExternalTransactionID), nullIfEmpty(t.ResolvedReferenceTransactionID),
		nullIfEmpty(t.FailureCode), resultBalanceCents(t),
		t.CreatedAt, t.UpdatedAt)
	if err != nil {
		return fmt.Errorf("postgres: create wager transaction %s: %w", t.ID, err)
	}
	return nil
}

// GetByID loads a transaction by its internal id.
func (r *WageringRepo) GetByID(ctx context.Context, q Querier, id string) (*wagering.WagerTransaction, error) {
	return queryWager(ctx, q, `SELECT `+wagerColumns+` FROM wager_transactions WHERE id = $1`,
		"id "+id, id)
}

// GetByIdempotencyKey loads a transaction by its idempotency key.
func (r *WageringRepo) GetByIdempotencyKey(ctx context.Context, q Querier, key string) (*wagering.WagerTransaction, error) {
	return queryWager(ctx, q, `SELECT `+wagerColumns+` FROM wager_transactions WHERE idempotency_key = $1`,
		"idempotency key "+key, key)
}

// GetByProviderAndExternal loads a transaction by provider and external transaction id.
func (r *WageringRepo) GetByProviderAndExternal(ctx context.Context, q Querier, providerID, externalID string) (*wagering.WagerTransaction, error) {
	return queryWager(ctx, q,
		`SELECT `+wagerColumns+` FROM wager_transactions WHERE provider_id = $1 AND external_transaction_id = $2`,
		"provider "+providerID+" external "+externalID, providerID, externalID)
}

// Update persists the mutable state of a transaction: status, failure code,
// result balance, resolved reference and updated_at.
func (r *WageringRepo) Update(ctx context.Context, tx pgx.Tx, t *wagering.WagerTransaction) error {
	if t == nil {
		return fmt.Errorf("%w: nil wager transaction", ErrInvalidArgument)
	}
	if err := t.Validate(); err != nil {
		return fmt.Errorf("postgres: update wager transaction %s: %w", t.ID, err)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE wager_transactions
		    SET status = $2,
		        failure_code = $3,
		        result_balance_cents = $4,
		        resolved_reference_transaction_id = $5,
		        updated_at = $6
		  WHERE id = $1`,
		t.ID, string(t.Status), nullIfEmpty(t.FailureCode), resultBalanceCents(t),
		nullIfEmpty(t.ResolvedReferenceTransactionID), t.UpdatedAt)
	if err != nil {
		return fmt.Errorf("postgres: update wager transaction %s: %w", t.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: wager transaction %s", ErrNotFound, t.ID)
	}
	return nil
}

func queryWager(ctx context.Context, q Querier, sql, what string, args ...any) (*wagering.WagerTransaction, error) {
	t, err := scanWager(q.QueryRow(ctx, sql, args...))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("%w: wager transaction with %s", ErrNotFound, what)
	case err != nil:
		return nil, fmt.Errorf("postgres: get wager transaction with %s: %w", what, err)
	}
	return t, nil
}

func scanWager(row pgx.Row) (*wagering.WagerTransaction, error) {
	var (
		t                                                             wagering.WagerTransaction
		kind, status, currency                                        string
		amountCents                                                   int64
		providerID, externalID, idemKey, requestHash, roundID, gameID *string
		referenceExternalID, resolvedReferenceID, failureCode         *string
		resultCents                                                   *int64
		createdAt, updatedAt                                          time.Time
	)
	if err := row.Scan(&t.ID, &t.WalletID, &providerID, &externalID, &idemKey, &requestHash,
		&kind, &status, &amountCents, &currency, &roundID, &gameID, &referenceExternalID,
		&resolvedReferenceID, &failureCode, &resultCents, &createdAt, &updatedAt); err != nil {
		return nil, err
	}

	amount, err := money.FromCents(amountCents, currency)
	if err != nil {
		return nil, fmt.Errorf("invalid amount of wager transaction %s: %w", t.ID, err)
	}
	t.Kind = wagering.Kind(kind)
	t.Status = wagering.Status(status)
	t.Amount = amount
	t.ProviderID = derefString(providerID)
	t.ExternalTransactionID = derefString(externalID)
	t.IdempotencyKey = derefString(idemKey)
	t.RequestHash = derefString(requestHash)
	t.RoundID = derefString(roundID)
	t.GameID = derefString(gameID)
	t.ReferenceExternalTransactionID = derefString(referenceExternalID)
	t.ResolvedReferenceTransactionID = derefString(resolvedReferenceID)
	t.FailureCode = derefString(failureCode)
	if resultCents != nil {
		result, err := money.FromCents(*resultCents, currency)
		if err != nil {
			return nil, fmt.Errorf("invalid result balance of wager transaction %s: %w", t.ID, err)
		}
		t.ResultBalance = &result
	}
	t.CreatedAt = createdAt.UTC()
	t.UpdatedAt = updatedAt.UTC()

	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("stored wager transaction %s violates domain invariants: %w", t.ID, err)
	}
	return &t, nil
}

func resultBalanceCents(t *wagering.WagerTransaction) any {
	if t.ResultBalance == nil {
		return nil
	}
	return t.ResultBalance.Cents()
}

// nullIfEmpty maps the domain's empty string to SQL NULL.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
