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
        python3 -m venv "${VENV_DIR}"
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
    if ! curl -skS -m 5 "${SCHEME}://${HOST}/maas-api/healthz" -o /dev/null; then
      SCHEME="http"
      echo "[smoke] HTTPS not available, falling back to HTTP"
    fi
  fi

  MAAS_API_BASE_URL="${SCHEME}://${HOST}/maas-api"
fi

export HOST
export MAAS_API_BASE_URL

echo "[smoke] MAAS_API_BASE_URL=${MAAS_API_BASE_URL}"
if [[ -n "${MODEL_NAME}" ]]; then
  echo "[smoke] Using MODEL_NAME=${MODEL_NAME}"
fi

USER="$(oc whoami)"
echo "[smoke] Performing smoke test for user: ${USER}"

# 1) Mint a MaaS token using your cluster token
mkdir -p "${DIR}/reports"
LOG="${DIR}/reports/smoke-${USER}.log"
: > "${LOG}"

FREE_OC_TOKEN="$(oc whoami -t || true)"
TOKEN_RESPONSE="$(curl -skS \
  -H "Authorization: Bearer ${FREE_OC_TOKEN}" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{"expiration":"10m"}' \
  "${MAAS_API_BASE_URL}/v1/tokens" || true)"

TOKEN="$(echo "${TOKEN_RESPONSE}" | jq -r .token 2>/dev/null || true)"
if [[ -z "${TOKEN}" || "${TOKEN}" == "null" ]]; then
  echo "[smoke] ERROR: could not mint MaaS token" | tee -a "${LOG}"
  echo "${TOKEN_RESPONSE}" | tee -a "${LOG}"
  exit 1
fi
export TOKEN

# Log a masked preview of the token to the log (not the console)
echo "[token] minted: len=$((${#TOKEN})) head=${TOKEN:0:12}…tail=${TOKEN: -8}" >> "${LOG}"

# 2) Get models, derive URL/ID if catalog returns them
MODELS_JSON="$(curl -skS -H "Authorization: Bearer ${TOKEN}" "${MAAS_API_BASE_URL}/v1/models" || true)"
MODEL_URL="$(echo "${MODELS_JSON}" | jq -r '(.data // .models // [])[0]?.url // empty' 2>/dev/null || true)"
MODEL_ID="$(echo  "${MODELS_JSON}" | jq -r '(.data // .models // [])[0]?.id  // empty' 2>/dev/null || true)"

# Fallbacks
if [[ -z "${MODEL_ID}" || "${MODEL_ID}" == "null" ]]; then
  if [[ -z "${MODEL_NAME:-}" ]]; then
    echo "[smoke] ERROR: catalog did not return a model id and MODEL_NAME not set" | tee -a "${LOG}"
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
  "${DIR}/tests/test_smoke.py"
)

python -c 'import pytest_html' >/dev/null 2>&1 || echo "[smoke] WARNING: pytest-html not found (but we still passed --html)"

pytest "${PYTEST_ARGS[@]}"

echo "[smoke] Reports:"
echo " - JUnit XML : ${XML}"
echo " - HTML      : ${HTML}"
echo " - Log       : ${LOG}"