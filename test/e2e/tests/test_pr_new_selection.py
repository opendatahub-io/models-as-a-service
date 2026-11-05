"""Report in JUnit why /group-test dropped the PR's new tests.

When run_e2e_tests.sh can't resolve the PR base or list the new tests, it falls
back to smoke-only and fails the run after all passes. This test fails with the
runner's selection log so the cause is in the report, not only in the console.
The runner exports E2E_PR_NEW_LOG whenever it attempts pr_new selection, and
E2E_NEW_TEST_IDS_FILE only when selection succeeded.
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

pytestmark = [pytest.mark.xdist_group("readonly"), pytest.mark.smoke]


def test_runner_selected_pr_new_tests():
    """Given the runner attempted pr_new selection (E2E_PR_NEW_LOG set),
    when it did not export E2E_NEW_TEST_IDS_FILE,
    then fail with the runner's selection log so the cause is in the report.
    """
    log_path = os.environ.get("E2E_PR_NEW_LOG", "")
    if not log_path:
        pytest.skip("run_e2e_tests.sh did not attempt pr_new selection")

    ids_file = os.environ.get("E2E_NEW_TEST_IDS_FILE", "")
    if ids_file and Path(ids_file).is_file():
        return

    try:
        detail = Path(log_path).read_text(encoding="utf-8").strip() or "(empty)"
    except OSError as exc:
        detail = f"(unreadable: {exc})"
    pytest.fail(
        "E2E_INCLUDE_NEW_FROM_DIFF=true but run_e2e_tests.sh did not export "
        "E2E_NEW_TEST_IDS_FILE, so tests added in this PR were not selected.\n"
        f"{log_path}:\n{detail}",
        pytrace=False,
    )
