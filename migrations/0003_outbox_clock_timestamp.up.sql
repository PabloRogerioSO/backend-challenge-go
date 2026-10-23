-- now() returns the transaction start time; clock_timestamp() returns the actual INSERT time.
-- Since events are inserted while holding the wallet lock, the created_at order
-- reflects the order in which wallet transactions were applied.
ALTER TABLE outbox ALTER COLUMN created_at SET DEFAULT clock_timestamp();