//go:build integration

package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/app/usecase"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wagering"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/wallet"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
)

var (
	testPool   *pgxpool.Pool
	walletRepo = postgres.NewWalletRepo()
	wagerRepo  = postgres.NewWageringRepo()
	ledgerRepo = postgres.NewLedgerRepo()
	outboxRepo = postgres.NewOutboxRepo()

	opener     *usecase.WalletOpener
	processor  *usecase.WagerProcessor
	reconciler *usecase.Reconciler
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

	if err := os.Setenv(postgres.EnvMaxConns, "20"); err != nil {
		fmt.Fprintf(os.Stderr, "set env: %v\n", err)
		return 1
	}
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

	opener = usecase.NewWalletOpener(testPool, walletRepo, wagerRepo, ledgerRepo, outboxRepo)
	processor = usecase.NewWagerProcessor(testPool, walletRepo, wagerRepo, ledgerRepo, outboxRepo)
	reconciler = usecase.NewReconciler(testPool, ledgerRepo)
	return m.Run()
}

func connectWithRetry(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	var lastErr error
	for i := 0; i < 20; i++ {
		pool, err := postgres.NewPool(ctx, dsn)
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

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newUUID(t *testing.T) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(context.Background(), "SELECT gen_random_uuid()::text").Scan(&id); err != nil {
		t.Fatalf("gen_random_uuid: %v", err)
	}
	return id
}

func brl(t *testing.T, cents int64) money.Money {
	t.Helper()
	m, err := money.FromCents(cents, "BRL")
	if err != nil {
		t.Fatalf("FromCents: %v", err)
	}
	return m
}

func openWallet(t *testing.T, cents int64) *wallet.Wallet {
	t.Helper()
	w, err := opener.OpenWallet(testCtx(t), "player-"+newUUID(t), brl(t, cents))
	if err != nil {
		t.Fatalf("OpenWallet: %v", err)
	}
	return w
}

func newInput(t *testing.T, walletID string, kind wagering.Kind, cents int64) usecase.ProcessWagerInput {
	t.Helper()
	in := usecase.ProcessWagerInput{
		ProviderID:            "provider-a",
		WalletID:              walletID,
		IdempotencyKey:        "idem-" + newUUID(t),
		ExternalTransactionID: "ext-" + newUUID(t),
		Kind:                  kind,
		Amount:                brl(t, cents),
		RoundID:               "round-1",
		GameID:                "game-1",
	}
	if kind == wagering.KindRefund || kind == wagering.KindRollback {
		in.ReferenceExternalTransactionID = "ext-ref-" + newUUID(t)
	}
	return in
}

func mustProcess(t *testing.T, in usecase.ProcessWagerInput) usecase.ProcessWagerOutput {
	t.Helper()
	out, err := processor.ProcessWager(testCtx(t), in)
	if err != nil {
		t.Fatalf("ProcessWager(%s %s): %v", in.Kind, in.Amount, err)
	}
	return out
}

// runParallel starts n goroutines, releases them together and waits for all.
func runParallel(n int, fn func(i int)) {
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fn(i)
		}()
	}
	close(start)
	wg.Wait()
}

func scanInt(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

func walletBalance(t *testing.T, walletID string) int64 {
	t.Helper()
	w, err := walletRepo.GetByID(context.Background(), testPool, walletID)
	if err != nil {
		t.Fatalf("GetByID(%s): %v", walletID, err)
	}
	return w.Balance().Cents()
}

func ledgerCount(t *testing.T, walletID, direction string) int {
	t.Helper()
	return scanInt(t, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = $2`, walletID, direction)
}

func wagerCount(t *testing.T, walletID string, kind wagering.Kind) int {
	t.Helper()
	return scanInt(t, `SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND kind = $2`, walletID, string(kind))
}

func eventCounts(t *testing.T, walletID string) map[string]int {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT event_type, count(*) FROM outbox WHERE message_group_id = $1 GROUP BY event_type`, walletID)
	if err != nil {
		t.Fatalf("event counts: %v", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var eventType string
		var n int
		if err := rows.Scan(&eventType, &n); err != nil {
			t.Fatalf("scan event counts: %v", err)
		}
		counts[eventType] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("event counts rows: %v", err)
	}
	return counts
}
