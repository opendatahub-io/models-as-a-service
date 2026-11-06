#!/usr/bin/env bash
# =============================================================================
# E2E Test Runner — pytest execution only
# =============================================================================
#
# Owns pytest invocation for MaaS E2E tests. Separated from
# prow_run_smoke_test.sh so that test-only changes don't touch deploy/validate
# logic. Implements the three-phase model:
#   Pass 1: parallel with pytest-xdist (-m "not serial and not serial_exclusive and
#           not legacy_ipp", --dist=loadgroup)
#   Pass 2a: serial_exclusive cluster-wide tests (-m "serial_exclusive …", single worker)
#   Pass 2b: serial cluster mutators and per-tenant legacy IPP (-m "(serial and not legacy_ipp)
#           or legacy_ipp_pass2b) and not serial_exclusive …",
#            E2E_SERIAL_WORKERS lanes, default 1)
#   Pass 3 (optional): -m nightly when MAAS_PYTEST_INCLUDE_NIGHTLY=true (e.g. EXTERNAL_OIDC deploys)
#
# Passes 1–2 always exclude @pytest.mark.nightly; the flag below only gates pass 3.
# When E2E_SMOKE_ONLY=true (/group-test sets this), passes 1–2 additionally require
# @pytest.mark.smoke so PR CI runs a low-churn subset instead of the full suite.
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
#   E2E_PARALLEL_WORKERS          pytest-xdist worker count (default: 5)
#   E2E_SERIAL_WORKERS            pytest-xdist worker count for serial pass 2b
#                                 (default: 1). Lanes split between files only, and
#                                 files that mutate the same shared model share one
#                                 group, so they can never land on two lanes.
#   E2E_MAXFAIL                   stop a pass after N failures (default: 5, 0 = never).
#                                 CI sets 0: on a shared control plane, five failures used
#                                 to truncate the pass, undercounting failures and leaving
#                                 the flake report covering only the part that ran.
#   E2E_AUTHPOLICY_PHASE_TIMEOUT  seconds (default: 120, parallel only)
#   E2E_MAAS_SUBSCRIPTION_PHASE_TIMEOUT  seconds (default: 120, parallel only)
#   E2E_GATEWAY_ENFORCED_TIMEOUT  seconds (default: 240, parallel only)
#   E2E_MULTITENANCY_PHASE_TIMEOUT seconds (default: 180, parallel only)
#   E2E_RECONCILE_WAIT            seconds between reconcile polls (default: 4)
#   ARTIFACTS_DIR                 output directory for JUnit/HTML (default: test/e2e/reports)
#   E2E_SMOKE_ONLY                When true, restrict passes 1–2 to @pytest.mark.smoke
#                                 (/group-test sets true; unset/false = full suite)
#   E2E_INCLUDE_NEW_FROM_DIFF     When true with E2E_SMOKE_ONLY, also select tests added
#                                 since E2E_DIFF_BASE (dynamic pr_new marker; no source edit)
#   E2E_DIFF_BASE                 Git ref/SHA for the PR base (or PULL_BASE_SHA / BASE_SHA)
#   E2E_DIFF_UPSTREAM_URL         Canonical upstream clone URL (not the PR fork). /group-test
#                                 sets this so merge-base does not use a stale fork origin/main.
#   E2E_DIFF_TARGET_BRANCH        Upstream base branch name (Pac {{target_branch}}, default main)
#   MAAS_PYTEST_INCLUDE_NIGHTLY   When true, run pass 3 (-m nightly) after serial pass
#   E2E_RUN_ONLY_OIDC_TESTS       When true, skip passes 1–2 and run only pass 3 (external
#                                 OIDC tests); requires EXTERNAL_OIDC=true
#   E2E_RUN_ONLY_GRID_TESTS       When true, restrict the OIDC-only pass to grid_oidc tests;
#                                 requires E2E_RUN_ONLY_OIDC_TESTS=true
#   E2E_LIMITADOR_TRIAGE          When true, run only the Limitador sync subset (-m limitador)
#                                 plus the serial pass; the broad parallel pass is skipped
#   E2E_LIMITADOR_WORKERS         xdist worker count for the Limitador triage pass
#                                 (default: 1; raise it to reproduce load-dependent stalls)
#
# Usage:
#   export GATEWAY_HOST=maas.apps.cluster.example.com TOKEN=$(oc whoami -t) ...
#   ./run_e2e_tests.sh                       # run all tests (unless E2E_SMOKE_ONLY=true)
#   ./run_e2e_tests.sh --smoke-only          # only @pytest.mark.smoke
#   ./run_e2e_tests.sh -- -k test_api_keys   # pass extra pytest args
#   ./run_e2e_tests.sh --serial-only         # skip parallel pass
#   ./run_e2e_tests.sh --limitador-triage    # only -m limitador + serial pass
# =============================================================================

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
TEST_DIR="$PROJECT_ROOT/test/e2e"

