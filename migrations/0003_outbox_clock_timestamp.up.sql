-- now() é o início da transação; clock_timestamp() é o instante real do INSERT.
-- Os eventos são inseridos sob o lock da carteira, então a ordem de created_at
-- passa a refletir a ordem em que as transações da carteira foram aplicadas.
ALTER TABLE outbox ALTER COLUMN created_at SET DEFAULT clock_timestamp();