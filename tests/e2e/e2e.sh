#!/usr/bin/env bash
# End-to-End test script for validating multi-instance concurrency, idempotency, and database consistency.

set -euo pipefail
export MSYS_NO_PATHCONV=1

echo "=== E2E MULTI-INSTANCE TEST ==="

# 1. Spin up infrastructure and 3 application instances
echo "Starting infrastructure and 3 app instances..."
docker compose down -v > /dev/null 2>&1 || true
docker compose up -d --build --scale app=3 > /dev/null

# 2. Run database migrations
echo "Waiting for Postgres..."
until docker compose exec -T postgres pg_isready -U wallet -d wallet > /dev/null 2>&1; do sleep 1; done
echo "Running database migrations..."
MSYS_NO_PATHCONV=1 docker run --rm --network backend-challenge-go_default -v "$(pwd)/migrations:/migrations" migrate/migrate:v4.18.1 -path=/migrations -database "postgres://wallet:wallet@postgres:5432/wallet?sslmode=disable" up > /dev/null

# 3. Provision SQS queues
echo "Provisioning SQS queues..."
bash scripts/provision-queues.sh > /dev/null

# 4. Wait for Keycloak and the app to be fully ready
echo "Waiting for app instances to be healthy..."
until [ "$(docker compose ps --format json | grep -c '"Health":"healthy"' || true)" -ge 3 ]; do sleep 2; done
sleep 5 # Extra time for Keycloak to finish warming up

# 5. Fetch Keycloak access token
echo "Fetching Keycloak token..."
TOKEN=$(./scripts/get-token.sh provider-a)
if [ -z "$TOKEN" ]; then
  echo "❌ Failed to fetch token from Keycloak."
  exit 1
fi

# 6. Create a test wallet with retry logic (in case app JWKS isn't ready)
echo "Creating test wallet..."
RETRIES=10
WALLET_ID=""
while [ $RETRIES -gt 0 ]; do
  WALLET_RESP=$(curl -s -X POST http://localhost:8080/wallets \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"playerId":"player-e2e","initialBalance":{"amount":"1000.00","currency":"BRL"}}' || true)
  
  WALLET_ID=$(echo "$WALLET_RESP" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  
  if [ -n "$WALLET_ID" ]; then
    break
  fi
  
  echo "  ⚠️ Wallet creation failed, retrying in 2s... Response: $WALLET_RESP"
  sleep 2
  RETRIES=$((RETRIES-1))
done

if [ -z "$WALLET_ID" ]; then
  echo "❌ Error: Failed to create wallet after 10 attempts."
  docker compose logs app | tail -n 20
  exit 1
fi
echo "✅ Wallet ID: $WALLET_ID"

# 7. Send 10 parallel HTTP requests for the exact same bet to test HTTP idempotency
echo "Sending 10 parallel HTTP requests..."
for i in {1..10}; do
  curl -s -X POST http://localhost:8080/wagering/transactions \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -H "Idempotency-Key: e2e-bet-1" \
    -d "{\"walletId\":\"$WALLET_ID\",\"externalTransactionId\":\"ext-e2e-1\",\"kind\":\"BET\",\"amount\":{\"amount\":\"10.00\",\"currency\":\"BRL\"},\"roundId\":\"r1\",\"gameId\":\"g1\"}" > /dev/null &
done
wait

# 8. Send 5 distinct messages to the SQS FIFO queue
echo "Sending 5 messages to SQS..."
for i in {1..5}; do
  docker compose exec -T localstack awslocal sqs send-message \
    --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
    --message-group-id "$WALLET_ID" \
    --message-deduplication-id "sqs-msg-$i" \
    --message-body "{\"messageId\":\"msg-$i\",\"type\":\"WagerTransactionRequested\",\"occurredAt\":\"2026-01-01T00:00:00Z\",\"data\":{\"providerId\":\"provider-a\",\"walletId\":\"$WALLET_ID\",\"externalTransactionId\":\"ext-sqs-$i\",\"idempotencyKey\":\"key-sqs-$i\",\"kind\":\"BET\",\"amount\":{\"amount\":\"5.00\",\"currency\":\"BRL\"}}}" > /dev/null
done

# 9. Wait for asynchronous processing to complete
echo "Waiting for asynchronous worker processing..."
sleep 5

# 10. Validate final database state
echo "Validating database state..."

# Assert final balance: 100000 (initial) - 1000 (HTTP bet) - 2500 (5 SQS bets) = 96500
DB_BALANCE=$(docker compose exec -T postgres psql -U wallet -d wallet -t -c "SELECT balance_cents FROM wallets WHERE id = '$WALLET_ID';" | tr -d '[:space:]')
if [ "$DB_BALANCE" == "96500" ]; then
  echo "✅ Balance validation passed: $DB_BALANCE"
else
  echo "❌ Balance validation failed. Expected 96500, got $DB_BALANCE"
  exit 1
fi

# Assert ledger entries: 1 debit for HTTP + 5 debits for SQS = 6 debits
LEDGER_COUNT=$(docker compose exec -T postgres psql -U wallet -d wallet -t -c "SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = '$WALLET_ID' AND direction = 'DEBIT';" | tr -d '[:space:]')
if [ "$LEDGER_COUNT" == "6" ]; then
  echo "✅ Ledger validation passed: 6 debits found"
else
  echo "❌ Ledger validation failed. Expected 6 debits, got $LEDGER_COUNT"
  exit 1
fi

# Assert outbox has been drained by the background worker
OUTBOX_PENDING=$(docker compose exec -T postgres psql -U wallet -d wallet -t -c "SELECT count(*) FROM outbox WHERE status = 'PENDING';" | tr -d '[:space:]')
if [ "$OUTBOX_PENDING" == "0" ]; then
  echo "✅ Outbox validation passed: All events published"
else
  echo "⚠️ Outbox validation warning: $OUTBOX_PENDING pending events remaining"
fi

echo "=== E2E TEST PASSED SUCCESSFULLY ==="
docker compose down -v