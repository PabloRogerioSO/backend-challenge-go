package wagering

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
)

var t0 = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

func amt(t *testing.T, cents int64) money.Money {
	t.Helper()
	m, err := money.FromCents(cents, "BRL")
	if err != nil {
		t.Fatalf("FromCents error: %v", err)
	}
	return m
}

// setClock makes every clock() call return a strictly later time.
func setClock(t *testing.T) {
	t.Helper()
	old := clock
	n := 0
	clock = func() time.Time {
		n++
		return t0.Add(time.Duration(n) * time.Minute)
	}
	t.Cleanup(func() { clock = old })
}

func extParams(t *testing.T, kind Kind, cents int64) NewExternalParams {
	t.Helper()
	p := NewExternalParams{
		ID:                    "tx-1",
		WalletID:              "w-1",
		Kind:                  kind,
		Amount:                amt(t, cents),
		ProviderID:            "provider-a",
		ExternalTransactionID: "ext-1",
		IdempotencyKey:        "idem-1",
		RequestHash:           "hash-1",
		RoundID:               "round-1",
		GameID:                "game-1",
		Now:                   t0,
	}
	if kind.RequiresReference() {
		p.ReferenceExternalTransactionID = "ext-0"
	}
	return p
}

func buildAt(t *testing.T, s Status) *WagerTransaction {
	t.Helper()
	tx, err := NewExternal(extParams(t, KindRefund, 500))
	if err != nil {
		t.Fatalf("NewExternal error: %v", err)
	}
	switch s {
	case StatusPending:
	case StatusPendingReference:
		err = tx.MarkPendingReference()
	case StatusProcessed:
		err = tx.MarkProcessed(amt(t, 1000))
	case StatusRejected:
		err = tx.MarkRejected("REJECTED_CODE")
	case StatusFailed:
		err = tx.MarkFailed("FAILED_CODE")
	}
	if err != nil {
		t.Fatalf("building status %s: %v", s, err)
	}
	return tx
}

func TestKindAndStatusHelpers(t *testing.T) {
	for _, k := range []Kind{KindOpening, KindBet, KindWin, KindLoss, KindRefund, KindRollback} {
		if !k.IsValid() {
			t.Errorf("%s should be valid", k)
		}
	}
	for _, k := range []Kind{"", "REVERSAL", "bet"} {
		if k.IsValid() || k.IsExternal() {
			t.Errorf("%q should be invalid", k)
		}
	}
	if KindOpening.IsExternal() || !KindBet.IsExternal() {
		t.Error("IsExternal wrong")
	}
	if !KindRefund.RequiresReference() || !KindRollback.RequiresReference() || KindBet.RequiresReference() {
		t.Error("RequiresReference wrong")
	}
	for _, s := range []Status{StatusPending, StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed} {
		if !s.IsValid() {
			t.Errorf("%s should be valid", s)
		}
	}
	for _, s := range []Status{"", "COMPLETED", "REVERSED"} {
		if s.IsValid() {
			t.Errorf("%q should be invalid", s)
		}
	}
	terminal := map[Status]bool{StatusProcessed: true, StatusRejected: true, StatusFailed: true}
	for _, s := range []Status{StatusPending, StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed} {
		if s.IsTerminal() != terminal[s] {
			t.Errorf("IsTerminal(%s) = %v", s, s.IsTerminal())
		}
	}
}

func TestNewExternalValid(t *testing.T) {
	tests := []struct {
		kind  Kind
		cents int64
	}{
		{KindBet, 500},
		{KindWin, 700},
		{KindLoss, 0},
		{KindRefund, 500},
		{KindRollback, 500},
	}
	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			p := extParams(t, tc.kind, tc.cents)
			tx, err := NewExternal(p)
			if err != nil {
				t.Fatalf("NewExternal error: %v", err)
			}
			if tx.Status != StatusPending || tx.Kind != tc.kind || tx.ResultBalance != nil {
				t.Fatalf("status=%s kind=%s result=%v", tx.Status, tx.Kind, tx.ResultBalance)
			}
			if tx.ID != p.ID || tx.WalletID != p.WalletID || tx.ProviderID != p.ProviderID ||
				tx.ExternalTransactionID != p.ExternalTransactionID || tx.IdempotencyKey != p.IdempotencyKey ||
				tx.RequestHash != p.RequestHash || tx.RoundID != p.RoundID || tx.GameID != p.GameID ||
				tx.ReferenceExternalTransactionID != p.ReferenceExternalTransactionID {
				t.Fatalf("fields not copied: %+v", tx)
			}
			if !tx.Amount.Equals(p.Amount) || !tx.CreatedAt.Equal(t0) || !tx.UpdatedAt.Equal(t0) {
				t.Fatalf("amount/timestamps wrong: %+v", tx)
			}
		})
	}
}

