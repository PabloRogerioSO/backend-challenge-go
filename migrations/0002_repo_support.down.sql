DROP INDEX IF EXISTS idx_wallet_ledger_entries_wallet_created_id;

CREATE INDEX idx_wallet_ledger_entries_wallet_created
    ON wallet_ledger_entries (wallet_id, created_at DESC);

ALTER TABLE inbox DROP COLUMN IF EXISTS payload_hash;

DROP INDEX IF EXISTS idx_outbox_pending_next_attempt;

CREATE INDEX idx_outbox_pending_created
    ON outbox (created_at)
    WHERE status = 'PENDING';

ALTER TABLE outbox DROP COLUMN IF EXISTS next_attempt_at;