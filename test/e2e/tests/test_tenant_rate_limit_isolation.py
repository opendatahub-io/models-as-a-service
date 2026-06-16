"""
E2E tests for per-tenant rate-limit isolation.

These tests use shared_test_tenants fixture to create two AITenant instances
and validate rate limit isolation between tenants.
"""

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
    response_summary,
    wait_for_status_phase,
)
from test_helper import (
    TIMEOUT,
    TLS_VERIFY,
    _get_cluster_token,
    _wait_for_token_rate_limit_policy,
    _create_llmis,
    _create_maas_model_ref,
    _delete_cr,
)
from multitenancy_helpers import GATEWAY_NAMESPACE


# Tenant rate-limit isolation tests are enabled by default (Phase 1 implementation)


@pytest.fixture(scope="module")
def tenant_env(shared_test_tenants):
    """Adapter fixture with tenant-specific models."""
    case_a, case_b = shared_test_tenants

    # Create models in each tenant namespace
    for case in (case_a, case_b):
        model_name = f"rate-test-model-{case['suffix']}"

        # Create LLMIS pointing to tenant gateway
        _create_llmis(model_name, case["tenant_ns"], case["gateway_name"], GATEWAY_NAMESPACE)

        # Create MaaSModelRef
        _create_maas_model_ref(model_name, case["tenant_ns"], model_name)

        # Store model info
        case["model_name"] = model_name
        case["model_namespace"] = case["tenant_ns"]

    # Rename keys to match existing test expectations
    tenant_a = {
        "name": case_a["tenant_label_name"],
        "namespace": case_a["tenant_ns"],
        "base_url": case_a["base_url"],
        "model_name": case_a["model_name"],
        "model_namespace": case_a["model_namespace"],
        "model_path": f"/{case_a['tenant_ns']}/{case_a['model_name']}",
    }
    tenant_b = {
        "name": case_b["tenant_label_name"],
        "namespace": case_b["tenant_ns"],
        "base_url": case_b["base_url"],
        "model_name": case_b["model_name"],
        "model_namespace": case_b["model_namespace"],
        "model_path": f"/{case_b['tenant_ns']}/{case_b['model_name']}",
    }

    yield tenant_a, tenant_b

    # Cleanup models
    for case in (case_a, case_b):
        _delete_cr("maasmodelref", case["model_name"], case["tenant_ns"])
        _delete_cr("llminferenceservice", case["model_name"], case["tenant_ns"])




@pytest.fixture
def tenant_rate_limit_setup(tenant_env):
    suffix = uuid.uuid4().hex[:6]
    tenant_a, tenant_b = tenant_env
    policy_name = f"e2e-rate-iso-auth-{suffix}"
    sub_a = f"e2e-rate-iso-a-{suffix}"
    sub_b = f"e2e-rate-iso-b-{suffix}"
    try:
        # Create auth policies for tenant models
        for tenant in tenant_env:
            apply_maas_auth_policy(policy_name, tenant["namespace"],
                                  model_ref=tenant["model_name"],
                                  model_namespace=tenant["model_namespace"])
            wait_for_status_phase("maasauthpolicy", policy_name, tenant["namespace"], expected_phase="Active")

        # Create subscriptions with different rate limits
        apply_maas_subscription(sub_a, tenant_a["namespace"],
                               model_ref=tenant_a["model_name"],
                               model_namespace=tenant_a["model_namespace"],
                               token_limit=3, window="1m")
        apply_maas_subscription(sub_b, tenant_b["namespace"],
                               model_ref=tenant_b["model_name"],
                               model_namespace=tenant_b["model_namespace"],
                               token_limit=100, window="1m")
        wait_for_status_phase("maassubscription", sub_a, tenant_a["namespace"], expected_phase=("Active", "Degraded"))
        wait_for_status_phase("maassubscription", sub_b, tenant_b["namespace"], expected_phase=("Active", "Degraded"))
        _wait_for_token_rate_limit_policy(tenant_a["model_name"], model_namespace=tenant_a["model_namespace"], timeout=120)

        oc_token = _get_cluster_token()
        key_a_response = create_api_key_at(
            tenant_a["base_url"],
            oc_token,
            f"e2e-rate-a-{suffix}",
            subscription=sub_a,
        )
        assert key_a_response.status_code in (200, 201), response_summary(key_a_response)
        key_b_response = create_api_key_at(
            tenant_b["base_url"],
            oc_token,
            f"e2e-rate-b-{suffix}",
            subscription=sub_b,
        )
        assert key_b_response.status_code in (200, 201), response_summary(key_b_response)

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


def _inference_at(api_base_url: str, api_key: str, model_path: str, model_name: str) -> requests.Response:
    url = f"{_gateway_base_from_api_url(api_base_url)}{model_path}/v1/completions"
    return requests.post(
        url,
        headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
        json={"model": model_name, "prompt": "Hello", "max_tokens": 1},
        timeout=TIMEOUT,
        verify=TLS_VERIFY,
    )


def _exhaust_until_429(api_base_url: str, api_key: str, model_path: str, model_name: str, *, attempts: int = 8) -> tuple[int, requests.Response]:
    successes = 0
    last = None
    for _ in range(attempts):
        last = _inference_at(api_base_url, api_key, model_path, model_name)
        if last.status_code == 200:
            successes += 1
        elif last.status_code == 429:
            return successes, last
        else:
            raise AssertionError(f"unexpected inference response: {response_summary(last)}")
        time.sleep(0.1)
    assert last is not None
    return successes, last


class TestTenantRateLimitIsolation:
    """S27 section 5 — rate-limit isolation."""

    def test_rate_limit_enforced_per_tenant(self, tenant_rate_limit_setup):
        """5.1: Tenant A's low quota is enforced on Tenant A traffic."""
        tenant_a = tenant_rate_limit_setup["tenant_a"]
        successes, response = _exhaust_until_429(
            tenant_a["base_url"],
            tenant_rate_limit_setup["key_a"],
            tenant_a["model_path"],
            tenant_a["model_name"]
        )
        assert successes > 0, f"Tenant A hit 429 before any successful inference: {response_summary(response)}"
        assert response.status_code == 429, (
            f"expected Tenant A to hit rate limit after {successes} successes: {response_summary(response)}"
        )

    def test_independent_tenant_rate_limits(self, tenant_rate_limit_setup):
        """5.2: Exhausting Tenant A does not consume Tenant B's quota."""
        tenant_a = tenant_rate_limit_setup["tenant_a"]
        tenant_b = tenant_rate_limit_setup["tenant_b"]
        successes, response = _exhaust_until_429(
            tenant_a["base_url"],
            tenant_rate_limit_setup["key_a"],
            tenant_a["model_path"],
            tenant_a["model_name"]
        )
        assert response.status_code == 429, f"Tenant A did not hit rate limit after {successes} successes"

        tenant_b_response = _inference_at(
            tenant_b["base_url"],
            tenant_rate_limit_setup["key_b"],
            tenant_b["model_path"],
            tenant_b["model_name"]
        )
        assert tenant_b_response.status_code == 200, (
            f"Tenant B should still have independent quota: {response_summary(tenant_b_response)}"
        )
