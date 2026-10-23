package outbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/infra/postgres"
)

type fakeStore struct {
	events    []postgres.Event
	published []string
	retried   map[string]time.Time
	lastErr   map[string]string
}

func newFakeStore(evs ...postgres.Event) *fakeStore {
	return &fakeStore{events: evs, retried: map[string]time.Time{}, lastErr: map[string]string{}}
}

func (f *fakeStore) ClaimBatch(context.Context, pgx.Tx, time.Time, int) ([]postgres.Event, error) {
	return f.events, nil
}
func (f *fakeStore) MarkPublished(_ context.Context, _ pgx.Tx, id string) error {
	f.published = append(f.published, id)
	return nil
}
func (f *fakeStore) MarkRetry(_ context.Context, _ pgx.Tx, id string, next time.Time, lastErr string) error {
	f.retried[id] = next
	f.lastErr[id] = lastErr
	return nil
}

type fakeSender struct {
	failFor map[string]bool // keyed by dedup id
	sent    []*awssqs.SendMessageInput
}

func (f *fakeSender) SendMessage(_ context.Context, in *awssqs.SendMessageInput, _ ...func(*awssqs.Options)) (*awssqs.SendMessageOutput, error) {
	if f.failFor[*in.MessageDeduplicationId] {
		return nil, errors.New("sqs unavailable")
	}
	f.sent = append(f.sent, in)
	return &awssqs.SendMessageOutput{}, nil
}

func runTx(_ context.Context, fn func(pgx.Tx) error) error { return fn(nil) }

func newPub(store *fakeStore, sender *fakeSender, now time.Time) *Publisher {
	p := NewPublisher(Config{QueueURL: "q", BaseBackoff: time.Second, MaxBackoff: time.Minute},
		store, sender, runTx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.now = func() time.Time { return now }
	return p
}

func TestTick_PublishesAndMarksPublished(t *testing.T) {
	store := newFakeStore(postgres.Event{ID: "1", EventID: "e1", MessageGroupID: "w1", EventType: "wager.settled", Payload: []byte(`{}`)})
	sender := &fakeSender{}
	n, err := newPub(store, sender, time.Now()).Tick(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if len(store.published) != 1 || store.published[0] != "1" {
		t.Fatalf("published=%v", store.published)
	}
	in := sender.sent[0]
	if *in.MessageGroupId != "w1" || *in.MessageDeduplicationId != "e1" {
		t.Fatalf("group/dedup wrong: %s %s", *in.MessageGroupId, *in.MessageDeduplicationId)
	}
}

func TestTick_FailureSchedulesBackoffRetry(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newFakeStore(
		postgres.Event{ID: "1", EventID: "e1", MessageGroupID: "w1", Attempts: 3},
		postgres.Event{ID: "2", EventID: "e2", MessageGroupID: "w2"},
	)
	sender := &fakeSender{failFor: map[string]bool{"e1": true}}
	if _, err := newPub(store, sender, now).Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := store.retried["1"], now.Add(4*time.Second); !got.Equal(want) {
		t.Fatalf("retry at %v, want %v", got, want)
	}
	if store.lastErr["1"] == "" {
		t.Fatal("expected last error recorded")
	}
	if len(store.published) != 1 || store.published[0] != "2" {
		t.Fatalf("published=%v", store.published)
	}
}

func TestBackoff(t *testing.T) {
	base, max := time.Second, 10*time.Second
	cases := map[int]time.Duration{0: time.Second, 1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 5: max, 100: max}
	for attempts, want := range cases {
		if got := Backoff(attempts, base, max); got != want {
			t.Errorf("Backoff(%d)=%v want %v", attempts, got, want)
		}
	}
}
