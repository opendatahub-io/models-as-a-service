"""
E2E tests for tenant-scoped authentication and API-key isolation.

Covers [MT S7] acceptance criteria (RHOAIENG-62570):
- No cross-tenant key leakage (tests 3.3, 3.5, 3.6)
- Identity isolation (tests 3.1, 3.2, 3.4, 3.6)
- Negative paths for cross-tenant access (tests 3.3, 3.6)

These tests use shared_test_tenants fixture to create two AITenant instances
and validate API key isolation between tenants.
"""

import os
import uuid

import pytest
import requests

from multitenancy_helpers import (
    create_api_key_at,
    delete_maas_auth_policy,
    delete_maas_subscription,
    get_api_key_at,
    list_subscriptions_at,
    make_tenant_model_accessible,
    provision_tenant_model,
    redact_sensitive,
    response_summary,
    search_api_keys_at,
    select_subscription_at,
    validate_api_key_at,
)
from test_helper import _get_cluster_token, _delete_cr


def _mint_oidc_token_for_tenant(tenant: str) -> str:
    """Return a fresh OIDC access token for the given tenant ('a' or 'b').

    Resolution order:
    1. OIDC_TOKEN_TENANT_A / OIDC_TOKEN_TENANT_B — pre-minted static tokens
       (backward-compat; note these expire in ~60s so may be stale by test time).
    2. OIDC_TOKEN_URL (tenant a) / OIDC_TOKEN_URL_TENANT_B — mint a fresh token
       via the Resource Owner Password Grant.

    Returns an empty string if no token source is available so the caller can
    skip the test gracefully.
    """
    static_key = f"OIDC_TOKEN_TENANT_{tenant.upper()}"
    static = os.environ.get(static_key, "")
    if static:
        return static

    if tenant == "a":
        token_url = os.environ.get("OIDC_TOKEN_URL", "")
        username = os.environ.get("OIDC_USERNAME", "alice_lead")
        password = os.environ.get("OIDC_PASSWORD", "letmein")
        client_id = os.environ.get("OIDC_CLIENT_ID", "test-client")
    else:
        token_url = os.environ.get("OIDC_TOKEN_URL_TENANT_B", "")
        username = os.environ.get("OIDC_USERNAME_TENANT_B", "charlie_sec_lead")
        password = os.environ.get("OIDC_PASSWORD_TENANT_B", "letmein")
        client_id = os.environ.get("OIDC_CLIENT_ID_TENANT_B", "test-client")

    if not token_url:
        return ""

    verify = os.environ.get("E2E_SKIP_TLS_VERIFY", "").lower() != "true"
    try:
        resp = requests.post(
            token_url,
            data={
                "grant_type": "password",
                "client_id": client_id,
                "username": username,
                "password": password,
            },
            timeout=30,
            verify=verify,
        )
        resp.raise_for_status()
        return resp.json().get("access_token", "")
    except Exception:
        return ""


# Tenant auth isolation tests are enabled by default (Phase 1 implementation)


@pytest.fixture(scope="module")
def tenant_env(shared_test_tenants):
    """Adapter fixture with tenant-specific models."""
    case_a, case_b = dict(shared_test_tenants[0]), dict(shared_test_tenants[1])

    for case in (case_a, case_b):
        model_name = f"auth-test-model-{case['suffix']}"
        provision_tenant_model(model_name, case["tenant_ns"], case["gateway_name"])
        case["model_name"] = model_name
        case["model_namespace"] = case["tenant_ns"]

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
            make_tenant_model_accessible(
                tenant["model_name"],
                tenant["namespace"],
                policy_name,
                subscription_name,
            )
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
        response = list_subscriptions_at(
            tenant_auth_setup["tenant_a"]["base_url"],
            tenant_api_keys["a"]["key"],
        )
        assert response.status_code == 200, (
            f"Tenant A key should work on Tenant A gateway: {response_summary(response)}"
        )
        data = response.json()
        assert isinstance(data, list), redact_sensitive(data)

    def test_api_key_rejected_cross_tenant(self, tenant_auth_setup, tenant_api_keys):
        """3.3: Tenant B rejects a key minted by Tenant A."""
        response = list_subscriptions_at(
            tenant_auth_setup["tenant_b"]["base_url"],
            tenant_api_keys["a"]["key"],
        )
        assert response.status_code in (401, 403), (
            f"Tenant A key should be rejected on Tenant B gateway (got {response.status_code}): {response_summary(response)}"
        )

    def test_oidc_token_validation_per_tenant(self, tenant_env):
        """3.4: Tenant OIDC tokens are accepted only by their configured tenant endpoint."""
        token_a = _mint_oidc_token_for_tenant("a")
        token_b = _mint_oidc_token_for_tenant("b")
        if not token_a or not token_b:
            pytest.skip(
                "Per-tenant OIDC tokens unavailable — set OIDC_TOKEN_URL (tenant-a) and "
                "OIDC_TOKEN_URL_TENANT_B (tenant-b), or OIDC_TOKEN_TENANT_A / OIDC_TOKEN_TENANT_B"
            )

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

    def test_api_key_metadata_not_leaked_cross_tenant(self, tenant_auth_setup, tenant_api_keys):
        """3.6: Tenant B cannot retrieve Tenant A's key metadata via GET /v1/api-keys/{id}."""
        oc_token = _get_cluster_token()

        # Try to GET tenant A's key from tenant B's gateway (should be rejected)
        response = get_api_key_at(
            tenant_auth_setup["tenant_b"]["base_url"],
            oc_token,
            tenant_api_keys["a"]["id"],
        )
        assert response.status_code == 404, (
            f"Tenant B should not retrieve Tenant A's key metadata "
            f"(expected 404, got {response.status_code}): {response_summary(response)}"
        )

        # Verify response body is exactly {"error": "API key not found"} with no leaked metadata
        body = response.json()
        assert body == {"error": "API key not found"}, (
            f"404 response should contain only error message, got: {redact_sensitive(body)}"
        )

        # Verify tenant A can still GET its own key (sanity check)
        response_a = get_api_key_at(
            tenant_auth_setup["tenant_a"]["base_url"],
            oc_token,
            tenant_api_keys["a"]["id"],
        )
        assert response_a.status_code == 200, (
            f"Tenant A should still retrieve its own key: {response_summary(response_a)}"
        )

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
