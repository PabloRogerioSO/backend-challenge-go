# Architecture and Technical Decisions

This document outlines the architectural decisions and trade-offs made while building this distributed wagering backend.

## Money Handling
- **Representation:** All monetary values are represented using `int64` (cents) and an ISO 4217 currency string. Floating-point types (`float32`, `float64`) are strictly forbidden to avoid precision loss.
- **Domain Rules:** The `Money` value object encapsulates strict parsing (rejecting scientific notation, scale > 2, etc.), arithmetic operations (with overflow protection), and currency compatibility checks.
- **Persistence:** Stored as `BIGINT` for the amount and `VARCHAR(3)` for the currency in PostgreSQL.

## Database Transactions and Concurrency
- **Isolation:** We use pessimistic locking (`SELECT ... FOR UPDATE`) on the `wallets` table. The transaction starts by locking the specific wallet row, ensuring that concurrent operations for the same wallet are serialized.
- **Parallelism:** Locks are strictly scoped to a single wallet ID. Concurrent requests for different wallets are processed completely in parallel.
- **Optimistic Lock:** The `wallets` table also contains a `version` column, used as an additional safety net during updates. If a version mismatch occurs, the transaction aborts.
- **Domain Updates:** To prevent lost updates and negative balances, the balance calculation happens in the domain entity before the SQL update, and the DB also enforces `CHECK (balance_cents >= 0)`.

## Idempotency
- **Persistent Idempotency:** Idempotency is enforced by the PostgreSQL database via unique constraints on `(provider_id, external_transaction_id)` and `idempotency_key`.
- **Payload Hash:** A canonical SHA-256 hash of the business payload is stored. If the same key arrives with the same hash, it's a replay (returns the original result). If the hash differs, it returns a `409 Conflict`.
- **Replay Result:** The original response balance is persisted in the `result_balance_cents` column and returned on replays, even if the wallet has since changed.
- **Inbox Pattern:** For SQS messages, the `inbox` table (`consumer_name`, `message_id`) ensures that duplicate deliveries are ignored. The `TryInsert` method is executed within the *same transaction* as the business logic (`ProcessWagerTx`).

## Ledger and Reversals
- **Append-Only:** `wallet_ledger_entries` is an append-only table enforced by a PostgreSQL trigger that blocks `UPDATE` and `DELETE` operations.
- **Immutability:** The trigger validates that `balance_after = balance_before ± amount`. 
- **Reversals (REFUND/ROLLBACK):** 
  - Both require a reference to a previous transaction. 
  - A `REFUND` credits the wallet back (reversing a `BET`). A `ROLLBACK` credits or debits the wallet (reversing a `BET` or `WIN`).
  - A transaction can only be reversed once (enforced by querying `FindProcessedReversal`).

## Pending References
- If a `REFUND` or `ROLLBACK` arrives before its referenced transaction, it is persisted with the status `PENDING_REFERENCE`.
- The `refworker` runs asynchronously, looking for `PENDING_REFERENCE` rows. It checks if the reference has been processed and, if so, executes the reversal.
- If a configurable TTL (e.g., 24 hours) expires, the transaction is marked as `REJECTED` with the code `REFERENCE_NOT_FOUND`.

## Inbox/Outbox Pattern
- **Transactional Outbox:** Domain events (e.g., `WagerTransactionProcessed`, `WalletBalanceChanged`) are inserted into the `outbox` table in the *same SQL transaction* as the domain changes.
- **Reliable Publishing:** A separate `outbox-publisher` worker polls the outbox table using `FOR UPDATE SKIP LOCKED`. It publishes events to SQS. If successful, it marks the event as published. If it fails, it applies exponential backoff.
- **Event ID Stability:** The `EventID` is generated at the time of the domain event, ensuring that retries do not create duplicate events in the message broker.

## Authentication and Authorization
- **OIDC/Keycloak:** The API validates JWTs using the JWKS endpoint from Keycloak.
- **Provider Isolation:** The `provider_id` claim is extracted from the token. The API strictly uses this value and ignores any `providerId` sent in the request body. 
- **Scopes:** External providers must have the `wager.write` scope. Internal routes (like creating wallets) require the `internal.wallet` scope.

## Observability
- **Structured Logging:** We use `log/slog` to emit JSON logs, capturing `correlationId`, `messageId`, `transactionId`, `walletId`, and `providerId` for distributed tracing.
- **Metrics:** Prometheus metrics are exposed at `/metrics` to track HTTP latencies, SQS duplicates (Inbox), outbox publish errors, DLQ arrivals, concurrency conflicts, and reconciliation divergences.

## Fx and Lifecycle
- **Composition:** We use Uber Fx (`go.uber.org/fx`) to wire dependencies, group them into modules, and manage the application lifecycle.
- **Graceful Shutdown:** The HTTP server, SQS consumer, outbox publisher, and reference worker are all started via `fx.Lifecycle`. On `SIGTERM`, the application stops accepting new HTTP requests and stops polling for new messages. In-flight work is allowed to finish before the database pool is closed.

## Multi-Instance Testing
- A dedicated E2E shell script (`tests/e2e/e2e.sh`) spins up 3 independent application instances, sends concurrent HTTP and SQS requests, and verifies that the database state remains strictly consistent without duplicate ledger entries.