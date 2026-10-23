package usecase

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/ledger"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wagering"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
)

// Outbox event types emitted by the use cases.
const (
	EventWagerTransactionProcessed        = "WagerTransactionProcessed"
	EventWagerTransactionRejected         = "WagerTransactionRejected"
	EventWagerTransactionPendingReference = "WagerTransactionPendingReference"
	EventWalletBalanceChanged             = "WalletBalanceChanged"
)

const (
	aggregateWagerTransaction = "wager_transaction"
	aggregateWallet           = "wallet"
)

type wagerTransactionEvent struct {
	TransactionID                  string       `json:"transaction_id"`
	WalletID                       string       `json:"wallet_id"`
	Kind                           string       `json:"kind"`
	Status                         string       `json:"status"`
	Amount                         money.Money  `json:"amount"`
	Balance                        *money.Money `json:"balance,omitempty"`
	ProviderID                     string       `json:"provider_id,omitempty"`
	ExternalTransactionID          string       `json:"external_transaction_id,omitempty"`
	ReferenceExternalTransactionID string       `json:"reference_external_transaction_id,omitempty"`
	FailureCode                    string       `json:"failure_code,omitempty"`
	OccurredAt                     time.Time    `json:"occurred_at"`
}

type walletBalanceChangedEvent struct {
	WalletID      string      `json:"wallet_id"`
	TransactionID string      `json:"transaction_id"`
	EntryID       string      `json:"entry_id"`
	Direction     string      `json:"direction"`
	Amount        money.Money `json:"amount"`
	BalanceBefore money.Money `json:"balance_before"`
	BalanceAfter  money.Money `json:"balance_after"`
	OccurredAt    time.Time   `json:"occurred_at"`
}

func eventTypeFor(status wagering.Status) (string, error) {
	switch status {
	case wagering.StatusProcessed:
		return EventWagerTransactionProcessed, nil
	case wagering.StatusRejected:
		return EventWagerTransactionRejected, nil
	case wagering.StatusPendingReference:
		return EventWagerTransactionPendingReference, nil
	}
	return "", fmt.Errorf("%w: no event for status %s", ErrInconsistentState, status)
}

// emitTransactionEvent inserts the event describing the final state of t.
func emitTransactionEvent(ctx context.Context, repo *postgres.OutboxRepo, tx pgx.Tx, t *wagering.WagerTransaction, at time.Time) error {
	eventType, err := eventTypeFor(t.Status)
	if err != nil {
		return err
	}
	return insertEvent(ctx, repo, tx, aggregateWagerTransaction, t.ID, eventType, t.WalletID, wagerTransactionEvent{
		TransactionID:                  t.ID,
		WalletID:                       t.WalletID,
		Kind:                           string(t.Kind),
		Status:                         string(t.Status),
		Amount:                         t.Amount,
		Balance:                        t.ResultBalance,
		ProviderID:                     t.ProviderID,
		ExternalTransactionID:          t.ExternalTransactionID,
		ReferenceExternalTransactionID: t.ReferenceExternalTransactionID,
		FailureCode:                    t.FailureCode,
		OccurredAt:                     at.UTC(),
	})
}

// emitBalanceChanged inserts the event describing a ledger movement.
func emitBalanceChanged(ctx context.Context, repo *postgres.OutboxRepo, tx pgx.Tx, e ledger.Entry, at time.Time) error {
	return insertEvent(ctx, repo, tx, aggregateWallet, e.WalletID(), EventWalletBalanceChanged, e.WalletID(), walletBalanceChangedEvent{
		WalletID:      e.WalletID(),
		TransactionID: e.TransactionID(),
		EntryID:       e.ID(),
		Direction:     string(e.Direction()),
		Amount:        e.Amount(),
		BalanceBefore: e.BalanceBefore(),
		BalanceAfter:  e.BalanceAfter(),
		OccurredAt:    at.UTC(),
	})
}

func insertEvent(ctx context.Context, repo *postgres.OutboxRepo, tx pgx.Tx, aggregateType, aggregateID, eventType, groupID string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("usecase: marshal %s event: %w", eventType, err)
	}
	ev := &postgres.Event{
		EventID:        newUUID(),
		AggregateType:  aggregateType,
		AggregateID:    aggregateID,
		EventType:      eventType,
		MessageGroupID: groupID,
		Payload:        data,
	}
	if err := repo.Insert(ctx, tx, ev); err != nil {
		return fmt.Errorf("usecase: insert %s event: %w", eventType, err)
	}
	return nil
}
