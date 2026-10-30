"""Failure evidence is bounded per pytest worker, before fixture teardown."""

import importlib.util
from pathlib import Path
from types import SimpleNamespace

import external_model_helpers as external
import pytest

spec = importlib.util.spec_from_file_location("e2e_failure_hooks", Path(__file__).parents[1] / "tests" / "conftest.py")
e2e_config = importlib.util.module_from_spec(spec)
spec.loader.exec_module(e2e_config)


def report_failure(config, nodeid, when="call", failed=True):
    item = SimpleNamespace(config=config, nodeid=nodeid)
    report = SimpleNamespace(failed=failed, when=when)
    hook = e2e_config.pytest_runtest_makereport(item, None)
    next(hook)
    with pytest.raises(StopIteration):
        hook.send(SimpleNamespace(get_result=lambda: report))


def test_failure_snapshots_are_capped_and_deduplicated_per_worker(monkeypatch):
    captures = []
    monkeypatch.setattr(external, "capture_external_processor_failure", captures.append)
    worker = SimpleNamespace(stash=pytest.Stash())
    first = "tests/test_external_model_routing.py::test_first"
    second = "tests/test_tenant_external_model.py::test_second"
    report_failure(worker, "tests/test_other.py::test_unrelated")
    report_failure(worker, first, failed=False)
    report_failure(worker, first, when="setup")
    report_failure(worker, first)
    report_failure(worker, second)
    report_failure(worker, "tests/test_external_model_routing.py::test_third")
    assert captures == [first, second]
    report_failure(SimpleNamespace(stash=pytest.Stash()), first)
    assert captures == [first, second, first]