# ── Parse script flags (before --) vs pytest args (after --) ─────────────
serial_only=false
limitador_triage=false
smoke_only=false
case "${E2E_LIMITADOR_TRIAGE:-false}" in
    1 | true | TRUE | yes | YES) limitador_triage=true ;;
esac
case "${E2E_SMOKE_ONLY:-false}" in
    1 | true | TRUE | yes | YES) smoke_only=true ;;
esac
extra_pytest_args=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        --serial-only) serial_only=true; shift ;;
        --limitador-triage) limitador_triage=true; shift ;;
        --smoke-only) smoke_only=true; shift ;;
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
E2E_PARALLEL_WORKERS="${E2E_PARALLEL_WORKERS:-5}"
if ! [[ "$E2E_PARALLEL_WORKERS" =~ ^[1-9][0-9]*$ ]]; then
    echo "ERROR: E2E_PARALLEL_WORKERS must be a positive integer (>= 1), got '$E2E_PARALLEL_WORKERS'" >&2
    exit 1
fi

ARTIFACTS_DIR="${ARTIFACTS_DIR:-${ARTIFACT_DIR:-${ARTIFACTS:-${LOG_DIR:-$TEST_DIR/reports}}}}"
mkdir -p "$ARTIFACTS_DIR"

# ── Phase timeouts (parallel only) ───────────────────────────────────────
if [[ "$E2E_PARALLEL_WORKERS" -gt 1 ]]; then
    export E2E_AUTHPOLICY_PHASE_TIMEOUT="${E2E_AUTHPOLICY_PHASE_TIMEOUT:-120}"
    export E2E_MAAS_SUBSCRIPTION_PHASE_TIMEOUT="${E2E_MAAS_SUBSCRIPTION_PHASE_TIMEOUT:-120}"
    export E2E_GATEWAY_ENFORCED_TIMEOUT="${E2E_GATEWAY_ENFORCED_TIMEOUT:-240}"
    export E2E_MULTITENANCY_PHASE_TIMEOUT="${E2E_MULTITENANCY_PHASE_TIMEOUT:-180}"
fi

# ── Venv ─────────────────────────────────────────────────────────────────
# shellcheck source=ensure_e2e_python.sh
source "$SCRIPT_DIR/ensure_e2e_python.sh"
if [[ -d "$TEST_DIR/.venv" ]] && ! "$TEST_DIR/.venv/bin/python" -c 'import sys; raise SystemExit(0 if sys.version_info >= (3, 10) else 1)' 2>/dev/null; then
    echo "Recreating e2e venv (existing interpreter is older than Python 3.10)..."
    rm -rf "$TEST_DIR/.venv"
fi
if [[ ! -d "$TEST_DIR/.venv" ]]; then
    echo "Creating Python venv for e2e tests ($E2E_PYTHON)..."
    "$E2E_PYTHON" -m venv "$TEST_DIR/.venv" --upgrade-deps
