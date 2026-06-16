"""
E2E tests for tenant-scoped authentication and API-key isolation.

These tests use shared_test_tenants fixture to create two AITenant instances
and validate API key isolation between tenants.
"""

import uuid

import pytest

from multitenancy_helpers import (
    apply_maas_auth_policy,
    apply_maas_subscription,
    create_api_key_at,
    delete_maas_auth_policy,
    delete_maas_subscription,
    get_api_key_at,
    list_subscriptions_at,
    redact_sensitive,
    response_summary,
    search_api_keys_at,
    select_subscription_at,
    validate_api_key_at,
    wait_for_status_phase,
)
from test_helper import _get_cluster_token, _create_llmis, _create_maas_model_ref, _delete_cr
from multitenancy_helpers import GATEWAY_NAMESPACE


# Tenant auth isolation tests are enabled by default (Phase 1 implementation)


@pytest.fixture(scope="module")
def tenant_env(shared_test_tenants):
    """Adapter fixture with tenant-specific models."""
    case_a, case_b = shared_test_tenants

    # Create models in each tenant namespace
    for case in (case_a, case_b):
        model_name = f"auth-test-model-{case['suffix']}"

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
def tenant_auth_setup(tenant_env):
    tenant_a, tenant_b = tenant_env
    suffix = uuid.uuid4().hex[:6]
    policy_name = f"e2e-auth-iso-{suffix}"
    subscription_name = f"e2e-auth-iso-{suffix}"
    try:
        for tenant in tenant_env:
            # Create auth policy and subscription for tenant's own model
            apply_maas_auth_policy(
                policy_name,
                tenant["namespace"],
                model_ref=tenant["model_name"],
                model_namespace=tenant["model_namespace"]
            )
            apply_maas_subscription(
                subscription_name,
                tenant["namespace"],
                model_ref=tenant["model_name"],
                model_namespace=tenant["model_namespace"]
            )
            wait_for_status_phase("maasauthpolicy", policy_name, tenant["namespace"], expected_phase="Active")
            wait_for_status_phase("maassubscription", subscription_name, tenant["namespace"], expected_phase=("Active", "Degraded"))
        yield {
            "tenant_a": tenant_a,
            "tenant_b": tenant_b,
            "policy": policy_name,
            "subscription": subscription_name,
        }
    finally:
        for tenant in tenant_env:
            delete_maas_auth_policy(policy_name, tenant["namespace"])
            delete_maas_subscription(subscription_name, tenant["namespace"])


@pytest.fixture
def tenant_api_keys(tenant_auth_setup):
    oc_token = _get_cluster_token()
    created = {}
    for key_name, tenant in (("a", tenant_auth_setup["tenant_a"]), ("b", tenant_auth_setup["tenant_b"])):
        response = create_api_key_at(
            tenant["base_url"],
            oc_token,
            f"e2e-auth-iso-{key_name}-{uuid.uuid4().hex[:6]}",
            subscription=tenant_auth_setup["subscription"],
        )
        assert response.status_code in (200, 201), (
            f"create key for {tenant['name']} failed: {response_summary(response)}"
        )
        created[key_name] = response.json()
    return created


