#!/usr/bin/env bash
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
export PYTHONPATH="${DIR}:${PYTHONPATH:-}"

# Python virtual environment setup
VENV_DIR="${DIR}/.venv"

setup_python_venv() {
    echo "[smoke] Setting up Python virtual environment..."
    
    # Create virtual environment if it doesn't exist
    if [[ ! -d "${VENV_DIR}" ]]; then
        echo "[smoke] Creating virtual environment at ${VENV_DIR}"
        python3 -m venv "${VENV_DIR}" --upgrade-deps
    fi
    
    # Activate virtual environment
    echo "[smoke] Activating virtual environment"
    source "${VENV_DIR}/bin/activate"
    
    # Upgrade pip and install requirements
    echo "[smoke] Installing Python dependencies"
    python -m pip install --upgrade pip --quiet
    python -m pip install -r "${DIR}/requirements.txt" --quiet
    
    echo "[smoke] Virtual environment setup complete"
}

# Setup and activate virtual environment
setup_python_venv

# Inputs via env or auto-discovery
HOST="${HOST:-}"
MAAS_API_BASE_URL="${MAAS_API_BASE_URL:-}"
MODEL_NAME="${MODEL_NAME:-}"

if [[ -z "${MAAS_API_BASE_URL}" ]]; then
  if [[ -z "${HOST}" ]]; then
    CLUSTER_DOMAIN="$(
      oc get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null \
      || oc get ingresses.config/cluster -o jsonpath='{.spec.domain}' 2>/dev/null \
      || true
    )"
    if [[ -z "${CLUSTER_DOMAIN}" ]]; then
      echo "[smoke] ERROR: could not detect cluster ingress domain" >&2
      exit 1
    fi
    HOST="maas.${CLUSTER_DOMAIN}"
  fi

  # Determine scheme: INSECURE_HTTP forces HTTP, otherwise try HTTPS first with fallback
  if [[ "${INSECURE_HTTP:-}" == "true" ]]; then
    SCHEME="http"
    echo "[smoke] Using HTTP (INSECURE_HTTP=true)"
  else
    SCHEME="https"
    if ! curl -skS -m 5 "${SCHEME}://${HOST}/maas-api/health" -o /dev/null; then
      SCHEME="http"
      echo "[smoke] HTTPS not available, falling back to HTTP"
    fi
  fi

  MAAS_API_BASE_URL="${SCHEME}://${HOST}/maas-api"
fi

# Extract HOST from MAAS_API_BASE_URL if not already set
if [[ -z "${HOST}" && -n "${MAAS_API_BASE_URL}" ]]; then
  HOST=$(echo "${MAAS_API_BASE_URL}" | sed -E 's|^[^:]+://([^/]+).*|\1|')
fi

export HOST
export GATEWAY_HOST="${HOST}"  # Required by test_subscription.py
export MAAS_API_BASE_URL

echo "[smoke] MAAS_API_BASE_URL=${MAAS_API_BASE_URL}"
if [[ -n "${MODEL_NAME}" ]]; then
  echo "[smoke] Using MODEL_NAME=${MODEL_NAME}"
fi

USER="$(oc whoami)"
echo "[smoke] Performing smoke test for user: ${USER}"

# 1) Get bootstrap token and mint API key for tests
mkdir -p "${DIR}/reports"
LOG="${DIR}/reports/smoke-${USER}.log"
: > "${LOG}"

# Get bootstrap token (cluster token used only for minting API keys)
BOOTSTRAP_TOKEN="$(oc whoami -t || true)"
if [[ -z "${BOOTSTRAP_TOKEN}" ]]; then
  echo "[smoke] ERROR: could not get bootstrap token via 'oc whoami -t'" | tee -a "${LOG}"
  echo "[smoke] Make sure you are logged into OpenShift" | tee -a "${LOG}"
  exit 1
fi

# Log token acquisition without exposing token content
echo "[bootstrap] acquired cluster token (len=${#BOOTSTRAP_TOKEN})" >> "${LOG}"

# Mint an API key using a bootstrap token
# Usage: mint_api_key <key_name> [bootstrap_token]
# All logs go to stderr; only the key is written to stdout
mint_api_key() {
  local key_name="${1:-e2e-smoke-key}"
  local token="${2:-${BOOTSTRAP_TOKEN}}"
  local response
  local api_key
  
  # Pre-flight check for jq
  if ! command -v jq >/dev/null 2>&1; then
    echo "[smoke] ERROR: jq is required to mint API keys" | tee -a "${LOG}" >&2
    return 1
  fi
  
  echo "[smoke] Minting API key '${key_name}' via ${MAAS_API_BASE_URL}/v1/api-keys..." | tee -a "${LOG}" >&2
  
  if ! response=$(curl -skS --max-time 30 -X POST \
    -H "Authorization: Bearer ${token}" \
    -H "Content-Type: application/json" \
    -d "{\"name\": \"${key_name}\", \"expiresIn\": \"2h\"}" \
    "${MAAS_API_BASE_URL}/v1/api-keys" 2>&1); then
    echo "[smoke] ERROR: Failed to reach ${MAAS_API_BASE_URL}/v1/api-keys" | tee -a "${LOG}" >&2
    return 1
  fi
  
  api_key=$(echo "${response}" | jq -r '.key // empty' 2>/dev/null || true)
  
  if [[ -z "${api_key}" || "${api_key}" == "null" ]]; then
    echo "[smoke] ERROR: Failed to mint API key" | tee -a "${LOG}" >&2
    echo "[smoke] Response from /v1/api-keys was not parseable (may contain sensitive data)" | tee -a "${LOG}" >&2
    return 1
  fi
  
  echo "[smoke] Successfully minted API key (len=${#api_key})" | tee -a "${LOG}" >&2
  printf '%s\n' "${api_key}"
}