fi
# shellcheck disable=SC1091
source "$TEST_DIR/.venv/bin/activate"
python -m pip install --upgrade pip --quiet
# Prefer the hash-pinned lock so CI installs exactly the reviewed set. Lock is
# compiled for Python 3.10+ (includes exceptiongroup/tomli on 3.10). Installing
# requirements.txt alone let pytest drift to 9.2.0, which silently disables
# --dist=loadgroup (see test/e2e/tests/test_xdist_config.py).
if [[ -f "$TEST_DIR/requirements-lock.txt" ]]; then
    python -m pip install --require-hashes --no-deps -r "$TEST_DIR/requirements-lock.txt" --quiet
else
    python -m pip install -r "$TEST_DIR/requirements.txt" --quiet
fi
python - <<'PY'
import sys
import pytest

major, minor = (int(x) for x in pytest.__version__.split(".")[:2])
if (major, minor) >= (9, 2):
    sys.exit(
        f"ERROR: pytest {pytest.__version__} breaks --dist=loadgroup "
        "(the @group nodeid suffix is no longer appended). Install pytest<9.2, "
        "e.g. from test/e2e/requirements-lock.txt."
    )
PY

# ── Output paths ─────────────────────────────────────────────────────────
user="$(oc whoami 2>/dev/null || echo 'unknown')"
html="$ARTIFACTS_DIR/e2e-${user}.html"
xml="$ARTIFACTS_DIR/e2e-${user}.xml"
xml_serial="${xml%.xml}-serial.xml"
xml_serial_exclusive="${xml%.xml}-serial-exclusive.xml"
xml_nightly="${xml%.xml}-nightly.xml"
xml_limitador="${xml%.xml}-limitador.xml"

# ── Marker tiers ─────────────────────────────────────────────────────────
# Nightly tests always run in pass 3 only (when enabled), never in passes 1–2.
_nightly_exclude=" and not nightly"
_include_nightly_pass=false
case "${MAAS_PYTEST_INCLUDE_NIGHTLY:-false}" in
    1 | true | TRUE | yes | YES) _include_nightly_pass=true ;;
esac
# E2E_RUN_ONLY_OIDC_TESTS skips passes 1–2 and runs only pass 3, which holds the
# external OIDC tests. The dedicated OIDC CI job uses it because /group-test
# already covers passes 1–2.
_run_only_oidc_tests=false
case "${E2E_RUN_ONLY_OIDC_TESTS:-false}" in
    1 | true | TRUE | yes | YES) _run_only_oidc_tests=true ;;
esac
_run_only_grid_tests=false
case "${E2E_RUN_ONLY_GRID_TESTS:-false}" in
    1 | true | TRUE | yes | YES) _run_only_grid_tests=true ;;
esac
if [[ "$_run_only_oidc_tests" == true ]]; then
    if [[ "${EXTERNAL_OIDC:-false}" != "true" ]]; then
        echo "ERROR: E2E_RUN_ONLY_OIDC_TESTS=true requires EXTERNAL_OIDC=true; the OIDC tests skip without it" >&2
        exit 1
    fi
    if [[ "$serial_only" == "true" ]]; then
        echo "ERROR: E2E_RUN_ONLY_OIDC_TESTS=true cannot be combined with --serial-only" >&2
        exit 1
    fi
    if [[ "$limitador_triage" == "true" ]]; then
        echo "ERROR: E2E_RUN_ONLY_OIDC_TESTS=true cannot be combined with --limitador-triage" >&2
        exit 1
    fi
    _include_nightly_pass=true
fi
_nightly_marker="nightly"
if [[ "$_run_only_grid_tests" == true ]]; then
    if [[ "$_run_only_oidc_tests" != true ]]; then
        echo "ERROR: E2E_RUN_ONLY_GRID_TESTS=true requires E2E_RUN_ONLY_OIDC_TESTS=true" >&2
        exit 1
    fi
    _nightly_marker="nightly and grid_oidc"