class TestTenantAuthIsolation:
    """S27 section 3 — authentication tenant isolation."""

    def test_api_key_creation_scoped_to_tenant(self, tenant_auth_setup, tenant_api_keys):
        """3.1: API key metadata is scoped to the tenant that minted it."""
        oc_token = _get_cluster_token()
        for key_name, tenant_key in (("a", "tenant_a"), ("b", "tenant_b")):
            tenant = tenant_auth_setup[tenant_key]
            key_id = tenant_api_keys[key_name]["id"]
            response = get_api_key_at(tenant["base_url"], oc_token, key_id)
            assert response.status_code == 200, f"GET key in {tenant['name']} failed: {response_summary(response)}"
            data = response.json()
            if data.get("tenant") is not None:
                assert data["tenant"] == tenant["name"]
            assert data.get("subscription") == tenant_auth_setup["subscription"]

    def test_api_key_validates_against_correct_tenant(self, tenant_auth_setup, tenant_api_keys):
        """3.2: API key validates on the tenant endpoint that minted it."""
        # Use list subscriptions endpoint which accepts API key authentication
        response = list_subscriptions_at(
            tenant_auth_setup["tenant_a"]["base_url"],
            tenant_api_keys["a"]["key"],
        )
        assert response.status_code == 200, (
            f"Tenant A key should work on Tenant A gateway: {response_summary(response)}"
        )
        # Verify the key works by checking we get a subscriptions array
        data = response.json()
        assert isinstance(data, list), redact_sensitive(data)

    def test_api_key_rejected_cross_tenant(self, tenant_auth_setup, tenant_api_keys):
        """3.3: Tenant B rejects a key minted by Tenant A."""
        # Try to use tenant A's key on tenant B's gateway - should be rejected
        response = list_subscriptions_at(
            tenant_auth_setup["tenant_b"]["base_url"],
            tenant_api_keys["a"]["key"],
        )
        assert response.status_code in (401, 403), (
            f"Tenant A key should be rejected on Tenant B gateway (got {response.status_code}): {response_summary(response)}"
        )

    def test_oidc_token_validation_per_tenant(self, tenant_env):
        """3.4: Tenant OIDC tokens are accepted only by their configured tenant endpoint."""
        token_a = os.environ.get("OIDC_TOKEN_TENANT_A", "")
        token_b = os.environ.get("OIDC_TOKEN_TENANT_B", "")
        if not token_a or not token_b:
            pytest.skip("OIDC_TOKEN_TENANT_A and OIDC_TOKEN_TENANT_B are required for per-tenant OIDC validation")

        tenant_a, tenant_b = tenant_env
        response_a = search_api_keys_at(tenant_a["base_url"], token_a)
        assert response_a.status_code != 401, f"Tenant A rejected its OIDC token: {response_summary(response_a)}"

        cross_response = search_api_keys_at(tenant_b["base_url"], token_a)
        assert cross_response.status_code in (401, 403), (
            f"Tenant B should reject Tenant A OIDC token: {response_summary(cross_response)}"
        )

    def test_api_key_list_scoped_to_tenant(self, tenant_auth_setup, tenant_api_keys):
        """3.5: API key search returns keys from the current tenant only."""
        oc_token = _get_cluster_token()
        response_a = search_api_keys_at(
            tenant_auth_setup["tenant_a"]["base_url"],
            oc_token,
            subscription=tenant_auth_setup["subscription"],
        )
        assert response_a.status_code == 200, f"Tenant A search failed: {response_summary(response_a)}"
        ids_a = {item["id"] for item in response_a.json().get("data", [])}
        assert tenant_api_keys["a"]["id"] in ids_a
        assert tenant_api_keys["b"]["id"] not in ids_a

        response_b = search_api_keys_at(
            tenant_auth_setup["tenant_b"]["base_url"],
            oc_token,
            subscription=tenant_auth_setup["subscription"],
        )
        assert response_b.status_code == 200, f"Tenant B search failed: {response_summary(response_b)}"
        ids_b = {item["id"] for item in response_b.json().get("data", [])}
        assert tenant_api_keys["b"]["id"] in ids_b
        assert tenant_api_keys["a"]["id"] not in ids_b

    def test_api_key_subscription_selection_uses_tenant_namespace(self, tenant_auth_setup, tenant_api_keys):
        """3.x/4.x: Internal subscription selection reports the tenant-local subscription namespace."""
        tenant_a = tenant_auth_setup["tenant_a"]
        response = select_subscription_at(
            tenant_a["base_url"],
            tenant_api_keys["a"]["key"],
            "e2e-auth-user",
            ["system:authenticated"],
            requested_subscription=tenant_auth_setup["subscription"],
            requested_model=f"{tenant_a['model_namespace']}/{tenant_a['model_name']}",
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("error") is None, redact_sensitive(data)
        assert data.get("namespace") == tenant_a["namespace"]
