package usecase

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/app/hashing"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/ledger"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wagering"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wallet"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
)

// Failure codes stored in wager_transactions.failure_code for rejected transactions.
const (
	FailureInsufficientFunds         = "INSUFFICIENT_FUNDS"
	FailureInsufficientFundsRollback = "INSUFFICIENT_FUNDS_ROLLBACK"
	FailureCurrencyMismatch          = "CURRENCY_MISMATCH"
	FailureInvalidReferenceKind      = "INVALID_REFERENCE_KIND"
	FailureReferenceWalletMismatch   = "REFERENCE_WALLET_MISMATCH"
	FailureReferenceNotProcessed     = "REFERENCE_NOT_PROCESSED"
	FailureReferenceCurrencyMismatch = "REFERENCE_CURRENCY_MISMATCH"
	FailureReferenceAmountMismatch   = "REFERENCE_AMOUNT_MISMATCH"
	FailureAlreadyReversed           = "ALREADY_REVERSED"
)

// ProcessWagerInput is a provider request. ProviderID comes from the
// authenticated identity; WalletID is the wallet that is locked and changed.
type ProcessWagerInput struct {
	ProviderID                     string
	WalletID                       string
	IdempotencyKey                 string
	ExternalTransactionID          string
	Kind                           wagering.Kind
	Amount                         money.Money
	RoundID                        string
	GameID                         string
	ReferenceExternalTransactionID string
}

// ProcessWagerOutput is the persisted outcome of a request. Balance is the
// wallet balance when the request was processed (result_balance_cents), so a
// replay returns exactly what the first response returned.
type ProcessWagerOutput struct {
	WalletID      string
	TransactionID string
	Kind          wagering.Kind
	Status        wagering.Status
	FailureCode   string
	Balance       money.Money
	Replayed      bool
}

// WagerProcessor applies provider transactions to wallets.
type WagerProcessor struct {
	db      DB
	wallets *postgres.WalletRepo
	wagers  *postgres.WageringRepo
	ledgers *postgres.LedgerRepo
	outbox  *postgres.OutboxRepo
	now     func() time.Time
}

