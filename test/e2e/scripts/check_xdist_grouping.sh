#!/usr/bin/env bash
# Fail fast when the installed pytest/pytest-xdist pair stops honouring
# `--dist=loadgroup`.
#
# Every module in test/e2e/tests declares an xdist_group, and pass 1 relies on
# xdist keeping a group on one worker: groups own shared cluster state (the
# gateway AuthPolicy, subscriptions, tenant fixtures). When grouping breaks,
# those tests run concurrently on several workers and the cluster churns under
# them — the symptom is a flaky gate, not an obvious failure.
#
# Regression this guards (pytest 9.2.0): Item._nodeid was replaced by the
# immutable NodeId, so xdist 3.8.0's `item._nodeid = "<nodeid>@<group>"` became
# a no-op. That `@<group>` nodeid suffix is the only channel
# LoadGroupScheduling uses, so grouping silently degrades to plain per-test
# `--dist=load`. See the pytest cap in test/e2e/requirements.txt.
#
# Usage: check_xdist_grouping.sh [python]   (default: the active `python`)
set -uo pipefail

PYTHON="${1:-python}"
WORKERS="${XDIST_GROUPING_CHECK_WORKERS:-4}"
TESTS="${XDIST_GROUPING_CHECK_TESTS:-8}"

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT

cat >"$tmpdir/test_grouping_probe.py" <<'PY'
import os

import pytest

pytestmark = pytest.mark.xdist_group("grouping_probe")

LOG = os.environ["GROUPING_PROBE_LOG"]
COUNT = int(os.environ["GROUPING_PROBE_TEST_COUNT"])


def _record(line):
    with open(LOG, "a", encoding="utf-8") as fh:
        fh.write(line + "\n")


def _worker():
    return os.environ.get("PYTEST_XDIST_WORKER", "master")


@pytest.fixture(scope="class")
def group_state():
    """Class-scoped: set up once per worker that receives part of the group."""
    _record("setup:" + _worker())
    return None


class TestGroupState:
    @pytest.mark.parametrize("index", range(COUNT))
    def test_group_member(self, group_state, index):
        _record("run:" + _worker())
PY

log="$tmpdir/probe.log"
: >"$log"

(
    cd "$tmpdir" || exit 1
    GROUPING_PROBE_LOG="$log" GROUPING_PROBE_TEST_COUNT="$TESTS" \
        "$PYTHON" -m pytest test_grouping_probe.py \
        -n "$WORKERS" --dist=loadgroup -q -p no:cacheprovider >"$tmpdir/pytest.out" 2>&1
)
rc=$?

setups="$(grep -c '^setup:' "$log" 2>/dev/null || true)"
workers="$(grep '^run:' "$log" 2>/dev/null | cut -d: -f2 | sort -u | paste -sd, -)"

if [[ $rc -ne 0 ]]; then
    echo "FAIL: grouping probe pytest run exited $rc" >&2
    sed -e 's/^/      | /' "$tmpdir/pytest.out" | tail -n 20 >&2
    exit 1
fi

if [[ "${setups:-0}" -ne 1 || "$workers" == *,* ]]; then
    echo "FAIL: --dist=loadgroup split one xdist_group across workers." >&2
    echo "      pytest: $("$PYTHON" -m pytest --version 2>&1 | head -1)" >&2
    echo "      xdist:  $("$PYTHON" -c 'import xdist; print(xdist.__version__)' 2>&1)" >&2
    echo "      class fixture setups: ${setups:-0} (expected 1)" >&2
    echo "      workers that ran the group: ${workers:-none} (expected 1)" >&2
    echo "      Grouping is broken; see this script and the pytest cap in" >&2
    echo "      test/e2e/requirements.txt." >&2
    sed -e 's/^/      | /' "$tmpdir/pytest.out" | tail -n 20 >&2
    exit 1
fi

echo "OK: --dist=loadgroup keeps an xdist_group on one worker (${workers})"
