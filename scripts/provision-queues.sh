#!/usr/bin/env bash
# Cria as filas SQS FIFO (principal + DLQ) no LocalStack.
# Requer: docker compose com o serviço "localstack" saudável.
set -euo pipefail

# Git Bash (Windows): impede conversão automática de caminhos como /tmp.
export MSYS_NO_PATHCONV=1

if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi

QUEUE_NAME="${SQS_QUEUE_NAME:-wager-transactions.fifo}"
DLQ_NAME="${SQS_DLQ_NAME:-wager-transactions-dlq.fifo}"
MAX_RECEIVE_COUNT="${SQS_MAX_RECEIVE_COUNT:-5}"

awsl() {
  docker compose exec -T localstack awslocal "$@" | tr -d '\r'
}

echo "Aguardando LocalStack ficar pronto..."
for i in $(seq 1 60); do
  if docker compose exec -T localstack curl -sf http://localhost:4566/_localstack/health >/dev/null 2>&1; then
    break
  fi
  if [ "$i" -eq 60 ]; then
    echo "LocalStack não ficou pronto a tempo" >&2
    exit 1
  fi
  sleep 2
done

echo "Criando DLQ: ${DLQ_NAME}"
cat <<EOF | docker compose exec -T localstack sh -c 'cat > /tmp/dlq-attributes.json'
{
  "FifoQueue": "true",
  "ContentBasedDeduplication": "false"
}
EOF
DLQ_URL="$(awsl sqs create-queue \
  --queue-name "${DLQ_NAME}" \
  --attributes file:///tmp/dlq-attributes.json \
  --query 'QueueUrl' --output text)"

DLQ_ARN="$(awsl sqs get-queue-attributes \
  --queue-url "${DLQ_URL}" \
  --attribute-names QueueArn \
  --query 'Attributes.QueueArn' --output text)"

echo "Criando fila principal: ${QUEUE_NAME} (DLQ ${DLQ_ARN}, maxReceiveCount=${MAX_RECEIVE_COUNT})"
REDRIVE="{\\\"deadLetterTargetArn\\\":\\\"${DLQ_ARN}\\\",\\\"maxReceiveCount\\\":\\\"${MAX_RECEIVE_COUNT}\\\"}"
cat <<EOF | docker compose exec -T localstack sh -c 'cat > /tmp/queue-attributes.json'
{
  "FifoQueue": "true",
  "ContentBasedDeduplication": "false",
  "RedrivePolicy": "${REDRIVE}"
}
EOF
QUEUE_URL="$(awsl sqs create-queue \
  --queue-name "${QUEUE_NAME}" \
  --attributes file:///tmp/queue-attributes.json \
  --query 'QueueUrl' --output text)"

echo "Filas prontas:"
echo "  principal: ${QUEUE_URL}"
echo "  dlq:       ${DLQ_URL}"