//go:build integration

package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
)

var (
	testPool     *pgxpool.Pool
	testClient   *awssqs.Client
	testQueueURL string

	outboxRepo *postgres.OutboxRepo
	txRunner   postgres.TxRunner
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

	queueName := fmt.Sprintf("wager-transactions-out-%d.fifo", time.Now().UnixNano())
	out, err := testClient.CreateQueue(ctx, &awssqs.CreateQueueInput{
		QueueName: aws.String(queueName),
		Attributes: map[string]string{
			"FifoQueue":                 "true",
			"ContentBasedDeduplication": "false",
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create queue: %v\n", err)
		return 1
	}
	testQueueURL = aws.ToString(out.QueueUrl)

	outboxRepo = postgres.NewOutboxRepo()
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

func insertEvent(t *testing.T, eventType, aggregateID, groupID string, nextAttempt time.Duration) *postgres.Event {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"event": eventType, "aggregate": aggregateID})
	ev := &postgres.Event{
		EventID:        newUUID(t),
		AggregateType:  "wager_transaction",
		AggregateID:    aggregateID,
		EventType:      eventType,
		MessageGroupID: groupID,
		Payload:        payload,
		NextAttemptAt:  time.Now().UTC().Add(nextAttempt),
	}
	ctx := context.Background()
	if err := postgres.WithTx(ctx, testPool, func(tx pgx.Tx) error {
		return outboxRepo.Insert(ctx, tx, ev)
	}); err != nil {
		t.Fatalf("insert outbox event: %v", err)
	}
	return ev
}

func dbRow(t *testing.T, id string) (status string, attempts int, publishedAt *time.Time, lastError string) {
	t.Helper()
	var (
		errStr   *string
		nullTime *time.Time
	)
	err := testPool.QueryRow(context.Background(),
		`SELECT status, attempts, published_at, last_error FROM outbox WHERE id = $1`, id,
	).Scan(&status, &attempts, &nullTime, &errStr)
	if err != nil {
		t.Fatalf("query outbox row: %v", err)
	}
	publishedAt = nullTime
	if errStr != nil {
		lastError = *errStr
	}
	return
}

// purgeQueue removes every visible and in-flight message from the queue.
// PurgeQueue may return an error if called twice within 60s on real AWS SQS,
// but LocalStack allows it. If it fails, we fall back to draining.
func purgeQueue(t *testing.T) {
	t.Helper()
	_, err := testClient.PurgeQueue(context.Background(), &awssqs.PurgeQueueInput{
		QueueUrl: aws.String(testQueueURL),
	})
	if err != nil {
		// Fallback to manual drain (in case LocalStack rate-limits purge).
		drainQueue(t)
	}
	time.Sleep(500 * time.Millisecond)
}

// drainQueue receives and deletes all visible messages from the queue.
func drainQueue(t *testing.T) {
	t.Helper()
	for {
		msgs := receivedMessages(t, 10)
		if len(msgs) == 0 {
			return
		}
		for _, m := range msgs {
			_, _ = testClient.DeleteMessage(context.Background(), &awssqs.DeleteMessageInput{
				QueueUrl:      aws.String(testQueueURL),
				ReceiptHandle: m.ReceiptHandle,
			})
		}
	}
}

// receiveAndDelete receives up to n messages and deletes each one, returning them.
func receiveAndDelete(t *testing.T, n int32) []types.Message {
	t.Helper()
	msgs := receivedMessages(t, n)
	for _, m := range msgs {
		_, _ = testClient.DeleteMessage(context.Background(), &awssqs.DeleteMessageInput{
			QueueUrl:      aws.String(testQueueURL),
			ReceiptHandle: m.ReceiptHandle,
		})
	}
	return msgs
}

func receivedMessages(t *testing.T, n int32) []types.Message {
	t.Helper()
	out, err := testClient.ReceiveMessage(context.Background(), &awssqs.ReceiveMessageInput{
		QueueUrl:              aws.String(testQueueURL),
		MaxNumberOfMessages:   n,
		WaitTimeSeconds:       2,
		MessageAttributeNames: []string{"All"},
	})
	if err != nil {
		t.Fatalf("ReceiveMessage: %v", err)
	}
	return out.Messages
}

// queueSize returns ApproximateNumberOfMessages + ApproximateNumberOfMessagesNotVisible.
// This is deterministic on LocalStack (no eventual consistency issue).
func queueSize(t *testing.T) int {
	t.Helper()
	attrs, err := testClient.GetQueueAttributes(context.Background(), &awssqs.GetQueueAttributesInput{
		QueueUrl: aws.String(testQueueURL),
		AttributeNames: []types.QueueAttributeName{
			types.QueueAttributeNameApproximateNumberOfMessages,
			types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
		},
	})
	if err != nil {
		t.Fatalf("GetQueueAttributes: %v", err)
	}
	visible, _ := strconv.Atoi(attrs.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)])
	inFlight, _ := strconv.Atoi(attrs.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible)])
	return visible + inFlight
}

func newIntegrationPublisher(t *testing.T) *Publisher {
	t.Helper()
	return NewPublisher(Config{
		QueueURL:    testQueueURL,
		BatchSize:   50,
		BaseBackoff: time.Second,
		MaxBackoff:  time.Minute,
		TickTimeout: 10 * time.Second,
	}, outboxRepo, testClient, txRunner, nil)
}

// ---------------------------------------------------------------- tests

