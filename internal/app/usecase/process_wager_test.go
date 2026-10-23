//go:build integration

package usecase_test

import (
	"errors"
	"testing"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/app/usecase"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wagering"
)

func TestProcessWager_ParallelSameBetIsAppliedOnce(t *testing.T) {
	ctx := testCtx(t)
	w := openWallet(t, 10000)
	in := newInput(t, w.ID(), wagering.KindBet, 2500)

	const n = 50
	outs := make([]usecase.ProcessWagerOutput, n)
	errs := make([]error, n)
	runParallel(n, func(i int) { outs[i], errs[i] = processor.ProcessWager(ctx, in) })

	replays := 0
	for i := range outs {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		o := outs[i]
		if o.Status != wagering.StatusProcessed || o.Balance.Cents() != 7500 || o.TransactionID != outs[0].TransactionID {
			t.Fatalf("request %d: unexpected output %+v", i, o)
		}
		if o.Replayed {
			replays++
		}
	}
	if replays != n-1 {
		t.Fatalf("replays = %d, want %d (exactly one request must have been applied)", replays, n-1)
	}
	if got := walletBalance(t, w.ID()); got != 7500 {
		t.Fatalf("balance = %d, want 7500", got)
	}
	if got := ledgerCount(t, w.ID(), "DEBIT"); got != 1 {
		t.Fatalf("DEBIT entries = %d, want 1", got)
	}
	if got := wagerCount(t, w.ID(), wagering.KindBet); got != 1 {
		t.Fatalf("BET transactions = %d, want 1", got)
	}
	counts := eventCounts(t, w.ID())
	if counts[usecase.EventWagerTransactionProcessed] != 2 || counts[usecase.EventWalletBalanceChanged] != 2 {
		t.Fatalf("events = %v, want opening + one bet only (2 processed, 2 balance changed)", counts)
	}
}

func TestProcessWager_TwoBetsOverBalanceOnlyOneWins(t *testing.T) {
	ctx := testCtx(t)
	w := openWallet(t, 10000)
	inputs := []usecase.ProcessWagerInput{
		newInput(t, w.ID(), wagering.KindBet, 8000),
		newInput(t, w.ID(), wagering.KindBet, 8000),
	}

	outs := make([]usecase.ProcessWagerOutput, 2)
	errs := make([]error, 2)
	runParallel(2, func(i int) { outs[i], errs[i] = processor.ProcessWager(ctx, inputs[i]) })

	var processed, rejected []usecase.ProcessWagerOutput
	for i := range outs {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		switch outs[i].Status {
		case wagering.StatusProcessed:
			processed = append(processed, outs[i])
		case wagering.StatusRejected:
			rejected = append(rejected, outs[i])
		default:
			t.Fatalf("request %d: unexpected status %s", i, outs[i].Status)
		}
	}
	if len(processed) != 1 || len(rejected) != 1 {
		t.Fatalf("processed=%d rejected=%d, want 1 and 1", len(processed), len(rejected))
	}
	if rejected[0].FailureCode != usecase.FailureInsufficientFunds || rejected[0].Replayed {
		t.Fatalf("rejected output = %+v", rejected[0])
	}
	if processed[0].Balance.Cents() != 2000 || rejected[0].Balance.Cents() != 2000 {
		t.Fatalf("balances = %d / %d, want 2000 / 2000", processed[0].Balance.Cents(), rejected[0].Balance.Cents())
	}
	if got := walletBalance(t, w.ID()); got != 2000 {
		t.Fatalf("final balance = %d, want 2000", got)
	}
	if got := ledgerCount(t, w.ID(), "DEBIT"); got != 1 {
		t.Fatalf("DEBIT entries = %d, want exactly 1", got)
	}
	if got := wagerCount(t, w.ID(), wagering.KindBet); got != 2 {
		t.Fatalf("BET transactions = %d, want 2 (one processed, one rejected)", got)
	}
}

