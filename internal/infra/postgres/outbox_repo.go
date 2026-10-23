package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const outboxColumns = `id::text, event_id::text, aggregate_type, aggregate_id, event_type, message_group_id,
	payload, status, attempts, last_error, next_attempt_at, created_at, published_at`

// Event is a row of the transactional outbox.
type Event struct {
	ID             string
	EventID        string
	AggregateType  string
	AggregateID    string
	EventType      string
	MessageGroupID string
	Payload        []byte
	Status         string
	Attempts       int
	LastError      string
	NextAttemptAt  time.Time
	CreatedAt      time.Time
	PublishedAt    *time.Time
}

// OutboxRepo persists outbox events.
type OutboxRepo struct{}

// NewOutboxRepo returns a stateless outbox repository.
func NewOutboxRepo() *OutboxRepo { return &OutboxRepo{} }

// Insert stores a new PENDING event inside the caller's transaction and fills
// ID, Status, Attempts, NextAttemptAt and CreatedAt. A zero NextAttemptAt means "now".
func (r *OutboxRepo) Insert(ctx context.Context, tx pgx.Tx, e *Event) error {
	if e == nil {
		return fmt.Errorf("%w: nil outbox event", ErrInvalidArgument)
	}
	for name, v := range map[string]string{
		"event id":         e.EventID,
		"aggregate type":   e.AggregateType,
		"aggregate id":     e.AggregateID,
		"event type":       e.EventType,
		"message group id": e.MessageGroupID,
	} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("%w: outbox %s is required", ErrInvalidArgument, name)
		}
	}
	if !json.Valid(e.Payload) {
		return fmt.Errorf("%w: outbox payload must be valid JSON", ErrInvalidArgument)
	}

	var nextAttempt any
	if !e.NextAttemptAt.IsZero() {
		nextAttempt = e.NextAttemptAt
	}
	var nextAttemptAt, createdAt time.Time
	err := tx.QueryRow(ctx,
		`INSERT INTO outbox (event_id, aggregate_type, aggregate_id, event_type, message_group_id, payload, next_attempt_at)
		 VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, now()))
		 RETURNING id::text, status, attempts, next_attempt_at, created_at`,
		e.EventID, e.AggregateType, e.AggregateID, e.EventType, e.MessageGroupID, e.Payload, nextAttempt,
	).Scan(&e.ID, &e.Status, &e.Attempts, &nextAttemptAt, &createdAt)
	if err != nil {
		return fmt.Errorf("postgres: insert outbox event %s: %w", e.EventID, err)
	}
	e.NextAttemptAt = nextAttemptAt.UTC()
	e.CreatedAt = createdAt.UTC()
	return nil
}

// ClaimBatch locks up to limit PENDING events that are due at now, skipping rows
// locked by other relays. The locks last until the transaction ends.
func (r *OutboxRepo) ClaimBatch(ctx context.Context, tx pgx.Tx, now time.Time, limit int) ([]Event, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("%w: limit must be positive", ErrInvalidArgument)
	}
	rows, err := tx.Query(ctx,
		`SELECT `+outboxColumns+`
		   FROM outbox
		  WHERE status = 'PENDING' AND next_attempt_at <= $1
		  ORDER BY created_at, id
		  LIMIT $2
		    FOR UPDATE SKIP LOCKED`,
		now, limit)
	if err != nil {
		return nil, fmt.Errorf("postgres: claim outbox batch: %w", err)
	}
	defer rows.Close()

	events := make([]Event, 0, limit)
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: claim outbox batch: %w", err)
	}
	return events, nil
}

// MarkPublished flags a PENDING event as PUBLISHED.
func (r *OutboxRepo) MarkPublished(ctx context.Context, tx pgx.Tx, id string) error {
	tag, err := tx.Exec(ctx,
		`UPDATE outbox
		    SET status = 'PUBLISHED', published_at = now(), attempts = attempts + 1, last_error = NULL
		  WHERE id = $1 AND status = 'PENDING'`,
		id)
	if err != nil {
		return fmt.Errorf("postgres: mark outbox event %s published: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: pending outbox event %s", ErrNotFound, id)
	}
	return nil
}

// MarkRetry records a failed attempt and schedules the next one.
func (r *OutboxRepo) MarkRetry(ctx context.Context, tx pgx.Tx, id string, nextAttempt time.Time, lastError string) error {
	tag, err := tx.Exec(ctx,
		`UPDATE outbox
		    SET attempts = attempts + 1, next_attempt_at = $2, last_error = $3
		  WHERE id = $1 AND status = 'PENDING'`,
		id, nextAttempt, nullIfEmpty(lastError))
	if err != nil {
		return fmt.Errorf("postgres: mark outbox event %s for retry: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: pending outbox event %s", ErrNotFound, id)
	}
	return nil
}

func scanEvent(row pgx.Row) (Event, error) {
	var (
		e         Event
		lastError *string
		published *time.Time
	)
	if err := row.Scan(&e.ID, &e.EventID, &e.AggregateType, &e.AggregateID, &e.EventType, &e.MessageGroupID,
		&e.Payload, &e.Status, &e.Attempts, &lastError, &e.NextAttemptAt, &e.CreatedAt, &published); err != nil {
		return Event{}, fmt.Errorf("postgres: scan outbox event: %w", err)
	}
	e.LastError = derefString(lastError)
	e.NextAttemptAt = e.NextAttemptAt.UTC()
	e.CreatedAt = e.CreatedAt.UTC()
	if published != nil {
		utc := published.UTC()
		e.PublishedAt = &utc
	}
	return e, nil
}
