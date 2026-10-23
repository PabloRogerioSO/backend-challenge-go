package refworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/app/usecase"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wagering"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
)

const (
	FailureReferenceNotFound = "REFERENCE_NOT_FOUND"
	EventProcessed           = "WagerTransactionProcessed"
	EventRejected            = "WagerTransactionRejected"
	aggregateType            = "wager_transaction"
)

type Store interface {
	ListPending(ctx context.Context, limit int) ([]postgres.PendingTransaction, error)
	ReferenceProcessed(ctx context.Context, providerID, referenceExternalTransactionID string) (bool, error)
	MarkProcessed(ctx context.Context, tx pgx.Tx, id string) (bool, error)
	MarkRejected(ctx context.Context, tx pgx.Tx, id, failureCode string) (bool, error)
}

type Outbox interface {
	Insert(ctx context.Context, tx pgx.Tx, ev *postgres.Event) error
}

type Processor interface {
	ProcessWager(ctx context.Context, in usecase.ProcessWagerInput) (usecase.ProcessWagerOutput, error)
}

type Config struct {
	Interval  time.Duration
	BatchSize int
	TTL       time.Duration
	Timeout   time.Duration
}

func (c Config) withDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = 10 * time.Second
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.TTL <= 0 {
		c.TTL = 24 * time.Hour
	}
	if c.Timeout <= 0 {
		c.Timeout = 60 * time.Second
	}
	return c
}

type Worker struct {
	cfg    Config
	store  Store
	outbox Outbox
	proc   Processor
	tx     postgres.TxRunner
	log    *slog.Logger
	now    func() time.Time
}

func NewWorker(cfg Config, store Store, outbox Outbox, proc Processor, tx postgres.TxRunner, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{cfg: cfg.withDefaults(), store: store, outbox: outbox, proc: proc, tx: tx, log: log, now: time.Now}
}

func (w *Worker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), w.cfg.Timeout)
			n, err := w.Tick(tctx)
			cancel()
			if err != nil {
				w.log.Error("reference worker tick finished with errors", "resolved", n, "err", err)
			}
		}
	}
}

func (w *Worker) Tick(ctx context.Context) (int, error) {
	pending, err := w.store.ListPending(ctx, w.cfg.BatchSize)
	if err != nil {
		return 0, err
	}
	var errs []error
	resolved := 0
	for _, p := range pending {
		done, err := w.handle(ctx, p)
		if err != nil {
			errs = append(errs, fmt.Errorf("pending %s: %w", p.ID, err))
			continue
		}
		if done {
			resolved++
		}
	}
	return resolved, errors.Join(errs...)
}

func (w *Worker) handle(ctx context.Context, p postgres.PendingTransaction) (bool, error) {
	found, err := w.store.ReferenceProcessed(ctx, p.ProviderID, p.ReferenceExternalTransactionID)
	if err != nil {
		return false, err
	}
	if found {
		amount, err := money.FromCents(p.AmountCents, p.Currency)
		if err != nil {
			return false, fmt.Errorf("invalid amount: %w", err)
		}
		in := usecase.ProcessWagerInput{
			ProviderID:                     p.ProviderID,
			WalletID:                       p.WalletID,
			IdempotencyKey:                 p.IdempotencyKey,
			ExternalTransactionID:          p.ExternalTransactionID,
			Kind:                           wagering.Kind(p.Kind),
			Amount:                         amount,
			RoundID:                        p.RoundID,
			GameID:                         p.GameID,
			ReferenceExternalTransactionID: p.ReferenceExternalTransactionID,
		}
		if _, err := w.proc.ProcessWager(ctx, in); err != nil {
			return false, fmt.Errorf("process wager: %w", err)
		}
		if err := w.settle(ctx, p, EventProcessed, ""); err != nil {
			return false, err
		}
		w.log.Info("pending reference resolved", "id", p.ID)
		return true, nil
	}
	if w.now().Sub(p.CreatedAt) > w.cfg.TTL {
		if err := w.settle(ctx, p, EventRejected, FailureReferenceNotFound); err != nil {
			return false, err
		}
		w.log.Warn("pending reference expired", "id", p.ID)
		return true, nil
	}
	return false, nil
}

type eventPayload struct {
	TransactionID         string    `json:"transactionId"`
	ProviderID            string    `json:"providerId"`
	WalletID              string    `json:"walletId"`
	ExternalTransactionID string    `json:"externalTransactionId"`
	Status                string    `json:"status"`
	FailureCode           string    `json:"failureCode,omitempty"`
	OccurredAt            time.Time `json:"occurredAt"`
}

func (w *Worker) settle(ctx context.Context, p postgres.PendingTransaction, eventType, failureCode string) error {
	status := "PROCESSED"
	if eventType == EventRejected {
		status = "REJECTED"
	}
	payload, err := json.Marshal(eventPayload{
		TransactionID:         p.ID,
		ProviderID:            p.ProviderID,
		WalletID:              p.WalletID,
		ExternalTransactionID: p.ExternalTransactionID,
		Status:                status,
		FailureCode:           failureCode,
		OccurredAt:            w.now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	return w.tx(ctx, func(tx pgx.Tx) error {
		var updated bool
		var err error
		if eventType == EventRejected {
			updated, err = w.store.MarkRejected(ctx, tx, p.ID, failureCode)
		} else {
			updated, err = w.store.MarkProcessed(ctx, tx, p.ID)
		}
		if err != nil {
			return err
		}
		if !updated {
			return nil
		}
		return w.outbox.Insert(ctx, tx, &postgres.Event{
			EventID:        p.ID + ":" + eventType,
			AggregateType:  aggregateType,
			AggregateID:    p.ID,
			EventType:      eventType,
			MessageGroupID: p.WalletID,
			Payload:        payload,
		})
	})
}
