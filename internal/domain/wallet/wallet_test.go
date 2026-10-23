package wallet

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func cents(t *testing.T, v int64, currency string) money.Money {
	t.Helper()
	m, err := money.FromCents(v, currency)
	if err != nil {
		t.Fatalf("FromCents(%d, %q) error: %v", v, currency, err)
	}
	return m
}

func newWallet(t *testing.T, balance int64) *Wallet {
	t.Helper()
	w, err := New("w-1", "player-1", cents(t, balance, "BRL"), t0)
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	return w
}

func setClock(t *testing.T, at time.Time) {
	t.Helper()
	old := clock
	clock = func() time.Time { return at }
	t.Cleanup(func() { clock = old })
}

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		player  string
		balance money.Money
		at      time.Time
		wantErr bool
	}{
		{"valid zero balance", "w-1", "p-1", cents(t, 0, "BRL"), t0, false},
		{"valid positive balance", "w-1", "p-1", cents(t, 5000, "USD"), t0, false},
		{"empty id", "", "p-1", cents(t, 0, "BRL"), t0, true},
		{"blank player id", "w-1", "   ", cents(t, 0, "BRL"), t0, true},
		{"empty player id", "w-1", "", cents(t, 0, "BRL"), t0, true},
		{"player id too long", "w-1", strings.Repeat("p", 65), cents(t, 0, "BRL"), t0, true},
		{"negative balance", "w-1", "p-1", cents(t, -1, "BRL"), t0, true},
		{"zero value money", "w-1", "p-1", money.Money{}, t0, true},
		{"unknown currency", "w-1", "p-1", money.Zero("XXX"), t0, true},
		{"zero time", "w-1", "p-1", cents(t, 0, "BRL"), time.Time{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, err := New(tc.id, tc.player, tc.balance, tc.at)
			if tc.wantErr {
				if !errors.Is(err, ErrInvalidWallet) {
					t.Fatalf("error = %v, want ErrInvalidWallet", err)
				}
				if w != nil {
					t.Fatal("wallet must be nil on error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if w.ID() != tc.id || w.PlayerID() != tc.player || w.Currency() != tc.balance.Currency() {
				t.Fatalf("unexpected identity: %s %s %s", w.ID(), w.PlayerID(), w.Currency())
			}
			if !w.Balance().Equals(tc.balance) || w.Version() != 1 {
				t.Fatalf("balance=%s version=%d", w.Balance(), w.Version())
			}
			if !w.CreatedAt().Equal(tc.at) || !w.UpdatedAt().Equal(tc.at) {
				t.Fatalf("timestamps = %s / %s", w.CreatedAt(), w.UpdatedAt())
			}
		})
	}
}

func TestRehydrate(t *testing.T) {
	later := t0.Add(time.Hour)

	w, err := Rehydrate("w-9", "p-9", cents(t, 700, "BRL"), 7, t0, later)
	if err != nil {
		t.Fatalf("Rehydrate error: %v", err)
	}
	if w.ID() != "w-9" || w.Version() != 7 || w.Balance().Cents() != 700 || !w.UpdatedAt().Equal(later) {
		t.Fatalf("unexpected rehydrated wallet: %+v", w)
	}

	tests := []struct {
		name      string
		id        string
		balance   money.Money
		version   int64
		createdAt time.Time
		updatedAt time.Time
	}{
		{"empty id", "", cents(t, 0, "BRL"), 1, t0, t0},
		{"version zero", "w-1", cents(t, 0, "BRL"), 0, t0, t0},
		{"negative version", "w-1", cents(t, 0, "BRL"), -3, t0, t0},
		{"negative balance", "w-1", cents(t, -10, "BRL"), 1, t0, t0},
		{"updated before created", "w-1", cents(t, 0, "BRL"), 1, later, t0},
		{"zero created at", "w-1", cents(t, 0, "BRL"), 1, time.Time{}, t0},
		{"missing currency", "w-1", money.Money{}, 1, t0, t0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Rehydrate(tc.id, "p-1", tc.balance, tc.version, tc.createdAt, tc.updatedAt); !errors.Is(err, ErrInvalidWallet) {
				t.Fatalf("error = %v, want ErrInvalidWallet", err)
			}
		})
	}
}