func TestNewExternalInvalid(t *testing.T) {
	tests := []struct {
		name   string
		kind   Kind
		cents  int64
		mutate func(*NewExternalParams)
	}{
		{"opening kind", KindOpening, 100, nil},
		{"unknown kind", "FOO", 100, nil},
		{"empty kind", "", 100, nil},
		{"loss with positive amount", KindLoss, 100, nil},
		{"bet zero", KindBet, 0, nil},
		{"win negative", KindWin, -100, nil},
		{"refund zero", KindRefund, 0, nil},
		{"missing provider", KindBet, 100, func(p *NewExternalParams) { p.ProviderID = "" }},
		{"blank provider", KindBet, 100, func(p *NewExternalParams) { p.ProviderID = "  " }},
		{"missing external id", KindBet, 100, func(p *NewExternalParams) { p.ExternalTransactionID = "" }},
		{"missing idempotency key", KindBet, 100, func(p *NewExternalParams) { p.IdempotencyKey = "" }},
		{"missing request hash", KindBet, 100, func(p *NewExternalParams) { p.RequestHash = "" }},
		{"refund without reference", KindRefund, 100, func(p *NewExternalParams) { p.ReferenceExternalTransactionID = "" }},
		{"rollback without reference", KindRollback, 100, func(p *NewExternalParams) { p.ReferenceExternalTransactionID = "" }},
		{"empty id", KindBet, 100, func(p *NewExternalParams) { p.ID = "" }},
		{"empty wallet id", KindBet, 100, func(p *NewExternalParams) { p.WalletID = "" }},
		{"amount without currency", KindBet, 100, func(p *NewExternalParams) { p.Amount = money.Money{} }},
		{"external id too long", KindBet, 100, func(p *NewExternalParams) { p.ExternalTransactionID = strings.Repeat("x", 129) }},
		{"provider id too long", KindBet, 100, func(p *NewExternalParams) { p.ProviderID = strings.Repeat("x", 65) }},
		{"round id too long", KindBet, 100, func(p *NewExternalParams) { p.RoundID = strings.Repeat("x", 129) }},
		{"zero time", KindBet, 100, func(p *NewExternalParams) { p.Now = time.Time{} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := extParams(t, tc.kind, tc.cents)
			if tc.mutate != nil {
				tc.mutate(&p)
			}
			tx, err := NewExternal(p)
			if !errors.Is(err, ErrInvalidTransaction) {
				t.Fatalf("error = %v, want ErrInvalidTransaction", err)
			}
			if tx != nil {
				t.Fatal("transaction must be nil on error")
			}
		})
	}
}

func TestNewOpening(t *testing.T) {
	tx, err := NewOpening("tx-open", "w-1", amt(t, 10000), t0)
	if err != nil {
		t.Fatalf("NewOpening error: %v", err)
	}
	if tx.Kind != KindOpening || tx.Status != StatusProcessed {
		t.Fatalf("kind=%s status=%s", tx.Kind, tx.Status)
	}
	if tx.ProviderID != "" || tx.ExternalTransactionID != "" || tx.IdempotencyKey != "" || tx.RequestHash != "" {
		t.Fatalf("opening must not carry external identity: %+v", tx)
	}
	if tx.ResultBalance == nil || !tx.ResultBalance.Equals(amt(t, 10000)) {
		t.Fatalf("result balance = %v", tx.ResultBalance)
	}

	invalid := []struct {
		name    string
		id, wal string
		amount  money.Money
		at      time.Time
	}{
		{"empty id", "", "w-1", amt(t, 1), t0},
		{"empty wallet", "tx", "", amt(t, 1), t0},
		{"zero amount", "tx", "w-1", amt(t, 0), t0},
		{"negative amount", "tx", "w-1", amt(t, -1), t0},
		{"no currency", "tx", "w-1", money.Money{}, t0},
		{"zero time", "tx", "w-1", amt(t, 1), time.Time{}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewOpening(tc.id, tc.wal, tc.amount, tc.at); !errors.Is(err, ErrInvalidTransaction) {
				t.Fatalf("error = %v, want ErrInvalidTransaction", err)
			}
		})
	}
}

func TestOpeningIsTerminal(t *testing.T) {
	setClock(t)
	tx, err := NewOpening("tx-open", "w-1", amt(t, 100), t0)
	if err != nil {
		t.Fatalf("NewOpening error: %v", err)
	}
	for name, err := range map[string]error{
		"processed":         tx.MarkProcessed(amt(t, 1)),
		"rejected":          tx.MarkRejected("X"),
		"failed":            tx.MarkFailed("X"),
		"pending_reference": tx.MarkPendingReference(),
	} {
		if !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("%s: error = %v, want ErrInvalidTransition", name, err)
		}
	}
	if tx.Status != StatusProcessed {
		t.Fatalf("status = %s", tx.Status)
	}
}

