//go:build integration

package sqs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/app/usecase"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
)

var (
	testPool   *pgxpool.Pool
	testClient *awssqs.Client
	queueURL   string

	wallets    *postgres.WalletRepo
	wagers     *postgres.WageringRepo
	ledgers    *postgres.LedgerRepo
	outboxRepo *postgres.OutboxRepo
	inboxRepo  *postgres.InboxRepo
	txRunner   postgres.TxRunner
	processor  *usecase.WagerProcessor
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

	ls, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "localstack/localstack:4.14.0",
			ExposedPorts: []string{"4566/tcp"},
			Env:          map[string]string{"SERVICES": "sqs"},
			WaitingFor:   wait.ForHTTP("/_localstack/health").WithPort("4566/tcp").WithStartupTimeout(120 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "start localstack: %v\n", err)
		return 1
	}
	defer func() { _ = ls.Terminate(context.Background()) }()

	lsHost, _ := ls.Host(ctx)
	lsPort, _ := ls.MappedPort(ctx, "4566/tcp")
	endpoint := fmt.Sprintf("http://%s:%s", lsHost, lsPort.Port())

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aws config: %v\n", err)
		return 1
	}
	testClient = awssqs.NewFromConfig(awsCfg, func(o *awssqs.Options) {
		o.BaseEndpoint = aws.String(endpoint)
	})

	out, err := testClient.CreateQueue(ctx, &awssqs.CreateQueueInput{
		QueueName: aws.String("wager-transactions.fifo"),
		Attributes: map[string]string{
			"FifoQueue":                 "true",
			"ContentBasedDeduplication": "false",
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create queue: %v\n", err)
		return 1
	}
	queueURL = aws.ToString(out.QueueUrl)

	wallets = postgres.NewWalletRepo()
	wagers = postgres.NewWageringRepo()
	ledgers = postgres.NewLedgerRepo()
	outboxRepo = postgres.NewOutboxRepo()
	inboxRepo = postgres.NewInboxRepo()
	txRunner = postgres.NewTxRunner(testPool)
	processor = usecase.NewWagerProcessor(testPool, wallets, wagers, ledgers, outboxRepo)

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
	w, err := usecase.NewWalletOpener(testPool, wallets, wagers, ledgers, outboxRepo).
		OpenWallet(context.Background(), "player-"+newUUID(t), m)
	if err != nil {
		t.Fatalf("OpenWallet: %v", err)
	}
	return w.ID()
}

func walletBalance(t *testing.T, id string) int64 {
	t.Helper()
	w, err := wallets.GetByID(context.Background(), testPool, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	return w.Balance().Cents()
}

func sendMessage(t *testing.T, msgID, groupID, body string) {
	t.Helper()
	_, err := testClient.SendMessage(context.Background(), &awssqs.SendMessageInput{
		QueueUrl:               aws.String(queueURL),
		MessageBody:            aws.String(body),
		MessageGroupId:         aws.String(groupID),
		MessageDeduplicationId: aws.String(msgID),
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
}

// receiveAndProcess fetches messages from SQS and passes them to the consumer's
// handleBatch method synchronously. This avoids the infinite polling loop of Run()
// and makes the test deterministic.
func receiveAndProcess(t *testing.T, consumer *Consumer) {
	t.Helper()
	out, err := testClient.ReceiveMessage(context.Background(), &awssqs.ReceiveMessageInput{
		QueueUrl:            aws.String(queueURL),
		MaxNumberOfMessages: 10,
		WaitTimeSeconds:     2,
		MessageSystemAttributeNames: []types.MessageSystemAttributeName{
			types.MessageSystemAttributeNameMessageGroupId,
		},
	})
	if err != nil {
		t.Fatalf("ReceiveMessage: %v", err)
	}
	consumer.handleBatch(context.Background(), out.Messages)
}

func queueMessageCount(t *testing.T) int {
	t.Helper()
	out, err := testClient.GetQueueAttributes(context.Background(), &awssqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(queueURL),
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages},
	})
	if err != nil {
		t.Fatalf("GetQueueAttributes: %v", err)
	}
	var count int
	fmt.Sscanf(out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)], "%d", &count)
	return count
}

// newIntegrationConsumer builds a real Consumer against LocalStack and Postgres.
// The name is intentionally different from newTestConsumer in consumer_test.go
// (which uses mocks) to avoid redeclaration in the same package.
func newIntegrationConsumer() *Consumer {
	return NewConsumer(Config{
		QueueURL:       queueURL,
		ConsumerName:   "integration-test-consumer",
		MaxMessages:    1,
		WaitSeconds:    1,
		ProcessTimeout: 5 * time.Second,
	}, testClient, txRunner, inboxRepo, processor, nil)
}

