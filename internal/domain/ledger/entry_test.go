package ledger

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
)

var t0 = time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)

func c(t *testing.T, cents int64) money.Money {
	t.Helper()
	m, err := money.FromCents(cents, "BRL")
	if err != nil {
		t.Fatalf("FromCents error: %v", err)
	}
	return m
}

func TestNewValid(t *testing.T) {
	tests := []struct {
		name                  string
		dir                   Direction
		amount, before, after int64
	}{
		{"debit", DirectionDebit, 250, 1000, 750},
		{"debit to zero", DirectionDebit, 250, 250, 0},
		{"credit", DirectionCredit, 250, 1000, 1250},
		{"credit from zero", DirectionCredit, 1, 0, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, err := New("e-1", "w-1", "tx-1", tc.dir, c(t, tc.amount), c(t, tc.before), c(t, tc.after), t0)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if e.ID() != "e-1" || e.WalletID() != "w-1" || e.TransactionID() != "tx-1" || e.Direction() != tc.dir {
				t.Fatalf("identity wrong: %+v", e)
			}
			if e.Amount().Cents() != tc.amount || e.BalanceBefore().Cents() != tc.before || e.BalanceAfter().Cents() != tc.after {
				t.Fatalf("amounts wrong: %v %v %v", e.Amount(), e.BalanceBefore(), e.BalanceAfter())
			}
			if !e.CreatedAt().Equal(t0) {
				t.Fatalf("created at = %s", e.CreatedAt())
			}
		})
	}
}

func TestNewInvalid(t *testing.T) {
	usd, err := money.FromCents(250, "USD")
	if err != nil {
		t.Fatalf("FromCents error: %v", err)
	}
	tests := []struct {
		name                    string
		id, wallet, transaction string
		dir                     Direction
		amount, before, after   money.Money
		at                      time.Time
	}{
		{"debit with credit math", "e", "w", "t", DirectionDebit, c(t, 250), c(t, 1000), c(t, 1250), t0},
		{"credit with debit math", "e", "w", "t", DirectionCredit, c(t, 250), c(t, 1000), c(t, 750), t0},
		{"wrong arithmetic", "e", "w", "t", DirectionDebit, c(t, 250), c(t, 1000), c(t, 700), t0},
		{"unchanged balance", "e", "w", "t", DirectionCredit, c(t, 250), c(t, 1000), c(t, 1000), t0},
		{"zero amount", "e", "w", "t", DirectionCredit, c(t, 0), c(t, 1000), c(t, 1000), t0},
		{"negative amount", "e", "w", "t", DirectionCredit, c(t, -250), c(t, 1000), c(t, 750), t0},
		{"negative balance before", "e", "w", "t", DirectionCredit, c(t, 1), c(t, -1), c(t, 0), t0},
		{"negative balance after", "e", "w", "t", DirectionDebit, c(t, 200), c(t, 100), c(t, -100), t0},
		{"unknown direction", "e", "w", "t", "SIDEWAYS", c(t, 250), c(t, 1000), c(t, 750), t0},
		{"empty direction", "e", "w", "t", "", c(t, 250), c(t, 1000), c(t, 750), t0},
		{"amount in other currency", "e", "w", "t", DirectionDebit, usd, c(t, 1000), c(t, 750), t0},
		{"balance after in other currency", "e", "w", "t", DirectionDebit, c(t, 250), c(t, 1000), usd, t0},
		{"amount without currency", "e", "w", "t", DirectionDebit, money.Money{}, c(t, 1000), c(t, 750), t0},
		{"empty id", "", "w", "t", DirectionDebit, c(t, 250), c(t, 1000), c(t, 750), t0},
		{"empty wallet id", "e", "", "t", DirectionDebit, c(t, 250), c(t, 1000), c(t, 750), t0},
		{"empty transaction id", "e", "w", "  ", DirectionDebit, c(t, 250), c(t, 1000), c(t, 750), t0},
		{"zero time", "e", "w", "t", DirectionDebit, c(t, 250), c(t, 1000), c(t, 750), time.Time{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.id, tc.wallet, tc.transaction, tc.dir, tc.amount, tc.before, tc.after, tc.at); !errors.Is(err, ErrInvalidEntry) {
				t.Fatalf("error = %v, want ErrInvalidEntry", err)
			}
		})
	}
}

func TestNewCurrencyMismatchIsReported(t *testing.T) {
	usd, err := money.FromCents(250, "USD")
	if err != nil {
		t.Fatalf("FromCents error: %v", err)
	}
	_, err = New("e", "w", "t", DirectionDebit, usd, c(t, 1000), c(t, 750), t0)
	if !errors.Is(err, ErrInvalidEntry) || !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Fatalf("error = %v, want ErrInvalidEntry and ErrCurrencyMismatch", err)
	}
}

func TestNewOverflowIsReported(t *testing.T) {
	_, err := New("e", "w", "t", DirectionCredit, c(t, 1), c(t, math.MaxInt64), c(t, math.MaxInt64), t0)
	if !errors.Is(err, ErrInvalidEntry) || !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("error = %v, want ErrInvalidEntry and ErrOverflow", err)
	}
}

func TestDirectionIsValid(t *testing.T) {
	if !DirectionDebit.IsValid() || !DirectionCredit.IsValid() {
		t.Fatal("DEBIT and CREDIT must be valid")
	}
	for _, d := range []Direction{"", "debit", "CREDITS"} {
		if d.IsValid() {
			t.Fatalf("%q must be invalid", d)
		}
	}
}