func TestTransitionMatrix(t *testing.T) {
	allowed := func(from, to Status) bool {
		switch from {
		case StatusPending:
			return true
		case StatusPendingReference:
			return to != StatusPendingReference
		}
		return false
	}
	balance := amt(t, 1000)
	actions := []struct {
		name string
		to   Status
		run  func(*WagerTransaction) error
	}{
		{"processed", StatusProcessed, func(tx *WagerTransaction) error { return tx.MarkProcessed(balance) }},
		{"rejected", StatusRejected, func(tx *WagerTransaction) error { return tx.MarkRejected("INSUFFICIENT_FUNDS") }},
		{"failed", StatusFailed, func(tx *WagerTransaction) error { return tx.MarkFailed("INTERNAL_ERROR") }},
		{"pending_reference", StatusPendingReference, func(tx *WagerTransaction) error { return tx.MarkPendingReference() }},
	}
	froms := []Status{StatusPending, StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed}

	for _, from := range froms {
		for _, act := range actions {
			t.Run(string(from)+"_to_"+act.name, func(t *testing.T) {
				setClock(t)
				tx := buildAt(t, from)
				before := *tx

				err := act.run(tx)
				if allowed(from, act.to) {
					if err != nil {
						t.Fatalf("unexpected error: %v", err)
					}
					if tx.Status != act.to {
						t.Fatalf("status = %s, want %s", tx.Status, act.to)
					}
					if !tx.UpdatedAt.After(before.UpdatedAt) {
						t.Fatalf("UpdatedAt not advanced: %s -> %s", before.UpdatedAt, tx.UpdatedAt)
					}
					switch act.to {
					case StatusProcessed:
						if tx.ResultBalance == nil || !tx.ResultBalance.Equals(balance) {
							t.Fatalf("result balance = %v", tx.ResultBalance)
						}
					case StatusRejected, StatusFailed:
						if tx.FailureCode == "" {
							t.Fatal("failure code not stored")
						}
					}
					if err := tx.Validate(); err != nil {
						t.Fatalf("Validate after transition: %v", err)
					}
					return
				}
				if !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("error = %v, want ErrInvalidTransition", err)
				}
				if tx.Status != before.Status || tx.FailureCode != before.FailureCode ||
					tx.ResultBalance != before.ResultBalance || !tx.UpdatedAt.Equal(before.UpdatedAt) {
					t.Fatalf("transaction mutated on rejected transition: before=%+v after=%+v", before, *tx)
				}
			})
		}
	}
}

func TestMarkPendingReferenceOnlyForReferenceKinds(t *testing.T) {
	setClock(t)
	for _, kind := range []Kind{KindBet, KindWin, KindLoss} {
		cents := int64(100)
		if kind == KindLoss {
			cents = 0
		}
		tx, err := NewExternal(extParams(t, kind, cents))
		if err != nil {
			t.Fatalf("NewExternal(%s) error: %v", kind, err)
		}
		if err := tx.MarkPendingReference(); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("%s: error = %v, want ErrInvalidTransition", kind, err)
		}
		if tx.Status != StatusPending {
			t.Fatalf("%s: status = %s", kind, tx.Status)
		}
	}
	for _, kind := range []Kind{KindRefund, KindRollback} {
		tx, err := NewExternal(extParams(t, kind, 100))
		if err != nil {
			t.Fatalf("NewExternal(%s) error: %v", kind, err)
		}
		if err := tx.MarkPendingReference(); err != nil {
			t.Fatalf("%s: unexpected error: %v", kind, err)
		}
	}
}

func TestMarkProcessedValidation(t *testing.T) {
	setClock(t)
	usd, err := money.FromCents(100, "USD")
	if err != nil {
		t.Fatalf("FromCents error: %v", err)
	}
	tests := []struct {
		name    string
		result  money.Money
		wantErr error
	}{
		{"currency mismatch", usd, money.ErrCurrencyMismatch},
		{"negative result", amt(t, -1), ErrInvalidTransaction},
		{"missing currency", money.Money{}, money.ErrCurrencyMismatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tx := buildAt(t, StatusPending)
			err := tx.MarkProcessed(tc.result)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if tx.Status != StatusPending || tx.ResultBalance != nil {
				t.Fatalf("transaction mutated: status=%s result=%v", tx.Status, tx.ResultBalance)
			}
		})
	}
	tx := buildAt(t, StatusPending)
	if err := tx.MarkProcessed(amt(t, 0)); err != nil {
		t.Fatalf("zero result balance must be accepted: %v", err)
	}
}

