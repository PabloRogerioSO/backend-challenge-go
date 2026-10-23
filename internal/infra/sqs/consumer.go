package sqs

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/app/usecase"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/metrics"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
)

// API is the subset of *sqs.Client used by the consumer.
type API interface {
	ReceiveMessage(ctx context.Context, in *awssqs.ReceiveMessageInput, opts ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, in *awssqs.DeleteMessageInput, opts ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error)
}

// Inbox is implemented by *postgres.InboxRepo.
type Inbox interface {
	TryInsert(ctx context.Context, tx pgx.Tx, consumerName, messageID, payloadHash string) (bool, error)
}

// Processor is implemented by *usecase.WagerProcessor.
type Processor interface {
	ProcessWagerTx(ctx context.Context, tx pgx.Tx, in usecase.ProcessWagerInput) (usecase.ProcessWagerOutput, error)
}

type Config struct {
	QueueURL       string
	ConsumerName   string
	Pollers        int
	MaxMessages    int32
	WaitSeconds    int32
	ProcessTimeout time.Duration
	ErrorBackoff   time.Duration
}

func (c Config) withDefaults() Config {
	if c.ConsumerName == "" {
		c.ConsumerName = "wager-consumer"
	}
	if c.Pollers <= 0 {
		c.Pollers = 2
	}
	if c.MaxMessages <= 0 || c.MaxMessages > 10 {
		c.MaxMessages = 10
	}
	if c.WaitSeconds < 0 || c.WaitSeconds > 20 {
		c.WaitSeconds = 20
	}
	if c.ProcessTimeout <= 0 {
		c.ProcessTimeout = 30 * time.Second
	}
	if c.ErrorBackoff <= 0 {
		c.ErrorBackoff = 2 * time.Second
	}
	return c
}

type Consumer struct {
	cfg    Config
	client API
	tx     postgres.TxRunner
	inbox  Inbox
	proc   Processor
	log    *slog.Logger
}

func NewConsumer(cfg Config, client API, tx postgres.TxRunner, inbox Inbox, proc Processor, log *slog.Logger) *Consumer {
	if log == nil {
		log = slog.Default()
	}
	return &Consumer{cfg: cfg.withDefaults(), client: client, tx: tx, inbox: inbox, proc: proc, log: log}
}

// Run blocks until ctx is cancelled and all in-flight messages are finished.
func (c *Consumer) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	for i := 0; i < c.cfg.Pollers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.poll(ctx)
		}()
	}
	wg.Wait()
	return nil
}

func (c *Consumer) poll(ctx context.Context) {
	for ctx.Err() == nil {
		out, err := c.client.ReceiveMessage(ctx, &awssqs.ReceiveMessageInput{
			QueueUrl:                    aws.String(c.cfg.QueueURL),
			MaxNumberOfMessages:         c.cfg.MaxMessages,
			WaitTimeSeconds:             c.cfg.WaitSeconds,
			MessageSystemAttributeNames: []types.MessageSystemAttributeName{types.MessageSystemAttributeNameMessageGroupId},
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.log.Error("sqs receive failed", "err", err)
			sleepCtx(ctx, c.cfg.ErrorBackoff)
			continue
		}
		c.handleBatch(ctx, out.Messages)
	}
}

func (c *Consumer) handleBatch(ctx context.Context, msgs []types.Message) {
	failedGroups := make(map[string]struct{})
	for _, msg := range msgs {
		if ctx.Err() != nil {
			return // Stop starting new work; unfinished messages return after visibility timeout.
		}
		group := msg.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]
		if _, failed := failedGroups[group]; failed {
			continue // Preserve FIFO order inside the group.
		}
		// In-flight work is detached from shutdown cancellation but bounded by a timeout.
		pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.ProcessTimeout)
		err := c.handle(pctx, msg)
		cancel()
		if err != nil {
			failedGroups[group] = struct{}{}
			c.log.Error("message not acknowledged; will be retried", "messageId", aws.ToString(msg.MessageId), "group", group, "err", err)
		}
	}
}

func (c *Consumer) handle(ctx context.Context, msg types.Message) error {
	body := []byte(aws.ToString(msg.Body))

	env, err := ParseEnvelope(body)
	if err != nil {
		c.log.Error("dropping poison message", "messageId", aws.ToString(msg.MessageId), "err", err)
		return c.delete(ctx, msg)
	}
	if g := msg.Attributes[string(types.MessageSystemAttributeNameMessageGroupId)]; g != "" && g != env.Data.WalletID {
		c.log.Error("dropping message: group id differs from walletId", "group", g, "walletId", env.Data.WalletID)
		return c.delete(ctx, msg)
	}

	hash := PayloadHash(body)
	err = c.tx(ctx, func(tx pgx.Tx) error {
		// Inbox insert and business processing share this transaction.
		inserted, err := c.inbox.TryInsert(ctx, tx, c.cfg.ConsumerName, env.MessageID, hash)
		if err != nil {
			return fmt.Errorf("inbox: %w", err)
		}
		if !inserted {
			c.log.Info("duplicate message ignored", "messageId", env.MessageID)
			metrics.SQSDuplicatesTotal.Inc()
			return nil
		}
		if _, err := c.proc.ProcessWagerTx(ctx, tx, env.Data); err != nil {
			return fmt.Errorf("process wager: %w", err)
		}
		return nil
	})
	if err != nil {
		return err // Rolled back, not deleted: SQS visibility timeout retries.
	}
	return c.delete(ctx, msg)
}

func (c *Consumer) delete(ctx context.Context, msg types.Message) error {
	_, err := c.client.DeleteMessage(ctx, &awssqs.DeleteMessageInput{
		QueueUrl:      aws.String(c.cfg.QueueURL),
		ReceiptHandle: msg.ReceiptHandle,
	})
	if err != nil {
		return fmt.Errorf("delete message: %w", err)
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
