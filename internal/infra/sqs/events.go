package sqs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/app/usecase"
)

// ErrInvalidMessage marks a message that can never be processed (poison).
var ErrInvalidMessage = errors.New("sqs: invalid message")

// Envelope is the SQS message body.
type Envelope struct {
	MessageID  string                    `json:"messageId"`
	Type       string                    `json:"type"`
	OccurredAt time.Time                 `json:"occurredAt"`
	Data       usecase.ProcessWagerInput `json:"data"`
}

// ParseEnvelope decodes and validates a message body.
func ParseEnvelope(body []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Envelope{}, fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	switch {
	case env.MessageID == "":
		return Envelope{}, fmt.Errorf("%w: missing messageId", ErrInvalidMessage)
	case env.Data.WalletID == "":
		return Envelope{}, fmt.Errorf("%w: missing data.walletId", ErrInvalidMessage)
	case env.Data.ProviderID == "":
		return Envelope{}, fmt.Errorf("%w: missing data.providerId", ErrInvalidMessage)
	case env.Data.IdempotencyKey == "":
		return Envelope{}, fmt.Errorf("%w: missing data.idempotencyKey", ErrInvalidMessage)
	}
	return env, nil
}

// PayloadHash returns the hex SHA-256 of the raw body, stored in the inbox.
func PayloadHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