func envelope(t *testing.T, msgID, walletID, idemKey, extID, kind string, cents int64) string {
	t.Helper()
	payload := map[string]any{
		"messageId":  msgID,
		"type":       "WagerTransactionRequested",
		"occurredAt": time.Now().UTC().Format(time.RFC3339Nano),
		"data": map[string]any{
			"providerId":            "provider-a",
			"walletId":              walletID,
			"idempotencyKey":        idemKey,
			"externalTransactionId": extID,
			"kind":                  kind,
			"amount":                map[string]string{"amount": fmt.Sprintf("%d.%02d", cents/100, cents%100), "currency": "BRL"},
			"roundId":               "r1",
			"gameId":                "g1",
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// ---------------------------------------------------------------- tests

// TestConsumer_HappyPath validates the full SQS -> Consumer -> Inbox -> UseCase -> Postgres flow.
func TestConsumer_HappyPath(t *testing.T) {
	walletID := openWallet(t, 10000)
	msgID := newUUID(t)
	body := envelope(t, msgID, walletID, "idem-"+msgID, "ext-"+msgID, "BET", 2500)

	sendMessage(t, msgID, walletID, body)

	consumer := newIntegrationConsumer()
	receiveAndProcess(t, consumer)

	if got := walletBalance(t, walletID); got != 7500 {
		t.Fatalf("balance = %d, want 7500", got)
	}

	var inboxCount int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM inbox WHERE consumer_name = $1 AND message_id = $2`,
		"integration-test-consumer", msgID).Scan(&inboxCount); err != nil {
		t.Fatalf("query inbox: %v", err)
	}
	if inboxCount != 1 {
		t.Fatalf("inbox rows = %d, want 1", inboxCount)
	}

	if count := queueMessageCount(t); count != 0 {
		t.Fatalf("queue still has %d messages, want 0", count)
	}
}

// TestConsumer_DuplicateMessageIgnored proves that receiving the same messageId
// twice is harmless: the inbox prevents the use case from running again.
func TestConsumer_DuplicateMessageIgnored(t *testing.T) {
	walletID := openWallet(t, 10000)
	msgID := newUUID(t)
	body := envelope(t, msgID, walletID, "idem-"+msgID, "ext-"+msgID, "BET", 2500)

	consumer := newIntegrationConsumer()

	sendMessage(t, msgID, walletID, body)
	receiveAndProcess(t, consumer)
	if got := walletBalance(t, walletID); got != 7500 {
		t.Fatalf("balance after first = %d, want 7500", got)
	}

	_, err := testClient.SendMessage(context.Background(), &awssqs.SendMessageInput{
		QueueUrl:               aws.String(queueURL),
		MessageBody:            aws.String(body),
		MessageGroupId:         aws.String(walletID),
		MessageDeduplicationId: aws.String(newUUID(t)),
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	receiveAndProcess(t, consumer)

	if got := walletBalance(t, walletID); got != 7500 {
		t.Fatalf("balance after duplicate = %d, want 7500 (no double debit)", got)
	}

	var debitCount int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'DEBIT'`,
		walletID).Scan(&debitCount); err != nil {
		t.Fatalf("query ledger: %v", err)
	}
	if debitCount != 1 {
		t.Fatalf("DEBIT entries = %d, want 1", debitCount)
	}
}

// TestConsumer_TransientErrorKeepsMessage proves that when the use case fails
// with an infrastructure error (e.g., wallet does not exist), the inbox insert
// is rolled back with the business transaction, so a retry can still happen.
func TestConsumer_TransientErrorKeepsMessage(t *testing.T) {
	unknownWallet := newUUID(t)
	msgID := newUUID(t)
	body := envelope(t, msgID, unknownWallet, "idem-"+msgID, "ext-"+msgID, "BET", 2500)

	sendMessage(t, msgID, unknownWallet, body)

	consumer := newIntegrationConsumer()
	receiveAndProcess(t, consumer)

	var inboxCount int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM inbox WHERE consumer_name = $1 AND message_id = $2`,
		"integration-test-consumer", msgID).Scan(&inboxCount); err != nil {
		t.Fatalf("query inbox: %v", err)
	}
	if inboxCount != 0 {
		t.Fatalf("inbox rows = %d, want 0 (transaction should have rolled back)", inboxCount)
	}
}

// TestConsumer_PoisonMessageDeleted proves that a malformed JSON message is
// deleted from the queue instead of blocking the consumer forever.
func TestConsumer_PoisonMessageDeleted(t *testing.T) {
	msgID := newUUID(t)
	body := `{"this is not valid json`

	sendMessage(t, msgID, "some-wallet", body)

	consumer := newIntegrationConsumer()
	receiveAndProcess(t, consumer)

	if count := queueMessageCount(t); count != 0 {
		t.Fatalf("queue has %d messages, want 0 (poison must be deleted)", count)
	}
}

// TestConsumer_GroupMismatchDeleted proves that a message whose MessageGroupId
// does not match the walletId is deleted (a security/integrity measure to avoid
// messages for the same wallet being processed out of order by mistake).
func TestConsumer_GroupMismatchDeleted(t *testing.T) {
	walletID := openWallet(t, 10000)
	msgID := newUUID(t)
	body := envelope(t, msgID, walletID, "idem-"+msgID, "ext-"+msgID, "BET", 2500)

	sendMessage(t, msgID, "attacker-wallet", body)

	consumer := newIntegrationConsumer()
	receiveAndProcess(t, consumer)

	if count := queueMessageCount(t); count != 0 {
		t.Fatalf("queue has %d messages, want 0 (mismatched group must be deleted)", count)
	}
	if got := walletBalance(t, walletID); got != 10000 {
		t.Fatalf("balance = %d, want 10000 (no debit should happen)", got)
	}
}