func TestDebit(t *testing.T) {
	ctx := context.Background()
	at := t0.Add(time.Minute)
	setClock(t, at)

	t.Run("reduces balance and bumps version", func(t *testing.T) {
		w := newWallet(t, 1000)
		if err := w.Debit(ctx, cents(t, 250, "BRL")); err != nil {
			t.Fatalf("Debit error: %v", err)
		}
		if w.Balance().Cents() != 750 || w.Version() != 2 || !w.UpdatedAt().Equal(at) {
			t.Fatalf("balance=%s version=%d updated=%s", w.Balance(), w.Version(), w.UpdatedAt())
		}
	})

	t.Run("exact balance leaves zero", func(t *testing.T) {
		w := newWallet(t, 500)
		if err := w.Debit(ctx, cents(t, 500, "BRL")); err != nil {
			t.Fatalf("Debit error: %v", err)
		}
		if !w.Balance().IsZero() || w.Version() != 2 {
			t.Fatalf("balance=%s version=%d", w.Balance(), w.Version())
		}
	})

	t.Run("insufficient funds leaves wallet untouched", func(t *testing.T) {
		w := newWallet(t, 100)
		err := w.Debit(ctx, cents(t, 101, "BRL"))
		if !errors.Is(err, ErrInsufficientFunds) {
			t.Fatalf("error = %v, want ErrInsufficientFunds", err)
		}
		if w.Balance().Cents() != 100 || w.Version() != 1 || !w.UpdatedAt().Equal(t0) {
			t.Fatalf("wallet mutated: balance=%s version=%d", w.Balance(), w.Version())
		}
	})

	t.Run("non-positive amount", func(t *testing.T) {
		for _, v := range []int64{0, -1} {
			w := newWallet(t, 100)
			if err := w.Debit(ctx, cents(t, v, "BRL")); !errors.Is(err, ErrInvalidAmount) {
				t.Fatalf("amount %d error = %v, want ErrInvalidAmount", v, err)
			}
			if w.Version() != 1 {
				t.Fatalf("version changed for amount %d", v)
			}
		}
	})

	t.Run("currency mismatch", func(t *testing.T) {
		w := newWallet(t, 1000)
		if err := w.Debit(ctx, cents(t, 10, "USD")); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("error = %v, want ErrCurrencyMismatch", err)
		}
		if w.Balance().Cents() != 1000 || w.Version() != 1 {
			t.Fatal("wallet mutated on currency mismatch")
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		w := newWallet(t, 1000)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := w.Debit(cctx, cents(t, 10, "BRL")); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if w.Balance().Cents() != 1000 || w.Version() != 1 {
			t.Fatal("wallet mutated on cancelled context")
		}
	})
}

func TestCredit(t *testing.T) {
	ctx := context.Background()
	at := t0.Add(2 * time.Minute)
	setClock(t, at)

	t.Run("increases balance and bumps version", func(t *testing.T) {
		w := newWallet(t, 1000)
		if err := w.Credit(ctx, cents(t, 250, "BRL")); err != nil {
			t.Fatalf("Credit error: %v", err)
		}
		if w.Balance().Cents() != 1250 || w.Version() != 2 || !w.UpdatedAt().Equal(at) {
			t.Fatalf("balance=%s version=%d updated=%s", w.Balance(), w.Version(), w.UpdatedAt())
		}
	})

	t.Run("overflow leaves wallet untouched", func(t *testing.T) {
		w := newWallet(t, math.MaxInt64)
		if err := w.Credit(ctx, cents(t, 1, "BRL")); !errors.Is(err, money.ErrOverflow) {
			t.Fatalf("error = %v, want ErrOverflow", err)
		}
		if w.Balance().Cents() != math.MaxInt64 || w.Version() != 1 {
			t.Fatal("wallet mutated on overflow")
		}
	})

	t.Run("non-positive amount", func(t *testing.T) {
		for _, v := range []int64{0, -5} {
			w := newWallet(t, 100)
			if err := w.Credit(ctx, cents(t, v, "BRL")); !errors.Is(err, ErrInvalidAmount) {
				t.Fatalf("amount %d error = %v, want ErrInvalidAmount", v, err)
			}
		}
	})

	t.Run("currency mismatch", func(t *testing.T) {
		w := newWallet(t, 100)
		if err := w.Credit(ctx, cents(t, 10, "EUR")); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("error = %v, want ErrCurrencyMismatch", err)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		w := newWallet(t, 100)
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		if err := w.Credit(cctx, cents(t, 10, "BRL")); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})
}

func TestVersionIncrementsOnEveryChange(t *testing.T) {
	ctx := context.Background()
	w := newWallet(t, 1000)
	steps := []struct {
		credit bool
		cents  int64
	}{{true, 100}, {false, 50}, {true, 1}, {false, 1051}}
	for i, s := range steps {
		var err error
		if s.credit {
			err = w.Credit(ctx, cents(t, s.cents, "BRL"))
		} else {
			err = w.Debit(ctx, cents(t, s.cents, "BRL"))
		}
		if err != nil {
			t.Fatalf("step %d error: %v", i, err)
		}
		if want := int64(i + 2); w.Version() != want {
			t.Fatalf("step %d version = %d, want %d", i, w.Version(), want)
		}
	}
	if !w.Balance().IsZero() {
		t.Fatalf("final balance = %s, want 0.00 BRL", w.Balance())
	}
}