func TestFailureCodeValidation(t *testing.T) {
	setClock(t)
	codes := map[string]string{
		"empty":    "",
		"blank":    "   ",
		"too long": strings.Repeat("c", 65),
	}
	for name, code := range codes {
		t.Run(name, func(t *testing.T) {
			rejected := buildAt(t, StatusPending)
			if err := rejected.MarkRejected(code); !errors.Is(err, ErrInvalidTransaction) {
				t.Fatalf("MarkRejected error = %v, want ErrInvalidTransaction", err)
			}
			if rejected.Status != StatusPending || rejected.FailureCode != "" {
				t.Fatal("MarkRejected mutated the transaction")
			}
			failed := buildAt(t, StatusPending)
			if err := failed.MarkFailed(code); !errors.Is(err, ErrInvalidTransaction) {
				t.Fatalf("MarkFailed error = %v, want ErrInvalidTransaction", err)
			}
			if failed.Status != StatusPending || failed.FailureCode != "" {
				t.Fatal("MarkFailed mutated the transaction")
			}
		})
	}
	ok := buildAt(t, StatusPending)
	if err := ok.MarkRejected(strings.Repeat("c", 64)); err != nil {
		t.Fatalf("64-char code must be accepted: %v", err)
	}
}

func TestResolveReference(t *testing.T) {
	setClock(t)

	t.Run("pending refund", func(t *testing.T) {
		tx := buildAt(t, StatusPending)
		before := tx.UpdatedAt
		if err := tx.ResolveReference("tx-bet"); err != nil {
			t.Fatalf("error: %v", err)
		}
		if tx.ResolvedReferenceTransactionID != "tx-bet" || !tx.UpdatedAt.After(before) {
			t.Fatalf("resolved=%q updated=%s", tx.ResolvedReferenceTransactionID, tx.UpdatedAt)
		}
	})

	t.Run("pending reference rollback", func(t *testing.T) {
		tx := buildAt(t, StatusPendingReference)
		if err := tx.ResolveReference("tx-bet"); err != nil {
			t.Fatalf("error: %v", err)
		}
		if tx.Status != StatusPendingReference {
			t.Fatalf("status changed to %s", tx.Status)
		}
	})

	t.Run("kind without reference", func(t *testing.T) {
		tx, err := NewExternal(extParams(t, KindBet, 100))
		if err != nil {
			t.Fatalf("NewExternal error: %v", err)
		}
		if err := tx.ResolveReference("tx-x"); !errors.Is(err, ErrInvalidTransaction) {
			t.Fatalf("error = %v, want ErrInvalidTransaction", err)
		}
	})

	t.Run("terminal", func(t *testing.T) {
		tx := buildAt(t, StatusProcessed)
		if err := tx.ResolveReference("tx-x"); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("error = %v, want ErrInvalidTransition", err)
		}
	})

	t.Run("blank and self reference", func(t *testing.T) {
		tx := buildAt(t, StatusPending)
		if err := tx.ResolveReference("  "); !errors.Is(err, ErrInvalidTransaction) {
			t.Fatalf("blank error = %v", err)
		}
		if err := tx.ResolveReference(tx.ID); !errors.Is(err, ErrInvalidTransaction) {
			t.Fatalf("self error = %v", err)
		}
		if tx.ResolvedReferenceTransactionID != "" {
			t.Fatal("reference must stay empty")
		}
	})
}

func TestValidateInvariants(t *testing.T) {
	negative := amt(t, -5)
	usd, err := money.FromCents(5, "USD")
	if err != nil {
		t.Fatalf("FromCents error: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*WagerTransaction)
	}{
		{"unknown status", func(tx *WagerTransaction) { tx.Status = "COMPLETED" }},
		{"unknown kind", func(tx *WagerTransaction) { tx.Kind = "REVERSAL" }},
		{"processed without result balance", func(tx *WagerTransaction) { tx.Status = StatusProcessed }},
		{"rejected without failure code", func(tx *WagerTransaction) { tx.Status = StatusRejected }},
		{"failed without failure code", func(tx *WagerTransaction) { tx.Status = StatusFailed }},
		{"pending reference on bet", func(tx *WagerTransaction) {
			tx.Kind = KindBet
			tx.ReferenceExternalTransactionID = ""
			tx.Status = StatusPendingReference
		}},
		{"negative result balance", func(tx *WagerTransaction) { tx.ResultBalance = &negative }},
		{"result balance in other currency", func(tx *WagerTransaction) { tx.ResultBalance = &usd }},
		{"updated before created", func(tx *WagerTransaction) { tx.UpdatedAt = tx.CreatedAt.Add(-time.Second) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tx := buildAt(t, StatusPending)
			if err := tx.Validate(); err != nil {
				t.Fatalf("base transaction invalid: %v", err)
			}
			tc.mutate(tx)
			if err := tx.Validate(); !errors.Is(err, ErrInvalidTransaction) {
				t.Fatalf("error = %v, want ErrInvalidTransaction", err)
			}
		})
	}
}