# Mint API key for tests
if ! TOKEN=$(mint_api_key "e2e-smoke-${USER}"); then
  echo "[smoke] ERROR: Failed to mint API key for tests" | tee -a "${LOG}"
  exit 1
fi
export TOKEN

# Admin token setup - add to odh-admins, then mint admin API key
setup_admin_token() {
  # Clear any stale inherited value to prevent false positive admin tests
  unset ADMIN_OC_TOKEN
  
  echo "[smoke] Setting up admin token for admin tests..."
  
  local current_user
  current_user=$(oc whoami)
  
  # Check if user has admin permissions
  if ! oc auth can-i patch groups &>/dev/null; then
    echo "[smoke] Current user lacks admin permissions - admin tests will be skipped"
    return 0
  fi

  # Add current user to odh-admins group so maas-api recognizes them as admin
  if oc get group odh-admins &>/dev/null; then
    oc adm groups add-users odh-admins "$current_user" 2>/dev/null || true
    echo "[smoke] Added $current_user to odh-admins group"
  else
    echo "[smoke] odh-admins group not found - admin tests will be skipped"
    return 0
  fi

  # Grant odh-admins RBAC so SAR-based admin check passes.
  # maas-api IsAdmin does a SubjectAccessReview: "can user create maasauthpolicies?"
  # The ODH operator will provide this via opendatahub-operator#3301; until then, create it here.
  local maas_sub_ns="${MAAS_SUBSCRIPTION_NAMESPACE:-models-as-a-service}"
  oc apply -f - <<RBAC_EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: maas-admin
rules:
- apiGroups: ["maas.opendatahub.io"]
  resources: ["maasauthpolicies", "maassubscriptions"]
  verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: odh-admins-maas-admin
  namespace: $maas_sub_ns
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: maas-admin
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: odh-admins
RBAC_EOF

  # Get admin bootstrap token
  local admin_bootstrap_token
  admin_bootstrap_token="$(oc whoami -t 2>/dev/null || true)"
  if [[ -z "${admin_bootstrap_token}" ]]; then
    echo "[smoke] Failed to get admin bootstrap token - admin tests will be skipped"
    return 0
  fi
  
  # Mint admin API key
  local admin_api_key
  if ! admin_api_key=$(mint_api_key "e2e-admin-${current_user}" "${admin_bootstrap_token}"); then
    echo "[smoke] Failed to mint admin API key - admin tests will be skipped"
    return 0
  fi
  
  if [[ -n "${admin_api_key}" ]]; then
    ADMIN_OC_TOKEN="${admin_api_key}"
    export ADMIN_OC_TOKEN
    echo "[smoke] Admin API key minted successfully - admin tests will run"
  fi
}

setup_admin_token

# 2) Get models, derive URL/ID if catalog returns them (retry for transient empty cache)
MODEL_ID=""
for _attempt in $(seq 1 10); do
  MODELS_JSON="$(curl -skS -H "Authorization: Bearer ${TOKEN}" "${MAAS_API_BASE_URL}/v1/models" 2>&1 || true)"
  MODEL_URL="$(echo "${MODELS_JSON}" | jq -r '(.data // .models // [])[0]?.url // empty' 2>/dev/null || true)"
  MODEL_ID="$(echo  "${MODELS_JSON}" | jq -r '(.data // .models // [])[0]?.id  // empty' 2>/dev/null || true)"
  if [[ -n "${MODEL_ID}" && "${MODEL_ID}" != "null" ]]; then
    break
  fi
  echo "[smoke] models catalog empty (attempt ${_attempt}/10), retrying in 3s..." | tee -a "${LOG}"
  sleep 3
done

# Fallbacks
if [[ -z "${MODEL_ID}" || "${MODEL_ID}" == "null" ]]; then
  if [[ -z "${MODEL_NAME:-}" ]]; then
    echo "[smoke] ERROR: catalog did not return a model id and MODEL_NAME not set" | tee -a "${LOG}"
    echo "[smoke] models response was: ${MODELS_JSON:0:500}"
    exit 2
  fi
  MODEL_ID="${MODEL_NAME}"
fi

if [[ -z "${MODEL_URL}" || "${MODEL_URL}" == "null" ]]; then
  _base="${MAAS_API_BASE_URL%/maas-api}"
  _base="${_base#https://}"; _base="${_base#http://}"
  MODEL_URL="https://${_base}/llm/${MODEL_ID}"
fi

export MODEL_URL="${MODEL_URL%/}/v1"
export MODEL_NAME="${MODEL_ID}"
echo "[smoke] Using MODEL_URL=${MODEL_URL}" | tee -a "${LOG}"

# 3) Pytest outputs
HTML="${DIR}/reports/smoke-${USER}.html"
XML="${DIR}/reports/smoke-${USER}.xml"

PYTEST_ARGS=(
  -q
  --maxfail=1
  --disable-warnings
  "--junitxml=${XML}"
  # ⬇️ add these 3 so output shows up in the HTML:
  --html="${HTML}" --self-contained-html
  --capture=tee-sys              # capture prints and also echo to console
  --show-capture=all             # include captured output in the report
  --log-level=INFO               # capture logging at INFO and above
  "${DIR}/tests/"
)

python -c 'import pytest_html' >/dev/null 2>&1 || echo "[smoke] WARNING: pytest-html not found (but we still passed --html)"

pytest "${PYTEST_ARGS[@]}"

echo "[smoke] Reports:"
echo " - JUnit XML : ${XML}"
echo " - HTML      : ${HTML}"
echo " - Log       : ${LOG}"