fi
if [[ "$limitador_triage" == "true" && "$serial_only" == "true" ]]; then
    echo "ERROR: --limitador-triage already runs the serial pass; drop --serial-only" >&2
    exit 1
fi
if [[ "$smoke_only" == "true" && "$limitador_triage" == "true" ]]; then
    echo "ERROR: --smoke-only / E2E_SMOKE_ONLY cannot be combined with --limitador-triage" >&2
    exit 1
fi
# OIDC-only jobs skip passes 1–2 entirely; smoke filtering does not apply there.
_smoke_require=""
_include_new_from_diff=false
case "${E2E_INCLUDE_NEW_FROM_DIFF:-false}" in
    1 | true | TRUE | yes | YES) _include_new_from_diff=true ;;
esac

# Resolve the PR-base ref for dynamic pr_new selection.
# Prefer an explicit SHA, then fetch the *upstream* target branch (not the fork's
# origin/main — /group-test clones source-repo-url at --depth 50). Never let
# merge-base failure abort under set -e; callers fall back to smoke-only.
_resolve_e2e_diff_base() {
    local base="${E2E_DIFF_BASE:-${PULL_BASE_SHA:-${BASE_SHA:-}}}"
    if [[ -n "${base}" ]]; then
        printf '%s\n' "${base}"
        return 0
    fi
    local upstream_url="${E2E_DIFF_UPSTREAM_URL:-}"
    local target_branch="${E2E_DIFF_TARGET_BRANCH:-main}"
    local mb=""
    if [[ -n "${upstream_url}" ]]; then
        # When CI configures an upstream URL, do not fall back to origin/* —
        # that may be a stale fork tip and would mis-classify upstream tests
        # as PR-new. Fail closed so the caller uses smoke-only.
        local ref="refs/e2e-diff-base/${target_branch}"
        if git -C "$PROJECT_ROOT" fetch --depth=50 "${upstream_url}" \
            "+refs/heads/${target_branch}:${ref}" >/dev/null 2>&1; then
            mb="$(git -C "$PROJECT_ROOT" merge-base HEAD "${ref}" 2>/dev/null || true)"
            if [[ -n "${mb}" ]]; then
                printf '%s\n' "${mb}"
                return 0
            fi
        fi
        return 1
    fi
    local cand
    for cand in "origin/${target_branch}" "origin/main" main; do
        if git -C "$PROJECT_ROOT" rev-parse --verify "${cand}" >/dev/null 2>&1; then
            mb="$(git -C "$PROJECT_ROOT" merge-base HEAD "${cand}" 2>/dev/null || true)"
            if [[ -n "${mb}" ]]; then
                printf '%s\n' "${mb}"
                return 0
            fi
        fi
    done
    return 1
}

if [[ "$smoke_only" == "true" && "$_run_only_oidc_tests" != true ]]; then
    if [[ "$_include_new_from_diff" == true ]]; then
        _diff_base=""
        if ! _diff_base="$(_resolve_e2e_diff_base)"; then
            _diff_base=""
        fi
        if [[ -z "${_diff_base}" ]]; then
            echo "WARNING: E2E_INCLUDE_NEW_FROM_DIFF=true but could not resolve a PR base" \
                "(set E2E_DIFF_BASE / PULL_BASE_SHA / BASE_SHA, or E2E_DIFF_UPSTREAM_URL +" \
                "E2E_DIFF_TARGET_BRANCH); smoke-only without pr_new" >&2
            _smoke_require="smoke and "
            echo "E2E smoke-only mode: restricting passes 1–2 to @pytest.mark.smoke"
        else
            _new_ids_file="${ARTIFACTS_DIR}/new-e2e-test-ids.txt"
            if ! python "$SCRIPT_DIR/check_new_e2e_test_tiers.py" --base "${_diff_base}" --list-new \
                >"${_new_ids_file}"; then
                echo "WARNING: failed to list new E2E tests against ${_diff_base};" \
                    "falling back to smoke-only without pr_new" >&2
                : >"${_new_ids_file}"
                _smoke_require="smoke and "
                echo "E2E smoke-only mode: restricting passes 1–2 to @pytest.mark.smoke"
            else
                _new_count="$(grep -cve '^[[:space:]]*$' "${_new_ids_file}" || true)"
                export E2E_NEW_TEST_IDS_FILE="${_new_ids_file}"
                _smoke_require="(smoke or pr_new) and "
                echo "E2E smoke-only + PR-new: -m '(smoke or pr_new) …' (base=${_diff_base}, new_tests=${_new_count}, ids=${_new_ids_file})"
            fi
        fi
    else
        _smoke_require="smoke and "
        echo "E2E smoke-only mode: restricting passes 1–2 to @pytest.mark.smoke"
    fi
