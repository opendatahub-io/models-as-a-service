"""A transient proxy error may be retried; unauthorized serving must fail."""

import pytest
import requests
import test_helper as helper
import test_tenant_model_inference as inference


@pytest.mark.parametrize("statuses", [(500, 403), (403,), (500, 200), (500, 500)])
def test_invalid_key_requires_a_real_denial_after_proxy_retry(monkeypatch, statuses):
    responses = []
    for status in statuses:
        response = requests.Response()
        response.status_code = status
        response._content = b"Internal Server Error." if status == 500 else b""
        if status == 403:
            response.headers["x-ext-auth-reason"] = "Unauthorized"
        responses.append(response)
    attempts = []

    def post(*args, **kwargs):
        attempts.append(True)
        return responses[len(attempts) - 1]

    monkeypatch.setattr(inference.requests, "post", post)
    monkeypatch.setattr(inference, "_get_tenant_gateway_url", lambda _: "https://tenant.example")
    monkeypatch.setattr(helper, "_wait_for_gateway_auth_enforced", lambda **kwargs: None)
    monkeypatch.setattr(helper, "_record_transient_retry", lambda *args: None)
    monkeypatch.setattr(
        inference, "_request_with_gateway_retry",
        lambda *args, **kwargs: helper._request_with_gateway_retry(*args, retries=2, delay=0, **kwargs),
    )
    cases = [{"gateway_name": "tenant", "model_path": "/models/test/v1"}]
    test = inference.TestTenantGatewayNegativeSecurity()
    if statuses[-1] == 403:
        test.test_invalid_api_key_denied_on_tenant_gateway(cases, 0)
    else:
        with pytest.raises(AssertionError, match="Expected 401/403"):
            test.test_invalid_api_key_denied_on_tenant_gateway(cases, 0)
    assert len(attempts) == len(statuses)
