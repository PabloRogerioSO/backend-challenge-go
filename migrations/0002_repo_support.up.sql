-- outbox: agendamento de tentativas (backoff do relay)
ALTER TABLE outbox
    ADD COLUMN next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now();

DROP INDEX idx_outbox_pending_created;

CREATE INDEX idx_outbox_pending_next_attempt
    ON outbox (next_attempt_at, created_at)
    WHERE status = 'PENDING';

-- inbox: hash do payload recebido
ALTER TABLE inbox
    ADD COLUMN payload_hash VARCHAR(64) NOT NULL DEFAULT '';

ALTER TABLE inbox
    ALTER COLUMN payload_hash DROP DEFAULT;

-- ledger: índice para paginação keyset (created_at DESC, id DESC)
DROP INDEX idx_wallet_ledger_entries_wallet_created;

CREATE INDEX idx_wallet_ledger_entries_wallet_created_id
    ON wallet_ledger_entries (wallet_id, created_at DESC, id DESC);