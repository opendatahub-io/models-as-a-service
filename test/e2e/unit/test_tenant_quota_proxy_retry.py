"""Quota exhaustion requires serving then denial, within one retry deadline."""

from types import SimpleNamespace

import pytest
import requests
import test_helper as helper
import test_tenant_rate_limit_isolation as isolation


@pytest.fixture
def quota_gateway(monkeypatch):
    clock = SimpleNamespace(now=1000.0)
    clock.time = lambda: clock.now
    clock.sleep = lambda delay: setattr(clock, "now", clock.now + delay)
    monkeypatch.setattr(isolation, "time", clock)
    monkeypatch.setattr(helper, "time", clock)
    monkeypatch.setattr(helper, "GATEWAY_PROPAGATION_RETRIES", 3)
    monkeypatch.setattr(helper, "GATEWAY_PROPAGATION_DELAY", 0)
    def shared_gateway_wait(**kwargs):
        raise AssertionError("a tenant-local probe must not wait on the shared default gateway")

    monkeypatch.setattr(helper, "_wait_for_gateway_auth_enforced", shared_gateway_wait)
    monkeypatch.setattr(helper, "_record_transient_retry", lambda *args: None)
    attempts, deadlines = [], []

    def retry(*args, **kwargs):
        deadlines.append(kwargs["deadline"])
        return helper._request_with_gateway_retry(*args, **kwargs)

    monkeypatch.setattr(isolation, "_request_with_gateway_retry", retry)

    def install(replies):
        def post(url, **kwargs):
            attempts.append((url, kwargs["json"]))
            clock.now += 0.1
            status, body = replies[min(len(attempts) - 1, len(replies) - 1)]
            result = requests.Response()
            result.status_code, result._content = status, body.encode()
            return result

        monkeypatch.setattr(isolation.requests, "post", post)
        return attempts, deadlines

    return install


def case():
    return {
        "tenant_a": {"base_url": "https://tenant-a.example/maas-api", "model_path": "/models/test", "backend_model_name": "test"},
        "key_a": "synthetic-quota-key",
    }


@pytest.mark.parametrize("replies", [
    [(500, "Internal Server Error."), (200, "ok"), (429, "quota")],
    [(429, "default deny"), (503, "Service Unavailable"), (200, "ok"), (429, "quota")],
    [(200, "ok"), (500, "Internal Server Error."), (429, "quota")],
])
def test_proxy_reload_does_not_abort_real_quota_exhaustion(quota_gateway, replies):
    attempts, deadlines = quota_gateway(replies)
    isolation.TestTenantRateLimitIsolation().test_rate_limit_enforced_per_tenant(case())
    assert len(attempts) == len(replies)
    assert deadlines and set(deadlines) == {1045.0}, "proxy retries must share the original quota deadline"
    assert all(payload["max_tokens"] == 1 for _, payload in attempts)


@pytest.mark.parametrize("status,body,attempts_expected", [
    (500, "Internal Server Error.", 3),
    (500, '{"error":"backend failed"}', 1),
    (403, "denied", 1),
    (403, "", 1),
    (429, "default deny", None),
    (200, "limiter never enforces", None),
])
def test_proxy_handling_cannot_make_broken_quota_enforcement_pass(quota_gateway, status, body, attempts_expected):
    attempts, deadlines = quota_gateway([(status, body)])
    with pytest.raises(AssertionError):
        isolation.TestTenantRateLimitIsolation().test_rate_limit_enforced_per_tenant(case())
    if attempts_expected is not None:
        assert len(attempts) == attempts_expected
    assert len(attempts) <= 45
    assert set(deadlines) == {1045.0}


def test_default_gateway_retry_retains_its_enforcement_wait(quota_gateway, monkeypatch):
    quota_gateway([(500, "Internal Server Error."), (200, "ok")])
    waits = []
    monkeypatch.setattr(helper, "_wait_for_gateway_auth_enforced", lambda **kwargs: waits.append(kwargs))
    result = helper._request_with_gateway_retry(requests.post, "https://default.example", json={"model": "test"})
    assert result.status_code == 200
    assert len(waits) == 1
