"""Dynamically mark PR-new tests so smoke-only can select (smoke or pr_new).

Must run as a hookwrapper tryfirst so markers exist before pytest's markexpr
filter (also tryfirst). A second pytest_collection_modifyitems in conftest.py
would overwrite this if defined in the same module.
"""

from __future__ import annotations

import logging
import os

import pytest

log = logging.getLogger(__name__)


def _load_pr_new_test_ids() -> frozenset[str]:
    path = os.environ.get("E2E_NEW_TEST_IDS_FILE", "").strip()
    if not path:
        return frozenset()
    try:
        with open(path, encoding="utf-8") as fh:
            return frozenset(
                line.strip() for line in fh if line.strip() and not line.startswith("#")
            )
    except OSError as exc:
        log.warning("E2E_NEW_TEST_IDS_FILE=%s unreadable: %s", path, exc)
        return frozenset()


@pytest.hookimpl(hookwrapper=True, tryfirst=True)
def pytest_collection_modifyitems(config, items):  # noqa: ARG001
    new_ids = _load_pr_new_test_ids()
    if new_ids:
        marker = pytest.mark.pr_new
        matched = 0
        for item in items:
            node_id = item.nodeid.split("[", 1)[0]
            if node_id in new_ids:
                item.add_marker(marker)
                matched += 1
        if matched:
            log.info(
                "Marked %d collected item(s) with pr_new from %s",
                matched,
                os.environ.get("E2E_NEW_TEST_IDS_FILE"),
            )
    yield
