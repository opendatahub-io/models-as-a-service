#!/bin/bash

set -euo pipefail

MAAS_API_NAMESPACE="${MAAS_API_NAMESPACE:-opendatahub}"
KEYCLOAK_REALM="${KEYCLOAK_REALM:-maas}"
OIDC_CLIENT_ID="${OIDC_CLIENT_ID:-maas-cli}"
OIDC_USERNAME="${OIDC_USERNAME:-alice}"
OIDC_PASSWORD="${OIDC_PASSWORD:-letmein}"
MODEL_PROMPT="${MODEL_PROMPT:-Hello from OIDC validation}"

log_info() {
  echo "[INFO] $*"
}

log_error() {
  echo "[ERROR] $*" >&2
}

resolve_gateway_host() {
  if [[ -n "${MAAS_GATEWAY_HOST:-}" ]]; then
    local host="${MAAS_GATEWAY_HOST#*://}"
    echo "https://${host}"
    return 0
  fi

  local listener_host
  listener_host=$(oc get gateway maas-default-gateway -n openshift-ingress -o jsonpath='{.spec.listeners[?(@.protocol=="HTTPS")].hostname}' 2>/dev/null | awk '{print $1}')
  if [[ -n "$listener_host" ]]; then
    echo "https://${listener_host}"
    return 0
  fi

  local cluster_domain
  cluster_domain=$(oc get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null || true)
  if [[ -n "$cluster_domain" ]]; then
    echo "https://maas.${cluster_domain}"
    return 0
  fi

  return 1
}

resolve_keycloak_host() {
  if [[ -n "${KEYCLOAK_HOST:-}" ]]; then
    local host="${KEYCLOAK_HOST#*://}"
    echo "https://${host}"
    return 0
  fi

  local cluster_domain
  cluster_domain=$(oc get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null || true)
  if [[ -n "$cluster_domain" ]]; then
    echo "https://keycloak.${cluster_domain}"
    return 0
  fi

  return 1
}

require_command() {
  local cmd="$1"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    log_error "Required command not found: $cmd"
    exit 1
  fi
}

require_command oc
require_command jq
require_command curl

MAAS_GATEWAY="$(resolve_gateway_host)" || {
  log_error "Could not determine MaaS gateway host"
  exit 1
}

KEYCLOAK_BASE="$(resolve_keycloak_host)" || {
  log_error "Could not determine Keycloak host; set KEYCLOAK_HOST"
  exit 1
}

OIDC_TOKEN_URL="${OIDC_TOKEN_URL:-${KEYCLOAK_BASE}/realms/${KEYCLOAK_REALM}/protocol/openid-connect/token}"
OIDC_TOKEN="${OIDC_TOKEN:-}"

if [[ -z "$OIDC_TOKEN" ]]; then
  log_info "Requesting OIDC token for ${OIDC_USERNAME}"
  OIDC_TOKEN=$(curl -sk -X POST "${OIDC_TOKEN_URL}" \
    -d "grant_type=password" \
    -d "client_id=${OIDC_CLIENT_ID}" \
    -d "username=${OIDC_USERNAME}" \
    -d "password=${OIDC_PASSWORD}" | jq -r '.access_token')
fi

if [[ -z "$OIDC_TOKEN" || "$OIDC_TOKEN" == "null" ]]; then
  log_error "Failed to obtain OIDC token"
  exit 1
fi

log_info "Creating MaaS API key with OIDC token"
CREATE_KEY_RESPONSE=$(curl -sk -w '\n%{http_code}' -X POST "${MAAS_GATEWAY}/maas-api/v1/api-keys" \
  -H "Authorization: Bearer ${OIDC_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"name":"oidc-validation-key"}')
CREATE_KEY_STATUS=$(echo "$CREATE_KEY_RESPONSE" | awk 'END{print}')
CREATE_KEY_BODY=$(echo "$CREATE_KEY_RESPONSE" | sed '$d')

if [[ "$CREATE_KEY_STATUS" != "201" ]]; then
  log_error "OIDC key mint failed with status ${CREATE_KEY_STATUS}"
  echo "$CREATE_KEY_BODY"
  exit 1
fi

API_KEY=$(echo "$CREATE_KEY_BODY" | jq -r '.key')
if [[ -z "$API_KEY" || "$API_KEY" == "null" ]]; then
  log_error "OIDC key mint response missing API key"
  echo "$CREATE_KEY_BODY"
  exit 1
fi

log_info "Listing models with minted API key"
MODELS_RESPONSE=$(curl -sk -w '\n%{http_code}' -H "Authorization: Bearer ${API_KEY}" \
  "${MAAS_GATEWAY}/maas-api/v1/models")
MODELS_STATUS=$(echo "$MODELS_RESPONSE" | awk 'END{print}')
MODELS_BODY=$(echo "$MODELS_RESPONSE" | sed '$d')

if [[ "$MODELS_STATUS" != "200" ]]; then
  log_error "Model listing with API key failed with status ${MODELS_STATUS}"
  echo "$MODELS_BODY"
  exit 1
fi

MODEL_ID=$(echo "$MODELS_BODY" | jq -r '.data[0].id // empty')
MODEL_URL=$(echo "$MODELS_BODY" | jq -r '.data[0].url // empty')

if [[ -z "$MODEL_ID" || -z "$MODEL_URL" ]]; then
  log_error "No models returned by /v1/models; deploy a sample model first"
  echo "$MODELS_BODY"
  exit 1
fi

log_info "Running inference against ${MODEL_ID}"
INFER_RESPONSE=$(curl -sk -w '\n%{http_code}' -X POST "${MODEL_URL}/v1/chat/completions" \
  -H "Authorization: Bearer ${API_KEY}" \
  -H "Content-Type: application/json" \
  -d "{\"model\":\"${MODEL_ID}\",\"messages\":[{\"role\":\"user\",\"content\":\"${MODEL_PROMPT}\"}],\"max_tokens\":16}")
INFER_STATUS=$(echo "$INFER_RESPONSE" | awk 'END{print}')
INFER_BODY=$(echo "$INFER_RESPONSE" | sed '$d')

if [[ "$INFER_STATUS" != "200" ]]; then
  log_error "Inference with minted API key failed with status ${INFER_STATUS}"
  echo "$INFER_BODY"
  exit 1
fi

cat <<EOF
[INFO] OIDC validation flow passed.
[INFO] Gateway: ${MAAS_GATEWAY}
[INFO] Token endpoint: ${OIDC_TOKEN_URL}
[INFO] Model: ${MODEL_ID}
[INFO] API key prefix: ${API_KEY:0:20}...
EOF
