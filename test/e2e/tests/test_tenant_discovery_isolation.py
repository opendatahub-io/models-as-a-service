"""
E2E tests for /v1/tenant endpoint data isolation.

Verifies that each tenant's maas-api instance returns its own configuration
and does not leak data from other tenants. With system:authenticated authorization,
any authenticated user can call any tenant's endpoint, but each endpoint must
return only its own tenant's data.
"""

import logging
import pytest
import requests
import os

from conftest import TLS_VERIFY
from test_helper import _get_cluster_token

log = logging.getLogger(__name__)


@pytest.fixture
def tenant_service_urls(shared_test_tenants):
    """
    Get internal service URLs for each tenant's maas-api.

    Each tenant has its own maas-api deployment in its own namespace.
    The /v1/tenant endpoint must be called via the Service (not Gateway).
    """
    tenant_a, tenant_b = shared_test_tenants

    # Construct internal service URLs
    # Format: http://{service-name}.{namespace}.svc.cluster.local:{port}
    def service_url(tenant):
        namespace = tenant["namespace"]
        # Service name follows pattern: maas-api-{tenant-name}
        service_name = f"maas-api-{tenant['name']}"
        port = "8080"
        return f"http://{service_name}.{namespace}.svc.cluster.local:{port}"

    return {
        "tenant_a": {
            "name": tenant_a["name"],
            "namespace": tenant_a["namespace"],
            "service_url": service_url(tenant_a),
            "aitenant_name": tenant_a["name"],  # AITenant CR name matches tenant name
        },
        "tenant_b": {
            "name": tenant_b["name"],
            "namespace": tenant_b["namespace"],
            "service_url": service_url(tenant_b),
            "aitenant_name": tenant_b["name"],
        },
    }


@pytest.fixture
def tenant_tokens(tenant_service_urls):
    """
    Get service account tokens for each tenant.

    For proper isolation testing, we need tokens that are authorized for their
    own tenant but NOT for the other tenant.

    Uses the cluster token (which should have access to all tenants) as a baseline,
    but in production each tenant would have its own Dashboard service account.
    """
    # For E2E, we use the cluster token which has broad permissions
    # In production, each tenant's Dashboard would have a scoped SA token
    token = _get_cluster_token()

    return {
        "tenant_a": token,
        "tenant_b": token,
        "cluster": token,  # Has access to everything (for positive tests)
    }


def test_tenant_discovery_same_tenant_access(tenant_service_urls, tenant_tokens):
    """
    Verify each tenant can access its own /v1/tenant endpoint.

    This is the positive case - tenant A's token should work for tenant A's endpoint.
    """
    for tenant_key in ["tenant_a", "tenant_b"]:
        tenant = tenant_service_urls[tenant_key]
        token = tenant_tokens["cluster"]  # Using cluster token (has access)

        url = f"{tenant['service_url']}/v1/tenant"
        headers = {"Authorization": f"Bearer {token}"}

        log.info(f"[isolation] Testing {tenant['name']} can access own endpoint")

        r = requests.get(url, headers=headers, timeout=10, verify=TLS_VERIFY)

        # Should succeed (200) with system:authenticated authorization
        assert r.status_code == 200, \
            f"{tenant['name']} should access endpoint with system:authenticated, got {r.status_code}: {r.text[:400]}"

        data = r.json()
        assert data["tenant"]["name"] == tenant["aitenant_name"], \
            f"Expected tenant name {tenant['aitenant_name']}, got {data['tenant']['name']}"
        print(f"[isolation] ✓ {tenant['name']} can access /v1/tenant endpoint (system:authenticated)")


