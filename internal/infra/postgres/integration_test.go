//go:build integration

package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/ledger"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wagering"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wallet"
)

var (
	testPool   *pgxpool.Pool
	walletRepo = NewWalletRepo()
	wagerRepo  = NewWageringRepo()
	ledgerRepo = NewLedgerRepo()
	inboxRepo  = NewInboxRepo()
	outboxRepo = NewOutboxRepo()
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "postgres:16",
			ExposedPorts: []string{"5432/tcp"},
			Env: map[string]string{
				"POSTGRES_USER":     "test",
				"POSTGRES_PASSWORD": "test",
				"POSTGRES_DB":       "wallet_test",
			},
			WaitingFor: wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if container != nil {
		defer func() { _ = container.Terminate(context.Background()) }()
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres container: %v\n", err)
		return 1
	}

	host, err := container.Host(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "container host: %v\n", err)
		return 1
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		fmt.Fprintf(os.Stderr, "container port: %v\n", err)
		return 1
	}
	dsn := fmt.Sprintf("postgres://test:test@%s:%s/wallet_test?sslmode=disable", host, port.Port())

	testPool, err = connectWithRetry(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect: %v\n", err)
		return 1
	}
	defer testPool.Close()

	if err := applyMigrations(ctx, testPool); err != nil {
		fmt.Fprintf(os.Stderr, "migrations: %v\n", err)
		return 1
	}
	return m.Run()
}

func connectWithRetry(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	var lastErr error
	for i := 0; i < 20; i++ {
		pool, err := NewPool(ctx, dsn)
		if err == nil {
			return pool, nil
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	return nil, lastErr
}

func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "migrations", "*.up.sql"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("no migration files found")
	}
	sort.Strings(files)
	for _, f := range files {
		content, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(content)); err != nil {
			return fmt.Errorf("apply %s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

// ---------------------------------------------------------------- helpers

func newUUID(t *testing.T) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), "SELECT gen_random_uuid()::text").Scan(&id); err != nil {
		t.Fatalf("gen_random_uuid: %v", err)
	}
	return id
}

func nowUTC() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

func brl(t *testing.T, cents int64) money.Money {
	t.Helper()
	m, err := money.FromCents(cents, "BRL")
	if err != nil {
		t.Fatalf("FromCents: %v", err)
	}
	return m
}

func inTx(fn func(tx pgx.Tx) error) error {
	return WithTx(context.Background(), testPool, fn)
}

func asPgError(err error) *pgconn.PgError {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr
	}
	return nil
}

func persistWallet(t *testing.T, balance int64) *wallet.Wallet {
	t.Helper()
	w, err := wallet.New(newUUID(t), "player-"+newUUID(t), brl(t, balance), nowUTC())
	if err != nil {
		t.Fatalf("wallet.New: %v", err)
	}
	if err := inTx(func(tx pgx.Tx) error { return walletRepo.Create(context.Background(), tx, w) }); err != nil {
		t.Fatalf("create wallet: %v", err)
	}
	return w
}

func wagerParams(t *testing.T, walletID string, kind wagering.Kind, cents int64) wagering.NewExternalParams {
	t.Helper()
	p := wagering.NewExternalParams{
		ID:                    newUUID(t),
		WalletID:              walletID,
		Kind:                  kind,
		Amount:                brl(t, cents),
		ProviderID:            "provider-a",
		ExternalTransactionID: "ext-" + newUUID(t),
		IdempotencyKey:        "idem-" + newUUID(t),
		RequestHash:           "hash-1",
		RoundID:               "round-1",
		GameID:                "game-1",
		Now:                   nowUTC(),
	}
	if kind == wagering.KindRefund || kind == wagering.KindRollback {
		p.ReferenceExternalTransactionID = "ext-ref"
	}
	return p
}

func persistWager(t *testing.T, walletID string, kind wagering.Kind, cents int64) *wagering.WagerTransaction {
	t.Helper()
	tx, err := wagering.NewExternal(wagerParams(t, walletID, kind, cents))
	if err != nil {
		t.Fatalf("wagering.NewExternal: %v", err)
	}
	if err := inTx(func(pgTx pgx.Tx) error { return wagerRepo.Create(context.Background(), pgTx, tx) }); err != nil {
		t.Fatalf("create wager transaction: %v", err)
	}
	return tx
}

