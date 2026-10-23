package usecase

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/ledger"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wagering"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wallet"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
)

// WalletOpener opens wallets, optionally with an opening balance.
type WalletOpener struct {
	db      DB
	wallets *postgres.WalletRepo
	wagers  *postgres.WageringRepo
	ledgers *postgres.LedgerRepo
	outbox  *postgres.OutboxRepo
	now     func() time.Time
}

// NewWalletOpener builds the use case.
func NewWalletOpener(db DB, wallets *postgres.WalletRepo, wagers *postgres.WageringRepo, ledgers *postgres.LedgerRepo, outbox *postgres.OutboxRepo) *WalletOpener {
	return &WalletOpener{
		db:      db,
		wallets: wallets,
		wagers:  wagers,
		ledgers: ledgers,
		outbox:  outbox,
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// OpenWallet creates the wallet of playerID in the currency of initial. When
// initial is positive, the same commit also records the OPENING transaction
// (PROCESSED), the CREDIT ledger entry and the WagerTransactionProcessed and
// WalletBalanceChanged outbox events. It returns ErrWalletAlreadyExists when
// the player already has a wallet in that currency.
func (o *WalletOpener) OpenWallet(ctx context.Context, playerID string, initial money.Money) (*wallet.Wallet, error) {
	at := o.now()
	w, err := wallet.New(newUUID(), playerID, initial, at)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}

	err = postgres.WithTx(ctx, o.db, func(tx pgx.Tx) error {
		if err := o.wallets.Create(ctx, tx, w); err != nil {
			if postgres.IsUniqueViolation(err, postgres.ConstraintWalletsPlayerCurrency) {
				return fmt.Errorf("%w: player %s currency %s", ErrWalletAlreadyExists, playerID, w.Currency())
			}
			return err
		}
		if initial.IsZero() {
			return nil
		}

		opening, err := wagering.NewOpening(newUUID(), w.ID(), initial, at)
		if err != nil {
			return fmt.Errorf("usecase: build opening transaction: %w", err)
		}
		if err := o.wagers.Create(ctx, tx, opening); err != nil {
			return err
		}

		entry, err := ledger.New(newUUID(), w.ID(), opening.ID, ledger.DirectionCredit,
			initial, money.Zero(initial.Currency()), initial, at)
		if err != nil {
			return fmt.Errorf("usecase: build opening ledger entry: %w", err)
		}
		if err := o.ledgers.Create(ctx, tx, &entry); err != nil {
			return err
		}

		if err := emitTransactionEvent(ctx, o.outbox, tx, opening, at); err != nil {
			return err
		}
		return emitBalanceChanged(ctx, o.outbox, tx, entry, at)
	})
	if err != nil {
		return nil, err
	}
	return w, nil
}
