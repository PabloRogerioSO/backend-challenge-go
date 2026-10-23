-- =====================================================================
-- wallets
-- =====================================================================
CREATE TABLE wallets (
    id            UUID        NOT NULL DEFAULT gen_random_uuid(),
    player_id     VARCHAR(64) NOT NULL,
    currency      VARCHAR(3)  NOT NULL,
    balance_cents BIGINT      NOT NULL DEFAULT 0,
    version       BIGINT      NOT NULL DEFAULT 1,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pk_wallets PRIMARY KEY (id),
    CONSTRAINT uq_wallets_player_currency UNIQUE (player_id, currency),
    CONSTRAINT ck_wallets_balance_non_negative CHECK (balance_cents >= 0),
    CONSTRAINT ck_wallets_currency_iso4217 CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT ck_wallets_player_id_not_blank CHECK (length(btrim(player_id)) > 0),
    CONSTRAINT ck_wallets_version_positive CHECK (version >= 1)
);

-- =====================================================================
-- wager_transactions
-- =====================================================================
CREATE TABLE wager_transactions (
    id                       UUID         NOT NULL DEFAULT gen_random_uuid(),
    wallet_id                UUID         NOT NULL,
    provider_id              VARCHAR(64),
    external_transaction_id  VARCHAR(128),
    idempotency_key          VARCHAR(128),
    request_hash             VARCHAR(64),
    kind                     VARCHAR(16)  NOT NULL,
    status                   VARCHAR(16)  NOT NULL,
    amount_cents             BIGINT       NOT NULL,
    currency                 VARCHAR(3)   NOT NULL,
    reference_transaction_id UUID,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at               TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CONSTRAINT pk_wager_transactions PRIMARY KEY (id),
    CONSTRAINT fk_wager_transactions_wallet
        FOREIGN KEY (wallet_id) REFERENCES wallets (id),
    CONSTRAINT fk_wager_transactions_reference
        FOREIGN KEY (reference_transaction_id) REFERENCES wager_transactions (id),
    CONSTRAINT ck_wager_transactions_kind
        CHECK (kind IN ('BET', 'WIN', 'LOSS', 'REVERSAL')),
    CONSTRAINT ck_wager_transactions_status
        CHECK (status IN ('PENDING', 'COMPLETED', 'FAILED', 'REVERSED')),
    CONSTRAINT ck_wager_transactions_amount_non_negative
        CHECK (amount_cents >= 0),
    CONSTRAINT ck_wager_transactions_loss_zero
        CHECK (kind <> 'LOSS' OR amount_cents = 0),
    CONSTRAINT ck_wager_transactions_currency_iso4217
        CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT ck_wager_transactions_provider_external_pair
        CHECK ((provider_id IS NULL) = (external_transaction_id IS NULL)),
    CONSTRAINT ck_wager_transactions_reversal_reference
        CHECK (kind <> 'REVERSAL' OR reference_transaction_id IS NOT NULL)
);

CREATE UNIQUE INDEX uq_wager_transactions_provider_external
    ON wager_transactions (provider_id, external_transaction_id)
    WHERE provider_id IS NOT NULL;

CREATE UNIQUE INDEX uq_wager_transactions_idempotency_key
    ON wager_transactions (idempotency_key)
    WHERE idempotency_key IS NOT NULL;

CREATE UNIQUE INDEX uq_wager_transactions_single_reversal
    ON wager_transactions (reference_transaction_id)
    WHERE kind = 'REVERSAL';

CREATE INDEX idx_wager_transactions_wallet_created
    ON wager_transactions (wallet_id, created_at DESC);

CREATE INDEX idx_wager_transactions_reference
    ON wager_transactions (reference_transaction_id)
    WHERE reference_transaction_id IS NOT NULL;

