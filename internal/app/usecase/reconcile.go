package usecase

import (
	"context"
	"errors"
	"fmt"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
)

// ReconciliationReport compares a wallet balance with its ledger.
type ReconciliationReport struct {
	WalletID      string
	Currency      string
	Credits       money.Money
	Debits        money.Money
	LedgerBalance money.Money // Credits - Debits
	WalletBalance money.Money
	Difference    money.Money // WalletBalance - LedgerBalance
	EntryCount    int64
	Consistent    bool
}

// Reconciler checks wallets against their ledgers.
type Reconciler struct {
	db      DB
	ledgers *postgres.LedgerRepo
}

// NewReconciler builds the use case.
func NewReconciler(db DB, ledgers *postgres.LedgerRepo) *Reconciler {
	return &Reconciler{db: db, ledgers: ledgers}
}

// Reconcile sums credits minus debits of the wallet ledger and compares the
// result with the stored wallet balance. Both numbers come from one SQL
// statement, so the comparison never sees a half-applied change.
func (r *Reconciler) Reconcile(ctx context.Context, walletID string) (ReconciliationReport, error) {
	if !isUUID(walletID) {
		return ReconciliationReport{}, fmt.Errorf("%w: wallet id must be a UUID", ErrInvalidInput)
	}
	totals, err := r.ledgers.TotalsByWallet(ctx, r.db, walletID)
	if err != nil {
		if errors.Is(err, postgres.ErrNotFound) {
			return ReconciliationReport{}, fmt.Errorf("%w: %s", ErrWalletNotFound, walletID)
		}
		return ReconciliationReport{}, err
	}

	credits, err := money.FromCents(totals.CreditsCents, totals.Currency)
	if err != nil {
		return ReconciliationReport{}, fmt.Errorf("usecase: reconcile %s credits: %w", walletID, err)
	}
	debits, err := money.FromCents(totals.DebitsCents, totals.Currency)
	if err != nil {
		return ReconciliationReport{}, fmt.Errorf("usecase: reconcile %s debits: %w", walletID, err)
	}
	walletBalance, err := money.FromCents(totals.WalletBalanceCents, totals.Currency)
	if err != nil {
		return ReconciliationReport{}, fmt.Errorf("usecase: reconcile %s balance: %w", walletID, err)
	}
	ledgerBalance, err := credits.Sub(debits)
	if err != nil {
		return ReconciliationReport{}, fmt.Errorf("usecase: reconcile %s ledger balance: %w", walletID, err)
	}
	difference, err := walletBalance.Sub(ledgerBalance)
	if err != nil {
		return ReconciliationReport{}, fmt.Errorf("usecase: reconcile %s difference: %w", walletID, err)
	}

	return ReconciliationReport{
		WalletID:      walletID,
		Currency:      totals.Currency,
		Credits:       credits,
		Debits:        debits,
		LedgerBalance: ledgerBalance,
		WalletBalance: walletBalance,
		Difference:    difference,
		EntryCount:    totals.Entries,
		Consistent:    difference.IsZero(),
	}, nil
}
