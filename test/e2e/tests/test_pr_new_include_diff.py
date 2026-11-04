"""Validate smoke ∪ PR-new selection (E2E_INCLUDE_NEW_FROM_DIFF).

Runs on /group-test via @pytest.mark.smoke. Confirms the dynamic pr_new plugin
selects listed node ids, and that the prow runner exported the ids file when
include-new is enabled.
"""

from __future__ import annotations

import os
import subprocess
import sys
from pathlib import Path

import pytest

pytestmark = [pytest.mark.xdist_group("readonly"), pytest.mark.smoke]

# Known non-smoke test: must be selectable via pr_new without being curated smoke.
_NON_SMOKE_NODE_ID = (
    "tests/test_multi_tenant_integration.py::"
    "TestMultiTenantIntegration::test_full_tenant_lifecycle_create_to_delete"
)


def test_include_new_from_diff_pr_new_selection(tmp_path: Path) -> None:
    """pr_new must select listed ids; group-test must export E2E_NEW_TEST_IDS_FILE."""
    e2e_root = Path(__file__).resolve().parents[1]
    ids_file = tmp_path / "synthetic-new-e2e-ids.txt"
    ids_file.write_text(_NON_SMOKE_NODE_ID + "\n", encoding="utf-8")

    env = os.environ.copy()
    env["E2E_NEW_TEST_IDS_FILE"] = str(ids_file)
    # Avoid inheriting a parent xdist worker id into the nested collect.
    env.pop("PYTEST_XDIST_WORKER", None)
    env.pop("PYTEST_XDIST_WORKER_COUNT", None)

    proc = subprocess.run(
        [
            sys.executable,
            "-m",
            "pytest",
            "--collect-only",
            "-q",
            "tests/test_multi_tenant_integration.py",
            "-m",
            "pr_new",
        ],
        cwd=str(e2e_root),
        env=env,
        capture_output=True,
        text=True,
        timeout=120,
        check=False,
    )
    assert proc.returncode == 0, (
        f"nested collect failed (rc={proc.returncode})\n"
        f"stdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
    )
    assert "test_full_tenant_lifecycle_create_to_delete" in proc.stdout, proc.stdout
    assert "1/" in proc.stdout or "1 test collected" in proc.stdout, proc.stdout

    include = os.environ.get("E2E_INCLUDE_NEW_FROM_DIFF", "").lower()
    if include in ("1", "true", "yes"):
        runner_ids = os.environ.get("E2E_NEW_TEST_IDS_FILE", "")
        assert runner_ids, (
            "E2E_INCLUDE_NEW_FROM_DIFF is set but E2E_NEW_TEST_IDS_FILE is missing; "
            "run_e2e_tests.sh should export it before pytest"
        )
        assert Path(runner_ids).is_file(), runner_ids