fi
_parallel_marker="${_smoke_require}not serial and not serial_exclusive and not legacy_ipp and not legacy_ipp_pass2b${_nightly_exclude}"
# Serial tests run in two passes. serial_exclusive tests scale a control plane
# down or delete a cluster-scoped CRD, so they run alone (pass 2a). Everything
# else is safe beside another lane provided files that mutate the same shared
# model share one xdist group (pass 2b).
_serial_exclusive_marker="${_smoke_require}serial_exclusive and not legacy_ipp${_nightly_exclude}"
_serial_marker="${_smoke_require}((serial and not legacy_ipp) or legacy_ipp_pass2b) and not serial_exclusive${_nightly_exclude}"
# The Limitador sync subset: tests that wait on a TokenRateLimitPolicy reaching
# Enforced=True. Only they can see a stalled limiter, and because an unapplied
# token limit still admits every request, the rest of the suite passes either way.
# Already-serial limitador tests are left to the serial pass so nothing runs twice.
_limitador_marker="limitador and not serial and not serial_exclusive and not legacy_ipp${_nightly_exclude}"
_pass_total=2
if [[ "$_include_nightly_pass" == true ]]; then
    _pass_total=3
fi

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
nightly_rc=0
any_pass_collected_tests=false

count_pytest_collected() {
    local summary collected=0
    set +e
    summary=$( "$@" --collect-only -q 2>&1 | tail -1 )
    set -e
    # Pytest uses "test" for a single collected item and "tests" otherwise.
    if [[ "$summary" =~ ([0-9]+)/[0-9]+\ tests?\ collected ]]; then
        collected="${BASH_REMATCH[1]}"
    elif [[ "$summary" =~ ([0-9]+)\ tests?\ collected ]]; then
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

run_limitador_pass() {
    local workers="${E2E_LIMITADOR_WORKERS:-1}"
    if ! [[ "$workers" =~ ^[1-9][0-9]*$ ]]; then
        echo "ERROR: E2E_LIMITADOR_WORKERS must be a positive integer (>= 1), got '$workers'" >&2
        exit 1
    fi
    echo "Running Limitador triage pass: -m '${_limitador_marker}' with ${workers} worker(s)"
    # The limitador tests are non-serial and worker-tenant scoped, so they report
    # as the parallel pass even at a single worker; the serial pass that follows
    # is what E2E_PYTEST_PASS=serial exists for.
    local xdist_args=()
    if [[ "$workers" -gt 1 ]]; then
        xdist_args=(-n "$workers" --dist=loadgroup)
    fi
    if ! run_pytest_pass "limitador triage" \
        env E2E_PYTEST_PASS=parallel PYTHONPATH="$TEST_DIR:${PYTHONPATH:-}" pytest \
        --maxfail="${E2E_MAXFAIL:-5}" \
        ${xdist_args[@]+"${xdist_args[@]}"} \
        --junitxml="$xml_limitador" \
        --html="${html%.html}-limitador.html" --self-contained-html \
        "${pytest_common_args[@]}" \
        -m "${_limitador_marker}"; then
        parallel_rc=1
    fi
}

run_serial_exclusive_pass() {
    # Always one worker: these tests scale a control plane down or delete a
    # cluster-scoped CRD, so a second lane would observe (or cause) the outage.
    echo "Running E2E pass 2a: exclusive serial tests (-m '${_serial_exclusive_marker}', 1 worker)"
    if ! run_pytest_pass "pass 2a (serial_exclusive)" \
        env E2E_PYTEST_PASS=serial PYTHONPATH="$TEST_DIR:${PYTHONPATH:-}" pytest \
        --maxfail="${E2E_MAXFAIL:-5}" \
        --junitxml="$xml_serial_exclusive" \
        --html="${html%.html}-serial-exclusive.html" --self-contained-html \
        "${pytest_common_args[@]}" \
        -m "${_serial_exclusive_marker}"; then
        serial_rc=1
    fi
}

run_serial_pass() {
    local workers="${E2E_SERIAL_WORKERS:-1}"
    if ! [[ "$workers" =~ ^[1-9][0-9]*$ ]]; then
        echo "ERROR: E2E_SERIAL_WORKERS must be a positive integer (>= 1), got '$workers'" >&2
        exit 1
    fi
    # Serial tests keep one xdist group per file, so lanes only ever split
    # between files. Files that mutate the same shared model share one group and
    # therefore one lane; serial_exclusive tests never reach this pass.
    local xdist_args=()
    if [[ "$workers" -gt 1 ]]; then
        xdist_args=(-n "$workers" --dist=loadgroup)
    fi
    echo "Running E2E pass 2b: serial cluster mutators (-m '${_serial_marker}', ${workers} worker(s))"
    if ! run_pytest_pass "pass 2b (serial)" \
        env E2E_PYTEST_PASS=serial PYTHONPATH="$TEST_DIR:${PYTHONPATH:-}" pytest \
        --maxfail="${E2E_MAXFAIL:-5}" \
        ${xdist_args[@]+"${xdist_args[@]}"} \
        --junitxml="$xml_serial" \
        --html="${html%.html}-serial.html" --self-contained-html \
        "${pytest_common_args[@]}" \
        -m "${_serial_marker}"; then
        serial_rc=1
    fi
}

run_nightly_pass() {
    if [[ "$_include_nightly_pass" != true ]]; then
        return 0
    fi
    echo "Running E2E pass 3/${_pass_total}: nightly (-m ${_nightly_marker}, EXTERNAL_OIDC=${EXTERNAL_OIDC:-false})"
    if ! run_pytest_pass "pass 3 (nightly)" \
        env E2E_PYTEST_PASS=nightly PYTHONPATH="$TEST_DIR:${PYTHONPATH:-}" pytest \
        --maxfail="${E2E_MAXFAIL:-5}" \
        --junitxml="$xml_nightly" \
        --html="${html%.html}-nightly.html" --self-contained-html \
        "${pytest_common_args[@]}" \
        -m "${_nightly_marker}"; then
        nightly_rc=1
    fi
}

maybe_run_serial_pass() {
    # Always run the serial pass, even when the parallel pass failed. The serial
    # tests are the cluster mutators most sensitive to state left behind by
    # pass 1, and skipping them turns a parallel failure into a silent coverage
    # gap: the job reports only the parallel error and never learns whether
    # serial tests would also have failed. Both rc values are reported at exit.
    if [[ "$parallel_rc" -ne 0 ]]; then
        echo "E2E pass 1 (parallel) failed; still running the serial passes (2a/2b) to report their result independently"
    fi
    run_serial_exclusive_pass
    run_serial_pass
}

maybe_run_nightly_pass() {
    if [[ "$parallel_rc" -ne 0 || "$serial_rc" -ne 0 ]]; then
        echo "Skipping E2E pass 3/${_pass_total} (nightly): earlier pass failed"
        return 0
    fi
    run_nightly_pass
}

if [[ "$_run_only_oidc_tests" == "true" ]]; then
    echo "Running E2E tests (OIDC tests only, E2E_RUN_ONLY_OIDC_TESTS=true; passes 1–2 skipped)"
    run_nightly_pass
elif [[ "$limitador_triage" == "true" ]]; then
    # Deliberately unconditional: a triage run exists to observe these passes, so
    # neither one is allowed to be skipped because the other failed.
    echo "Running E2E tests (Limitador triage only: -m '${_limitador_marker}' then -m '${_serial_marker}'; the broad parallel pass is skipped)"
    run_limitador_pass
    snapshot_parallel_pass_pods || echo "WARNING: failed to snapshot pods after the triage pass"
    run_serial_exclusive_pass
    run_serial_pass
elif [[ "$serial_only" == "true" ]]; then
    echo "Running E2E tests (serial passes only: -m '${_serial_exclusive_marker}', then -m '${_serial_marker}')"
    run_serial_exclusive_pass
    run_serial_pass
elif [[ "$E2E_PARALLEL_WORKERS" -le 1 ]]; then
    # Single worker: still split by marker so module-scoped worker fixtures never
    # see both serial and parallel tests from the same file in one session.
    echo "Running E2E pass 1/${_pass_total}: non-serial (E2E_PARALLEL_WORKERS=${E2E_PARALLEL_WORKERS}, -m '${_parallel_marker}')"
    if ! run_pytest_pass "pass 1 (non-serial)" \
        env E2E_PYTEST_PASS=parallel PYTHONPATH="$TEST_DIR:${PYTHONPATH:-}" pytest \
        --maxfail="${E2E_MAXFAIL:-5}" \
        --junitxml="$xml" \
        --html="$html" --self-contained-html \
        "${pytest_common_args[@]}" \
        -m "${_parallel_marker}"; then
        parallel_rc=1
    fi
    snapshot_parallel_pass_pods || echo "WARNING: failed to snapshot pods after the parallel pass"
    maybe_run_serial_pass
    maybe_run_nightly_pass
else
    echo "Running E2E pass 1/${_pass_total}: parallel (E2E_PARALLEL_WORKERS=${E2E_PARALLEL_WORKERS}, --dist=loadgroup, -m '${_parallel_marker}')"
    if ! run_pytest_pass "pass 1 (non-serial)" \
        env E2E_PYTEST_PASS=parallel PYTHONPATH="$TEST_DIR:${PYTHONPATH:-}" pytest \
        --maxfail="${E2E_MAXFAIL:-5}" \
        -n "$E2E_PARALLEL_WORKERS" --dist=loadgroup \
        --junitxml="$xml" \
        --html="$html" --self-contained-html \
        "${pytest_common_args[@]}" \
        -m "${_parallel_marker}"; then
        parallel_rc=1
    fi
    snapshot_parallel_pass_pods || echo "WARNING: failed to snapshot pods after the parallel pass"
    maybe_run_serial_pass
    maybe_run_nightly_pass
fi

# ── Result ───────────────────────────────────────────────────────────────
if [[ "$any_pass_collected_tests" != "true" ]]; then
    echo "❌ ERROR: no tests collected in any pass"
    exit 1
fi
if [[ "$parallel_rc" -ne 0 || "$serial_rc" -ne 0 || "$nightly_rc" -ne 0 ]]; then
    echo "❌ ERROR: E2E tests failed (parallel_rc=${parallel_rc}, serial_rc=${serial_rc}, nightly_rc=${nightly_rc})"
    exit 1
fi

echo "✅ E2E tests completed"
echo " - JUnit XML : ${xml}"
if [[ -f "$xml_serial_exclusive" ]]; then
    echo " - JUnit XML (serial exclusive pass): ${xml_serial_exclusive}"
fi
if [[ -f "$xml_serial" ]]; then
    echo " - JUnit XML (serial pass): ${xml_serial}"
fi
if [[ -f "$xml_limitador" ]]; then
    echo " - JUnit XML (limitador triage): ${xml_limitador}"
fi
if [[ -f "$xml_nightly" ]]; then
    echo " - JUnit XML (nightly pass): ${xml_nightly}"
fi
echo " - HTML      : ${html}"