// NewWagerProcessor builds the use case.
func NewWagerProcessor(db DB, wallets *postgres.WalletRepo, wagers *postgres.WageringRepo, ledgers *postgres.LedgerRepo, outbox *postgres.OutboxRepo) *WagerProcessor {
	return &WagerProcessor{
		db:      db,
		wallets: wallets,
		wagers:  wagers,
		ledgers: ledgers,
		outbox:  outbox,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

type wagerRequest struct {
	ProviderID                     string      `json:"provider_id"`
	WalletID                       string      `json:"wallet_id"`
	Kind                           string      `json:"kind"`
	Amount                         money.Money `json:"amount"`
	ExternalTransactionID          string      `json:"external_transaction_id"`
	RoundID                        string      `json:"round_id"`
	GameID                         string      `json:"game_id"`
	ReferenceExternalTransactionID string      `json:"reference_external_transaction_id"`
}

// ProcessWager processes a BET, WIN, LOSS, REFUND or ROLLBACK atomically.
//
// The transaction starts by locking the wallet row (SELECT ... FOR UPDATE);
// only then is the idempotency key looked up, so concurrent duplicates are
// serialized by the lock. The same key with the same payload replays the
// persisted result; the same key with another payload is ErrIdempotencyConflict.
func (p *WagerProcessor) ProcessWager(ctx context.Context, in ProcessWagerInput) (ProcessWagerOutput, error) {
	if err := validateInput(in); err != nil {
		return ProcessWagerOutput{}, err
	}
	hash, err := hashing.HashCanonical(wagerRequest{
		ProviderID:                     in.ProviderID,
		WalletID:                       in.WalletID,
		Kind:                           string(in.Kind),
		Amount:                         in.Amount,
		ExternalTransactionID:          in.ExternalTransactionID,
		RoundID:                        in.RoundID,
		GameID:                         in.GameID,
		ReferenceExternalTransactionID: in.ReferenceExternalTransactionID,
	})
	if err != nil {
		return ProcessWagerOutput{}, fmt.Errorf("usecase: hash wager request: %w", err)
	}

	for attempt := 1; ; attempt++ {
		out, err := p.processOnce(ctx, in, hash)
		if err == nil {
			return out, nil
		}
		// Requests for different wallets are not serialized by the wallet lock, so
		// two of them may race on the same key. The loser retries once and then
		// finds the committed row (replay or conflict).
		racing := postgres.IsUniqueViolation(err, postgres.ConstraintWagerIdempotencyKey) ||
			postgres.IsUniqueViolation(err, postgres.ConstraintWagerProviderExternal)
		if !racing || attempt >= 2 {
			return ProcessWagerOutput{}, err
		}
	}
}

func validateInput(in ProcessWagerInput) error {
	if !isUUID(in.WalletID) {
		return fmt.Errorf("%w: wallet id must be a UUID", ErrInvalidInput)
	}
	if in.ReferenceExternalTransactionID != "" && in.ReferenceExternalTransactionID == in.ExternalTransactionID {
		return fmt.Errorf("%w: a transaction cannot reference itself", ErrInvalidInput)
	}
	// Reuse the domain rules (kind, amount, required identity fields, lengths).
	_, err := wagering.NewExternal(wagering.NewExternalParams{
		ID:                             "probe",
		WalletID:                       in.WalletID,
		Kind:                           in.Kind,
		Amount:                         in.Amount,
		ProviderID:                     in.ProviderID,
		ExternalTransactionID:          in.ExternalTransactionID,
		IdempotencyKey:                 in.IdempotencyKey,
		RequestHash:                    "probe",
		RoundID:                        in.RoundID,
		GameID:                         in.GameID,
		ReferenceExternalTransactionID: in.ReferenceExternalTransactionID,
		Now:                            time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	return nil
}

func (p *WagerProcessor) processOnce(ctx context.Context, in ProcessWagerInput, hash string) (ProcessWagerOutput, error) {
	var out ProcessWagerOutput
	err := postgres.WithTx(ctx, p.db, func(tx pgx.Tx) error {
		// 1. Pessimistic lock: always the first statement of the transaction.
		w, err := p.wallets.GetByIDForUpdate(ctx, tx, in.WalletID)
		if err != nil {
			if errors.Is(err, postgres.ErrNotFound) {
				return fmt.Errorf("%w: %s", ErrWalletNotFound, in.WalletID)
			}
			return err
		}

		// 2. Persistent idempotency, evaluated while holding the lock.
		existing, err := p.wagers.GetByIdempotencyKey(ctx, tx, in.IdempotencyKey)
		switch {
		case err == nil:
			replay, replayErr := replayOutput(existing, hash)
			if replayErr != nil {
				return replayErr
			}
			out = replay
			return nil
		case !errors.Is(err, postgres.ErrNotFound):
			return err
		}

		// 3. The external id may not be reused under another idempotency key.
		_, err = p.wagers.GetByProviderAndExternal(ctx, tx, in.ProviderID, in.ExternalTransactionID)
		switch {
		case err == nil:
			return fmt.Errorf("%w: provider %s external id %s",
				ErrDuplicateExternalTransaction, in.ProviderID, in.ExternalTransactionID)
		case !errors.Is(err, postgres.ErrNotFound):
			return err
		}

		// 4. Apply the transaction.
		applied, err := p.apply(ctx, tx, w, in, hash)
		if err != nil {
			return err
		}
		out = applied
		return nil
	})
	if err != nil {
		return ProcessWagerOutput{}, err
	}
	return out, nil
}

func replayOutput(t *wagering.WagerTransaction, hash string) (ProcessWagerOutput, error) {
	if t.RequestHash != hash {
		return ProcessWagerOutput{}, fmt.Errorf("%w: key %s", ErrIdempotencyConflict, t.IdempotencyKey)
	}
	if t.ResultBalance == nil {
		return ProcessWagerOutput{}, fmt.Errorf("%w: transaction %s has no result balance", ErrInconsistentState, t.ID)
	}
	return ProcessWagerOutput{
		WalletID:      t.WalletID,
		TransactionID: t.ID,
		Kind:          t.Kind,
		Status:        t.Status,
		FailureCode:   t.FailureCode,
		Balance:       *t.ResultBalance,
		Replayed:      true,
	}, nil
}

func (p *WagerProcessor) apply(ctx context.Context, tx pgx.Tx, w *wallet.Wallet, in ProcessWagerInput, hash string) (ProcessWagerOutput, error) {
	at := p.now()
	t, err := wagering.NewExternal(wagering.NewExternalParams{
		ID:                             newUUID(),
		WalletID:                       w.ID(),
		Kind:                           in.Kind,
		Amount:                         in.Amount,
		ProviderID:                     in.ProviderID,
		ExternalTransactionID:          in.ExternalTransactionID,
		IdempotencyKey:                 in.IdempotencyKey,
		RequestHash:                    hash,
		RoundID:                        in.RoundID,
		GameID:                         in.GameID,
		ReferenceExternalTransactionID: in.ReferenceExternalTransactionID,
		Now:                            at,
	})
	if err != nil {
		return ProcessWagerOutput{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}

	// Decide in memory (wallet and transaction), then persist everything.
	entry, err := p.decide(ctx, tx, w, t)
	if err != nil {
		return ProcessWagerOutput{}, err
	}

	if err := p.wagers.Create(ctx, tx, t); err != nil {
		return ProcessWagerOutput{}, err
	}
	if entry != nil {
		if err := p.ledgers.Create(ctx, tx, entry); err != nil {
			return ProcessWagerOutput{}, err
		}
		if err := p.wallets.UpdateBalance(ctx, tx, w); err != nil {
			return ProcessWagerOutput{}, err
		}
	}
	if err := emitTransactionEvent(ctx, p.outbox, tx, t, at); err != nil {
		return ProcessWagerOutput{}, err
	}
	if entry != nil {
		if err := emitBalanceChanged(ctx, p.outbox, tx, *entry, at); err != nil {
			return ProcessWagerOutput{}, err
		}
	}

	if t.ResultBalance == nil {
		return ProcessWagerOutput{}, fmt.Errorf("%w: transaction %s finished without result balance", ErrInconsistentState, t.ID)
	}
	return ProcessWagerOutput{
		WalletID:      t.WalletID,
		TransactionID: t.ID,
		Kind:          t.Kind,
		Status:        t.Status,
		FailureCode:   t.FailureCode,
		Balance:       *t.ResultBalance,
	}, nil
}

// decide moves t to its final status and applies the balance change to w (in
// memory). It returns the ledger entry when money moved, nil otherwise.
func (p *WagerProcessor) decide(ctx context.Context, tx pgx.Tx, w *wallet.Wallet, t *wagering.WagerTransaction) (*ledger.Entry, error) {
	if t.Amount.Currency() != w.Currency() {
		return nil, rejectTx(w, t, FailureCurrencyMismatch)
	}

	switch t.Kind {
	case wagering.KindBet:
		return p.move(ctx, w, t, ledger.DirectionDebit, FailureInsufficientFunds)
	case wagering.KindWin:
		return p.move(ctx, w, t, ledger.DirectionCredit, "")
	case wagering.KindLoss:
		if err := t.MarkProcessed(w.Balance()); err != nil {
			return nil, fmt.Errorf("usecase: process loss %s: %w", t.ID, err)
		}
		return nil, nil
	case wagering.KindRefund, wagering.KindRollback:
		return p.reverse(ctx, tx, w, t)
	}
	return nil, fmt.Errorf("%w: unsupported kind %s", ErrInvalidInput, t.Kind)
}

// move debits or credits the wallet by the transaction amount. Insufficient
// funds reject the transaction with insufficientCode and leave the wallet untouched.
func (p *WagerProcessor) move(ctx context.Context, w *wallet.Wallet, t *wagering.WagerTransaction, dir ledger.Direction, insufficientCode string) (*ledger.Entry, error) {
	before := w.Balance()

	var err error
	if dir == ledger.DirectionDebit {
		err = w.Debit(ctx, t.Amount)
	} else {
		err = w.Credit(ctx, t.Amount)
	}
	switch {
	case errors.Is(err, wallet.ErrInsufficientFunds):
		return nil, rejectTx(w, t, insufficientCode)
	case err != nil:
		return nil, fmt.Errorf("usecase: %s of transaction %s: %w", dir, t.ID, err)
	}

	after := w.Balance()
	entry, err := ledger.New(newUUID(), w.ID(), t.ID, dir, t.Amount, before, after, p.now())
	if err != nil {
		return nil, fmt.Errorf("usecase: build ledger entry of transaction %s: %w", t.ID, err)
	}
	if err := t.MarkProcessed(after); err != nil {
		return nil, fmt.Errorf("usecase: process transaction %s: %w", t.ID, err)
	}
	return &entry, nil
}

// reverse handles REFUND and ROLLBACK. REFUND reverses a BET (credit);
// ROLLBACK reverses a BET (credit) or a WIN (debit). The reference must be
// PROCESSED, belong to the same wallet, match currency and amount, and may be
// reversed only once. An unknown reference parks the transaction as PENDING_REFERENCE.
func (p *WagerProcessor) reverse(ctx context.Context, tx pgx.Tx, w *wallet.Wallet, t *wagering.WagerTransaction) (*ledger.Entry, error) {
	ref, err := p.wagers.GetByProviderAndExternal(ctx, tx, t.ProviderID, t.ReferenceExternalTransactionID)
	if errors.Is(err, postgres.ErrNotFound) {
		if err := t.MarkPendingReference(); err != nil {
			return nil, fmt.Errorf("usecase: park transaction %s: %w", t.ID, err)
		}
		balance := w.Balance()
		t.ResultBalance = &balance
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if ref.WalletID != w.ID() {
		return nil, rejectTx(w, t, FailureReferenceWalletMismatch)
	}
	if err := t.ResolveReference(ref.ID); err != nil {
		return nil, fmt.Errorf("usecase: resolve reference of %s: %w", t.ID, err)
	}

	switch {
	case !reversible(t.Kind, ref.Kind):
		return nil, rejectTx(w, t, FailureInvalidReferenceKind)
	case ref.Status != wagering.StatusProcessed:
		return nil, rejectTx(w, t, FailureReferenceNotProcessed)
	case ref.Amount.Currency() != t.Amount.Currency():
		return nil, rejectTx(w, t, FailureReferenceCurrencyMismatch)
	case ref.Amount.Cents() != t.Amount.Cents():
		return nil, rejectTx(w, t, FailureReferenceAmountMismatch)
	}

	_, err = p.wagers.FindProcessedReversal(ctx, tx, ref.ID)
	switch {
	case err == nil:
		return nil, rejectTx(w, t, FailureAlreadyReversed)
	case !errors.Is(err, postgres.ErrNotFound):
		return nil, err
	}

	if ref.Kind == wagering.KindBet {
		return p.move(ctx, w, t, ledger.DirectionCredit, "")
	}
	return p.move(ctx, w, t, ledger.DirectionDebit, FailureInsufficientFundsRollback)
}

func reversible(kind, refKind wagering.Kind) bool {
	switch kind {
	case wagering.KindRefund:
		return refKind == wagering.KindBet
	case wagering.KindRollback:
		return refKind == wagering.KindBet || refKind == wagering.KindWin
	}
	return false
}

// rejectTx rejects t and records the current wallet balance as its result balance.
func rejectTx(w *wallet.Wallet, t *wagering.WagerTransaction, code string) error {
	if err := t.MarkRejected(code); err != nil {
		return fmt.Errorf("usecase: reject transaction %s: %w", t.ID, err)
	}
	balance := w.Balance()
	// O domínio exige que ResultBalance tenha a mesma moeda de Amount.
	// Se houver divergência de moeda, salvamos zero na moeda da transação.
	if t.Amount.Currency() != balance.Currency() {
		balance = money.Zero(t.Amount.Currency())
	}
	t.ResultBalance = &balance
	return nil
}
