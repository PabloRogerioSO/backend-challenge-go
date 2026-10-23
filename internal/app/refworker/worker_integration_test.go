//go:build integration

package refworker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/app/usecase"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wagering"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
)

var (
	testPool    *pgxpool.Pool
	wallets     *postgres.WalletRepo
	wagers      *postgres.WageringRepo
	ledgers     *postgres.LedgerRepo
	outboxRepo  *postgres.OutboxRepo
	pendingRepo *postgres.PendingRepo
	processor   *usecase.WagerProcessor
	opener      *usecase.WalletOpener
	txRunner    postgres.TxRunner
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	ctx := context.Background()

	pg, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
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
	if err != nil {
		fmt.Fprintf(os.Stderr, "start postgres: %v\n", err)
		return 1
	}
	defer func() { _ = pg.Terminate(context.Background()) }()

	pgHost, _ := pg.Host(ctx)
	pgPort, _ := pg.MappedPort(ctx, "5432/tcp")
	dsn := fmt.Sprintf("postgres://test:test@%s:%s/wallet_test?sslmode=disable", pgHost, pgPort.Port())
	testPool, err = postgres.NewPool(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "connect postgres: %v\n", err)
		return 1
	}
	defer testPool.Close()

	if err := applyMigrations(ctx, testPool); err != nil {
		fmt.Fprintf(os.Stderr, "migrations: %v\n", err)
		return 1
	}

	wallets = postgres.NewWalletRepo()
	wagers = postgres.NewWageringRepo()
	ledgers = postgres.NewLedgerRepo()
	outboxRepo = postgres.NewOutboxRepo()
	pendingRepo = postgres.NewPendingRepo(testPool)
	processor = usecase.NewWagerProcessor(testPool, wallets, wagers, ledgers, outboxRepo)
	opener = usecase.NewWalletOpener(testPool, wallets, wagers, ledgers, outboxRepo)
	txRunner = postgres.NewTxRunner(testPool)

	return m.Run()
}

func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "migrations", "*.up.sql"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no migration files found")
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

func openWallet(t *testing.T, cents int64) string {
	t.Helper()
	m, err := money.FromCents(cents, "BRL")
	if err != nil {
		t.Fatalf("FromCents: %v", err)
	}
	w, err := opener.OpenWallet(context.Background(), "player-"+newUUID(t), m)
	if err != nil {
		t.Fatalf("OpenWallet: %v", err)
	}
	return w.ID()
}

func processWager(t *testing.T, in usecase.ProcessWagerInput) usecase.ProcessWagerOutput {
	t.Helper()
	out, err := processor.ProcessWager(context.Background(), in)
	if err != nil {
		t.Fatalf("ProcessWager(%s): %v", in.Kind, err)
	}
	return out
}

// newBET builds and processes a BET on walletID with the given external id.
func newBET(t *testing.T, walletID, extID string, cents int64) usecase.ProcessWagerOutput {
	t.Helper()
	amount, err := money.FromCents(cents, "BRL")
	if err != nil {
		t.Fatalf("FromCents: %v", err)
	}
	return processWager(t, usecase.ProcessWagerInput{
		ProviderID:            "provider-a",
		WalletID:              walletID,
		IdempotencyKey:        "idem-" + extID,
		ExternalTransactionID: extID,
		Kind:                  wagering.KindBet,
		Amount:                amount,
		RoundID:               "round-1",
		GameID:                "game-1",
	})
}

// newREFUND builds a REFUND referencing refExtID. If the reference does not
// exist yet, the transaction ends up in PENDING_REFERENCE.
func newREFUND(t *testing.T, walletID, extID, refExtID string, cents int64) usecase.ProcessWagerOutput {
	t.Helper()
	amount, err := money.FromCents(cents, "BRL")
	if err != nil {
		t.Fatalf("FromCents: %v", err)
	}
	return processWager(t, usecase.ProcessWagerInput{
		ProviderID:                     "provider-a",
		WalletID:                       walletID,
		IdempotencyKey:                 "idem-" + extID,
		ExternalTransactionID:          extID,
		Kind:                           wagering.KindRefund,
		Amount:                         amount,
		RoundID:                        "round-1",
		GameID:                         "game-1",
		ReferenceExternalTransactionID: refExtID,
	})
}

// newIntegrationWorker builds a Worker wired to Postgres with an injectable clock.
// The name differs from newWorker (in worker_test.go) to avoid collisions.
func newIntegrationWorker(t *testing.T, ttl time.Duration, now time.Time) *Worker {
	t.Helper()
	w := NewWorker(Config{
		TTL:       ttl,
		BatchSize: 100,
		Timeout:   10 * time.Second,
	}, pendingRepo, outboxRepo, processor, txRunner, nil)
	w.now = func() time.Time { return now }
	return w
}

