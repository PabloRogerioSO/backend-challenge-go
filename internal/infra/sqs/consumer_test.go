package sqs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/app/usecase"
)

type fakeAPI struct{ deleted []string }

func (f *fakeAPI) ReceiveMessage(context.Context, *awssqs.ReceiveMessageInput, ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
	return &awssqs.ReceiveMessageOutput{}, nil
}

func (f *fakeAPI) DeleteMessage(_ context.Context, in *awssqs.DeleteMessageInput, _ ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
	f.deleted = append(f.deleted, aws.ToString(in.ReceiptHandle))
	return &awssqs.DeleteMessageOutput{}, nil
}

type crashingDeleteAPI struct {
	fakeAPI *fakeAPI
}

func (c *crashingDeleteAPI) ReceiveMessage(ctx context.Context, in *awssqs.ReceiveMessageInput, opts ...func(*awssqs.Options)) (*awssqs.ReceiveMessageOutput, error) {
	return c.fakeAPI.ReceiveMessage(ctx, in, opts...)
}

func (c *crashingDeleteAPI) DeleteMessage(ctx context.Context, in *awssqs.DeleteMessageInput, opts ...func(*awssqs.Options)) (*awssqs.DeleteMessageOutput, error) {
	return nil, errors.New("simulated crash before delete")
}

type fakeInbox struct {
	inserted bool
	err      error
}

func (f *fakeInbox) TryInsert(context.Context, pgx.Tx, string, string, string) (bool, error) {
	return f.inserted, f.err
}

type fakeProc struct {
	calls int
	err   error
	gotTx pgx.Tx
}

func (f *fakeProc) ProcessWagerTx(_ context.Context, tx pgx.Tx, _ usecase.ProcessWagerInput) (usecase.ProcessWagerOutput, error) {
	f.calls++
	f.gotTx = tx
	return usecase.ProcessWagerOutput{}, f.err
}

type fakeTxToken struct{ pgx.Tx }

func fakeTx(committed *int) func(context.Context, func(pgx.Tx) error) error {
	return func(_ context.Context, fn func(pgx.Tx) error) error {
		if err := fn(&fakeTxToken{}); err != nil {
			return err
		}
		*committed++
		return nil
	}
}

func msg(handle, group, id string) types.Message {
	body := `{"messageId":"` + id + `","type":"wager.requested","occurredAt":"2026-01-01T00:00:00Z",` +
		`"data":{"providerId":"p1","walletId":"` + group + `","idempotencyKey":"k-` + id + `"}}`
	return types.Message{
		Body:          aws.String(body),
		ReceiptHandle: aws.String(handle),
		Attributes:    map[string]string{"MessageGroupId": group},
	}
}

func newTestConsumer(api API, inbox Inbox, proc Processor, commits *int) *Consumer {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewConsumer(Config{QueueURL: "q"}, api, fakeTx(commits), inbox, proc, log)
}

func TestConsumer_SuccessCommitsAndDeletes(t *testing.T) {
	api, proc, commits := &fakeAPI{}, &fakeProc{}, 0
	c := newTestConsumer(api, &fakeInbox{inserted: true}, proc, &commits)

	c.handleBatch(context.Background(), []types.Message{msg("h1", "w1", "m1")})

	if proc.calls != 1 || commits != 1 || len(api.deleted) != 1 {
		t.Fatalf("calls=%d commits=%d deleted=%v", proc.calls, commits, api.deleted)
	}
	if proc.gotTx == nil {
		t.Fatal("use case did not receive the shared tx")
	}
}

func TestConsumer_DuplicateDeletesWithoutProcessing(t *testing.T) {
	api, proc, commits := &fakeAPI{}, &fakeProc{}, 0
	c := newTestConsumer(api, &fakeInbox{inserted: false}, proc, &commits)

	c.handleBatch(context.Background(), []types.Message{msg("h1", "w1", "m1")})

	if proc.calls != 0 || len(api.deleted) != 1 {
		t.Fatalf("calls=%d deleted=%v", proc.calls, api.deleted)
	}
}

