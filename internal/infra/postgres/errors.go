// Package postgres implements persistence with pgx/v5 and explicit SQL.
// It maps database rows to domain types; the domain never imports this package.
package postgres

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// ErrNotFound is returned when a row does not exist.
	ErrNotFound = errors.New("postgres: not found")
	// ErrOptimisticLock is returned when a versioned update affected no row.
	ErrOptimisticLock = errors.New("postgres: optimistic lock conflict")
	// ErrInvalidCursor is returned for malformed pagination cursors.
	ErrInvalidCursor = errors.New("postgres: invalid cursor")
	// ErrInvalidArgument is returned for invalid inputs detected before hitting the database.
	ErrInvalidArgument = errors.New("postgres: invalid argument")
)

// Names of the unique constraints/indexes that callers may need to recognize.
const (
	ConstraintWalletsPlayerCurrency   = "uq_wallets_player_currency"
	ConstraintWagerProviderExternal   = "uq_wager_transactions_provider_external"
	ConstraintWagerIdempotencyKey     = "uq_wager_transactions_idempotency_key"
	ConstraintLedgerWalletTransaction = "uq_wallet_ledger_entries_wallet_transaction"
	ConstraintInboxConsumerMessage    = "uq_inbox_consumer_message"
	ConstraintOutboxEventID           = "uq_outbox_event_id"
)

const (
	sqlStateUniqueViolation = "23505"
	sqlStateCheckViolation  = "23514"
)

func pgErrorWithCode(err error, code string) *pgconn.PgError {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == code {
		return pgErr
	}
	return nil
}

// IsUniqueViolation reports whether err is a unique violation. When constraint
// is not empty, the violated constraint (or index) name must match it.
func IsUniqueViolation(err error, constraint string) bool {
	pgErr := pgErrorWithCode(err, sqlStateUniqueViolation)
	return pgErr != nil && (constraint == "" || pgErr.ConstraintName == constraint)
}

// IsCheckViolation reports whether err is a CHECK violation. When constraint
// is not empty, the violated constraint name must match it.
func IsCheckViolation(err error, constraint string) bool {
	pgErr := pgErrorWithCode(err, sqlStateCheckViolation)
	return pgErr != nil && (constraint == "" || pgErr.ConstraintName == constraint)
}
