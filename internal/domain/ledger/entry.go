// Package ledger models the append-only wallet ledger entries.
package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
)

// ErrInvalidEntry is returned when a ledger entry violates its invariants.
var ErrInvalidEntry = errors.New("ledger: invalid entry")

// Direction tells whether an entry takes money out of (DEBIT) or puts money into (CREDIT) a wallet.
type Direction string

const (
	DirectionDebit  Direction = "DEBIT"
	DirectionCredit Direction = "CREDIT"
)

// IsValid reports whether d is DEBIT or CREDIT.
func (d Direction) IsValid() bool {
	return d == DirectionDebit || d == DirectionCredit
}

// Entry is an immutable ledger line: one balance movement caused by one transaction.
type Entry struct {
	id            string
	walletID      string
	transactionID string
	direction     Direction
	amount        money.Money
	balanceBefore money.Money
	balanceAfter  money.Money
	createdAt     time.Time
}

// New validates and builds an entry. It is also used to rehydrate persisted entries.
// amount must be positive, balances must not be negative, all values must share one
// currency and balanceAfter must equal balanceBefore -/+ amount for DEBIT/CREDIT.
func New(id, walletID, transactionID string, direction Direction, amount, balanceBefore, balanceAfter money.Money, createdAt time.Time) (Entry, error) {
	if strings.TrimSpace(id) == "" {
		return Entry{}, fmt.Errorf("%w: id is required", ErrInvalidEntry)
	}
	if strings.TrimSpace(walletID) == "" {
		return Entry{}, fmt.Errorf("%w: wallet id is required", ErrInvalidEntry)
	}
	if strings.TrimSpace(transactionID) == "" {
		return Entry{}, fmt.Errorf("%w: transaction id is required", ErrInvalidEntry)
	}
	if !direction.IsValid() {
		return Entry{}, fmt.Errorf("%w: unknown direction %q", ErrInvalidEntry, direction)
	}
	for name, m := range map[string]money.Money{"amount": amount, "balance before": balanceBefore, "balance after": balanceAfter} {
		if _, err := money.FromCents(m.Cents(), m.Currency()); err != nil {
			return Entry{}, fmt.Errorf("%w: %s: %w", ErrInvalidEntry, name, err)
		}
	}
	if amount.Currency() != balanceBefore.Currency() || amount.Currency() != balanceAfter.Currency() {
		return Entry{}, fmt.Errorf("%w: %w: amount %s, before %s, after %s",
			ErrInvalidEntry, money.ErrCurrencyMismatch, amount, balanceBefore, balanceAfter)
	}
	if amount.Sign() <= 0 {
		return Entry{}, fmt.Errorf("%w: amount must be positive, got %s", ErrInvalidEntry, amount)
	}
	if balanceBefore.IsNegative() || balanceAfter.IsNegative() {
		return Entry{}, fmt.Errorf("%w: balances must not be negative (before %s, after %s)", ErrInvalidEntry, balanceBefore, balanceAfter)
	}

	var expected money.Money
	var err error
	if direction == DirectionDebit {
		expected, err = balanceBefore.Sub(amount)
	} else {
		expected, err = balanceBefore.Add(amount)
	}
	if err != nil {
		return Entry{}, fmt.Errorf("%w: %w", ErrInvalidEntry, err)
	}
	if !expected.Equals(balanceAfter) {
		return Entry{}, fmt.Errorf("%w: %s of %s on %s must result in %s, got %s",
			ErrInvalidEntry, direction, amount, balanceBefore, expected, balanceAfter)
	}
	if createdAt.IsZero() {
		return Entry{}, fmt.Errorf("%w: created at is required", ErrInvalidEntry)
	}

	return Entry{
		id:            id,
		walletID:      walletID,
		transactionID: transactionID,
		direction:     direction,
		amount:        amount,
		balanceBefore: balanceBefore,
		balanceAfter:  balanceAfter,
		createdAt:     createdAt,
	}, nil
}

// ID returns the entry identifier.
func (e Entry) ID() string { return e.id }

// WalletID returns the affected wallet.
func (e Entry) WalletID() string { return e.walletID }

// TransactionID returns the transaction that caused the movement.
func (e Entry) TransactionID() string { return e.transactionID }

// Direction returns DEBIT or CREDIT.
func (e Entry) Direction() Direction { return e.direction }

// Amount returns the moved amount (always positive).
func (e Entry) Amount() money.Money { return e.amount }

// BalanceBefore returns the wallet balance before the movement.
func (e Entry) BalanceBefore() money.Money { return e.balanceBefore }

// BalanceAfter returns the wallet balance after the movement.
func (e Entry) BalanceAfter() money.Money { return e.balanceAfter }

// CreatedAt returns the time the entry was recorded.
func (e Entry) CreatedAt() time.Time { return e.createdAt }