func TestConsumer_TransientErrorDoesNotCommitNorDelete(t *testing.T) {
	api, proc, commits := &fakeAPI{}, &fakeProc{err: errors.New("db down")}, 0
	c := newTestConsumer(api, &fakeInbox{inserted: true}, proc, &commits)

	c.handleBatch(context.Background(), []types.Message{msg("h1", "w1", "m1")})

	if commits != 0 || len(api.deleted) != 0 {
		t.Fatalf("commits=%d deleted=%v, want 0 and []", commits, api.deleted)
	}
}

// Simulates the scenario where the process crashes after the database commit
// but before calling DeleteMessage on SQS. The message must not be deleted so SQS redelivers it.
func TestConsumer_CrashAfterCommitBeforeDelete(t *testing.T) {
	api, proc, commits := &fakeAPI{}, &fakeProc{}, 0
	crashAPI := &crashingDeleteAPI{fakeAPI: api}
	c := newTestConsumer(crashAPI, &fakeInbox{inserted: true}, proc, &commits)

	c.handleBatch(context.Background(), []types.Message{msg("h1", "w1", "m1")})

	if commits != 1 {
		t.Fatalf("expected 1 commit, got %d", commits)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("expected no deleted messages, got %v", api.deleted)
	}
}

func TestConsumer_FailedGroupSkipsRestOfGroupOnly(t *testing.T) {
	api, proc, commits := &fakeAPI{}, &fakeProc{err: errors.New("db down")}, 0
	c := newTestConsumer(api, &fakeInbox{inserted: true}, proc, &commits)

	c.handleBatch(context.Background(), []types.Message{
		msg("h1", "w1", "m1"), msg("h2", "w1", "m2"), msg("h3", "w2", "m3"),
	})

	if proc.calls != 2 {
		t.Fatalf("expected 2 attempts, got %d", proc.calls)
	}
}

func TestConsumer_PoisonMessageIsDeleted(t *testing.T) {
	api, proc, commits := &fakeAPI{}, &fakeProc{}, 0
	c := newTestConsumer(api, &fakeInbox{inserted: true}, proc, &commits)

	c.handleBatch(context.Background(), []types.Message{{
		Body: aws.String("not-json"), ReceiptHandle: aws.String("h1"),
		Attributes: map[string]string{"MessageGroupId": "w1"},
	}})

	if proc.calls != 0 || len(api.deleted) != 1 {
		t.Fatalf("calls=%d deleted=%v", proc.calls, api.deleted)
	}
}

func TestConsumer_GroupMismatchIsDeleted(t *testing.T) {
	api, proc, commits := &fakeAPI{}, &fakeProc{}, 0
	c := newTestConsumer(api, &fakeInbox{inserted: true}, proc, &commits)

	m := msg("h1", "w1", "m1")
	m.Attributes["MessageGroupId"] = "other-wallet"
	c.handleBatch(context.Background(), []types.Message{m})

	if proc.calls != 0 || len(api.deleted) != 1 {
		t.Fatalf("calls=%d deleted=%v", proc.calls, api.deleted)
	}
}

func TestConsumer_CancelledContextStartsNoNewMessages(t *testing.T) {
	api, proc, commits := &fakeAPI{}, &fakeProc{}, 0
	c := newTestConsumer(api, &fakeInbox{inserted: true}, proc, &commits)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.handleBatch(ctx, []types.Message{msg("h1", "w1", "m1")})

	if proc.calls != 0 {
		t.Fatalf("expected no processing after shutdown, got %d", proc.calls)
	}
}

func TestParseEnvelope_Validation(t *testing.T) {
	if _, err := ParseEnvelope([]byte(`{"messageId":"m","data":{}}`)); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("expected ErrInvalidMessage, got %v", err)
	}
	if PayloadHash([]byte("a")) == PayloadHash([]byte("b")) {
		t.Fatal("hash collision on different payloads")
	}
}
