"""Standalone maas-api end-to-end tests.

These tests run against a maas-api deployment that has no MaaS subscription
CRDs, no maas-controller, and no Kuadrant. They are skipped unless
STANDALONE_MAAS_API_URL points at the standalone service.

The deployment is created by test/e2e/standalone/run.sh.
"""

import os

import pytest
import requests

STANDALONE_URL = os.environ.get("STANDALONE_MAAS_API_URL", "").rstrip("/")
TIMEOUT = int(os.environ.get("STANDALONE_E2E_TIMEOUT", "30"))

pytestmark = [
    pytest.mark.skipif(
        not STANDALONE_URL,
        reason="STANDALONE_MAAS_API_URL is not set",
    ),
    pytest.mark.xdist_group("standalone"),
]

IDENTITY_HEADERS = {
    "X-MaaS-Username": "standalone-e2e",
    "X-MaaS-Group": '["standalone-e2e"]',
}


def _url(path: str) -> str:
    return f"{STANDALONE_URL}{path}"


def _create_key(name: str) -> str:
    response = requests.post(
        _url("/v1/api-keys"),
        headers={**IDENTITY_HEADERS, "Content-Type": "application/json"},
        json={"name": name},
        timeout=TIMEOUT,
    )
    assert response.status_code == 201, response.text
    body = response.json()
    assert body["key"].startswith("sk-oai-")
    assert not body.get("subscription"), "standalone keys must not bind a subscription"
    return body["key"]


def test_health_endpoint():
    response = requests.get(_url("/health"), timeout=TIMEOUT)
    assert response.status_code == 200, response.text


def test_create_and_validate_unbound_key():
    key = _create_key("standalone-e2e-key")

    validate = requests.post(
        _url("/internal/v1/api-keys/validate"),
        json={"key": key},
        timeout=TIMEOUT,
    )
    assert validate.status_code == 200, validate.text
    result = validate.json()
    assert result["valid"] is True
    assert result["username"] == "standalone-e2e"
    assert not result.get("subscription")


def test_models_endpoint_accepts_unbound_key():
    key = _create_key("standalone-models-key")

    response = requests.get(
        _url("/v1/models"),
        headers={
            **IDENTITY_HEADERS,
            "Authorization": f"Bearer {key}",
        },
        timeout=TIMEOUT,
    )
    assert response.status_code == 200, response.text
    body = response.json()
    assert body["object"] == "list"
    assert body["data"] == [], "no MaaSModelRef CRD means an empty catalog"


def test_subscription_scoped_key_creation_is_rejected():
    response = requests.post(
        _url("/v1/api-keys"),
        headers={**IDENTITY_HEADERS, "Content-Type": "application/json"},
        json={"name": "standalone-bad-key", "subscription": "some-subscription"},
        timeout=TIMEOUT,
    )
    assert response.status_code == 400, response.text
    assert "standalone" in response.text.lower()


def test_subscription_search_is_rejected():
    response = requests.post(
        _url("/v1/api-keys/search"),
        headers={**IDENTITY_HEADERS, "Content-Type": "application/json"},
        json={"filters": {"subscription": "some-subscription"}},
        timeout=TIMEOUT,
    )
    assert response.status_code == 400, response.text
    assert "standalone" in response.text.lower()


def test_subscription_endpoints_are_unavailable():
    for path in ("/v1/subscriptions", "/v1/model/some-model/subscriptions"):
        response = requests.get(_url(path), headers=IDENTITY_HEADERS, timeout=TIMEOUT)
        assert response.status_code == 503, f"{path}: {response.status_code} {response.text}"
