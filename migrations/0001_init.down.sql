DROP TABLE IF EXISTS outbox;
DROP TABLE IF EXISTS inbox;

DROP TRIGGER IF EXISTS trg_wallet_ledger_entries_immutable_row ON wallet_ledger_entries;
DROP TRIGGER IF EXISTS trg_wallet_ledger_entries_immutable_stmt ON wallet_ledger_entries;
DROP TABLE IF EXISTS wallet_ledger_entries;
DROP FUNCTION IF EXISTS fn_ledger_immutable();

DROP TABLE IF EXISTS wager_transactions;
DROP TABLE IF EXISTS wallets;