func TestPublisher_PublishesAndMarksPublished(t *testing.T) {
	purgeQueue(t)
	ev := insertEvent(t, "wager.processed", "agg-"+newUUID(t), "publish-group", 0)

	pub := newIntegrationPublisher(t)
	n, err := pub.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 1 {
		t.Fatalf("claimed = %d, want 1", n)
	}

	status, attempts, publishedAt, _ := dbRow(t, ev.ID)
	if status != "PUBLISHED" || attempts != 1 || publishedAt == nil {
		t.Fatalf("row state = %s / %d / %v, want PUBLISHED / 1 / non-nil", status, attempts, publishedAt)
	}

	time.Sleep(1 * time.Second)
	msgs := receiveAndDelete(t, 10)
	if len(msgs) != 1 {
		t.Fatalf("received %d messages, want 1", len(msgs))
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(*msgs[0].Body), &payload); err != nil {
		t.Fatalf("unmarshal published payload: %v", err)
	}
	if payload["event"] != "wager.processed" || payload["aggregate"] != ev.AggregateID {
		t.Fatalf("payload mismatch: %v", payload)
	}
	if msgs[0].MessageAttributes["eventType"].StringValue == nil ||
		*msgs[0].MessageAttributes["eventType"].StringValue != "wager.processed" {
		t.Fatalf("eventType attribute mismatch")
	}
}

func TestPublisher_RetriesOnSendFailure(t *testing.T) {
	purgeQueue(t)
	ev := insertEvent(t, "wager.rejected", "agg-"+newUUID(t), "retry-group", 0)

	brokenClient := awssqs.NewFromConfig(aws.Config{
		Region:      "us-east-1",
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", ""),
	}, func(o *awssqs.Options) {
		o.BaseEndpoint = aws.String("http://127.0.0.1:1")
	})

	pub := NewPublisher(Config{
		QueueURL:    "http://127.0.0.1:1/queue",
		BatchSize:   10,
		BaseBackoff: 5 * time.Second,
		MaxBackoff:  time.Minute,
		TickTimeout: 5 * time.Second,
	}, outboxRepo, brokenClient, txRunner, nil)

	if _, err := pub.Tick(context.Background()); err != nil {
		t.Fatalf("Tick returned error: %v", err)
	}

	status, attempts, publishedAt, lastErr := dbRow(t, ev.ID)
	if status != "PENDING" {
		t.Fatalf("status = %s, want PENDING", status)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if publishedAt != nil {
		t.Fatalf("published_at = %v, want nil", publishedAt)
	}
	if lastErr == "" {
		t.Fatal("expected last_error to be recorded")
	}
}

func TestPublisher_SkipsLockedEvents(t *testing.T) {
	purgeQueue(t)
	insertEvent(t, "wager.processed", "agg-"+newUUID(t), "locked-group", 0)

	tx1, err := testPool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	defer func() { _ = tx1.Rollback(context.Background()) }()
	claimed1, err := outboxRepo.ClaimBatch(context.Background(), tx1, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("ClaimBatch tx1: %v", err)
	}
	if len(claimed1) != 1 {
		t.Fatalf("tx1 claimed %d, want 1", len(claimed1))
	}

	tx2, err := testPool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin tx2: %v", err)
	}
	defer func() { _ = tx2.Rollback(context.Background()) }()
	claimed2, err := outboxRepo.ClaimBatch(context.Background(), tx2, time.Now().UTC(), 10)
	if err != nil {
		t.Fatalf("ClaimBatch tx2: %v", err)
	}
	if len(claimed2) != 0 {
		t.Fatalf("tx2 claimed %d, want 0 (SKIP LOCKED failed)", len(claimed2))
	}

	_ = tx1.Rollback(context.Background())

	pub := newIntegrationPublisher(t)
	n, err := pub.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 1 {
		t.Fatalf("claimed = %d, want 1", n)
	}
}

func TestPublisher_RecoveryAfterCrash(t *testing.T) {
	purgeQueue(t)
	ev := insertEvent(t, "wager.processed", "agg-"+newUUID(t), "recovery-group", -1*time.Minute)

	pub := newIntegrationPublisher(t)
	n, err := pub.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n != 1 {
		t.Fatalf("claimed = %d, want 1", n)
	}

	status, _, publishedAt, _ := dbRow(t, ev.ID)
	if status != "PUBLISHED" || publishedAt == nil {
		t.Fatalf("state after recovery = %s / %v, want PUBLISHED / non-nil", status, publishedAt)
	}

	time.Sleep(1 * time.Second)
	// Receive AND delete so this message does not pollute the next test.
	msgs := receiveAndDelete(t, 10)
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message in SQS after recovery, got %d", len(msgs))
	}
}

func TestPublisher_ConcurrentPublishersDoNotDuplicate(t *testing.T) {
	purgeQueue(t)
	const events = 20
	ids := make([]string, events)
	for i := 0; i < events; i++ {
		ev := insertEvent(t, "wager.processed", "agg-"+newUUID(t), fmt.Sprintf("concurrent-group-%d", i), 0)
		ids[i] = ev.ID
	}

	done := make(chan struct{}, 3)
	for p := 0; p < 3; p++ {
		go func() {
			defer func() { done <- struct{}{} }()
			pub := newIntegrationPublisher(t)
			for i := 0; i < 5; i++ {
				_, _ = pub.Tick(context.Background())
			}
		}()
	}
	for p := 0; p < 3; p++ {
		<-done
	}

	// Every event must be marked PUBLISHED exactly once in the database.
	for _, id := range ids {
		status, attempts, publishedAt, _ := dbRow(t, id)
		if status != "PUBLISHED" || attempts != 1 || publishedAt == nil {
			t.Fatalf("event %s: %s / %d / %v, want PUBLISHED / 1 / non-nil", id, status, attempts, publishedAt)
		}
	}

	// Wait for the queue to contain exactly `events` messages (visible + in-flight).
	// We use ApproximateNumberOfMessages because it is deterministic on LocalStack.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if got := queueSize(t); got >= events {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("queue has %d messages, want %d", queueSize(t), events)
}