func TestProcessWager_DifferentWalletsInParallel(t *testing.T) {
	ctx := testCtx(t)
	wa := openWallet(t, 10000)
	wb := openWallet(t, 10000)

	const perWallet = 20
	inputs := make([]usecase.ProcessWagerInput, 0, 2*perWallet)
	for i := 0; i < perWallet; i++ {
		inputs = append(inputs, newInput(t, wa.ID(), wagering.KindBet, 100))
	}
	for i := 0; i < perWallet; i++ {
		inputs = append(inputs, newInput(t, wb.ID(), wagering.KindBet, 100))
	}

	outs := make([]usecase.ProcessWagerOutput, len(inputs))
	errs := make([]error, len(inputs))
	runParallel(len(inputs), func(i int) { outs[i], errs[i] = processor.ProcessWager(ctx, inputs[i]) })

	for i := range outs {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		if outs[i].Status != wagering.StatusProcessed || outs[i].Replayed {
			t.Fatalf("request %d: unexpected output %+v", i, outs[i])
		}
	}
	for _, w := range []string{wa.ID(), wb.ID()} {
		if got := walletBalance(t, w); got != 8000 {
			t.Fatalf("wallet %s balance = %d, want 8000", w, got)
		}
		if got := ledgerCount(t, w, "DEBIT"); got != perWallet {
			t.Fatalf("wallet %s DEBIT entries = %d, want %d", w, got, perWallet)
		}
		report, err := reconciler.Reconcile(ctx, w)
		if err != nil || !report.Consistent {
			t.Fatalf("wallet %s reconciliation = %+v (%v)", w, report, err)
		}
	}
}

func TestProcessWager_ReplayReturnsOriginalBalance(t *testing.T) {
	w := openWallet(t, 10000)
	bet := newInput(t, w.ID(), wagering.KindBet, 2500)

	first := mustProcess(t, bet)
	if first.Replayed || first.Balance.Cents() != 7500 {
		t.Fatalf("first output = %+v", first)
	}
	if later := mustProcess(t, newInput(t, w.ID(), wagering.KindWin, 1000)); later.Balance.Cents() != 8500 {
		t.Fatalf("win balance = %d, want 8500", later.Balance.Cents())
	}

	replay := mustProcess(t, bet)
	if !replay.Replayed || replay.TransactionID != first.TransactionID || replay.Status != wagering.StatusProcessed {
		t.Fatalf("replay output = %+v", replay)
	}
	if replay.Balance.Cents() != 7500 {
		t.Fatalf("replay balance = %d, want the original 7500", replay.Balance.Cents())
	}
	if got := walletBalance(t, w.ID()); got != 8500 {
		t.Fatalf("wallet balance = %d, want 8500 (replay must not move money)", got)
	}
}

func TestProcessWager_IdempotencyConflict(t *testing.T) {
	ctx := testCtx(t)
	w := openWallet(t, 10000)
	other := openWallet(t, 10000)
	in := newInput(t, w.ID(), wagering.KindBet, 2500)
	mustProcess(t, in)

	changedAmount := in
	changedAmount.Amount = brl(t, 2600)
	changedExternal := in
	changedExternal.ExternalTransactionID = "ext-" + newUUID(t)
	changedKind := in
	changedKind.Kind = wagering.KindWin
	changedWallet := in
	changedWallet.WalletID = other.ID()

	cases := map[string]usecase.ProcessWagerInput{
		"amount":      changedAmount,
		"external id": changedExternal,
		"kind":        changedKind,
		"wallet":      changedWallet,
	}
	for name, c := range cases {
		if _, err := processor.ProcessWager(ctx, c); !errors.Is(err, usecase.ErrIdempotencyConflict) {
			t.Fatalf("%s: error = %v, want ErrIdempotencyConflict", name, err)
		}
	}
	if got := walletBalance(t, w.ID()); got != 7500 {
		t.Fatalf("balance = %d, want 7500", got)
	}
	if got := walletBalance(t, other.ID()); got != 10000 {
		t.Fatalf("other wallet balance = %d, want 10000", got)
	}
	if got := wagerCount(t, w.ID(), wagering.KindBet); got != 1 {
		t.Fatalf("BET transactions = %d, want 1", got)
	}
}

