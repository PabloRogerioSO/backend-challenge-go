package refworker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/app/usecase"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
)

type fakeStore struct {
	pending   []postgres.PendingTransaction
	found     map[string]bool
	processed []string
	rejected  map[string]string
	noUpdate  bool
}

func (f *fakeStore) ListPending(context.Context, int) ([]postgres.PendingTransaction, error) {
	return f.pending, nil
}
func (f *fakeStore) ReferenceProcessed(_ context.Context, _ string, ref string) (bool, error) {
	return f.found[ref], nil
}
func (f *fakeStore) MarkProcessed(_ context.Context, _ pgx.Tx, id string) (bool, error) {
	if f.noUpdate {
		return false, nil
	}
	f.processed = append(f.processed, id)
	return true, nil
}
func (f *fakeStore) MarkRejected(_ context.Context, _ pgx.Tx, id, code string) (bool, error) {
	if f.noUpdate {
		return false, nil
	}
	f.rejected[id] = code
	return true, nil
}

type fakeOutbox struct{ events []postgres.Event }

func (f *fakeOutbox) Insert(_ context.Context, _ pgx.Tx, ev *postgres.Event) error {
	f.events = append(f.events, *ev)
	return nil
}

type fakeProc struct {
	calls int
	err   error
}

func (f *fakeProc) ProcessWager(context.Context, usecase.ProcessWagerInput) (usecase.ProcessWagerOutput, error) {
	f.calls++
	return usecase.ProcessWagerOutput{}, f.err
}

func runTx(_ context.Context, fn func(pgx.Tx) error) error { return fn(nil) }

func pendingTx(id, ref string, created time.Time) postgres.PendingTransaction {
	return postgres.PendingTransaction{
		ID: id, CreatedAt: created,
		ProviderID: "p1", WalletID: "w1", ReferenceExternalTransactionID: ref,
		AmountCents: 1000, Currency: "BRL",
	}
}

func newWorker(store *fakeStore, ob *fakeOutbox, proc *fakeProc, now time.Time) *Worker {
	w := NewWorker(Config{TTL: 24 * time.Hour}, store, ob, proc, runTx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.now = func() time.Time { return now }
	return w
}

func newStore(p ...postgres.PendingTransaction) *fakeStore {
	return &fakeStore{pending: p, found: map[string]bool{}, rejected: map[string]string{}}
}

func TestTick_ReferenceFoundProcessesAndEmitsEvent(t *testing.T) {
	now := time.Now()
	store := newStore(pendingTx("1", "r1", now.Add(-time.Hour)))
	store.found["r1"] = true
	ob, proc := &fakeOutbox{}, &fakeProc{}

	n, err := newWorker(store, ob, proc, now).Tick(context.Background())
	if err != nil || n != 1 || proc.calls != 1 || len(store.processed) != 1 {
		t.Fatalf("n=%d err=%v calls=%d processed=%v", n, err, proc.calls, store.processed)
	}
	if len(ob.events) != 1 || ob.events[0].EventType != EventProcessed || ob.events[0].MessageGroupID != "w1" {
		t.Fatalf("unexpected events: %+v", ob.events)
	}
}

func TestTick_ExpiredIsRejectedAndEmitsEvent(t *testing.T) {
	now := time.Now()
	store := newStore(pendingTx("1", "missing", now.Add(-25*time.Hour)))
	ob, proc := &fakeOutbox{}, &fakeProc{}

	n, err := newWorker(store, ob, proc, now).Tick(context.Background())
	if err != nil || n != 1 || store.rejected["1"] != FailureReferenceNotFound || proc.calls != 0 {
		t.Fatalf("n=%d err=%v rejected=%v calls=%d", n, err, store.rejected, proc.calls)
	}
	if len(ob.events) != 1 || ob.events[0].EventType != EventRejected {
		t.Fatalf("unexpected events: %+v", ob.events)
	}
}

func TestTick_YoungWithoutReferenceStaysPending(t *testing.T) {
	now := time.Now()
	store := newStore(pendingTx("1", "missing", now.Add(-time.Hour)))
	ob := &fakeOutbox{}

	n, err := newWorker(store, ob, &fakeProc{}, now).Tick(context.Background())
	if err != nil || n != 0 || len(ob.events) != 0 {
		t.Fatalf("n=%d err=%v events=%v", n, err, ob.events)
	}
}

func TestTick_AlreadySettledEmitsNoDuplicateEvent(t *testing.T) {
	now := time.Now()
	store := newStore(pendingTx("1", "r1", now))
	store.found["r1"] = true
	store.noUpdate = true
	ob := &fakeOutbox{}

	if _, err := newWorker(store, ob, &fakeProc{}, now).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ob.events) != 0 {
		t.Fatalf("expected no event, got %+v", ob.events)
	}
}

func TestTick_ProcessorErrorLeavesPendingAndContinues(t *testing.T) {
	now := time.Now()
	store := newStore(pendingTx("1", "r1", now), pendingTx("2", "r2", now))
	store.found["r1"], store.found["r2"] = true, true
	ob, proc := &fakeOutbox{}, &fakeProc{err: errors.New("db down")}

	n, err := newWorker(store, ob, proc, now).Tick(context.Background())
	if err == nil || n != 0 || len(ob.events) != 0 || proc.calls != 2 {
		t.Fatalf("n=%d err=%v events=%v calls=%d", n, err, ob.events, proc.calls)
	}
}
