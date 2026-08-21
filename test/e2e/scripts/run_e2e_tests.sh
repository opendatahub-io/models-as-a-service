#!/bin/bash
# =============================================================================
# E2E Test Runner — pytest execution only
# =============================================================================
#
# Owns pytest invocation for MaaS E2E tests. Separated from
# prow_run_smoke_test.sh so that test-only changes don't touch deploy/validate
# logic. Implements the two-phase model:
#   Pass 1: parallel with pytest-xdist (-m "not serial", --dist=loadgroup)
#   Pass 2: serial cluster mutators (-m serial, single worker)
#
# Called by:
#   - prow_run_smoke_test.sh (CI: deploy → validate → THIS)
#   - run-tests-quick.sh     (local: env setup → THIS)
#   - directly               (when env is already exported)
#
# Required env vars (caller must export):
#   GATEWAY_HOST, TOKEN, ADMIN_OC_TOKEN,
#   DEPLOYMENT_NAMESPACE, MAAS_SUBSCRIPTION_NAMESPACE
#
# Optional env vars:
#   E2E_PARALLEL_WORKERS          pytest-xdist worker count (default: 7)
#   E2E_AUTHPOLICY_PHASE_TIMEOUT  seconds (default: 120, parallel only)
#   E2E_MAAS_SUBSCRIPTION_PHASE_TIMEOUT  seconds (default: 90, parallel only)
#   E2E_GATEWAY_ENFORCED_TIMEOUT  seconds (default: 240, parallel only)
#   E2E_MULTITENANCY_PHASE_TIMEOUT seconds (default: 180, parallel only)
#   E2E_RECONCILE_WAIT            seconds between reconcile polls (default: 4)
#   ARTIFACTS_DIR                 output directory for JUnit/HTML (default: test/e2e/reports)
#
# Usage:
#   export GATEWAY_HOST=maas.apps.cluster.example.com TOKEN=$(oc whoami -t) ...
#   ./run_e2e_tests.sh                       # run all tests
#   ./run_e2e_tests.sh -- -k test_api_keys   # pass extra pytest args
#   ./run_e2e_tests.sh --serial-only         # skip parallel pass
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
TEST_DIR="$PROJECT_ROOT/test/e2e"

# ── Parse script flags (before --) vs pytest args (after --) ─────────────
serial_only=false
extra_pytest_args=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        --serial-only) serial_only=true; shift ;;
        --) shift; extra_pytest_args=("$@"); break ;;
        *) extra_pytest_args+=("$1"); shift ;;
    esac
done

# ── Defaults ─────────────────────────────────────────────────────────────
export E2E_RECONCILE_WAIT="${E2E_RECONCILE_WAIT:-4}"
E2E_PARALLEL_WORKERS="${E2E_PARALLEL_WORKERS:-7}"
if ! [[ "$E2E_PARALLEL_WORKERS" =~ ^[1-9][0-9]*$ ]]; then
    echo "ERROR: E2E_PARALLEL_WORKERS must be a positive integer (>= 1), got '$E2E_PARALLEL_WORKERS'" >&2
    exit 1
fi

ARTIFACTS_DIR="${ARTIFACTS_DIR:-${ARTIFACT_DIR:-${ARTIFACTS:-${LOG_DIR:-$TEST_DIR/reports}}}}"
mkdir -p "$ARTIFACTS_DIR"

# ── Phase timeouts (parallel only) ───────────────────────────────────────
if [[ "$E2E_PARALLEL_WORKERS" -gt 1 ]]; then
    export E2E_AUTHPOLICY_PHASE_TIMEOUT="${E2E_AUTHPOLICY_PHASE_TIMEOUT:-120}"
    export E2E_MAAS_SUBSCRIPTION_PHASE_TIMEOUT="${E2E_MAAS_SUBSCRIPTION_PHASE_TIMEOUT:-90}"
    export E2E_GATEWAY_ENFORCED_TIMEOUT="${E2E_GATEWAY_ENFORCED_TIMEOUT:-240}"
    export E2E_MULTITENANCY_PHASE_TIMEOUT="${E2E_MULTITENANCY_PHASE_TIMEOUT:-180}"
fi

# ── Venv ─────────────────────────────────────────────────────────────────
if [[ ! -d "$TEST_DIR/.venv" ]]; then
    echo "Creating Python venv for e2e tests..."
    python3 -m venv "$TEST_DIR/.venv" --upgrade-deps
fi
source "$TEST_DIR/.venv/bin/activate"
python -m pip install --upgrade pip --quiet
python -m pip install -r "$TEST_DIR/requirements.txt" --quiet

# ── Output paths ─────────────────────────────────────────────────────────
user="$(oc whoami 2>/dev/null || echo 'unknown')"
html="$ARTIFACTS_DIR/e2e-${user}.html"
xml="$ARTIFACTS_DIR/e2e-${user}.xml"
xml_serial="${xml%.xml}-serial.xml"

