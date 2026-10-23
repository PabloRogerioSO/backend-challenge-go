package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wallet"
)

const walletColumns = `id::text, player_id, currency, balance_cents, version, created_at, updated_at`

// WalletRepo persists wallets.
type WalletRepo struct{}

// NewWalletRepo returns a stateless wallet repository.
func NewWalletRepo() *WalletRepo { return &WalletRepo{} }

// Create inserts a new wallet.
func (r *WalletRepo) Create(ctx context.Context, tx pgx.Tx, w *wallet.Wallet) error {
	if w == nil {
		return fmt.Errorf("%w: nil wallet", ErrInvalidArgument)
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO wallets (id, player_id, currency, balance_cents, version, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		w.ID(), w.PlayerID(), w.Currency(), w.Balance().Cents(), w.Version(), w.CreatedAt(), w.UpdatedAt())
	if err != nil {
		return fmt.Errorf("postgres: create wallet %s: %w", w.ID(), err)
	}
	return nil
}

// GetByID loads a wallet without locking it.
func (r *WalletRepo) GetByID(ctx context.Context, q Querier, id string) (*wallet.Wallet, error) {
	return r.get(ctx, q, `SELECT `+walletColumns+` FROM wallets WHERE id = $1`, id)
}

// GetByIDForUpdate loads a wallet and locks its row until the transaction ends.
func (r *WalletRepo) GetByIDForUpdate(ctx context.Context, tx pgx.Tx, id string) (*wallet.Wallet, error) {
	return r.get(ctx, tx, `SELECT `+walletColumns+` FROM wallets WHERE id = $1 FOR UPDATE`, id)
}

// UpdateBalance persists exactly one domain change (Debit or Credit) of a wallet.
// The domain already bumped the version, so the row must still be at Version()-1;
// otherwise ErrOptimisticLock is returned.
func (r *WalletRepo) UpdateBalance(ctx context.Context, tx pgx.Tx, w *wallet.Wallet) error {
	if w == nil {
		return fmt.Errorf("%w: nil wallet", ErrInvalidArgument)
	}
	expected := w.Version() - 1
	if expected < 1 {
		return fmt.Errorf("%w: wallet %s at version %d has no pending change", ErrInvalidArgument, w.ID(), w.Version())
	}
	tag, err := tx.Exec(ctx,
		`UPDATE wallets
		    SET balance_cents = $3, version = $4, updated_at = $5
		  WHERE id = $1 AND version = $2`,
		w.ID(), expected, w.Balance().Cents(), w.Version(), w.UpdatedAt())
	if err != nil {
		return fmt.Errorf("postgres: update wallet %s: %w", w.ID(), err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: wallet %s expected version %d", ErrOptimisticLock, w.ID(), expected)
	}
	return nil
}

func (r *WalletRepo) get(ctx context.Context, q Querier, sql, id string) (*wallet.Wallet, error) {
	w, err := scanWallet(q.QueryRow(ctx, sql, id))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, fmt.Errorf("%w: wallet %s", ErrNotFound, id)
	case err != nil:
		return nil, fmt.Errorf("postgres: get wallet %s: %w", id, err)
	}
	return w, nil
}

func scanWallet(row pgx.Row) (*wallet.Wallet, error) {
	var (
		id, playerID, currency string
		cents, version         int64
		createdAt, updatedAt   time.Time
	)
	if err := row.Scan(&id, &playerID, &currency, &cents, &version, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	balance, err := money.FromCents(cents, currency)
	if err != nil {
		return nil, fmt.Errorf("invalid balance of wallet %s: %w", id, err)
	}
	return wallet.Rehydrate(id, playerID, balance, version, createdAt.UTC(), updatedAt.UTC())
}