func newEntry(t *testing.T, walletID, txID string, dir ledger.Direction, amount, before, after int64, at time.Time) ledger.Entry {
	t.Helper()
	e, err := ledger.New(newUUID(t), walletID, txID, dir, brl(t, amount), brl(t, before), brl(t, after), at)
	if err != nil {
		t.Fatalf("ledger.New: %v", err)
	}
	return e
}

func persistEntry(t *testing.T, e ledger.Entry) error {
	t.Helper()
	return inTx(func(tx pgx.Tx) error { return ledgerRepo.Create(context.Background(), tx, &e) })
}

// ---------------------------------------------------------------- WithTx

func TestWithTx(t *testing.T) {
	ctx := context.Background()

	newWalletValue := func(t *testing.T) *wallet.Wallet {
		t.Helper()
		w, err := wallet.New(newUUID(t), "player-"+newUUID(t), brl(t, 0), nowUTC())
		if err != nil {
			t.Fatalf("wallet.New: %v", err)
		}
		return w
	}

	t.Run("commits on success", func(t *testing.T) {
		w := newWalletValue(t)
		if err := inTx(func(tx pgx.Tx) error { return walletRepo.Create(ctx, tx, w) }); err != nil {
			t.Fatalf("WithTx: %v", err)
		}
		if _, err := walletRepo.GetByID(ctx, testPool, w.ID()); err != nil {
			t.Fatalf("wallet should be committed: %v", err)
		}
	})

	t.Run("rolls back on error", func(t *testing.T) {
		w := newWalletValue(t)
		boom := errors.New("boom")
		err := inTx(func(tx pgx.Tx) error {
			if err := walletRepo.Create(ctx, tx, w); err != nil {
				return err
			}
			return boom
		})
		if !errors.Is(err, boom) {
			t.Fatalf("error = %v, want boom", err)
		}
		if _, err := walletRepo.GetByID(ctx, testPool, w.ID()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("wallet should be rolled back, got %v", err)
		}
	})

	t.Run("rolls back and re-panics", func(t *testing.T) {
		w := newWalletValue(t)
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			_ = inTx(func(tx pgx.Tx) error {
				if err := walletRepo.Create(ctx, tx, w); err != nil {
					return err
				}
				panic("kaboom")
			})
		}()
		if recovered != "kaboom" {
			t.Fatalf("recovered = %v, want kaboom", recovered)
		}
		if _, err := walletRepo.GetByID(ctx, testPool, w.ID()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("wallet should be rolled back, got %v", err)
		}
	})
}

// ---------------------------------------------------------------- wallets

