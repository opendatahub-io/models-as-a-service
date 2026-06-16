"""
E2E tests for tenant-scoped MaaSSubscription selection.

These tests use shared_test_tenants fixture to create two AITenant instances
and validate subscription selection isolation between tenants.
"""

import uuid

import pytest

from multitenancy_helpers import (
    apply_maas_subscription,
    create_api_key_at,
    delete_maas_subscription,
    list_subscriptions_at,
    redact_sensitive,
    response_summary,
    select_subscription_at,
    wait_for_status_phase,
)
from test_helper import _get_cluster_token, _create_llmis, _create_maas_model_ref, _delete_cr
from multitenancy_helpers import GATEWAY_NAMESPACE


# Tenant subscription isolation tests are enabled by default (Phase 1 implementation)


@pytest.fixture(scope="module")
def tenant_env(shared_test_tenants):
    """Adapter fixture with tenant-specific models."""
    case_a, case_b = shared_test_tenants

    # Create models in each tenant namespace
    for case in (case_a, case_b):
        model_name = f"sub-test-model-{case['suffix']}"

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
    }
    tenant_b = {
        "name": case_b["tenant_label_name"],
        "namespace": case_b["tenant_ns"],
        "base_url": case_b["base_url"],
        "model_name": case_b["model_name"],
        "model_namespace": case_b["model_namespace"],
    }

    yield tenant_a, tenant_b

    # Cleanup models
    for case in (case_a, case_b):
        _delete_cr("maasmodelref", case["model_name"], case["tenant_ns"])
        _delete_cr("llminferenceservice", case["model_name"], case["tenant_ns"])


@pytest.fixture
def tenant_subscriptions(tenant_env):
    tenant_a, tenant_b = tenant_env
    suffix = uuid.uuid4().hex[:6]
    shared_name = f"e2e-shared-sub-{suffix}"
    tenant_a_only = f"e2e-a-only-{suffix}"
    tenant_b_only = f"e2e-b-only-{suffix}"
    tenant_a, tenant_b = tenant_env
    try:
        # Create subscriptions using tenant's own models
        apply_maas_subscription(shared_name, tenant_a["namespace"],
                               model_ref=tenant_a["model_name"],
                               model_namespace=tenant_a["model_namespace"],
                               token_limit=50, priority=10)
        apply_maas_subscription(shared_name, tenant_b["namespace"],
                               model_ref=tenant_b["model_name"],
                               model_namespace=tenant_b["model_namespace"],
                               token_limit=500, priority=20)
        apply_maas_subscription(tenant_a_only, tenant_a["namespace"],
                               model_ref=tenant_a["model_name"],
                               model_namespace=tenant_a["model_namespace"],
                               token_limit=75, priority=30)
        apply_maas_subscription(tenant_b_only, tenant_b["namespace"],
                               model_ref=tenant_b["model_name"],
                               model_namespace=tenant_b["model_namespace"],
                               token_limit=750, priority=30)
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
        f"create API key for {tenant['name']} failed: {response_summary(response)}"
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
        assert response_a.status_code == 200, f"Tenant A list failed: {response_summary(response_a)}"
        ids_a = {item["subscription_id_header"] for item in response_a.json()}
        assert tenant_subscriptions["tenant_a_only"] in ids_a
        assert tenant_subscriptions["tenant_b_only"] not in ids_a

        response_b = list_subscriptions_at(tenant_b["base_url"], key_b)
        assert response_b.status_code == 200, f"Tenant B list failed: {response_summary(response_b)}"
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

        requested_model_a = f"{tenant_a['model_namespace']}/{tenant_a['model_name']}"
        response_a = select_subscription_at(
            tenant_a["base_url"],
            key_a,
            "e2e-sub-user",
            ["system:authenticated"],
            requested_subscription=shared,
            requested_model=requested_model_a,
        )
        assert response_a.status_code == 200
        data_a = response_a.json()
        assert data_a.get("error") is None, redact_sensitive(data_a)
        assert data_a.get("name") == shared
        assert data_a.get("namespace") == tenant_a["namespace"]

        requested_model_b = f"{tenant_b['model_namespace']}/{tenant_b['model_name']}"
        response_b = select_subscription_at(
            tenant_b["base_url"],
            key_b,
            "e2e-sub-user",
            ["system:authenticated"],
            requested_subscription=shared,
            requested_model=requested_model_b,
        )
        assert response_b.status_code == 200
        data_b = response_b.json()
        assert data_b.get("error") is None, redact_sensitive(data_b)
        assert data_b.get("name") == shared
        assert data_b.get("namespace") == tenant_b["namespace"]
