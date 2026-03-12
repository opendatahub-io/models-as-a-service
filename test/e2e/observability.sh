#!/usr/bin/env bash
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$DIR/../.." && pwd)"
export PYTHONPATH="${DIR}:${PYTHONPATH:-}"

# Source shared helper functions (setup_python_venv, etc.)
source "$PROJECT_ROOT/scripts/deployment-helpers.sh"

# Setup and activate virtual environment (shared with smoke.sh)
VENV_DIR="${DIR}/.venv"
setup_python_venv "$VENV_DIR" "observability"

# Inputs via env or auto-discovery (same as smoke.sh for make_test_request)
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
      echo "[observability] ERROR: could not detect cluster ingress domain" >&2
      exit 1
    fi
    HOST="maas.${CLUSTER_DOMAIN}"
  fi

  if [[ "${INSECURE_HTTP:-}" == "true" ]]; then
    SCHEME="http"
    echo "[observability] Using HTTP (INSECURE_HTTP=true)"
  else
    SCHEME="https"
    if ! curl -skS -m 5 "${SCHEME}://${HOST}/maas-api/healthz" -o /dev/null; then
      SCHEME="http"
      echo "[observability] HTTPS not available, falling back to HTTP"
    fi
  fi

  MAAS_API_BASE_URL="${SCHEME}://${HOST}/maas-api"
fi

export HOST
export MAAS_API_BASE_URL

echo "[observability] Performing observability tests"
echo "[observability] MAAS_API_BASE_URL=${MAAS_API_BASE_URL}"
if [[ -n "${MODEL_NAME}" ]]; then
  echo "[observability] Using MODEL_NAME=${MODEL_NAME}"
fi

USER="$(oc whoami 2>/dev/null || echo 'unknown')"
USER="$(printf '%s' "$USER" | tr ':/@\\' '----' | sed 's/--*/-/g; s/^-//; s/-$//')"
USER="${USER:-unknown}"

mkdir -p "${DIR}/reports"
HTML="${DIR}/reports/observability-${USER}.html"
XML="${DIR}/reports/observability-${USER}.xml"

PYTEST_ARGS=(
  -v
  --tb=short
  "--junitxml=${XML}"
  --html="${HTML}" --self-contained-html
  --capture=tee-sys
  --show-capture=all
  --log-level=INFO
  "${DIR}/tests/test_observability.py"
)

python -c 'import pytest_html' >/dev/null 2>&1 || echo "[observability] WARNING: pytest-html not found (but we still passed --html)"

pytest "${PYTEST_ARGS[@]}"

echo "[observability] Reports:"
echo " - JUnit XML : ${XML}"
echo " - HTML      : ${HTML}"
