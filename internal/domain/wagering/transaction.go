// Package wagering models transactions received from game providers
// (opening balance, bets, wins, losses, refunds and rollbacks).
package wagering

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
)

// Column sizes of wager_transactions (VARCHAR lengths).
const (
	maxProviderID          = 64
	maxExternalID          = 128
	maxIdempotencyKey      = 128
	maxRequestHash         = 64
	maxRoundID             = 128
	maxGameID              = 128
	maxReferenceExternalID = 128
	maxFailureCode         = 64
)

var (
	// ErrInvalidTransaction is returned when a transaction violates its invariants.
	ErrInvalidTransaction = errors.New("wagering: invalid transaction")
	// ErrInvalidTransition is returned for status changes that are not allowed.
	ErrInvalidTransition = errors.New("wagering: invalid status transition")
)

// clock provides the time used to stamp updates; tests replace it.
var clock = func() time.Time { return time.Now().UTC() }

// Kind is the type of a wager transaction.
type Kind string

const (
	KindOpening  Kind = "OPENING"
	KindBet      Kind = "BET"
	KindWin      Kind = "WIN"
	KindLoss     Kind = "LOSS"
	KindRefund   Kind = "REFUND"
	KindRollback Kind = "ROLLBACK"
)

// IsValid reports whether k is one of the known kinds.
func (k Kind) IsValid() bool {
	switch k {
	case KindOpening, KindBet, KindWin, KindLoss, KindRefund, KindRollback:
		return true
	}
	return false
}

// IsExternal reports whether the kind may be sent by a provider (everything but OPENING).
func (k Kind) IsExternal() bool { return k.IsValid() && k != KindOpening }

// RequiresReference reports whether the kind must point to another external transaction.
func (k Kind) RequiresReference() bool { return k == KindRefund || k == KindRollback }

// Status is the processing state of a wager transaction.
type Status string

const (
	StatusPending          Status = "PENDING"
	StatusPendingReference Status = "PENDING_REFERENCE"
	StatusProcessed        Status = "PROCESSED"
	StatusRejected         Status = "REJECTED"
	StatusFailed           Status = "FAILED"
)

// IsValid reports whether s is one of the known statuses.
func (s Status) IsValid() bool {
	switch s {
	case StatusPending, StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed:
		return true
	}
	return false
}

// IsTerminal reports whether no further transition is allowed from s.
func (s Status) IsTerminal() bool {
	return s == StatusProcessed || s == StatusRejected || s == StatusFailed
}

// WagerTransaction mirrors a row of wager_transactions. Optional text columns
// use the empty string for NULL. State must only change through the Mark* and
// ResolveReference methods.
type WagerTransaction struct {
	ID                             string
	WalletID                       string
	Kind                           Kind
	Status                         Status
	Amount                         money.Money
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	RequestHash                    string
	RoundID                        string
	GameID                         string
	ReferenceExternalTransactionID string
	ResolvedReferenceTransactionID string
	FailureCode                    string
	ResultBalance                  *money.Money
	CreatedAt                      time.Time
	UpdatedAt                      time.Time
}

// NewExternalParams carries the data of a transaction received from a provider.
type NewExternalParams struct {
	ID                             string
	WalletID                       string
	Kind                           Kind
	Amount                         money.Money
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	RequestHash                    string
	RoundID                        string
	GameID                         string
	ReferenceExternalTransactionID string
	Now                            time.Time
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidTransaction, fmt.Sprintf(format, args...))
}

