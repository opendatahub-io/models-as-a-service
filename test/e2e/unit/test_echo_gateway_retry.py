"""Echo fidelity tolerates a proxy reload, not backend errors or altered data."""

import json

import pytest
import requests
import test_echo_large_io as echo
import test_helper as helper


def response(status, body):
    result = requests.Response()
    result.status_code = status
    result._content = body.encode()
    return result


@pytest.fixture
def gateway(monkeypatch):
    attempts = []
    monkeypatch.setattr(echo, "_gateway_url", lambda: "https://gateway.example.test")
    monkeypatch.setattr(helper, "GATEWAY_PROPAGATION_RETRIES", 3)
    monkeypatch.setattr(helper, "GATEWAY_PROPAGATION_DELAY", 0)
    monkeypatch.setattr(helper, "_wait_for_gateway_auth_enforced", lambda **kwargs: None)
    monkeypatch.setattr(helper, "_record_transient_retry", lambda *args: None)

    def install(replies):
        def post(url, **kwargs):
            attempts.append(kwargs["json"])
            status, body = replies[min(len(attempts) - 1, len(replies) - 1)]
            if body is None:
                body = json.dumps({"choices": [{"message": {"content": kwargs["json"]["messages"][0]["content"]}}]})
            return response(status, body)

        monkeypatch.setattr(echo.requests, "post", post)
        return attempts

    return install


@pytest.mark.parametrize("status,body", [(500, "Internal Server Error."), (503, "Service Unavailable")])
def test_echo_fidelity_recovers_from_a_bounded_proxy_error(gateway, status, body):
    attempts = gateway([(status, body), (200, None)])
    echo.TestEchoFidelity().test_response_mirrors_prompt("synthetic-test-key")
    assert len(attempts) == 2
    assert attempts[0] == attempts[1], "retry must preserve the exact prompt"


@pytest.mark.parametrize("status,body,attempts_expected", [
    (500, "Internal Server Error.", 3),
    (500, '{"error":"backend failed"}', 1),
    (400, '{"error":"invalid request"}', 1),
    (403, "", 1),
    (200, '{"choices":[{"message":{"content":"corrupted"}}]}', 1),
])
def test_echo_fidelity_still_fails_for_permanent_errors_or_wrong_content(gateway, status, body, attempts_expected):
    attempts = gateway([(status, body)])
    with pytest.raises(AssertionError):
        echo.TestEchoFidelity().test_response_mirrors_prompt("synthetic-test-key")
    assert len(attempts) == attempts_expected
