package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/metrics"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
)

// Store is implemented by *postgres.OutboxRepo.
type Store interface {
	ClaimBatch(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]postgres.Event, error)
	MarkPublished(ctx context.Context, tx pgx.Tx, id string) error
	MarkRetry(ctx context.Context, tx pgx.Tx, id string, nextAttempt time.Time, lastError string) error
}

// Sender is the subset of *sqs.Client used by the publisher.
type Sender interface {
	SendMessage(ctx context.Context, in *awssqs.SendMessageInput, opts ...func(*awssqs.Options)) (*awssqs.SendMessageOutput, error)
}

type Config struct {
	QueueURL    string
	Interval    time.Duration
	BatchSize   int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
	TickTimeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = time.Second
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 50
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = 2 * time.Second
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = 5 * time.Minute
	}
	if c.TickTimeout <= 0 {
		c.TickTimeout = 30 * time.Second
	}
	return c
}

type Publisher struct {
	cfg    Config
	store  Store
	sender Sender
	tx     postgres.TxRunner
	log    *slog.Logger
	now    func() time.Time
}

func NewPublisher(cfg Config, store Store, sender Sender, tx postgres.TxRunner, log *slog.Logger) *Publisher {
	if log == nil {
		log = slog.Default()
	}
	return &Publisher{cfg: cfg.withDefaults(), store: store, sender: sender, tx: tx, log: log, now: time.Now}
}

// Run ticks until ctx is cancelled; a tick in progress is allowed to finish.
func (p *Publisher) Run(ctx context.Context) error {
	ticker := time.NewTicker(p.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			for ctx.Err() == nil {
				tctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.cfg.TickTimeout)
				n, err := p.Tick(tctx)
				cancel()
				if err != nil {
					p.log.Error("outbox tick failed", "err", err)
					break
				}
				if n < p.cfg.BatchSize {
					break // drained; wait for next tick
				}
			}
		}
	}
}

// Tick claims one batch and publishes it. Returns the number of claimed events.
func (p *Publisher) Tick(ctx context.Context) (int, error) {
	claimed := 0
	err := p.tx(ctx, func(tx pgx.Tx) error {
		events, err := p.store.ClaimBatch(ctx, tx, p.now(), p.cfg.BatchSize)
		if err != nil {
			return fmt.Errorf("claim batch: %w", err)
		}
		claimed = len(events)
		for _, ev := range events {
			if sendErr := p.send(ctx, ev); sendErr != nil {
				metrics.OutboxPublishErrorsTotal.Inc()
				next := p.now().Add(Backoff(ev.Attempts, p.cfg.BaseBackoff, p.cfg.MaxBackoff))
				p.log.Warn("outbox publish failed; scheduling retry", "eventId", ev.EventID, "next", next, "err", sendErr)
				if err := p.store.MarkRetry(ctx, tx, ev.ID, next, truncate(sendErr.Error(), 500)); err != nil {
					return fmt.Errorf("mark retry: %w", err)
				}
				continue
			}
			// Marked only after SQS confirmed the send.
			if err := p.store.MarkPublished(ctx, tx, ev.ID); err != nil {
				return fmt.Errorf("mark published: %w", err)
			}
		}
		return nil
	})
	return claimed, err
}

func (p *Publisher) send(ctx context.Context, ev postgres.Event) error {
	_, err := p.sender.SendMessage(ctx, &awssqs.SendMessageInput{
		QueueUrl:               aws.String(p.cfg.QueueURL),
		MessageBody:            aws.String(string(ev.Payload)),
		MessageGroupId:         aws.String(ev.MessageGroupID),
		MessageDeduplicationId: aws.String(ev.EventID),
		MessageAttributes: map[string]types.MessageAttributeValue{
			"eventType":     {DataType: aws.String("String"), StringValue: aws.String(ev.EventType)},
			"aggregateType": {DataType: aws.String("String"), StringValue: aws.String(ev.AggregateType)},
		},
	})
	return err
}

// Backoff returns base*2^(attempts-1) capped at max (attempts < 1 is treated as 1).
func Backoff(attempts int, base, max time.Duration) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts-1 >= 30 {
		return max
	}
	d := base << uint(attempts-1)
	if d <= 0 || d > max {
		return max
	}
	return d
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
