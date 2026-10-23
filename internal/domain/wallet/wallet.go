// Package wallet holds the Wallet aggregate: a player's balance in one currency.
package wallet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
)

const maxPlayerIDLength = 64

var (
	// ErrInvalidWallet is returned when a wallet violates its invariants.
	ErrInvalidWallet = errors.New("wallet: invalid wallet")
	// ErrInvalidAmount is returned when a debit or credit amount is not positive.
	ErrInvalidAmount = errors.New("wallet: invalid amount")
	// ErrInsufficientFunds is returned when a debit exceeds the balance.
	ErrInsufficientFunds = errors.New("wallet: insufficient funds")
)

// clock provides the time used to stamp updates; tests replace it.
var clock = func() time.Time { return time.Now().UTC() }

// Wallet is a player's balance in a single currency. The balance is never negative.
type Wallet struct {
	id        string
	playerID  string
	balance   money.Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

// New creates a wallet with version 1. The wallet currency is the currency of initialBalance.
func New(id, playerID string, initialBalance money.Money, now time.Time) (*Wallet, error) {
	w := &Wallet{
		id:        id,
		playerID:  playerID,
		balance:   initialBalance,
		version:   1,
		createdAt: now,
		updatedAt: now,
	}
	if err := w.validate(); err != nil {
		return nil, err
	}
	return w, nil
}

// Rehydrate rebuilds a wallet from persisted state, enforcing the same invariants.
func Rehydrate(id, playerID string, balance money.Money, version int64, createdAt, updatedAt time.Time) (*Wallet, error) {
	w := &Wallet{
		id:        id,
		playerID:  playerID,
		balance:   balance,
		version:   version,
		createdAt: createdAt,
		updatedAt: updatedAt,
	}
	if err := w.validate(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Wallet) validate() error {
	if strings.TrimSpace(w.id) == "" {
		return fmt.Errorf("%w: id is required", ErrInvalidWallet)
	}
	if strings.TrimSpace(w.playerID) == "" {
		return fmt.Errorf("%w: player id is required", ErrInvalidWallet)
	}
	if utf8.RuneCountInString(w.playerID) > maxPlayerIDLength {
		return fmt.Errorf("%w: player id exceeds %d characters", ErrInvalidWallet, maxPlayerIDLength)
	}
	if _, err := money.FromCents(w.balance.Cents(), w.balance.Currency()); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidWallet, err)
	}
	if w.balance.IsNegative() {
		return fmt.Errorf("%w: balance must not be negative, got %s", ErrInvalidWallet, w.balance)
	}
	if w.version < 1 {
		return fmt.Errorf("%w: version must be >= 1, got %d", ErrInvalidWallet, w.version)
	}
	if w.createdAt.IsZero() {
		return fmt.Errorf("%w: created at is required", ErrInvalidWallet)
	}
	if w.updatedAt.Before(w.createdAt) {
		return fmt.Errorf("%w: updated at is before created at", ErrInvalidWallet)
	}
	return nil
}

// ID returns the wallet identifier.
func (w *Wallet) ID() string { return w.id }

// PlayerID returns the owner identifier.
func (w *Wallet) PlayerID() string { return w.playerID }

// Currency returns the wallet currency.
func (w *Wallet) Currency() string { return w.balance.Currency() }

// Balance returns the current balance.
func (w *Wallet) Balance() money.Money { return w.balance }

// Version returns the optimistic version, incremented on every balance change.
func (w *Wallet) Version() int64 { return w.version }

// CreatedAt returns the creation time.
func (w *Wallet) CreatedAt() time.Time { return w.createdAt }

// UpdatedAt returns the time of the last balance change.
func (w *Wallet) UpdatedAt() time.Time { return w.updatedAt }

// Debit subtracts amount from the balance. The wallet is unchanged on error.
func (w *Wallet) Debit(ctx context.Context, amount money.Money) error {
	if err := w.checkMovement(ctx, "debit", amount); err != nil {
		return err
	}
	if w.balance.Cents() < amount.Cents() {
		return fmt.Errorf("%w: balance %s, debit %s", ErrInsufficientFunds, w.balance, amount)
	}
	next, err := w.balance.Sub(amount)
	if err != nil {
		return fmt.Errorf("wallet: debit: %w", err)
	}
	w.apply(next)
	return nil
}

// Credit adds amount to the balance. The wallet is unchanged on error.
func (w *Wallet) Credit(ctx context.Context, amount money.Money) error {
	if err := w.checkMovement(ctx, "credit", amount); err != nil {
		return err
	}
	next, err := w.balance.Add(amount)
	if err != nil {
		return fmt.Errorf("wallet: credit: %w", err)
	}
	w.apply(next)
	return nil
}

func (w *Wallet) checkMovement(ctx context.Context, op string, amount money.Money) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("wallet: %s: %w", op, err)
	}
	if amount.Currency() != w.balance.Currency() {
		return fmt.Errorf("%w: wallet %s, amount %s", money.ErrCurrencyMismatch, w.balance.Currency(), amount)
	}
	if amount.Sign() <= 0 {
		return fmt.Errorf("%w: %s amount must be positive, got %s", ErrInvalidAmount, op, amount)
	}
	return nil
}

func (w *Wallet) apply(next money.Money) {
	w.balance = next
	w.version++
	w.updatedAt = clock()
}