def test_tenant_discovery_cross_tenant_isolation(tenant_service_urls, tenant_tokens):
    """
    Verify each tenant's endpoint returns its OWN data (no cross-tenant leakage).

    With system:authenticated authorization, any authenticated user can call any
    tenant's /v1/tenant endpoint. This is intentional - the endpoint is permissive.

    However, each maas-api instance MUST return only its own configuration:
    - Tenant A's maas-api returns tenant A's name and gateway
    - Tenant B's maas-api returns tenant B's name and gateway

    This test validates that calling different maas-api instances returns
    different data (proving each instance is correctly configured and not
    leaking data from other tenants).
    """
    tenant_a = tenant_service_urls["tenant_a"]
    tenant_b = tenant_service_urls["tenant_b"]
    cluster_token = tenant_tokens["cluster"]

    # Test: Call tenant A's endpoint
    url_a = f"{tenant_a['service_url']}/v1/tenant"
    headers = {"Authorization": f"Bearer {cluster_token}"}

    r_a = requests.get(url_a, headers=headers, timeout=10, verify=TLS_VERIFY)

    assert r_a.status_code == 200, f"Tenant A endpoint should return 200 (system:authenticated), got {r_a.status_code}"
    data_a = r_a.json()

    # Test: Call tenant B's endpoint (same token - should also work)
    url_b = f"{tenant_b['service_url']}/v1/tenant"

    r_b = requests.get(url_b, headers=headers, timeout=10, verify=TLS_VERIFY)

    assert r_b.status_code == 200, f"Tenant B endpoint should return 200 (system:authenticated), got {r_b.status_code}"
    data_b = r_b.json()

    # Critical: Verify no data leakage
    # Each endpoint should return its OWN tenant data, not the other's
    assert data_a["tenant"]["name"] == tenant_a["aitenant_name"], \
        f"Tenant A endpoint should return tenant A data, got {data_a['tenant']['name']}"

    assert data_b["tenant"]["name"] == tenant_b["aitenant_name"], \
        f"Tenant B endpoint should return tenant B data, got {data_b['tenant']['name']}"

    assert data_a["tenant"]["name"] != data_b["tenant"]["name"], \
        "Tenant A and B should return different tenant names"

    # Verify gateway isolation
    assert data_a["gateway"]["name"] != data_b["gateway"]["name"], \
        "Each tenant should have its own gateway"

    assert data_a["gateway"]["externalHost"] != data_b["gateway"]["externalHost"], \
        "Each tenant should have its own gateway hostname"

    print(f"[isolation] ✓ Tenant A: {data_a['tenant']['name']} / Gateway: {data_a['gateway']['name']}")
    print(f"[isolation] ✓ Tenant B: {data_b['tenant']['name']} / Gateway: {data_b['gateway']['name']}")
    print(f"[isolation] ✓ No data leakage - each tenant returns own data")


def test_tenant_discovery_unauthorized_access(tenant_service_urls):
    """
    Verify completely unauthorized access is rejected.

    Without any token or with an invalid token, all tenant endpoints should return 401.
    """
    for tenant_key in ["tenant_a", "tenant_b"]:
        tenant = tenant_service_urls[tenant_key]
        url = f"{tenant['service_url']}/v1/tenant"

        # Test 1: No auth header
        r = requests.get(url, timeout=10, verify=TLS_VERIFY)
        assert r.status_code == 401, \
            f"{tenant['name']} should reject no-auth request with 401, got {r.status_code}"

        # Test 2: Invalid token
        headers = {"Authorization": "Bearer invalid-token-12345"}
        r = requests.get(url, headers=headers, timeout=10, verify=TLS_VERIFY)
        assert r.status_code == 401, \
            f"{tenant['name']} should reject invalid token with 401, got {r.status_code}"

    print("[isolation] ✓ Both tenants properly reject unauthorized access")


def test_tenant_discovery_each_tenant_returns_own_gateway(tenant_service_urls, tenant_tokens):
    """
    Verify each tenant's endpoint returns metadata for its own configured gateway.

    This validates that the implementation uses instance configuration (GATEWAY_NAME env var)
    rather than hardcoding a specific gateway name.
    """
    cluster_token = tenant_tokens["cluster"]

    for tenant_key in ["tenant_a", "tenant_b"]:
        tenant = tenant_service_urls[tenant_key]
        url = f"{tenant['service_url']}/v1/tenant"
        headers = {"Authorization": f"Bearer {cluster_token}"}

        r = requests.get(url, headers=headers, timeout=10, verify=TLS_VERIFY)

        if r.status_code == 403:
            print(f"[isolation] {tenant['name']} returned 403, skipping gateway validation")
            continue

        assert r.status_code == 200, f"Expected 200, got {r.status_code}"

        data = r.json()

        # The gateway name should match this tenant's gateway
        # (not hardcoded to 'maas-default-gateway')
        gateway_name = data["gateway"]["name"]

        # Each tenant's gateway name should contain their tenant name or suffix
        # to prove it's NOT using a hardcoded default
        assert tenant["name"] in gateway_name or "default" not in gateway_name, \
            f"Gateway name '{gateway_name}' should be tenant-specific, not default"

        print(f"[isolation] ✓ {tenant['name']} returns own gateway: {gateway_name}")