-- =====================================================================
-- wallet_ledger_entries (append-only)
-- =====================================================================
CREATE TABLE wallet_ledger_entries (
    id                   UUID        NOT NULL DEFAULT gen_random_uuid(),
    wallet_id            UUID        NOT NULL,
    transaction_id       UUID        NOT NULL,
    direction            VARCHAR(6)  NOT NULL,
    amount_cents         BIGINT      NOT NULL,
    currency             VARCHAR(3)  NOT NULL,
    balance_before_cents BIGINT      NOT NULL,
    balance_after_cents  BIGINT      NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT pk_wallet_ledger_entries PRIMARY KEY (id),
    CONSTRAINT fk_wallet_ledger_entries_wallet
        FOREIGN KEY (wallet_id) REFERENCES wallets (id),
    CONSTRAINT fk_wallet_ledger_entries_transaction
        FOREIGN KEY (transaction_id) REFERENCES wager_transactions (id),
    CONSTRAINT uq_wallet_ledger_entries_wallet_transaction
        UNIQUE (wallet_id, transaction_id),
    CONSTRAINT ck_wallet_ledger_entries_direction
        CHECK (direction IN ('DEBIT', 'CREDIT')),
    CONSTRAINT ck_wallet_ledger_entries_amount_positive
        CHECK (amount_cents > 0),
    CONSTRAINT ck_wallet_ledger_entries_balances_non_negative
        CHECK (balance_before_cents >= 0 AND balance_after_cents >= 0),
    CONSTRAINT ck_wallet_ledger_entries_currency_iso4217
        CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT ck_wallet_ledger_entries_direction_math
        CHECK (
            (direction = 'DEBIT'  AND balance_after_cents = balance_before_cents - amount_cents)
            OR
            (direction = 'CREDIT' AND balance_after_cents = balance_before_cents + amount_cents)
        )
);

CREATE INDEX idx_wallet_ledger_entries_wallet_created
    ON wallet_ledger_entries (wallet_id, created_at DESC);

CREATE INDEX idx_wallet_ledger_entries_transaction
    ON wallet_ledger_entries (transaction_id);

CREATE FUNCTION fn_ledger_immutable() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'wallet_ledger_entries is append-only: % is not allowed', TG_OP
        USING HINT = 'Corrija lançamentos inserindo uma nova entrada compensatória.';
    RETURN NULL;
END;
$$;

-- Nível de statement: dispara mesmo quando nenhuma linha seria afetada.
CREATE TRIGGER trg_wallet_ledger_entries_immutable_stmt
    BEFORE UPDATE OR DELETE OR TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT
    EXECUTE FUNCTION fn_ledger_immutable();

-- Nível de linha: cobre qualquer caminho que escape do statement-level.
CREATE TRIGGER trg_wallet_ledger_entries_immutable_row
    BEFORE UPDATE OR DELETE ON wallet_ledger_entries
    FOR EACH ROW
    EXECUTE FUNCTION fn_ledger_immutable();

-- =====================================================================
-- inbox
-- =====================================================================
CREATE TABLE inbox (
    id            UUID         NOT NULL DEFAULT gen_random_uuid(),
    consumer_name VARCHAR(128) NOT NULL,
    message_id    VARCHAR(256) NOT NULL,
    payload       JSONB,
    received_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    processed_at  TIMESTAMPTZ,
    CONSTRAINT pk_inbox PRIMARY KEY (id),
    CONSTRAINT uq_inbox_consumer_message UNIQUE (consumer_name, message_id),
    CONSTRAINT ck_inbox_consumer_name_not_blank CHECK (length(btrim(consumer_name)) > 0),
    CONSTRAINT ck_inbox_message_id_not_blank CHECK (length(btrim(message_id)) > 0)
);

-- =====================================================================
-- outbox
-- =====================================================================
CREATE TABLE outbox (
    id                UUID         NOT NULL DEFAULT gen_random_uuid(),
    event_id          UUID         NOT NULL,
    aggregate_type    VARCHAR(64)  NOT NULL,
    aggregate_id      VARCHAR(64)  NOT NULL,
    event_type        VARCHAR(128) NOT NULL,
    message_group_id  VARCHAR(128) NOT NULL,
    payload           JSONB        NOT NULL,
    status            VARCHAR(16)  NOT NULL DEFAULT 'PENDING',
    attempts          INTEGER      NOT NULL DEFAULT 0,
    last_error        TEXT,
    created_at        TIMESTAMPTZ  NOT NULL DEFAULT now(),
    published_at      TIMESTAMPTZ,
    CONSTRAINT pk_outbox PRIMARY KEY (id),
    CONSTRAINT uq_outbox_event_id UNIQUE (event_id),
    CONSTRAINT ck_outbox_status CHECK (status IN ('PENDING', 'PUBLISHED', 'FAILED')),
    CONSTRAINT ck_outbox_attempts_non_negative CHECK (attempts >= 0),
    CONSTRAINT ck_outbox_published_at_consistent
        CHECK ((status = 'PUBLISHED') = (published_at IS NOT NULL))
);

CREATE INDEX idx_outbox_pending_created
    ON outbox (created_at)
    WHERE status = 'PENDING';