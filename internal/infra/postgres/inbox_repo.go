package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

const maxPayloadHashLength = 64

// InboxRepo records consumed messages for deduplication.
type InboxRepo struct{}

// NewInboxRepo returns a stateless inbox repository.
func NewInboxRepo() *InboxRepo { return &InboxRepo{} }

// TryInsert records (consumerName, messageID) and reports whether the row was
// inserted. It returns false, nil when the message was already recorded.
func (r *InboxRepo) TryInsert(ctx context.Context, tx pgx.Tx, consumerName, messageID, payloadHash string) (bool, error) {
	if strings.TrimSpace(consumerName) == "" || strings.TrimSpace(messageID) == "" {
		return false, fmt.Errorf("%w: consumer name and message id are required", ErrInvalidArgument)
	}
	if len(payloadHash) > maxPayloadHashLength {
		return false, fmt.Errorf("%w: payload hash exceeds %d characters", ErrInvalidArgument, maxPayloadHashLength)
	}
	tag, err := tx.Exec(ctx,
		`INSERT INTO inbox (consumer_name, message_id, payload_hash)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (consumer_name, message_id) DO NOTHING`,
		consumerName, messageID, payloadHash)
	if err != nil {
		return false, fmt.Errorf("postgres: insert inbox message %s/%s: %w", consumerName, messageID, err)
	}
	return tag.RowsAffected() == 1, nil
}
