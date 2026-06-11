"""
E2E tests for per-tenant rate-limit isolation (MT S4).

Run with:
  ENABLE_S4_E2E=true
  MAAS_API_BASE_URL_TENANT_A / MAAS_API_BASE_URL_TENANT_B
  TENANT_A_NAMESPACE / TENANT_B_NAMESPACE
"""

import os
import time
import uuid

import pytest
import requests

from multitenancy_helpers import (
    apply_maas_auth_policy,
    apply_maas_subscription,
    create_api_key_at,
    delete_maas_auth_policy,
    delete_maas_subscription,
    env_bool,
    require_tenant_api_base_urls,
    wait_for_status_phase,
)
from test_helper import (
    MODEL_NAME,
    MODEL_NAMESPACE,
    MODEL_PATH,
    MODEL_REF,
    TIMEOUT,
    TLS_VERIFY,
    _get_cluster_token,
    _wait_for_token_rate_limit_policy,
)


pytestmark = pytest.mark.skipif(
    not env_bool("ENABLE_S4_E2E"),
    reason="S4 tenant rate-limit isolation E2E is gated; set ENABLE_S4_E2E=true once the backing implementation lands",
)


@pytest.fixture(scope="module")
def tenant_env():
    urls = require_tenant_api_base_urls("TENANT_A", "TENANT_B")
    tenant_a = {
        "slot": "TENANT_A",
        "name": os.environ.get("TENANT_A_NAME", "tenant-a"),
        "namespace": os.environ.get("TENANT_A_NAMESPACE", ""),
        "base_url": urls["TENANT_A"],
    }
    tenant_b = {
        "slot": "TENANT_B",
        "name": os.environ.get("TENANT_B_NAME", "tenant-b"),
        "namespace": os.environ.get("TENANT_B_NAMESPACE", ""),
        "base_url": urls["TENANT_B"],
    }
    missing = [item["slot"] for item in (tenant_a, tenant_b) if not item["namespace"]]
    if missing:
        pytest.fail(f"tenant namespaces not configured; set {', '.join(f'{slot}_NAMESPACE' for slot in missing)}")
    return tenant_a, tenant_b


@pytest.fixture
def tenant_rate_limit_setup(tenant_env):
    suffix = uuid.uuid4().hex[:6]
    tenant_a, tenant_b = tenant_env
    policy_name = f"e2e-rate-iso-auth-{suffix}"
    sub_a = f"e2e-rate-iso-a-{suffix}"
    sub_b = f"e2e-rate-iso-b-{suffix}"
    try:
        for tenant in tenant_env:
            apply_maas_auth_policy(policy_name, tenant["namespace"])
            wait_for_status_phase("maasauthpolicy", policy_name, tenant["namespace"], expected_phase="Active")

        apply_maas_subscription(sub_a, tenant_a["namespace"], token_limit=3, window="1m")
        apply_maas_subscription(sub_b, tenant_b["namespace"], token_limit=100, window="1m")
        wait_for_status_phase("maassubscription", sub_a, tenant_a["namespace"], expected_phase=("Active", "Degraded"))
        wait_for_status_phase("maassubscription", sub_b, tenant_b["namespace"], expected_phase=("Active", "Degraded"))
        _wait_for_token_rate_limit_policy(MODEL_REF, model_namespace=MODEL_NAMESPACE, timeout=120)

        oc_token = _get_cluster_token()
        key_a_response = create_api_key_at(
            tenant_a["base_url"],
            oc_token,
            f"e2e-rate-a-{suffix}",
            subscription=sub_a,
        )
        assert key_a_response.status_code in (200, 201), key_a_response.text
        key_b_response = create_api_key_at(
            tenant_b["base_url"],
            oc_token,
            f"e2e-rate-b-{suffix}",
            subscription=sub_b,
        )
        assert key_b_response.status_code in (200, 201), key_b_response.text

        yield {
            "tenant_a": tenant_a,
            "tenant_b": tenant_b,
            "policy": policy_name,
            "subscription_a": sub_a,
            "subscription_b": sub_b,
            "key_a": key_a_response.json()["key"],
            "key_b": key_b_response.json()["key"],
        }
    finally:
        for tenant in tenant_env:
            delete_maas_auth_policy(policy_name, tenant["namespace"])
        delete_maas_subscription(sub_a, tenant_a["namespace"])
        delete_maas_subscription(sub_b, tenant_b["namespace"])


def _gateway_base_from_api_url(api_base_url: str) -> str:
    stripped = api_base_url.rstrip("/")
    if stripped.endswith("/maas-api"):
        return stripped[: -len("/maas-api")]
    return stripped


def _inference_at(api_base_url: str, api_key: str) -> requests.Response:
    url = f"{_gateway_base_from_api_url(api_base_url)}{MODEL_PATH}/v1/completions"
    return requests.post(
        url,
        headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
        json={"model": MODEL_NAME, "prompt": "Hello", "max_tokens": 1},
        timeout=TIMEOUT,
        verify=TLS_VERIFY,
    )


def _exhaust_until_429(api_base_url: str, api_key: str, *, attempts: int = 8) -> tuple[int, requests.Response]:
    successes = 0
    last = None
    for _ in range(attempts):
        last = _inference_at(api_base_url, api_key)
        if last.status_code == 200:
            successes += 1
        elif last.status_code == 429:
            return successes, last
        else:
            raise AssertionError(f"unexpected inference status {last.status_code}: {last.text[:300]}")
        time.sleep(0.1)
    assert last is not None
    return successes, last


class TestTenantRateLimitIsolation:
    """S27 section 5 — rate-limit isolation."""

    def test_rate_limit_enforced_per_tenant(self, tenant_rate_limit_setup):
        """5.1: Tenant A's low quota is enforced on Tenant A traffic."""
        tenant_a = tenant_rate_limit_setup["tenant_a"]
        successes, response = _exhaust_until_429(tenant_a["base_url"], tenant_rate_limit_setup["key_a"])
        assert successes > 0, f"Tenant A hit 429 before any successful inference: {response.text[:300]}"
        assert response.status_code == 429, (
            f"expected Tenant A to hit rate limit, got {response.status_code} after {successes} successes: {response.text[:300]}"
        )

    def test_independent_tenant_rate_limits(self, tenant_rate_limit_setup):
        """5.2: Exhausting Tenant A does not consume Tenant B's quota."""
        tenant_a = tenant_rate_limit_setup["tenant_a"]
        tenant_b = tenant_rate_limit_setup["tenant_b"]
        successes, response = _exhaust_until_429(tenant_a["base_url"], tenant_rate_limit_setup["key_a"])
        assert response.status_code == 429, f"Tenant A did not hit rate limit after {successes} successes"

        tenant_b_response = _inference_at(tenant_b["base_url"], tenant_rate_limit_setup["key_b"])
        assert tenant_b_response.status_code == 200, (
            f"Tenant B should still have independent quota, got {tenant_b_response.status_code}: {tenant_b_response.text[:300]}"
        )