func walletBalance(t *testing.T, walletID string) int64 {
	t.Helper()
	w, err := wallets.GetByID(context.Background(), testPool, walletID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	return w.Balance().Cents()
}

// txStatus reads the persisted status of a transaction by external id.
func txStatus(t *testing.T, providerID, extID string) (wagering.Status, string) {
	t.Helper()
	tx, err := wagers.GetByProviderAndExternal(context.Background(), testPool, providerID, extID)
	if err != nil {
		t.Fatalf("GetByProviderAndExternal(%s): %v", extID, err)
	}
	return tx.Status, tx.FailureCode
}

// outboxEventCount counts outbox events of a given type for an aggregate.
func outboxEventCount(t *testing.T, aggregateID, eventType string) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM outbox WHERE aggregate_id = $1 AND event_type = $2`,
		aggregateID, eventType).Scan(&n); err != nil {
		t.Fatalf("query outbox: %v", err)
	}
	return n
}

// ---------------------------------------------------------------- tests

// TestWorker_ResolvesPendingReference proves that a REFUND that arrived
// before its referenced BET is resolved once the BET shows up.
func TestWorker_ResolvesPendingReference(t *testing.T) {
	walletID := openWallet(t, 10000)

	// REFUND arrives first: the reference does not exist yet.
	refund := newREFUND(t, walletID, "ext-refund-1", "ext-bet-1", 2500)
	if refund.Status != wagering.StatusPendingReference {
		t.Fatalf("refund status = %s, want PENDING_REFERENCE", refund.Status)
	}

	// BET arrives now, the reference is satisfied.
	newBET(t, walletID, "ext-bet-1", 2500)

	// Worker runs with a fresh clock.
	worker := newIntegrationWorker(t, 24*time.Hour, time.Now().UTC())
	n, err := worker.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 1 {
		t.Fatalf("resolved = %d, want 1", n)
	}

	// REFUND must now be PROCESSED.
	status, failure := txStatus(t, "provider-a", "ext-refund-1")
	if status != wagering.StatusProcessed || failure != "" {
		t.Fatalf("refund after tick = %s / %q, want PROCESSED / empty", status, failure)
	}

	// Balance: 100 - 25 (BET) + 25 (REFUND) = 100.
	if got := walletBalance(t, walletID); got != 10000 {
		t.Fatalf("balance = %d, want 10000", got)
	}

	// One WagerTransactionProcessed event for the refund.
	refundID := refund.TransactionID
	if got := outboxEventCount(t, refundID, EventProcessed); got != 1 {
		t.Fatalf("outbox processed events for refund = %d, want 1", got)
	}
}

// TestWorker_ExpiresOldReference proves that a REFUND whose reference never
// arrives within the TTL is rejected with REFERENCE_NOT_FOUND.
func TestWorker_ExpiresOldReference(t *testing.T) {
	walletID := openWallet(t, 10000)

	// Simulate an old PENDING_REFERENCE by updating created_at directly.
	refund := newREFUND(t, walletID, "ext-refund-2", "ext-bet-never", 2500)
	if _, err := testPool.Exec(context.Background(),
		`UPDATE wager_transactions SET created_at = $1 WHERE id = $2`,
		time.Now().UTC().Add(-25*time.Hour), refund.TransactionID); err != nil {
		t.Fatalf("update created_at: %v", err)
	}

	worker := newIntegrationWorker(t, 24*time.Hour, time.Now().UTC())
	n, err := worker.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 1 {
		t.Fatalf("resolved = %d, want 1", n)
	}

	status, failure := txStatus(t, "provider-a", "ext-refund-2")
	if status != wagering.StatusRejected || failure != FailureReferenceNotFound {
		t.Fatalf("refund after tick = %s / %q, want REJECTED / %q", status, failure, FailureReferenceNotFound)
	}

	// Balance is unchanged (refund was never applied).
	if got := walletBalance(t, walletID); got != 10000 {
		t.Fatalf("balance = %d, want 10000", got)
	}

	// A WagerTransactionRejected event must be recorded.
	if got := outboxEventCount(t, refund.TransactionID, EventRejected); got != 1 {
		t.Fatalf("outbox rejected events = %d, want 1", got)
	}
}

// TestWorker_YoungPendingStaysPending proves that a fresh PENDING_REFERENCE is
// not rejected before its TTL expires.
func TestWorker_YoungPendingStaysPending(t *testing.T) {
	walletID := openWallet(t, 10000)

	refund := newREFUND(t, walletID, "ext-refund-3", "ext-bet-future", 2500)
	if refund.Status != wagering.StatusPendingReference {
		t.Fatalf("refund status = %s, want PENDING_REFERENCE", refund.Status)
	}

	worker := newIntegrationWorker(t, 24*time.Hour, time.Now().UTC())
	n, err := worker.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 0 {
		t.Fatalf("resolved = %d, want 0", n)
	}

	status, _ := txStatus(t, "provider-a", "ext-refund-3")
	if status != wagering.StatusPendingReference {
		t.Fatalf("refund after tick = %s, want PENDING_REFERENCE", status)
	}
}
