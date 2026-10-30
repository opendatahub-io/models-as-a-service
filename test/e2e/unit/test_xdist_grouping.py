"""Exercise real workers: collection alone cannot verify xdist group affinity."""


def test_group_affinity_keeps_fixtures_shared_and_groups_parallel(pytester):
    template = '''
        import os
        from pathlib import Path
        import pytest

        pytestmark = pytest.mark.xdist_group({group!r})

        @pytest.fixture(scope="module")
        def tenant():
            worker = os.environ["PYTEST_XDIST_WORKER"]
            Path(__file__).with_name("{prefix}-" + worker + ".txt").write_text(worker)
            return worker

        @pytest.mark.parametrize("case", range(8))
        def test_tenant_request(tenant, case):
            assert tenant
    '''
    pytester.makepyfile(
        test_first=template.format(group="first-tenant", prefix="first"),
        test_second=template.format(group="second-tenant", prefix="second"),
    )
    result = pytester.runpytest_subprocess("-n", "2", "--dist=loadgroup", "-q", timeout=45)
    result.assert_outcomes(passed=16)
    first = list(pytester.path.glob("first-gw*.txt"))
    second = list(pytester.path.glob("second-gw*.txt"))
    assert len(first) == len(second) == 1, (
        "loadgroup split a tenant fixture across workers: "
        f"first={[path.name for path in first]}, second={[path.name for path in second]}"
    )
    assert first[0].read_text() != second[0].read_text(), (
        "independent tenant groups must retain parallel execution"
    )
