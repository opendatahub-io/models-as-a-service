"""Guards for the pytest/xdist configuration the parallel pass depends on.

The parallel pass runs with ``--dist=loadgroup``: every test carries an
``xdist_group`` marker (enforced in ``conftest.pytest_collection_modifyitems``)
and xdist pins all tests of a group to one worker so the group's expensive
session/module fixtures are set up once instead of once per worker.

pytest 9.2.0 regressed this silently: the ``@<group>`` nodeid suffix is no
longer appended, so ``loadgroup`` degrades to plain ``load`` and fixtures with
cluster-wide side effects (``shared_test_tenants``, the worker tenant bootstrap)
are paid for on every worker. The pass stayed green, just ~2x slower and far
more contended. These tests make that regression loud.
"""

import os

import pytest

pytestmark = pytest.mark.xdist_group("readonly")


def test_pytest_version_supports_loadgroup() -> None:
    """pytest >= 9.2.0 drops the @group nodeid suffix and breaks loadgroup."""
    import pytest as _pytest

    major, minor = (int(part) for part in _pytest.__version__.split(".")[:2])
    assert (major, minor) < (9, 2), (
        f"pytest {_pytest.__version__} breaks --dist=loadgroup "
        "(the @group nodeid suffix is no longer appended). "
        "Pin pytest<9.2 in test/e2e/requirements.txt and install "
        "test/e2e/requirements-lock.txt in CI."
    )


def test_xdist_group_suffix_present(request) -> None:
    """Under a group scheduler, grouped tests must carry the @<group> nodeid suffix."""
    if not os.environ.get("PYTEST_XDIST_WORKER"):
        pytest.skip("only meaningful under pytest-xdist")

    assert "@" in request.node.nodeid, (
        "nodeid has no @<group> suffix, so pytest-xdist is not honouring "
        f"xdist_group: {request.node.nodeid}. Either the pass is running with "
        "--dist=load instead of --dist=loadgroup, or pytest >= 9.2.0 is "
        "installed (loadgroup regression); pytest<9.2 is required."
    )
