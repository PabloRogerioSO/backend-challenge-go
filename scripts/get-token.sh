#!/usr/bin/env bash
# Script to obtain a JWT access token from Keycloak using the OAuth 2.0 client_credentials flow.
# Usage: ./scripts/get-token.sh <provider-a|provider-b>

set -euo pipefail

CLIENT_ID="${1:-}"
if [[ "$CLIENT_ID" != "provider-a" && "$CLIENT_ID" != "provider-b" ]]; then
  echo "Usage: $0 <provider-a|provider-b>" >&2
  exit 1
fi

# Keycloak configuration (fallbacks match the docker-compose setup)
KEYCLOAK_URL="${KEYCLOAK_URL:-http://localhost:8081}"
KEYCLOAK_REALM="${KEYCLOAK_REALM:-backend}"
CLIENT_SECRET="${CLIENT_SECRET:-${CLIENT_ID}-secret}"

TOKEN_URL="${KEYCLOAK_URL}/realms/${KEYCLOAK_REALM}/protocol/openid-connect/token"

# Request the token from Keycloak
RESPONSE="$(curl -sS -X POST "$TOKEN_URL" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  --data-urlencode "grant_type=client_credentials" \
  --data-urlencode "client_id=${CLIENT_ID}" \
  --data-urlencode "client_secret=${CLIENT_SECRET}")" || {
  echo "Error: Failed to connect to Keycloak at ${TOKEN_URL}" >&2
  exit 1
}

# Extract the access_token from the JSON response
TOKEN="$(printf '%s' "$RESPONSE" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')"

if [[ -z "$TOKEN" ]]; then
  echo "Error: Keycloak response did not contain an access_token. Response body:" >&2
  echo "$RESPONSE" >&2
  exit 1
fi

printf '%s\n' "$TOKEN"