func TestProcessWager_KindsAndValidation(t *testing.T) {
	ctx := testCtx(t)
	w := openWallet(t, 10000)

	loss := mustProcess(t, newInput(t, w.ID(), wagering.KindLoss, 0))
	if loss.Status != wagering.StatusProcessed || loss.Balance.Cents() != 10000 {
		t.Fatalf("loss output = %+v", loss)
	}
	if got := ledgerCount(t, w.ID(), "DEBIT") + ledgerCount(t, w.ID(), "CREDIT"); got != 1 {
		t.Fatalf("ledger entries = %d, want only the opening credit", got)
	}
	counts := eventCounts(t, w.ID())
	if counts[usecase.EventWagerTransactionProcessed] != 2 || counts[usecase.EventWalletBalanceChanged] != 1 {
		t.Fatalf("events after LOSS = %v", counts)
	}

	if win := mustProcess(t, newInput(t, w.ID(), wagering.KindWin, 700)); win.Balance.Cents() != 10700 {
		t.Fatalf("win balance = %d, want 10700", win.Balance.Cents())
	}

	usd, err := money.FromCents(100, "USD")
	if err != nil {
		t.Fatalf("FromCents: %v", err)
	}
	wrongCurrency := newInput(t, w.ID(), wagering.KindBet, 100)
	wrongCurrency.Amount = usd
	if out := mustProcess(t, wrongCurrency); out.Status != wagering.StatusRejected || out.FailureCode != usecase.FailureCurrencyMismatch {
		t.Fatalf("currency mismatch output = %+v", out)
	}

	first := newInput(t, w.ID(), wagering.KindBet, 100)
	mustProcess(t, first)
	dup := newInput(t, w.ID(), wagering.KindBet, 100)
	dup.ExternalTransactionID = first.ExternalTransactionID
	if _, err := processor.ProcessWager(ctx, dup); !errors.Is(err, usecase.ErrDuplicateExternalTransaction) {
		t.Fatalf("duplicate external id error = %v, want ErrDuplicateExternalTransaction", err)
	}

	missingKey := newInput(t, w.ID(), wagering.KindBet, 100)
	missingKey.IdempotencyKey = ""
	opening := newInput(t, w.ID(), wagering.KindOpening, 100)
	lossWithAmount := newInput(t, w.ID(), wagering.KindLoss, 100)
	badWallet := newInput(t, "not-a-uuid", wagering.KindBet, 100)
	refundWithoutRef := newInput(t, w.ID(), wagering.KindRefund, 100)
	refundWithoutRef.ReferenceExternalTransactionID = ""
	for name, c := range map[string]usecase.ProcessWagerInput{
		"missing idempotency key": missingKey,
		"opening kind":            opening,
		"loss with amount":        lossWithAmount,
		"wallet id not a uuid":    badWallet,
		"refund without ref":      refundWithoutRef,
	} {
		if _, err := processor.ProcessWager(ctx, c); !errors.Is(err, usecase.ErrInvalidInput) {
			t.Fatalf("%s: error = %v, want ErrInvalidInput", name, err)
		}
	}

	unknown := newInput(t, newUUID(t), wagering.KindBet, 100)
	if _, err := processor.ProcessWager(ctx, unknown); !errors.Is(err, usecase.ErrWalletNotFound) {
		t.Fatalf("unknown wallet error = %v, want ErrWalletNotFound", err)
	}
}

