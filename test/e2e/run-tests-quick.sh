#!/bin/bash
# =============================================================================
# Quick E2E Test Runner
# =============================================================================
#
# Runs E2E tests directly without full prow deployment.
# Much faster than prow_run_smoke_test.sh when infrastructure is already deployed.
#
# Usage:
#   ./run-tests-quick.sh [pytest args]
#
# By default runs the same smoke test file list as CI. Pass a file path
# to override the list and run only that file.
#
# Examples:
#   ./run-tests-quick.sh                          # Run smoke suite (same as CI)
#   ./run-tests-quick.sh -k test_auth             # Filter smoke suite by name
#   ./run-tests-quick.sh --maxfail=2              # Stop after 2 failures
#   ./run-tests-quick.sh tests/test_api_keys.py   # Run specific file (replaces smoke list)
#
# =============================================================================

set -euo pipefail

# Find project root
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# Get cluster domain
CLUSTER_DOMAIN="$(oc get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"
HOST="maas.${CLUSTER_DOMAIN}"

# Set required environment variables
export GATEWAY_HOST="${HOST}"
export DEPLOYMENT_NAMESPACE="${DEPLOYMENT_NAMESPACE:-opendatahub}"
export MAAS_SUBSCRIPTION_NAMESPACE="${MAAS_SUBSCRIPTION_NAMESPACE:-models-as-a-service}"
export E2E_SKIP_TLS_VERIFY=true
export MODEL_NAME="facebook-opt-125m-simulated"

# Get tokens
export TOKEN=$(oc whoami -t 2>/dev/null || oc create token default -n default --duration=1h)
export ADMIN_OC_TOKEN="${TOKEN}"  # Same for local testing

# Additional optional vars
export MAAS_API_BASE_URL="https://${HOST}/maas-api"
export K8S_CLUSTER_URL=$(oc whoami --show-server)
export CLUSTER_DOMAIN="${CLUSTER_DOMAIN}"
export INSECURE_HTTP="false"

echo "=========================================="
echo "Quick E2E Test Runner"
echo "=========================================="
echo ""
echo "Environment:"
echo "  GATEWAY_HOST: ${GATEWAY_HOST}"
echo "  DEPLOYMENT_NAMESPACE: ${DEPLOYMENT_NAMESPACE}"
echo "  MAAS_SUBSCRIPTION_NAMESPACE: ${MAAS_SUBSCRIPTION_NAMESPACE}"
echo "  MAAS_API_BASE_URL: ${MAAS_API_BASE_URL}"
echo "  TOKEN: ${TOKEN:0:20}..."
export E2E_PARALLEL_WORKERS="${E2E_PARALLEL_WORKERS:-7}"
if ! [[ "$E2E_PARALLEL_WORKERS" =~ ^[1-9][0-9]*$ ]]; then
    echo "ERROR: E2E_PARALLEL_WORKERS must be a positive integer (>= 1), got '$E2E_PARALLEL_WORKERS'" >&2
    exit 1
fi
if [[ "$E2E_PARALLEL_WORKERS" -gt 1 ]]; then
    echo "  E2E_PARALLEL_WORKERS: ${E2E_PARALLEL_WORKERS}"
fi
echo ""
echo "Running tests..."
echo ""

# Delegate to the shared test runner (same script CI uses).
export ARTIFACTS_DIR="${ARTIFACTS_DIR:-$PROJECT_ROOT/test/e2e/reports}"
exec "$PROJECT_ROOT/test/e2e/scripts/run_e2e_tests.sh" "$@"
