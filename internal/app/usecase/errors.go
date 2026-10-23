// Package usecase holds the transactional application rules: opening wallets,
// processing wagers and reconciling balances.
package usecase

import (
	"errors"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
)

var (
	// ErrInvalidInput is returned when the request itself is malformed.
	ErrInvalidInput = errors.New("usecase: invalid input")
	// ErrWalletNotFound is returned when the wallet does not exist.
	ErrWalletNotFound = errors.New("usecase: wallet not found")
	// ErrWalletAlreadyExists is returned when the player already has a wallet in that currency.
	ErrWalletAlreadyExists = errors.New("usecase: wallet already exists")
	// ErrIdempotencyConflict is returned when an idempotency key is reused with a different payload.
	ErrIdempotencyConflict = errors.New("usecase: idempotency key reused with a different payload")
	// ErrDuplicateExternalTransaction is returned when an external transaction id was already
	// registered under another idempotency key.
	ErrDuplicateExternalTransaction = errors.New("usecase: external transaction already registered with another idempotency key")
	// ErrInconsistentState is returned when persisted data violates an expected invariant.
	ErrInconsistentState = errors.New("usecase: inconsistent persisted state")
)

// DB is the database handle required by the use cases; *pgxpool.Pool implements it.
type DB interface {
	postgres.TxBeginner
	postgres.Querier
}