func TestProcessWager_Reversals(t *testing.T) {
	w := openWallet(t, 10000)

	bet := newInput(t, w.ID(), wagering.KindBet, 2500)
	mustProcess(t, bet)

	refund := newInput(t, w.ID(), wagering.KindRefund, 2500)
	refund.ReferenceExternalTransactionID = bet.ExternalTransactionID
	out := mustProcess(t, refund)
	if out.Status != wagering.StatusProcessed || out.Balance.Cents() != 10000 {
		t.Fatalf("refund output = %+v", out)
	}
	if again := mustProcess(t, refund); !again.Replayed || again.Balance.Cents() != 10000 {
		t.Fatalf("refund replay = %+v", again)
	}

	secondRefund := newInput(t, w.ID(), wagering.KindRefund, 2500)
	secondRefund.ReferenceExternalTransactionID = bet.ExternalTransactionID
	rollbackAfterRefund := newInput(t, w.ID(), wagering.KindRollback, 2500)
	rollbackAfterRefund.ReferenceExternalTransactionID = bet.ExternalTransactionID
	for name, in := range map[string]usecase.ProcessWagerInput{"second refund": secondRefund, "rollback after refund": rollbackAfterRefund} {
		got := mustProcess(t, in)
		if got.Status != wagering.StatusRejected || got.FailureCode != usecase.FailureAlreadyReversed || got.Balance.Cents() != 10000 {
			t.Fatalf("%s output = %+v", name, got)
		}
	}
	if got := walletBalance(t, w.ID()); got != 10000 {
		t.Fatalf("balance after reversals = %d, want 10000", got)
	}

	other := newInput(t, w.ID(), wagering.KindBet, 1000)
	mustProcess(t, other)
	wrongAmount := newInput(t, w.ID(), wagering.KindRefund, 999)
	wrongAmount.ReferenceExternalTransactionID = other.ExternalTransactionID
	if got := mustProcess(t, wrongAmount); got.FailureCode != usecase.FailureReferenceAmountMismatch {
		t.Fatalf("amount mismatch output = %+v", got)
	}

	win := newInput(t, w.ID(), wagering.KindWin, 500)
	mustProcess(t, win)
	refundOfWin := newInput(t, w.ID(), wagering.KindRefund, 500)
	refundOfWin.ReferenceExternalTransactionID = win.ExternalTransactionID
	if got := mustProcess(t, refundOfWin); got.FailureCode != usecase.FailureInvalidReferenceKind {
		t.Fatalf("refund of win output = %+v", got)
	}

	before := walletBalance(t, w.ID())
	pending := newInput(t, w.ID(), wagering.KindRefund, 100)
	pendingOut := mustProcess(t, pending)
	if pendingOut.Status != wagering.StatusPendingReference || pendingOut.Balance.Cents() != before {
		t.Fatalf("pending output = %+v", pendingOut)
	}
	if replay := mustProcess(t, pending); !replay.Replayed || replay.Status != wagering.StatusPendingReference {
		t.Fatalf("pending replay = %+v", replay)
	}
	if got := walletBalance(t, w.ID()); got != before {
		t.Fatalf("pending reference moved money: %d -> %d", before, got)
	}
	if got := eventCounts(t, w.ID())[usecase.EventWagerTransactionPendingReference]; got != 1 {
		t.Fatalf("pending reference events = %d, want 1", got)
	}
}

func TestProcessWager_RollbackOfWinNeedsFunds(t *testing.T) {
	w := openWallet(t, 1000)
	win := newInput(t, w.ID(), wagering.KindWin, 5000)
	mustProcess(t, win)
	mustProcess(t, newInput(t, w.ID(), wagering.KindBet, 5500))

	rollback := newInput(t, w.ID(), wagering.KindRollback, 5000)
	rollback.ReferenceExternalTransactionID = win.ExternalTransactionID
	out := mustProcess(t, rollback)
	if out.Status != wagering.StatusRejected || out.FailureCode != usecase.FailureInsufficientFundsRollback {
		t.Fatalf("rollback output = %+v", out)
	}
	if got := walletBalance(t, w.ID()); got != 500 {
		t.Fatalf("balance = %d, want 500", got)
	}
	if got := ledgerCount(t, w.ID(), "DEBIT"); got != 1 {
		t.Fatalf("DEBIT entries = %d, want 1 (the bet only)", got)
	}

	topUp := newInput(t, w.ID(), wagering.KindWin, 5000)
	mustProcess(t, topUp)
	retry := newInput(t, w.ID(), wagering.KindRollback, 5000)
	retry.ReferenceExternalTransactionID = win.ExternalTransactionID
	if out := mustProcess(t, retry); out.Status != wagering.StatusProcessed || out.Balance.Cents() != 500 {
		t.Fatalf("rollback with funds output = %+v", out)
	}
}
