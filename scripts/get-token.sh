#!/usr/bin/env bash
# Obtém um access token do Keycloak local via client_credentials.
#
# Uso:
#   ./scripts/get-token.sh provider-a
#   ./scripts/get-token.sh provider-b
#
# Variáveis opcionais:
#   KEYCLOAK_URL     (padrão: http://localhost:8080)
#   KEYCLOAK_REALM   (padrão: backend-challenge)
#   CLIENT_SECRET    (padrão: <client_id>-secret)
#
# Imprime somente o token no stdout; erros vão para o stderr.

set -euo pipefail

CLIENT_ID="${1:-}"
if [[ "$CLIENT_ID" != "provider-a" && "$CLIENT_ID" != "provider-b" ]]; then
  echo "uso: $0 <provider-a|provider-b>" >&2
  exit 1
fi

KEYCLOAK_URL="${KEYCLOAK_URL:-http://localhost:8080}"
KEYCLOAK_REALM="${KEYCLOAK_REALM:-backend-challenge}"
CLIENT_SECRET="${CLIENT_SECRET:-${CLIENT_ID}-secret}"

TOKEN_URL="${KEYCLOAK_URL}/realms/${KEYCLOAK_REALM}/protocol/openid-connect/token"

RESPONSE="$(curl -sS -X POST "$TOKEN_URL" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  --data-urlencode "grant_type=client_credentials" \
  --data-urlencode "client_id=${CLIENT_ID}" \
  --data-urlencode "client_secret=${CLIENT_SECRET}")" || {
  echo "erro: não foi possível conectar ao Keycloak em ${TOKEN_URL}" >&2
  exit 1
}

TOKEN="$(printf '%s' "$RESPONSE" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')"

if [[ -z "$TOKEN" ]]; then
  echo "erro: resposta do Keycloak sem access_token:" >&2
  echo "$RESPONSE" >&2
  exit 1
fi

printf '%s\n' "$TOKEN"