"""Quota exhaustion must follow a successful inference after policy propagation."""

import json
from types import SimpleNamespace

import pytest
import requests
import test_helper as helper
import test_subscription as subscription


@pytest.fixture
def quota_test(monkeypatch):
    elapsed = [0.0]
    clock = SimpleNamespace(
        time=lambda: elapsed[0],
        sleep=lambda seconds: elapsed.__setitem__(0, elapsed[0] + seconds),
    )
    monkeypatch.setattr(helper, "time", clock)
    monkeypatch.setattr(helper, "RECONCILE_WAIT", 1)
    monkeypatch.setattr(subscription, "time", clock)
    monkeypatch.setattr(subscription, "RATE_LIMIT_ENFORCEMENT_TIMEOUT", 2)
    created_subscriptions = []
    deleted = []
    for name in (
        "_create_test_auth_policy", "_wait_for_maas_auth_policy_phase",
        "_wait_for_subscription_inference_ready", "_wait_for_cr_absent",
    ):
        monkeypatch.setattr(subscription, name, lambda *args, **kwargs: None)
    monkeypatch.setattr(
        subscription, "_create_test_subscription",
        lambda **kwargs: created_subscriptions.append(kwargs),
    )
    monkeypatch.setattr(subscription, "_get_cluster_token", lambda: "test-token")
    monkeypatch.setattr(subscription, "_create_api_key", lambda *args, **kwargs: "test-key")
    monkeypatch.setattr(subscription, "_delete_cr", lambda *args: deleted.append(args))

    def run(statuses, *, usage=None):
        calls = []

        def infer(api_key, **kwargs):
            calls.append((api_key, kwargs))
            status = statuses[min(len(calls) - 1, len(statuses) - 1)]
            response = requests.Response()
            response.status_code = status
            if status == 200:
                response._content = json.dumps({"usage": usage if usage is not None else {
                    "prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2,
                }}).encode()
            else:
                response._content = b"Too Many Requests" if status == 429 else b"response"
            return response

        monkeypatch.setattr(subscription, "_inference", infer)
        subscription.TestSubscriptionEnforcement().test_rate_limit_exhaustion_gets_429()
        return calls

    return SimpleNamespace(run=run, subscriptions=created_subscriptions, deleted=deleted)


def test_initial_default_deny_is_not_mistaken_for_exhausted_quota(quota_test):
    calls = quota_test.run([429, 200, 429])
    assert len(calls) == 4
    assert all(kwargs["max_tokens"] == 1 for _, kwargs in calls)
    assert all(kwargs["path"] == subscription.UNCONFIGURED_MODEL_PATH for _, kwargs in calls)
    assert quota_test.subscriptions[0]["token_limit"] == 10


def test_first_success_counts_toward_exhaustion(quota_test):
    assert len(quota_test.run([200, 429])) == 3


@pytest.mark.parametrize(
    "usage,message",
    [
        ({"prompt_tokens": 1, "completion_tokens": 0, "total_tokens": 1}, "non-positive or invalid usage"),
        ({"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 3}, "inconsistent usage"),
    ],
)
def test_readiness_response_must_have_valid_token_usage(quota_test, usage, message):
    with pytest.raises(AssertionError, match=message):
        quota_test.run([200, 429], usage=usage)
    assert [kind for kind, _ in quota_test.deleted] == ["maassubscription", "maasauthpolicy"]


@pytest.mark.parametrize(
    "statuses,message",
    [
        ([429], "Expected 200 within"),
        ([200], "No HTTP 429"),
        ([200, 500], "Unexpected HTTP 500"),
        ([200, 429, 200], "HTTP 200 after token-limit 429"),
        ([200, 429, 500], "Unexpected HTTP 500"),
    ],
)
def test_persistent_denial_missing_enforcement_and_backend_errors_still_fail(quota_test, statuses, message):
    with pytest.raises(AssertionError, match=message):
        quota_test.run(statuses)
    assert [kind for kind, _ in quota_test.deleted] == ["maassubscription", "maasauthpolicy"]


def test_repeated_runs_get_independent_subscription_counters(quota_test):
    quota_test.run([200, 429])
    quota_test.run([200, 429])
    first, second = quota_test.subscriptions
    assert first["name"] != second["name"]