func TestWalletRepo_CreateAndGet(t *testing.T) {
	ctx := context.Background()
	w := persistWallet(t, 1000)

	got, err := walletRepo.GetByID(ctx, testPool, w.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID() != w.ID() || got.PlayerID() != w.PlayerID() || got.Currency() != "BRL" ||
		!got.Balance().Equals(w.Balance()) || got.Version() != 1 ||
		!got.CreatedAt().Equal(w.CreatedAt()) || !got.UpdatedAt().Equal(w.UpdatedAt()) {
		t.Fatalf("loaded wallet differs: %+v vs %+v", got, w)
	}

	if _, err := walletRepo.GetByID(ctx, testPool, newUUID(t)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing wallet error = %v, want ErrNotFound", err)
	}

	err = inTx(func(tx pgx.Tx) error {
		locked, err := walletRepo.GetByIDForUpdate(ctx, tx, w.ID())
		if err != nil {
			return err
		}
		if locked.ID() != w.ID() {
			return fmt.Errorf("locked wallet id = %s", locked.ID())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("GetByIDForUpdate: %v", err)
	}
}

func TestWalletRepo_UniquePlayerCurrency(t *testing.T) {
	ctx := context.Background()
	first := persistWallet(t, 0)

	dup, err := wallet.New(newUUID(t), first.PlayerID(), brl(t, 0), nowUTC())
	if err != nil {
		t.Fatalf("wallet.New: %v", err)
	}
	err = inTx(func(tx pgx.Tx) error { return walletRepo.Create(ctx, tx, dup) })
	if !IsUniqueViolation(err, ConstraintWalletsPlayerCurrency) {
		t.Fatalf("error = %v, want unique violation on %s", err, ConstraintWalletsPlayerCurrency)
	}

	usd, err := money.FromCents(0, "USD")
	if err != nil {
		t.Fatalf("FromCents: %v", err)
	}
	other, err := wallet.New(newUUID(t), first.PlayerID(), usd, nowUTC())
	if err != nil {
		t.Fatalf("wallet.New: %v", err)
	}
	if err := inTx(func(tx pgx.Tx) error { return walletRepo.Create(ctx, tx, other) }); err != nil {
		t.Fatalf("same player in another currency must be allowed: %v", err)
	}
}

func TestWalletRepo_BalanceCheck(t *testing.T) {
	ctx := context.Background()

	_, err := testPool.Exec(ctx,
		`INSERT INTO wallets (id, player_id, currency, balance_cents) VALUES ($1, $2, 'BRL', -1)`,
		newUUID(t), "p-"+newUUID(t))
	if !IsCheckViolation(err, "ck_wallets_balance_non_negative") {
		t.Fatalf("insert error = %v, want check violation", err)
	}

	w := persistWallet(t, 100)
	_, err = testPool.Exec(ctx, `UPDATE wallets SET balance_cents = -1 WHERE id = $1`, w.ID())
	if !IsCheckViolation(err, "ck_wallets_balance_non_negative") {
		t.Fatalf("update error = %v, want check violation", err)
	}
}

func TestWalletRepo_UpdateBalanceOptimisticLock(t *testing.T) {
	ctx := context.Background()
	w := persistWallet(t, 1000)

	a, err := walletRepo.GetByID(ctx, testPool, w.ID())
	if err != nil {
		t.Fatalf("GetByID a: %v", err)
	}
	b, err := walletRepo.GetByID(ctx, testPool, w.ID())
	if err != nil {
		t.Fatalf("GetByID b: %v", err)
	}

	if err := a.Debit(ctx, brl(t, 100)); err != nil {
		t.Fatalf("Debit: %v", err)
	}
	if err := inTx(func(tx pgx.Tx) error { return walletRepo.UpdateBalance(ctx, tx, a) }); err != nil {
		t.Fatalf("UpdateBalance a: %v", err)
	}

	if err := b.Credit(ctx, brl(t, 50)); err != nil {
		t.Fatalf("Credit: %v", err)
	}
	err = inTx(func(tx pgx.Tx) error { return walletRepo.UpdateBalance(ctx, tx, b) })
	if !errors.Is(err, ErrOptimisticLock) {
		t.Fatalf("stale update error = %v, want ErrOptimisticLock", err)
	}

	got, err := walletRepo.GetByID(ctx, testPool, w.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Balance().Cents() != 900 || got.Version() != 2 {
		t.Fatalf("balance=%s version=%d, want 900 / 2", got.Balance(), got.Version())
	}

	fresh, err := walletRepo.GetByID(ctx, testPool, w.ID())
	if err != nil {
		t.Fatalf("GetByID fresh: %v", err)
	}
	err = inTx(func(tx pgx.Tx) error { return walletRepo.UpdateBalance(ctx, tx, fresh) })
	if !errors.Is(err, ErrOptimisticLock) {
		t.Fatalf("update without change error = %v, want ErrOptimisticLock", err)
	}
}

func TestWalletRepo_ForUpdateBlocksOtherTransactions(t *testing.T) {
	ctx := context.Background()
	w := persistWallet(t, 100)

	tx1, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	defer func() { _ = tx1.Rollback(ctx) }()
	if _, err := walletRepo.GetByIDForUpdate(ctx, tx1, w.ID()); err != nil {
		t.Fatalf("lock in tx1: %v", err)
	}

	tx2, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer func() { _ = tx2.Rollback(ctx) }()
	if _, err := tx2.Exec(ctx, "SET LOCAL lock_timeout = '200ms'"); err != nil {
		t.Fatalf("set lock_timeout: %v", err)
	}
	_, err = walletRepo.GetByIDForUpdate(ctx, tx2, w.ID())
	if pgErr := asPgError(err); pgErr == nil || pgErr.Code != "55P03" {
		t.Fatalf("second lock error = %v, want lock_not_available (55P03)", err)
	}
}

// ---------------------------------------------------------------- wager transactions

func TestWageringRepo_CreateAndGet(t *testing.T) {
	ctx := context.Background()
	w := persistWallet(t, 0)
	bet := persistWager(t, w.ID(), wagering.KindBet, 500)

	checks := map[string]func() (*wagering.WagerTransaction, error){
		"by id": func() (*wagering.WagerTransaction, error) { return wagerRepo.GetByID(ctx, testPool, bet.ID) },
		"by idempotency key": func() (*wagering.WagerTransaction, error) {
			return wagerRepo.GetByIdempotencyKey(ctx, testPool, bet.IdempotencyKey)
		},
		"by provider and external": func() (*wagering.WagerTransaction, error) {
			return wagerRepo.GetByProviderAndExternal(ctx, testPool, bet.ProviderID, bet.ExternalTransactionID)
		},
	}
	for name, get := range checks {
		got, err := get()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.ID != bet.ID || got.WalletID != w.ID() || got.Kind != wagering.KindBet ||
			got.Status != wagering.StatusPending || !got.Amount.Equals(bet.Amount) ||
			got.ProviderID != bet.ProviderID || got.ExternalTransactionID != bet.ExternalTransactionID ||
			got.IdempotencyKey != bet.IdempotencyKey || got.RequestHash != bet.RequestHash ||
			got.RoundID != "round-1" || got.GameID != "game-1" ||
			got.ResultBalance != nil || got.FailureCode != "" || got.ResolvedReferenceTransactionID != "" ||
			!got.CreatedAt.Equal(bet.CreatedAt) {
			t.Fatalf("%s: loaded transaction differs: %+v vs %+v", name, got, bet)
		}
	}

	if _, err := wagerRepo.GetByID(ctx, testPool, newUUID(t)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByID missing = %v", err)
	}
	if _, err := wagerRepo.GetByIdempotencyKey(ctx, testPool, "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByIdempotencyKey missing = %v", err)
	}
	if _, err := wagerRepo.GetByProviderAndExternal(ctx, testPool, "provider-a", "nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetByProviderAndExternal missing = %v", err)
	}
}

func TestWageringRepo_OpeningRoundTrip(t *testing.T) {
	ctx := context.Background()
	w := persistWallet(t, 0)
	opening, err := wagering.NewOpening(newUUID(t), w.ID(), brl(t, 10000), nowUTC())
	if err != nil {
		t.Fatalf("NewOpening: %v", err)
	}
	if err := inTx(func(tx pgx.Tx) error { return wagerRepo.Create(ctx, tx, opening) }); err != nil {
		t.Fatalf("Create opening: %v", err)
	}
	got, err := wagerRepo.GetByID(ctx, testPool, opening.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Kind != wagering.KindOpening || got.Status != wagering.StatusProcessed ||
		got.ProviderID != "" || got.ExternalTransactionID != "" || got.IdempotencyKey != "" || got.RequestHash != "" ||
		got.ResultBalance == nil || got.ResultBalance.Cents() != 10000 {
		t.Fatalf("opening round trip differs: %+v", got)
	}
}

func TestWageringRepo_UniqueConstraints(t *testing.T) {
	ctx := context.Background()
	w := persistWallet(t, 0)
	bet := persistWager(t, w.ID(), wagering.KindBet, 500)

	sameExternal := wagerParams(t, w.ID(), wagering.KindBet, 500)
	sameExternal.ProviderID = bet.ProviderID
	sameExternal.ExternalTransactionID = bet.ExternalTransactionID
	dup1, err := wagering.NewExternal(sameExternal)
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	err = inTx(func(tx pgx.Tx) error { return wagerRepo.Create(ctx, tx, dup1) })
	if !IsUniqueViolation(err, ConstraintWagerProviderExternal) {
		t.Fatalf("error = %v, want unique violation on %s", err, ConstraintWagerProviderExternal)
	}

	sameKey := wagerParams(t, w.ID(), wagering.KindBet, 500)
	sameKey.IdempotencyKey = bet.IdempotencyKey
	dup2, err := wagering.NewExternal(sameKey)
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	err = inTx(func(tx pgx.Tx) error { return wagerRepo.Create(ctx, tx, dup2) })
	if !IsUniqueViolation(err, ConstraintWagerIdempotencyKey) {
		t.Fatalf("error = %v, want unique violation on %s", err, ConstraintWagerIdempotencyKey)
	}

	otherProvider := wagerParams(t, w.ID(), wagering.KindBet, 500)
	otherProvider.ProviderID = "provider-b"
	otherProvider.ExternalTransactionID = bet.ExternalTransactionID
	ok, err := wagering.NewExternal(otherProvider)
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	if err := inTx(func(tx pgx.Tx) error { return wagerRepo.Create(ctx, tx, ok) }); err != nil {
		t.Fatalf("same external id under another provider must be allowed: %v", err)
	}
}

func TestWageringRepo_Update(t *testing.T) {
	ctx := context.Background()
	w := persistWallet(t, 0)
	bet := persistWager(t, w.ID(), wagering.KindBet, 500)
	refund := persistWager(t, w.ID(), wagering.KindRefund, 500)

	if err := refund.MarkPendingReference(); err != nil {
		t.Fatalf("MarkPendingReference: %v", err)
	}
	if err := inTx(func(tx pgx.Tx) error { return wagerRepo.Update(ctx, tx, refund) }); err != nil {
		t.Fatalf("Update pending reference: %v", err)
	}
	got, err := wagerRepo.GetByID(ctx, testPool, refund.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != wagering.StatusPendingReference {
		t.Fatalf("status = %s, want PENDING_REFERENCE", got.Status)
	}

	if err := refund.ResolveReference(bet.ID); err != nil {
		t.Fatalf("ResolveReference: %v", err)
	}
	if err := refund.MarkProcessed(brl(t, 900)); err != nil {
		t.Fatalf("MarkProcessed: %v", err)
	}
	if err := inTx(func(tx pgx.Tx) error { return wagerRepo.Update(ctx, tx, refund) }); err != nil {
		t.Fatalf("Update processed: %v", err)
	}
	got, err = wagerRepo.GetByID(ctx, testPool, refund.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != wagering.StatusProcessed || got.ResolvedReferenceTransactionID != bet.ID ||
		got.ResultBalance == nil || got.ResultBalance.Cents() != 900 ||
		!got.UpdatedAt.Equal(refund.UpdatedAt.Truncate(time.Microsecond)) {
		t.Fatalf("persisted state differs: %+v", got)
	}

	if err := bet.MarkRejected("INSUFFICIENT_FUNDS"); err != nil {
		t.Fatalf("MarkRejected: %v", err)
	}
	if err := inTx(func(tx pgx.Tx) error { return wagerRepo.Update(ctx, tx, bet) }); err != nil {
		t.Fatalf("Update rejected: %v", err)
	}
	got, err = wagerRepo.GetByID(ctx, testPool, bet.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Status != wagering.StatusRejected || got.FailureCode != "INSUFFICIENT_FUNDS" {
		t.Fatalf("rejected state differs: %+v", got)
	}

	ghost, err := wagering.NewExternal(wagerParams(t, w.ID(), wagering.KindBet, 100))
	if err != nil {
		t.Fatalf("NewExternal: %v", err)
	}
	err = inTx(func(tx pgx.Tx) error { return wagerRepo.Update(ctx, tx, ghost) })
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of unknown transaction error = %v, want ErrNotFound", err)
	}
}

// ---------------------------------------------------------------- ledger

func TestLedgerRepo_UniqueWalletTransaction(t *testing.T) {
	w := persistWallet(t, 0)
	tx := persistWager(t, w.ID(), wagering.KindWin, 100)
	at := nowUTC()

	if err := persistEntry(t, newEntry(t, w.ID(), tx.ID, ledger.DirectionCredit, 100, 0, 100, at)); err != nil {
		t.Fatalf("first entry: %v", err)
	}
	err := persistEntry(t, newEntry(t, w.ID(), tx.ID, ledger.DirectionCredit, 100, 100, 200, at))
	if !IsUniqueViolation(err, ConstraintLedgerWalletTransaction) {
		t.Fatalf("error = %v, want unique violation on %s", err, ConstraintLedgerWalletTransaction)
	}
}

func TestLedgerRepo_TriggerBlocksMutation(t *testing.T) {
	ctx := context.Background()
	w := persistWallet(t, 0)
	tx := persistWager(t, w.ID(), wagering.KindWin, 100)
	entry := newEntry(t, w.ID(), tx.ID, ledger.DirectionCredit, 100, 0, 100, nowUTC())
	if err := persistEntry(t, entry); err != nil {
		t.Fatalf("persist entry: %v", err)
	}

	statements := map[string]string{
		"update row":     fmt.Sprintf("UPDATE wallet_ledger_entries SET amount_cents = 0 WHERE id = '%s'", entry.ID()),
		"update all":     "UPDATE wallet_ledger_entries SET amount_cents = 0",
		"update no rows": "UPDATE wallet_ledger_entries SET amount_cents = 0 WHERE false",
		"delete row":     fmt.Sprintf("DELETE FROM wallet_ledger_entries WHERE id = '%s'", entry.ID()),
		"delete all":     "DELETE FROM wallet_ledger_entries",
		"truncate":       "TRUNCATE wallet_ledger_entries",
	}
	for name, stmt := range statements {
		_, err := testPool.Exec(ctx, stmt)
		pgErr := asPgError(err)
		if pgErr == nil || pgErr.Code != "P0001" || !strings.Contains(pgErr.Message, "append-only") {
			t.Fatalf("%s: error = %v, want append-only exception", name, err)
		}
	}

	var amount int64
	if err := testPool.QueryRow(ctx, `SELECT amount_cents FROM wallet_ledger_entries WHERE id = $1`, entry.ID()).Scan(&amount); err != nil {
		t.Fatalf("entry must still exist: %v", err)
	}
	if amount != 100 {
		t.Fatalf("amount = %d, want 100", amount)
	}
}

func TestLedgerRepo_ListByWalletCursor(t *testing.T) {
	ctx := context.Background()
	w := persistWallet(t, 0)

	base := nowUTC().Add(-time.Hour)
	times := []time.Time{base, base.Add(time.Second), base.Add(2 * time.Second), base.Add(2 * time.Second), base.Add(3 * time.Second)}
	var want []ledger.Entry
	balance := int64(0)
	for _, at := range times {
		tx := persistWager(t, w.ID(), wagering.KindWin, 100)
		e := newEntry(t, w.ID(), tx.ID, ledger.DirectionCredit, 100, balance, balance+100, at)
		if err := persistEntry(t, e); err != nil {
			t.Fatalf("persist entry: %v", err)
		}
		balance += 100
		want = append(want, e)
	}
	sort.Slice(want, func(i, j int) bool {
		ti, tj := want[i].CreatedAt(), want[j].CreatedAt()
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return want[i].ID() > want[j].ID()
	})

	var got []ledger.Entry
	cursor, pages := "", 0
	for {
		page, next, err := ledgerRepo.ListByWalletCursor(ctx, testPool, w.ID(), cursor, 2)
		if err != nil {
			t.Fatalf("ListByWalletCursor: %v", err)
		}
		got = append(got, page...)
		pages++
		if next == "" {
			break
		}
		if pages > 10 {
			t.Fatal("too many pages")
		}
		cursor = next
	}
	if pages != 3 || len(got) != len(want) {
		t.Fatalf("pages=%d entries=%d, want 3 / %d", pages, len(got), len(want))
	}
	for i := range want {
		if got[i].ID() != want[i].ID() || !got[i].CreatedAt().Equal(want[i].CreatedAt()) ||
			got[i].Direction() != ledger.DirectionCredit || got[i].Amount().Cents() != 100 {
			t.Fatalf("entry %d = %s, want %s", i, got[i].ID(), want[i].ID())
		}
	}

	all, next, err := ledgerRepo.ListByWalletCursor(ctx, testPool, w.ID(), "", 5)
	if err != nil || len(all) != 5 || next != "" {
		t.Fatalf("limit equal to total: len=%d next=%q err=%v", len(all), next, err)
	}

	empty, next, err := ledgerRepo.ListByWalletCursor(ctx, testPool, newUUID(t), "", 10)
	if err != nil || len(empty) != 0 || next != "" {
		t.Fatalf("empty wallet: len=%d next=%q err=%v", len(empty), next, err)
	}

	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for _, bad := range []string{"!!!", enc("abc"), enc("123|nope"), enc("x|" + newUUID(t))} {
		if _, _, err := ledgerRepo.ListByWalletCursor(ctx, testPool, w.ID(), bad, 2); !errors.Is(err, ErrInvalidCursor) {
			t.Fatalf("cursor %q error = %v, want ErrInvalidCursor", bad, err)
		}
	}
}

// ---------------------------------------------------------------- inbox

func TestInboxRepo_TryInsert(t *testing.T) {
	ctx := context.Background()
	consumer := "consumer-" + newUUID(t)

	try := func(c, m, h string) bool {
		t.Helper()
		var inserted bool
		err := inTx(func(tx pgx.Tx) error {
			var e error
			inserted, e = inboxRepo.TryInsert(ctx, tx, c, m, h)
			return e
		})
		if err != nil {
			t.Fatalf("TryInsert(%s, %s): %v", c, m, err)
		}
		return inserted
	}

	if !try(consumer, "msg-1", "hash-a") {
		t.Fatal("first insert must report inserted")
	}
	if try(consumer, "msg-1", "hash-b") {
		t.Fatal("duplicate (consumer, message) must report not inserted")
	}
	if !try("other-"+consumer, "msg-1", "hash-a") {
		t.Fatal("same message for another consumer must be inserted")
	}
	if !try(consumer, "msg-2", "hash-a") {
		t.Fatal("another message for the same consumer must be inserted")
	}

	_, err := testPool.Exec(ctx,
		`INSERT INTO inbox (consumer_name, message_id, payload_hash) VALUES ($1, 'msg-1', 'x')`, consumer)
	if !IsUniqueViolation(err, ConstraintInboxConsumerMessage) {
		t.Fatalf("raw duplicate error = %v, want unique violation on %s", err, ConstraintInboxConsumerMessage)
	}

	var stored string
	if err := testPool.QueryRow(ctx,
		`SELECT payload_hash FROM inbox WHERE consumer_name = $1 AND message_id = 'msg-1'`, consumer).Scan(&stored); err != nil {
		t.Fatalf("select payload_hash: %v", err)
	}
	if stored != "hash-a" {
		t.Fatalf("payload_hash = %q, want hash-a", stored)
	}

	err = inTx(func(tx pgx.Tx) error {
		_, e := inboxRepo.TryInsert(ctx, tx, "  ", "msg", "h")
		return e
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("blank consumer error = %v, want ErrInvalidArgument", err)
	}
}

// ---------------------------------------------------------------- outbox

type outboxState struct {
	status      string
	attempts    int
	lastError   *string
	publishedAt *time.Time
	nextAttempt time.Time
}

func fetchOutbox(t *testing.T, id string) outboxState {
	t.Helper()
	var s outboxState
	err := testPool.QueryRow(context.Background(),
		`SELECT status, attempts, last_error, published_at, next_attempt_at FROM outbox WHERE id = $1`, id,
	).Scan(&s.status, &s.attempts, &s.lastError, &s.publishedAt, &s.nextAttempt)
	if err != nil {
		t.Fatalf("fetch outbox %s: %v", id, err)
	}
	return s
}

func TestOutboxRepo(t *testing.T) {
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, "TRUNCATE outbox"); err != nil {
		t.Fatalf("truncate outbox: %v", err)
	}
	now := time.Now().UTC()

	insert := func(eventID string, next time.Time) *Event {
		t.Helper()
		ev := &Event{
			EventID:        eventID,
			AggregateType:  "wager_transaction",
			AggregateID:    "agg-1",
			EventType:      "wager.processed",
			MessageGroupID: "group-1",
			Payload:        []byte(`{"n":1}`),
			NextAttemptAt:  next,
		}
		if err := inTx(func(tx pgx.Tx) error { return outboxRepo.Insert(ctx, tx, ev) }); err != nil {
			t.Fatalf("Insert: %v", err)
		}
		return ev
	}

	e1 := insert(newUUID(t), now.Add(-2*time.Minute))
	e2 := insert(newUUID(t), now.Add(-time.Minute))
	e3 := insert(newUUID(t), now.Add(time.Hour))
	if e1.ID == "" || e1.Status != "PENDING" || e1.Attempts != 0 || e1.CreatedAt.IsZero() {
		t.Fatalf("Insert did not fill the event: %+v", e1)
	}

	err := inTx(func(tx pgx.Tx) error {
		return outboxRepo.Insert(ctx, tx, &Event{
			EventID: e1.EventID, AggregateType: "a", AggregateID: "b", EventType: "c", MessageGroupID: "d", Payload: []byte(`{}`),
		})
	})
	if !IsUniqueViolation(err, ConstraintOutboxEventID) {
		t.Fatalf("duplicate event id error = %v, want unique violation on %s", err, ConstraintOutboxEventID)
	}
	err = inTx(func(tx pgx.Tx) error {
		return outboxRepo.Insert(ctx, tx, &Event{
			EventID: newUUID(t), AggregateType: "a", AggregateID: "b", EventType: "c", MessageGroupID: "d", Payload: []byte(`not json`),
		})
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid payload error = %v, want ErrInvalidArgument", err)
	}

	txA, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	defer func() { _ = txA.Rollback(ctx) }()
	claimedA, err := outboxRepo.ClaimBatch(ctx, txA, now, 10)
	if err != nil {
		t.Fatalf("ClaimBatch A: %v", err)
	}
	if len(claimedA) != 2 || claimedA[0].ID != e1.ID || claimedA[1].ID != e2.ID {
		t.Fatalf("claimed A = %+v, want e1 then e2 (e3 is not due)", claimedA)
	}
	var payload map[string]any
	if err := json.Unmarshal(claimedA[0].Payload, &payload); err != nil || payload["n"] != float64(1) {
		t.Fatalf("payload = %s (%v)", claimedA[0].Payload, err)
	}
	if claimedA[0].Status != "PENDING" || claimedA[0].PublishedAt != nil || claimedA[0].LastError != "" {
		t.Fatalf("claimed event state wrong: %+v", claimedA[0])
	}

	txB, err := testPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin B: %v", err)
	}
	defer func() { _ = txB.Rollback(ctx) }()
	claimedB, err := outboxRepo.ClaimBatch(ctx, txB, now, 10)
	if err != nil {
		t.Fatalf("ClaimBatch B: %v", err)
	}
	if len(claimedB) != 0 {
		t.Fatalf("SKIP LOCKED: second relay claimed %d rows, want 0", len(claimedB))
	}
	if err := txB.Rollback(ctx); err != nil {
		t.Fatalf("rollback B: %v", err)
	}
	if err := txA.Rollback(ctx); err != nil {
		t.Fatalf("rollback A: %v", err)
	}

	retryAt := now.Add(time.Hour)
	err = inTx(func(tx pgx.Tx) error {
		if err := outboxRepo.MarkPublished(ctx, tx, e1.ID); err != nil {
			return err
		}
		return outboxRepo.MarkRetry(ctx, tx, e2.ID, retryAt, "boom")
	})
	if err != nil {
		t.Fatalf("mark published/retry: %v", err)
	}

	s1 := fetchOutbox(t, e1.ID)
	if s1.status != "PUBLISHED" || s1.attempts != 1 || s1.publishedAt == nil || s1.lastError != nil {
		t.Fatalf("e1 state = %+v", s1)
	}
	s2 := fetchOutbox(t, e2.ID)
	if s2.status != "PENDING" || s2.attempts != 1 || s2.lastError == nil || *s2.lastError != "boom" ||
		s2.nextAttempt.Before(retryAt.Add(-time.Second)) {
		t.Fatalf("e2 state = %+v", s2)
	}

	err = inTx(func(tx pgx.Tx) error { return outboxRepo.MarkPublished(ctx, tx, e1.ID) })
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("publishing twice error = %v, want ErrNotFound", err)
	}
	err = inTx(func(tx pgx.Tx) error { return outboxRepo.MarkRetry(ctx, tx, e1.ID, retryAt, "x") })
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("retrying a published event error = %v, want ErrNotFound", err)
	}

	var due []Event
	err = inTx(func(tx pgx.Tx) error {
		var e error
		due, e = outboxRepo.ClaimBatch(ctx, tx, now, 10)
		return e
	})
	if err != nil || len(due) != 0 {
		t.Fatalf("nothing should be due now: len=%d err=%v", len(due), err)
	}

	var later []Event
	err = inTx(func(tx pgx.Tx) error {
		var e error
		later, e = outboxRepo.ClaimBatch(ctx, tx, now.Add(2*time.Hour), 10)
		return e
	})
	if err != nil || len(later) != 2 || later[0].ID != e2.ID || later[1].ID != e3.ID {
		t.Fatalf("claim in the future = %+v (err %v), want e2 then e3", later, err)
	}
	if later[0].Attempts != 1 || later[0].LastError != "boom" {
		t.Fatalf("retry data lost: %+v", later[0])
	}

	if err := inTx(func(tx pgx.Tx) error {
		_, e := outboxRepo.ClaimBatch(ctx, tx, now, 0)
		return e
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("limit 0 error = %v, want ErrInvalidArgument", err)
	}
}