# ── Test file list ───────────────────────────────────────────────────────
e2e_test_files=(
    "$TEST_DIR/tests/test_api_keys.py"
    "$TEST_DIR/tests/test_namespace_scoping.py"
    "$TEST_DIR/tests/test_negative_security.py"
    "$TEST_DIR/tests/test_subscription.py"
    "$TEST_DIR/tests/test_models_endpoint.py"
    "$TEST_DIR/tests/test_external_models.py"
    "$TEST_DIR/tests/test_smoke.py"
    "$TEST_DIR/tests/test_tenant.py"
    "$TEST_DIR/tests/test_config_tenant.py"
    "$TEST_DIR/tests/test_tenant_discovery.py"
    "$TEST_DIR/tests/test_aitenant_lifecycle.py"
    "$TEST_DIR/tests/test_tenant_namespace_discovery.py"
    "$TEST_DIR/tests/test_tenant_discovery_isolation.py"
    "$TEST_DIR/tests/test_gateway_scoped_authpolicy.py"
    "$TEST_DIR/tests/test_multi_tenant_integration.py"
    "$TEST_DIR/tests/test_multi_tenant_maas_api.py"
    "$TEST_DIR/tests/test_tenant_model_inference.py"
    "$TEST_DIR/tests/test_tenant_auth_isolation.py"
    "$TEST_DIR/tests/test_tenant_subscription_isolation.py"
    "$TEST_DIR/tests/test_tenant_rate_limit_isolation.py"
    "$TEST_DIR/tests/test_per_tenant_ipp_isolation.py"
    "$TEST_DIR/tests/test_external_oidc.py"
)

# If extra args include a path (file or directory), skip the default smoke list
# so users can target specific tests: ./run_e2e_tests.sh -- tests/test_api_keys.py
# Resolve relative paths against TEST_DIR so they work regardless of cwd.
resolved_extra_args=()
has_path_arg=false
for arg in "${extra_pytest_args[@]}"; do
    if [[ -e "$TEST_DIR/$arg" ]]; then
        resolved_extra_args+=("$TEST_DIR/$arg")
        has_path_arg=true
    elif [[ -e "$arg" ]]; then
        resolved_extra_args+=("$arg")
        has_path_arg=true
    else
        resolved_extra_args+=("$arg")
    fi
done

if $has_path_arg; then
    pytest_common_args=(
        -v --disable-warnings
        --capture=tee-sys --show-capture=all --log-level=INFO
        "${resolved_extra_args[@]}"
    )
else
    pytest_common_args=(
        -v --disable-warnings
        --capture=tee-sys --show-capture=all --log-level=INFO
        "${e2e_test_files[@]}"
        "${extra_pytest_args[@]}"
    )
fi

# ── Run ──────────────────────────────────────────────────────────────────
parallel_rc=0
serial_rc=0

if [[ "$serial_only" == "true" || "$E2E_PARALLEL_WORKERS" -le 1 ]]; then
    echo "Running E2E tests serially (E2E_PARALLEL_WORKERS=${E2E_PARALLEL_WORKERS})"
    if ! PYTHONPATH="$TEST_DIR:${PYTHONPATH:-}" pytest \
        --maxfail=5 \
        --junitxml="$xml" \
        --html="$html" --self-contained-html \
        "${pytest_common_args[@]}"; then
        parallel_rc=1
    fi
else
    echo "Running E2E pass 1/2: parallel (E2E_PARALLEL_WORKERS=${E2E_PARALLEL_WORKERS}, --dist=loadgroup, -m 'not serial')"
    if ! PYTHONPATH="$TEST_DIR:${PYTHONPATH:-}" pytest \
        --maxfail=5 \
        -n "$E2E_PARALLEL_WORKERS" --dist=loadgroup \
        -m "not serial" \
        --junitxml="$xml" \
        --html="$html" --self-contained-html \
        "${pytest_common_args[@]}"; then
        parallel_rc=1
    fi

    echo "Running E2E pass 2/2: serial cluster mutators (-m serial, single worker)"
    if ! PYTHONPATH="$TEST_DIR:${PYTHONPATH:-}" pytest \
        --maxfail=5 \
        -m serial \
        --junitxml="$xml_serial" \
        --html="${html%.html}-serial.html" --self-contained-html \
        "${pytest_common_args[@]}"; then
        serial_rc=1
    fi
fi

# ── Result ───────────────────────────────────────────────────────────────
if [[ "$parallel_rc" -ne 0 || "$serial_rc" -ne 0 ]]; then
    echo "❌ ERROR: E2E tests failed (parallel_rc=${parallel_rc}, serial_rc=${serial_rc})"
    exit 1
fi

echo "✅ E2E tests completed"
echo " - JUnit XML : ${xml}"
if [[ -f "$xml_serial" ]]; then
    echo " - JUnit XML (serial pass): ${xml_serial}"
fi
echo " - HTML      : ${html}"