// NewExternal creates a provider transaction in PENDING status. OPENING is rejected.
func NewExternal(p NewExternalParams) (*WagerTransaction, error) {
	if p.Kind == KindOpening {
		return nil, invalidf("kind OPENING is not allowed for external transactions")
	}
	t := &WagerTransaction{
		ID:                             p.ID,
		WalletID:                       p.WalletID,
		Kind:                           p.Kind,
		Status:                         StatusPending,
		Amount:                         p.Amount,
		ProviderID:                     p.ProviderID,
		ExternalTransactionID:          p.ExternalTransactionID,
		IdempotencyKey:                 p.IdempotencyKey,
		RequestHash:                    p.RequestHash,
		RoundID:                        p.RoundID,
		GameID:                         p.GameID,
		ReferenceExternalTransactionID: p.ReferenceExternalTransactionID,
		CreatedAt:                      p.Now,
		UpdatedAt:                      p.Now,
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return t, nil
}

// NewOpening creates the internal opening-balance transaction, already PROCESSED.
// Its result balance is the opening amount.
func NewOpening(id, walletID string, amount money.Money, now time.Time) (*WagerTransaction, error) {
	result := amount
	t := &WagerTransaction{
		ID:            id,
		WalletID:      walletID,
		Kind:          KindOpening,
		Status:        StatusProcessed,
		Amount:        amount,
		ResultBalance: &result,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return t, nil
}

// Validate checks every invariant of the transaction (database rules and domain rules).
func (t *WagerTransaction) Validate() error {
	if strings.TrimSpace(t.ID) == "" {
		return invalidf("id is required")
	}
	if strings.TrimSpace(t.WalletID) == "" {
		return invalidf("wallet id is required")
	}
	if !t.Kind.IsValid() {
		return invalidf("unknown kind %q", t.Kind)
	}
	if !t.Status.IsValid() {
		return invalidf("unknown status %q", t.Status)
	}
	if _, err := money.FromCents(t.Amount.Cents(), t.Amount.Currency()); err != nil {
		return fmt.Errorf("%w: amount: %w", ErrInvalidTransaction, err)
	}
	if t.Kind == KindLoss {
		if !t.Amount.IsZero() {
			return invalidf("LOSS amount must be zero, got %s", t.Amount)
		}
	} else if t.Amount.Sign() <= 0 {
		return invalidf("%s amount must be positive, got %s", t.Kind, t.Amount)
	}

	identity := []string{t.ProviderID, t.ExternalTransactionID, t.IdempotencyKey, t.RequestHash}
	if t.Kind == KindOpening {
		for _, v := range identity {
			if v != "" {
				return invalidf("OPENING must not carry provider, external id, idempotency key or request hash")
			}
		}
	} else {
		for _, v := range identity {
			if strings.TrimSpace(v) == "" {
				return invalidf("%s requires provider, external id, idempotency key and request hash", t.Kind)
			}
		}
	}
	if t.Kind.RequiresReference() && strings.TrimSpace(t.ReferenceExternalTransactionID) == "" {
		return invalidf("%s requires a reference external transaction id", t.Kind)
	}

	limits := []struct {
		name  string
		value string
		max   int
	}{
		{"provider id", t.ProviderID, maxProviderID},
		{"external transaction id", t.ExternalTransactionID, maxExternalID},
		{"idempotency key", t.IdempotencyKey, maxIdempotencyKey},
		{"request hash", t.RequestHash, maxRequestHash},
		{"round id", t.RoundID, maxRoundID},
		{"game id", t.GameID, maxGameID},
		{"reference external transaction id", t.ReferenceExternalTransactionID, maxReferenceExternalID},
		{"failure code", t.FailureCode, maxFailureCode},
	}
	for _, l := range limits {
		if utf8.RuneCountInString(l.value) > l.max {
			return invalidf("%s exceeds %d characters", l.name, l.max)
		}
	}

	switch t.Status {
	case StatusPendingReference:
		if !t.Kind.RequiresReference() {
			return invalidf("status PENDING_REFERENCE is only valid for REFUND and ROLLBACK")
		}
	case StatusProcessed:
		if t.ResultBalance == nil {
			return invalidf("PROCESSED requires a result balance")
		}
	case StatusRejected, StatusFailed:
		if strings.TrimSpace(t.FailureCode) == "" {
			return invalidf("%s requires a failure code", t.Status)
		}
	}

	if t.ResultBalance != nil {
		if t.ResultBalance.Currency() != t.Amount.Currency() {
			return fmt.Errorf("%w: result balance %s: %w", ErrInvalidTransaction, t.ResultBalance, money.ErrCurrencyMismatch)
		}
		if t.ResultBalance.IsNegative() {
			return invalidf("result balance must not be negative, got %s", t.ResultBalance)
		}
	}

	if t.CreatedAt.IsZero() {
		return invalidf("created at is required")
	}
	if t.UpdatedAt.Before(t.CreatedAt) {
		return invalidf("updated at is before created at")
	}
	return nil
}

func (t *WagerTransaction) canTransition(to Status) error {
	if t.Status.IsTerminal() {
		return fmt.Errorf("%w: %s is terminal, cannot move to %s", ErrInvalidTransition, t.Status, to)
	}
	if t.Status == to {
		return fmt.Errorf("%w: already %s", ErrInvalidTransition, to)
	}
	return nil
}

func (t *WagerTransaction) touch() {
	t.UpdatedAt = clock()
}

func validateFailureCode(code string) error {
	if strings.TrimSpace(code) == "" {
		return invalidf("failure code is required")
	}
	if utf8.RuneCountInString(code) > maxFailureCode {
		return invalidf("failure code exceeds %d characters", maxFailureCode)
	}
	return nil
}

// MarkProcessed moves a non-terminal transaction to PROCESSED and stores the
// wallet balance at processing time (used to replay the response).
func (t *WagerTransaction) MarkProcessed(resultBalance money.Money) error {
	if err := t.canTransition(StatusProcessed); err != nil {
		return err
	}
	if resultBalance.Currency() != t.Amount.Currency() {
		return fmt.Errorf("%w: result balance %s: %w", ErrInvalidTransaction, resultBalance, money.ErrCurrencyMismatch)
	}
	if resultBalance.IsNegative() {
		return invalidf("result balance must not be negative, got %s", resultBalance)
	}
	result := resultBalance
	t.Status = StatusProcessed
	t.ResultBalance = &result
	t.touch()
	return nil
}

// MarkRejected moves a non-terminal transaction to REJECTED with a business failure code.
func (t *WagerTransaction) MarkRejected(code string) error {
	if err := t.canTransition(StatusRejected); err != nil {
		return err
	}
	if err := validateFailureCode(code); err != nil {
		return err
	}
	t.Status = StatusRejected
	t.FailureCode = code
	t.touch()
	return nil
}

// MarkFailed moves a non-terminal transaction to FAILED with a technical failure code.
func (t *WagerTransaction) MarkFailed(code string) error {
	if err := t.canTransition(StatusFailed); err != nil {
		return err
	}
	if err := validateFailureCode(code); err != nil {
		return err
	}
	t.Status = StatusFailed
	t.FailureCode = code
	t.touch()
	return nil
}

// MarkPendingReference parks a REFUND or ROLLBACK whose referenced transaction is not known yet.
func (t *WagerTransaction) MarkPendingReference() error {
	if !t.Kind.RequiresReference() {
		return fmt.Errorf("%w: %s cannot wait for a reference", ErrInvalidTransition, t.Kind)
	}
	if err := t.canTransition(StatusPendingReference); err != nil {
		return err
	}
	t.Status = StatusPendingReference
	t.touch()
	return nil
}

// ResolveReference records the internal id of the transaction referenced by a REFUND or ROLLBACK.
func (t *WagerTransaction) ResolveReference(referenceID string) error {
	if !t.Kind.RequiresReference() {
		return invalidf("%s does not reference another transaction", t.Kind)
	}
	if t.Status.IsTerminal() {
		return fmt.Errorf("%w: %s is terminal, cannot resolve reference", ErrInvalidTransition, t.Status)
	}
	if strings.TrimSpace(referenceID) == "" {
		return invalidf("reference id is required")
	}
	if referenceID == t.ID {
		return invalidf("a transaction cannot reference itself")
	}
	t.ResolvedReferenceTransactionID = referenceID
	t.touch()
	return nil
}
