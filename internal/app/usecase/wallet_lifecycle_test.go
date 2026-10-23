//go:build integration

package usecase_test

import (
	"context"
	"errors"
	"testing"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/app/usecase"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wagering"
)

func TestOpenWallet(t *testing.T) {
	ctx := testCtx(t)
	player := "player-" + newUUID(t)

	w, err := opener.OpenWallet(ctx, player, brl(t, 10000))
	if err != nil {
		t.Fatalf("OpenWallet: %v", err)
	}
	if w.PlayerID() != player || w.Balance().Cents() != 10000 || w.Version() != 1 {
		t.Fatalf("unexpected wallet: %s %s v%d", w.PlayerID(), w.Balance(), w.Version())
	}
	if got := walletBalance(t, w.ID()); got != 10000 {
		t.Fatalf("persisted balance = %d, want 10000", got)
	}
	if got := ledgerCount(t, w.ID(), "CREDIT"); got != 1 {
		t.Fatalf("CREDIT entries = %d, want 1", got)
	}
	if got := wagerCount(t, w.ID(), wagering.KindOpening); got != 1 {
		t.Fatalf("OPENING transactions = %d, want 1", got)
	}
	counts := eventCounts(t, w.ID())
	if counts[usecase.EventWagerTransactionProcessed] != 1 || counts[usecase.EventWalletBalanceChanged] != 1 {
		t.Fatalf("events = %v, want one processed and one balance changed", counts)
	}

	if _, err := opener.OpenWallet(ctx, player, brl(t, 5)); !errors.Is(err, usecase.ErrWalletAlreadyExists) {
		t.Fatalf("duplicate wallet error = %v, want ErrWalletAlreadyExists", err)
	}
	if got := walletBalance(t, w.ID()); got != 10000 {
		t.Fatalf("balance after conflict = %d, want 10000", got)
	}
	if got := ledgerCount(t, w.ID(), "CREDIT"); got != 1 {
		t.Fatalf("CREDIT entries after conflict = %d, want 1", got)
	}
}

func TestOpenWallet_ZeroAndInvalid(t *testing.T) {
	ctx := testCtx(t)

	zero, err := opener.OpenWallet(ctx, "player-"+newUUID(t), brl(t, 0))
	if err != nil {
		t.Fatalf("OpenWallet zero: %v", err)
	}
	if zero.Balance().Cents() != 0 || wagerCount(t, zero.ID(), wagering.KindOpening) != 0 {
		t.Fatalf("zero wallet must have no opening transaction")
	}
	if counts := eventCounts(t, zero.ID()); len(counts) != 0 {
		t.Fatalf("zero wallet events = %v, want none", counts)
	}
	if got := scanInt(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, zero.ID()); got != 0 {
		t.Fatalf("zero wallet ledger entries = %d, want 0", got)
	}

	if _, err := opener.OpenWallet(ctx, "player-"+newUUID(t), brl(t, -1)); !errors.Is(err, usecase.ErrInvalidInput) {
		t.Fatalf("negative balance error = %v, want ErrInvalidInput", err)
	}
	if _, err := opener.OpenWallet(ctx, "   ", brl(t, 0)); !errors.Is(err, usecase.ErrInvalidInput) {
		t.Fatalf("blank player error = %v, want ErrInvalidInput", err)
	}
}

func TestReconcile(t *testing.T) {
	ctx := testCtx(t)
	w := openWallet(t, 10000)
	mustProcess(t, newInput(t, w.ID(), wagering.KindBet, 2500))
	mustProcess(t, newInput(t, w.ID(), wagering.KindWin, 700))

	report, err := reconciler.Reconcile(ctx, w.ID())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !report.Consistent || report.Credits.Cents() != 10700 || report.Debits.Cents() != 2500 ||
		report.LedgerBalance.Cents() != 8200 || report.WalletBalance.Cents() != 8200 ||
		!report.Difference.IsZero() || report.EntryCount != 3 || report.Currency != "BRL" {
		t.Fatalf("report = %+v", report)
	}

	if _, err := testPool.Exec(context.Background(),
		`UPDATE wallets SET balance_cents = balance_cents + 1 WHERE id = $1`, w.ID()); err != nil {
		t.Fatalf("corrupt balance: %v", err)
	}
	report, err = reconciler.Reconcile(ctx, w.ID())
	if err != nil {
		t.Fatalf("Reconcile after corruption: %v", err)
	}
	if report.Consistent || report.Difference.Cents() != 1 {
		t.Fatalf("corrupted report = %+v, want inconsistent with difference 1", report)
	}

	if _, err := reconciler.Reconcile(ctx, newUUID(t)); !errors.Is(err, usecase.ErrWalletNotFound) {
		t.Fatalf("unknown wallet error = %v, want ErrWalletNotFound", err)
	}
	if _, err := reconciler.Reconcile(ctx, "nope"); !errors.Is(err, usecase.ErrInvalidInput) {
		t.Fatalf("invalid id error = %v, want ErrInvalidInput", err)
	}
}
