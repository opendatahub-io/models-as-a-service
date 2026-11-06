"""Canary for /group-test pr_new selection.

A full_suite test added in this PR. Smoke-only /group-test runs it only if the
runner listed it as new and pr_new_selection.py marked it, so its presence in
the report is the evidence that selection works. Remove before merge: once on
main it is no longer new.
"""

from __future__ import annotations

import os
from pathlib import Path

import pytest

pytestmark = [pytest.mark.xdist_group("readonly"), pytest.mark.full_suite]


def test_pr_new_canary_selected(request):
    """Given a full_suite test added in this PR,
    when /group-test runs smoke-only with E2E_INCLUDE_NEW_FROM_DIFF=true,
    then it runs only through pr_new, with its node id in the runner's list.
    """
    ids_file = os.environ.get("E2E_NEW_TEST_IDS_FILE", "")
    if not ids_file:
        pytest.skip("not a pr_new run")
    ids = Path(ids_file).read_text(encoding="utf-8").split()
    assert request.node.nodeid in ids
