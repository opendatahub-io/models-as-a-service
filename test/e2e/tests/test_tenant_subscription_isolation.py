"""
E2E tests for tenant-scoped MaaSSubscription selection (MT S4/S5).

Run with:
  ENABLE_S4_E2E=true
  MAAS_API_BASE_URL_TENANT_A / MAAS_API_BASE_URL_TENANT_B
  TENANT_A_NAMESPACE / TENANT_B_NAMESPACE
"""

import os
import uuid

import pytest

from multitenancy_helpers import (
    apply_maas_subscription,
    create_api_key_at,
    delete_maas_subscription,
    env_bool,
    list_subscriptions_at,
    require_tenant_api_base_urls,
    select_subscription_at,
    wait_for_status_phase,
)
from test_helper import MODEL_NAMESPACE, MODEL_REF, _get_cluster_token


pytestmark = pytest.mark.skipif(
    not env_bool("ENABLE_S4_E2E"),
    reason="S4 tenant subscription isolation E2E is gated; set ENABLE_S4_E2E=true once the backing implementation lands",
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
def tenant_subscriptions(tenant_env):
    suffix = uuid.uuid4().hex[:6]
    shared_name = f"e2e-shared-sub-{suffix}"
    tenant_a_only = f"e2e-a-only-{suffix}"
    tenant_b_only = f"e2e-b-only-{suffix}"
    tenant_a, tenant_b = tenant_env
    try:
        apply_maas_subscription(shared_name, tenant_a["namespace"], token_limit=50, priority=10)
        apply_maas_subscription(shared_name, tenant_b["namespace"], token_limit=500, priority=20)
        apply_maas_subscription(tenant_a_only, tenant_a["namespace"], token_limit=75, priority=30)
        apply_maas_subscription(tenant_b_only, tenant_b["namespace"], token_limit=750, priority=30)
        for tenant, names in ((tenant_a, [shared_name, tenant_a_only]), (tenant_b, [shared_name, tenant_b_only])):
            for name in names:
                wait_for_status_phase("maassubscription", name, tenant["namespace"], expected_phase=("Active", "Degraded"))
        yield {
            "shared": shared_name,
            "tenant_a_only": tenant_a_only,
            "tenant_b_only": tenant_b_only,
            "tenant_a": tenant_a,
            "tenant_b": tenant_b,
        }
    finally:
        for namespace in (tenant_a["namespace"], tenant_b["namespace"]):
            for name in (shared_name, tenant_a_only, tenant_b_only):
                delete_maas_subscription(name, namespace)


def _create_key_for_subscription(tenant: dict[str, str], subscription: str) -> str:
    response = create_api_key_at(
        tenant["base_url"],
        _get_cluster_token(),
        f"e2e-sub-iso-{uuid.uuid4().hex[:6]}",
        subscription=subscription,
    )
    assert response.status_code in (200, 201), (
        f"create API key for {tenant['name']} failed: {response.status_code} {response.text}"
    )
    return response.json()["key"]


class TestTenantSubscriptionIsolation:
    """S27 section 4 — subscription isolation."""

    def test_subscription_list_scoped_to_tenant(self, tenant_subscriptions):
        """4.1: Subscription list contains current tenant subscriptions and excludes the other tenant."""
        tenant_a = tenant_subscriptions["tenant_a"]
        tenant_b = tenant_subscriptions["tenant_b"]
        key_a = _create_key_for_subscription(tenant_a, tenant_subscriptions["tenant_a_only"])
        key_b = _create_key_for_subscription(tenant_b, tenant_subscriptions["tenant_b_only"])

        response_a = list_subscriptions_at(tenant_a["base_url"], key_a)
        assert response_a.status_code == 200, f"Tenant A list failed: {response_a.status_code} {response_a.text}"
        ids_a = {item["subscription_id_header"] for item in response_a.json()}
        assert tenant_subscriptions["tenant_a_only"] in ids_a
        assert tenant_subscriptions["tenant_b_only"] not in ids_a

        response_b = list_subscriptions_at(tenant_b["base_url"], key_b)
        assert response_b.status_code == 200, f"Tenant B list failed: {response_b.status_code} {response_b.text}"
        ids_b = {item["subscription_id_header"] for item in response_b.json()}
        assert tenant_subscriptions["tenant_b_only"] in ids_b
        assert tenant_subscriptions["tenant_a_only"] not in ids_b

    def test_subscription_selection_per_tenant(self, tenant_subscriptions):
        """4.2: Same-named subscriptions resolve to the namespace behind each tenant endpoint."""
        shared = tenant_subscriptions["shared"]
        tenant_a = tenant_subscriptions["tenant_a"]
        tenant_b = tenant_subscriptions["tenant_b"]
        key_a = _create_key_for_subscription(tenant_a, shared)
        key_b = _create_key_for_subscription(tenant_b, shared)

        requested_model = f"{MODEL_NAMESPACE}/{MODEL_REF}"
        response_a = select_subscription_at(
            tenant_a["base_url"],
            key_a,
            "e2e-sub-user",
            ["system:authenticated"],
            requested_subscription=shared,
            requested_model=requested_model,
        )
        assert response_a.status_code == 200
        data_a = response_a.json()
        assert data_a.get("error") is None, data_a
        assert data_a.get("name") == shared
        assert data_a.get("namespace") == tenant_a["namespace"]

        response_b = select_subscription_at(
            tenant_b["base_url"],
            key_b,
            "e2e-sub-user",
            ["system:authenticated"],
            requested_subscription=shared,
            requested_model=requested_model,
        )
        assert response_b.status_code == 200
        data_b = response_b.json()
        assert data_b.get("error") is None, data_b
        assert data_b.get("name") == shared
        assert data_b.get("namespace") == tenant_b["namespace"]
