package postgres

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/ledger"
	"github.com/PabloRogerioSO/backend-challenge-go/internal/domain/money"
)

const (
	defaultLedgerPageSize = 50
	maxLedgerPageSize     = 200

	ledgerColumns = `id::text, wallet_id::text, transaction_id::text, direction, amount_cents, currency,
	balance_before_cents, balance_after_cents, created_at`
)

var cursorIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// LedgerRepo persists the append-only wallet ledger.
type LedgerRepo struct{}

// NewLedgerRepo returns a stateless ledger repository.
func NewLedgerRepo() *LedgerRepo { return &LedgerRepo{} }

// Create appends an entry. The database also enforces append-only semantics by trigger.
func (r *LedgerRepo) Create(ctx context.Context, tx pgx.Tx, e *ledger.Entry) error {
	if e == nil {
		return fmt.Errorf("%w: nil ledger entry", ErrInvalidArgument)
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO wallet_ledger_entries (
		     id, wallet_id, transaction_id, direction, amount_cents, currency,
		     balance_before_cents, balance_after_cents, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		e.ID(), e.WalletID(), e.TransactionID(), string(e.Direction()),
		e.Amount().Cents(), e.Amount().Currency(),
		e.BalanceBefore().Cents(), e.BalanceAfter().Cents(), e.CreatedAt())
	if err != nil {
		return fmt.Errorf("postgres: create ledger entry %s: %w", e.ID(), err)
	}
	return nil
}

// ListByWalletCursor returns up to limit entries ordered by created_at DESC, id DESC.
// The cursor is opaque: pass "" for the first page and the returned cursor for the
// next one. The returned cursor is empty when there are no more entries.
func (r *LedgerRepo) ListByWalletCursor(ctx context.Context, q Querier, walletID, cursor string, limit int) ([]ledger.Entry, string, error) {
	if strings.TrimSpace(walletID) == "" {
		return nil, "", fmt.Errorf("%w: wallet id is required", ErrInvalidArgument)
	}
	if limit <= 0 {
		limit = defaultLedgerPageSize
	}
	if limit > maxLedgerPageSize {
		limit = maxLedgerPageSize
	}

	var (
		rows pgx.Rows
		err  error
	)
	if cursor == "" {
		rows, err = q.Query(ctx,
			`SELECT `+ledgerColumns+`
			   FROM wallet_ledger_entries
			  WHERE wallet_id = $1
			  ORDER BY created_at DESC, id DESC
			  LIMIT $2`,
			walletID, limit+1)
	} else {
		createdAt, id, decodeErr := decodeCursor(cursor)
		if decodeErr != nil {
			return nil, "", decodeErr
		}
		rows, err = q.Query(ctx,
			`SELECT `+ledgerColumns+`
			   FROM wallet_ledger_entries
			  WHERE wallet_id = $1
			    AND (created_at, id) < ($2, $3::uuid)
			  ORDER BY created_at DESC, id DESC
			  LIMIT $4`,
			walletID, createdAt, id, limit+1)
	}
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list ledger entries of wallet %s: %w", walletID, err)
	}
	defer rows.Close()

	entries := make([]ledger.Entry, 0, limit+1)
	for rows.Next() {
		e, err := scanLedgerEntry(rows)
		if err != nil {
			return nil, "", err
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("postgres: list ledger entries of wallet %s: %w", walletID, err)
	}

	next := ""
	if len(entries) > limit {
		entries = entries[:limit]
		last := entries[limit-1]
		next = encodeCursor(last.CreatedAt(), last.ID())
	}
	return entries, next, nil
}

func scanLedgerEntry(row pgx.Row) (ledger.Entry, error) {
	var (
		id, walletID, transactionID, direction, currency string
		amountCents, beforeCents, afterCents             int64
		createdAt                                        time.Time
	)
	if err := row.Scan(&id, &walletID, &transactionID, &direction, &amountCents, &currency,
		&beforeCents, &afterCents, &createdAt); err != nil {
		return ledger.Entry{}, fmt.Errorf("postgres: scan ledger entry: %w", err)
	}
	amount, err := money.FromCents(amountCents, currency)
	if err != nil {
		return ledger.Entry{}, fmt.Errorf("postgres: ledger entry %s: %w", id, err)
	}
	before, err := money.FromCents(beforeCents, currency)
	if err != nil {
		return ledger.Entry{}, fmt.Errorf("postgres: ledger entry %s: %w", id, err)
	}
	after, err := money.FromCents(afterCents, currency)
	if err != nil {
		return ledger.Entry{}, fmt.Errorf("postgres: ledger entry %s: %w", id, err)
	}
	e, err := ledger.New(id, walletID, transactionID, ledger.Direction(direction), amount, before, after, createdAt.UTC())
	if err != nil {
		return ledger.Entry{}, fmt.Errorf("postgres: stored ledger entry %s violates domain invariants: %w", id, err)
	}
	return e, nil
}

func encodeCursor(createdAt time.Time, id string) string {
	raw := fmt.Sprintf("%d|%s", createdAt.UnixNano(), id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: not base64", ErrInvalidCursor)
	}
	nanoText, id, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, "", fmt.Errorf("%w: missing separator", ErrInvalidCursor)
	}
	nano, err := strconv.ParseInt(nanoText, 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: invalid timestamp", ErrInvalidCursor)
	}
	if !cursorIDPattern.MatchString(id) {
		return time.Time{}, "", fmt.Errorf("%w: invalid id", ErrInvalidCursor)
	}
	return time.Unix(0, nano).UTC(), strings.ToLower(id), nil
}
