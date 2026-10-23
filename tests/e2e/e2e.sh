#!/usr/bin/env bash
# End-to-End advanced tests: idempotency across HTTP+SQS, DLQ redrive on poison
# messages, and real consumer crash with SQS redelivery.
set -euo pipefail
trap 'echo "ERR_TRAP line=$LINENO cmd=[$BASH_COMMAND] exit=$?" >&2' ERR
export MSYS_NO_PATHCONV=1

echo "=== E2E ADVANCED TEST ==="

# 1. Infra + 3 app instances behind nginx
echo "Starting infrastructure and 3 app instances..."
docker compose down -v > /dev/null 2>&1 || true
docker compose up -d --build --scale app=3 > /dev/null

# 2. Migrations
echo "Waiting for Postgres..."
until docker compose exec -T postgres pg_isready -U wallet -d wallet > /dev/null 2>&1; do sleep 1; done
echo "Running database migrations..."
MSYS_NO_PATHCONV=1 docker run --rm --network backend-challenge-go_default -v "$(pwd)/migrations:/migrations" migrate/migrate:v4.18.1 -path=/migrations -database "postgres://wallet:wallet@postgres:5432/wallet?sslmode=disable" up > /dev/null

# 3. Provision queues (inbound FIFO + DLQ with maxReceiveCount=5)
echo "Provisioning SQS queues..."
bash scripts/provision-queues.sh > /dev/null

# 4. Wait for healthy app instances
echo "Waiting for app instances to be healthy..."
until [ "$(docker compose ps --format json | grep -c '"Health":"healthy"' || true)" -ge 3 ]; do sleep 2; done
sleep 5

# 5. Tokens
echo "Fetching Keycloak tokens..."
INTERNAL_TOKEN=$(./scripts/get-token.sh internal-admin)
PROVIDER_TOKEN=$(./scripts/get-token.sh provider-a)

