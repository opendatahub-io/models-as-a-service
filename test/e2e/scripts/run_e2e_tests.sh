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

# Per-pass -m filters are owned by this script; forwarded markers can defeat the split.
reject_forwarded_marker_args() {
    local arg
    for arg in "${extra_pytest_args[@]}"; do
        case "$arg" in
            -m|--markers|-m*|--markers=*)
                echo "ERROR: forwarded pytest marker ($arg) is not allowed; run_e2e_tests.sh applies per-pass -m filters" >&2
                exit 1
                ;;
        esac
    done
}
reject_forwarded_marker_args

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

# If extra args include a path (file or directory), replace the default test
# directory so users can target specific tests: ./run_e2e_tests.sh -- tests/test_api_keys.py
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
        "$TEST_DIR/tests"
        "${extra_pytest_args[@]}"
    )
fi

# ── Run ──────────────────────────────────────────────────────────────────
parallel_rc=0
serial_rc=0
any_pass_collected_tests=false

count_pytest_collected() {
    local summary collected=0
    set +e
    summary=$( "$@" --collect-only -q 2>&1 | tail -1 )
    set -e
    if [[ "$summary" =~ ([0-9]+)/[0-9]+\ tests\ collected ]]; then
        collected="${BASH_REMATCH[1]}"
    elif [[ "$summary" =~ ([0-9]+)\ tests\ collected ]]; then
        collected="${BASH_REMATCH[1]}"
    elif [[ "$summary" != *"no tests collected"* ]]; then
        echo "WARNING: could not parse pytest collection summary: ${summary}" >&2
    fi
    echo "$collected"
}

# pytest exit 5 = no tests collected (e.g. -k matched only the other marker pass).
run_pytest_pass() {
    local pass_label="$1"
    shift
    local collected=0
    local rc=0

    collected=$(count_pytest_collected "$@")
    if [[ "$collected" -eq 0 ]]; then
        echo "Note: ${pass_label} collected no tests (pytest exit 5 acceptable for this pass)"
        set +e
        "$@"
        rc=$?
        set -e
        if [[ "$rc" -eq 0 || "$rc" -eq 5 ]]; then
            return 0
        fi
        return 1
    fi

    any_pass_collected_tests=true
    set +e
    "$@"
    rc=$?
    set -e
    if [[ "$rc" -eq 0 ]]; then
        return 0
    fi
    if [[ "$rc" -eq 5 ]]; then
        echo "Note: ${pass_label} collected no tests at run time (pytest exit 5), treating as success"
        return 0
    fi
    return 1
}

# Serial tests scale the Kuadrant operator and maas-controller to 0, which
# replaces the pods that ran during the parallel pass along with their restart
# history and previous-container logs. Snapshot them first. Best-effort.
snapshot_parallel_pass_pods() (
    # shellcheck source=auth_utils.sh
    source "$SCRIPT_DIR/auth_utils.sh"
    local ns
    for ns in "${RHCL_NAMESPACE:-kuadrant-system}" "$DEPLOYMENT_NAMESPACE"; do
        if kubectl get namespace "$ns" &>/dev/null; then
            collect_namespace_pod_logs "$ns" "$ARTIFACTS_DIR/pod-logs-after-parallel/$ns"
        fi
    done
)

run_serial_pass() {
    echo "Running E2E pass 2/2: serial cluster mutators (-m serial, single worker)"
    if ! run_pytest_pass "pass 2 (serial)" \
        env E2E_PYTEST_PASS=serial PYTHONPATH="$TEST_DIR:${PYTHONPATH:-}" pytest \
        --maxfail=5 \
        --junitxml="$xml_serial" \
        --html="${html%.html}-serial.html" --self-contained-html \
        "${pytest_common_args[@]}" \
        -m serial; then
        serial_rc=1
    fi
}

maybe_run_serial_pass() {
    if [[ "$parallel_rc" -ne 0 ]]; then
        echo "Skipping E2E pass 2/2 (serial): parallel pass failed"
        return 0
    fi
    run_serial_pass
}

if [[ "$serial_only" == "true" ]]; then
    echo "Running E2E tests (serial pass only, -m serial)"
    run_serial_pass
elif [[ "$E2E_PARALLEL_WORKERS" -le 1 ]]; then
    # Single worker: still split by marker so module-scoped worker fixtures never
    # see both serial and parallel tests from the same file in one session.
    echo "Running E2E pass 1/2: non-serial (E2E_PARALLEL_WORKERS=${E2E_PARALLEL_WORKERS}, -m 'not serial')"
    if ! run_pytest_pass "pass 1 (non-serial)" \
        env E2E_PYTEST_PASS=parallel PYTHONPATH="$TEST_DIR:${PYTHONPATH:-}" pytest \
        --maxfail=5 \
        --junitxml="$xml" \
        --html="$html" --self-contained-html \
        "${pytest_common_args[@]}" \
        -m "not serial"; then
        parallel_rc=1
    fi
    snapshot_parallel_pass_pods || echo "WARNING: failed to snapshot pods after the parallel pass"
    maybe_run_serial_pass
else
    echo "Running E2E pass 1/2: parallel (E2E_PARALLEL_WORKERS=${E2E_PARALLEL_WORKERS}, --dist=loadgroup, -m 'not serial')"
    if ! run_pytest_pass "pass 1 (non-serial)" \
        env E2E_PYTEST_PASS=parallel PYTHONPATH="$TEST_DIR:${PYTHONPATH:-}" pytest \
        --maxfail=5 \
        -n "$E2E_PARALLEL_WORKERS" --dist=loadgroup \
        --junitxml="$xml" \
        --html="$html" --self-contained-html \
        "${pytest_common_args[@]}" \
        -m "not serial"; then
        parallel_rc=1
    fi
    snapshot_parallel_pass_pods || echo "WARNING: failed to snapshot pods after the parallel pass"
    maybe_run_serial_pass
fi

# ── Result ───────────────────────────────────────────────────────────────
if [[ "$any_pass_collected_tests" != "true" ]]; then
    echo "❌ ERROR: no tests collected in any pass"
    exit 1
fi
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
