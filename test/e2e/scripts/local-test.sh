#!/bin/bash
# Run MaaS E2E tests against a local Kind cluster deployed by local-deploy.sh.
#
# This script:
#   1. Port-forwards the gateway
#   2. Creates a test token
#   3. Runs pytest with the right env vars for Kind
#   4. Cleans up
#
# Usage:
#   ./test/e2e/scripts/local-test.sh                         # Run all local-compatible tests
#   ./test/e2e/scripts/local-test.sh -k test_create_api_key  # Run specific test
#   ./test/e2e/scripts/local-test.sh tests/test_api_keys.py  # Run specific file
#
# Environment variables:
#   KIND_CLUSTER_NAME    - Cluster name (default: maas-local)
#   MAAS_NAMESPACE       - MaaS namespace (default: maas-system)
#   GATEWAY_NAMESPACE    - Gateway namespace (default: istio-system)
#   LOCAL_PORT           - Local port for gateway port-forward (default: 19080)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-maas-local}"
MAAS_NAMESPACE="${MAAS_NAMESPACE:-maas-system}"
GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-istio-system}"
LOCAL_PORT="${LOCAL_PORT:-19080}"

# ─── Preflight ──────────────────────────────────────────────────────────────

if ! kind get clusters 2>/dev/null | grep -q "^${KIND_CLUSTER_NAME}$"; then
  echo "ERROR: Kind cluster '${KIND_CLUSTER_NAME}' not found."
  echo "Run ./test/e2e/scripts/local-deploy.sh first."
  exit 1
fi

kubectl config use-context "kind-${KIND_CLUSTER_NAME}" &>/dev/null

# Verify MaaS API is running
if ! kubectl get deployment maas-api -n "$MAAS_NAMESPACE" &>/dev/null; then
  echo "ERROR: maas-api not found in namespace '${MAAS_NAMESPACE}'."
  echo "Run ./test/e2e/scripts/local-deploy.sh first."
  exit 1
fi

# ─── Python venv ────────────────────────────────────────────────────────────

VENV_DIR="${E2E_DIR}/.venv"
if [[ ! -d "$VENV_DIR" ]]; then
  echo "Creating Python venv..."
  python3 -m venv "$VENV_DIR" --upgrade-deps
fi
# shellcheck disable=SC1091
source "$VENV_DIR/bin/activate"
pip install -q -r "$E2E_DIR/requirements.txt"

# ─── Port-forward ──────────────────────────────────────────────────────────

# Kill any existing port-forward on this port
lsof -ti "tcp:${LOCAL_PORT}" 2>/dev/null | xargs kill 2>/dev/null || true

kubectl port-forward -n "$GATEWAY_NAMESPACE" svc/maas-default-gateway-istio "${LOCAL_PORT}:80" > /dev/null 2>&1 &
PF_PID=$!
sleep 3

# Verify port-forward is working
if ! kill -0 $PF_PID 2>/dev/null; then
  echo "ERROR: Port-forward failed to start."
  exit 1
fi

cleanup() {
  kill $PF_PID 2>/dev/null
  wait $PF_PID 2>/dev/null || true
}
trap cleanup EXIT

# ─── Environment ────────────────────────────────────────────────────────────

export GATEWAY_HOST="localhost:${LOCAL_PORT}"
export MAAS_API_BASE_URL="http://localhost:${LOCAL_PORT}/maas-api"
export INSECURE_HTTP="true"
export E2E_SKIP_TLS_VERIFY="true"
export DEPLOYMENT_NAMESPACE="$MAAS_NAMESPACE"
export MAAS_SUBSCRIPTION_NAMESPACE="models-as-a-service"

# Create a K8s service account token for auth
export TOKEN
TOKEN=$(kubectl create token maas-api -n "$MAAS_NAMESPACE" --duration=1h --audience=https://kubernetes.default.svc)

# Model name from the test fixtures deployed by local-deploy.sh
export MODEL_NAME="${MODEL_NAME:-llm-katan-openai}"

# ─── Smoke check ────────────────────────────────────────────────────────────

echo "============================================"
echo "  MaaS Local E2E Tests"
echo "============================================"
echo ""
echo "  Gateway:  http://localhost:${LOCAL_PORT}"
echo "  MaaS API: ${MAAS_API_BASE_URL}"
echo "  Model:    ${MODEL_NAME}"
echo ""

# Quick sanity check
HTTP_CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://localhost:${LOCAL_PORT}/v1/models" 2>/dev/null || echo "000")
if [[ "$HTTP_CODE" == "401" ]]; then
  echo "  Gateway: OK (401 — auth enforced)"
elif [[ "$HTTP_CODE" == "200" ]]; then
  echo "  Gateway: OK (200)"
else
  echo "  WARNING: Gateway returned HTTP ${HTTP_CODE}"
fi

HEALTH=$(curl -s --max-time 5 "${MAAS_API_BASE_URL}/health" 2>/dev/null || echo "")
if [[ "$HEALTH" == *"healthy"* ]]; then
  echo "  Health:  OK"
else
  echo "  WARNING: Health check failed"
fi
echo ""

# ─── Run tests ──────────────────────────────────────────────────────────────

# Default: run MaaS management tests (API keys, models endpoint).
# Model inference tests (TestAPIKeyModelInference) are excluded because they
# hit the model URL directly from the Mac host, which isn't routable to the
# Kind docker network. These tests work when BBR + gateway egress is configured.
#
# To run all tests: ./local-test.sh --run-all
# To run specific tests: ./local-test.sh -k test_create_api_key

PYTEST_ARGS=(-v --tb=short)
RUN_ALL=false

# Parse our args vs pytest args
PASSTHROUGH_ARGS=()
for arg in "$@"; do
  case "$arg" in
    --run-all) RUN_ALL=true ;;
    *) PASSTHROUGH_ARGS+=("$arg") ;;
  esac
done

if [[ "$RUN_ALL" == "false" && ${#PASSTHROUGH_ARGS[@]} -eq 0 ]]; then
  # Default: run MaaS management tests that work with port-forward.
  #
  # Excluded tests (require model inference endpoint routable from Mac host):
  #   TestAPIKeyModelInference    - posts to model /v1/completions
  #   TestAPIKeyRevocationE2E    (2 of 4) - revoke_then_create, revoke_keys_rejected_at_gateway
  #   TestAPIKeySubscriptionPhases - creates API keys and hits model endpoint
  #   test_models_endpoint.py     - api_key fixture hits model endpoint in setup
  #
  # These will work once BBR is deployed and gateway egress to llm-katan is configured.
  PYTEST_ARGS+=(
    -k "not (TestAPIKeyModelInference or TestAPIKeySubscriptionPhases or test_revoke_keys_rejected_at_gateway or test_revoke_then_create_new_key_works)"
    "${E2E_DIR}/tests/test_api_keys.py"
  )
else
  PYTEST_ARGS+=("${PASSTHROUGH_ARGS[@]}")
fi

echo "Running: pytest ${PYTEST_ARGS[*]}"
echo ""

cd "$E2E_DIR"
python -m pytest "${PYTEST_ARGS[@]}"