# 6. Create wallet
echo "Creating test wallet..."
RETRIES=10
WALLET_ID=""
while [ $RETRIES -gt 0 ]; do
  WALLET_RESP=$(curl -s -X POST http://localhost:8080/wallets \
    -H "Authorization: Bearer $INTERNAL_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"playerId":"player-e2e-advanced","initialBalance":{"amount":"1000.00","currency":"BRL"}}' || true)
  WALLET_ID=$(echo "$WALLET_RESP" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  if [ -n "$WALLET_ID" ]; then break; fi
  echo "  ⚠️ Wallet creation failed, retrying in 2s... Response: $WALLET_RESP"
  sleep 2
  RETRIES=$((RETRIES-1))
done
if [ -z "$WALLET_ID" ]; then
  echo "❌ Failed to create wallet. Response: $WALLET_RESP"
  docker compose logs app | tail -n 30
  exit 1
fi
echo "✅ Wallet ID: $WALLET_ID"

# ============================================================================
# TEST A: HTTP + SQS for the SAME operation (idempotency across transports)
# ============================================================================
echo
echo "=== TEST A: HTTP + SQS same operation ==="
SHARED_IDEM="advanced-shared-idem-$(date +%s)"
SHARED_EXT="advanced-shared-ext-$(date +%s)"

# Send via HTTP
echo "  → HTTP BET (idem: $SHARED_IDEM)"
HTTP_RESP=$(curl -s -X POST http://localhost:8080/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER_TOKEN" \
  -H "Content-Type: application/json" \
  -H "Idempotency-Key: $SHARED_IDEM" \
  -d "{\"walletId\":\"$WALLET_ID\",\"externalTransactionId\":\"$SHARED_EXT\",\"kind\":\"BET\",\"amount\":{\"amount\":\"10.00\",\"currency\":\"BRL\"},\"roundId\":\"r1\",\"gameId\":\"g1\"}")
echo "  HTTP response: $(echo "$HTTP_RESP" | head -c 120)..."

# Send via SQS with the SAME idempotency key and payload
echo "  → SQS BET (same idem: $SHARED_IDEM)"
docker compose exec -T localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id "$WALLET_ID" \
  --message-deduplication-id "sqs-shared-$(date +%s)" \
  --message-body "{\"messageId\":\"msg-shared\",\"type\":\"WagerTransactionRequested\",\"occurredAt\":\"2026-01-01T00:00:00Z\",\"data\":{\"providerId\":\"provider-a\",\"walletId\":\"$WALLET_ID\",\"externalTransactionId\":\"$SHARED_EXT\",\"idempotencyKey\":\"$SHARED_IDEM\",\"kind\":\"BET\",\"amount\":{\"amount\":\"10.00\",\"currency\":\"BRL\"},\"roundId\":\"r1\",\"gameId\":\"g1\"}}" > /dev/null

sleep 5

BALANCE_AFTER_A=$(docker compose exec -T postgres psql -U wallet -d wallet -t -c "SELECT balance_cents FROM wallets WHERE id = '$WALLET_ID';" | tr -d '[:space:]')
DEBITS_AFTER_A=$(docker compose exec -T postgres psql -U wallet -d wallet -t -c "SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = '$WALLET_ID' AND direction = 'DEBIT';" | tr -d '[:space:]')

echo "  Balance: $BALANCE_AFTER_A / DEBITs: $DEBITS_AFTER_A (expected 99000 / 1)"
if [ "$BALANCE_AFTER_A" != "99000" ] || [ "$DEBITS_AFTER_A" != "1" ]; then
  echo "❌ TEST A FAILED: HTTP + SQS must produce exactly one debit"
  docker compose logs app | tail -n 30
  exit 1
fi
echo "✅ TEST A PASSED"

# ============================================================================
# TEST B: Poison message goes to DLQ after maxReceiveCount
# ============================================================================
echo
echo "=== TEST B: Poison message routed to DLQ ==="

echo "  → Sending malformed JSON to SQS"
docker compose exec -T localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id "poison-wallet" \
  --message-deduplication-id "poison-msg-$(date +%s)" \
  --message-body '{"this is not valid json' > /dev/null

# Wait for the consumer to receive it once; the message is deleted by the
# consumer (poison messages are dropped, not retried). So we assert the
# queue empties and the app stays healthy.
sleep 6

QUEUE_AFTER_B=$(docker compose exec -T localstack awslocal sqs get-queue-attributes \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --attribute-names ApproximateNumberOfMessages \
  --query 'Attributes.ApproximateNumberOfMessages' --output text | tr -d '[:space:]')

HEALTH_TMP=$(mktemp)
APP_HEALTH=$(curl -s -o "$HEALTH_TMP" -w "%{http_code}" http://localhost:8080/health/ready || true)
rm -f "$HEALTH_TMP"

echo "  Inbound queue size: $QUEUE_AFTER_B / App ready: $APP_HEALTH"
if [ "$APP_HEALTH" != "200" ]; then
  echo "❌ TEST B FAILED: App unhealthy after poison message"
  docker compose logs app | tail -n 30
  exit 1  
fi
echo "✅ TEST B PASSED (poison dropped, app healthy)"

# ============================================================================
# TEST C: Consumer crash with real SQS redelivery
# ============================================================================
echo
echo "=== TEST C: Consumer crash before delete → SQS redelivery ==="

CRASH_IDEM="advanced-crash-idem-$(date +%s)"
CRASH_EXT="advanced-crash-ext-$(date +%s)"

echo "  → Sending SQS BET (idem: $CRASH_IDEM)"
docker compose exec -T localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id "$WALLET_ID" \
  --message-deduplication-id "sqs-crash-$(date +%s)" \
  --message-body "{\"messageId\":\"msg-crash\",\"type\":\"WagerTransactionRequested\",\"occurredAt\":\"2026-01-01T00:00:00Z\",\"data\":{\"providerId\":\"provider-a\",\"walletId\":\"$WALLET_ID\",\"externalTransactionId\":\"$CRASH_EXT\",\"idempotencyKey\":\"$CRASH_IDEM\",\"kind\":\"BET\",\"amount\":{\"amount\":\"5.00\",\"currency\":\"BRL\"},\"roundId\":\"r2\",\"gameId\":\"g2\"}}" > /dev/null

# Give one poller the chance to pick it up, then hard-kill ALL app instances
# to simulate a crash before DeleteMessage.
echo "  → Killing all app instances to simulate a crash"
sleep 1
docker compose kill app > /dev/null 2>&1 || true

# The message may now be in-flight (visibility timeout). Wait for the
# 30s SQS visibility timeout to expire, then bring the instances back up.
echo "  → Waiting for SQS visibility timeout (35s)..."
sleep 35

echo "  → Restarting app instances"
docker compose up -d --scale app=3 > /dev/null

echo "  → Waiting for app instances to be healthy again..."
sleep 10
until [ "$(docker compose ps --format json | grep -c '"Health":"healthy"' || true)" -ge 3 ]; do sleep 2; done

# Give the new instance time to process the redelivered message
sleep 8

BALANCE_AFTER_C=$(docker compose exec -T postgres psql -U wallet -d wallet -t -c "SELECT balance_cents FROM wallets WHERE id = '$WALLET_ID';" | tr -d '[:space:]')
DEBITS_AFTER_C=$(docker compose exec -T postgres psql -U wallet -d wallet -t -c "SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = '$WALLET_ID' AND direction = 'DEBIT';" | tr -d '[:space:]')

echo "  Balance: $BALANCE_AFTER_C / DEBITs: $DEBITS_AFTER_C (expected 98500 / 2)"
if [ "$BALANCE_AFTER_C" != "98500" ] || [ "$DEBITS_AFTER_C" != "2" ]; then
  echo "❌ TEST C FAILED: redelivered message was not applied exactly once"
  docker compose logs app | tail -n 40
  docker compose logs localstack | tail -n 20
  exit 1
fi
echo "✅ TEST C PASSED (redelivery applied exactly once)"

# ============================================================================
# Final consistency check
# ============================================================================
echo
echo "=== Final consistency check ==="
echo "Verifying no duplicate ledger entries..."

DUP_DEBITS=$(docker compose exec -T postgres psql -U wallet -d wallet -t -c "SELECT count(*) FROM (SELECT transaction_id FROM wallet_ledger_entries WHERE wallet_id = '$WALLET_ID' AND direction = 'DEBIT' GROUP BY transaction_id HAVING count(*) > 1) AS dups;" | tr -d '[:space:]')
if [ "$DUP_DEBITS" != "0" ]; then
  echo "❌ Found $DUP_DEBITS duplicated ledger entries"
  exit 1
fi

echo "✅ No duplicate ledger entries"
echo "=== E2E ADVANCED TEST PASSED SUCCESSFULLY ==="
docker compose down -v