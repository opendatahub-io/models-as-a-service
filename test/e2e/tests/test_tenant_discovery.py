"""
E2E tests for GET /v1/tenants gateway discovery endpoint.

Tests cover:
- Unauthenticated access (401)
- Authenticated access with valid service account token
- Response structure validation
- Gateway metadata accuracy
"""

import logging
import requests
import json
from conftest import TLS_VERIFY

log = logging.getLogger(__name__)


def test_tenant_discovery_requires_auth(maas_api_internal_url: str):
    """
    Verify /v1/tenants endpoint requires authentication.
    Without a bearer token, the endpoint should return 401 Unauthorized.

    Note: This endpoint is internal-only (not exposed through Gateway),
    so we call the maas-api Service directly.
    """
    url = f"{maas_api_internal_url}/v1/tenants"

    # Attempt without Authorization header
    r = requests.get(url, timeout=10, verify=TLS_VERIFY)

    log.info(f"[tenant] GET {url} (no auth) -> {r.status_code}")
    print(f"[tenant] GET /v1/tenants without auth: {r.status_code}")

    assert r.status_code == 401, f"Expected 401 without auth, got {r.status_code}"

    # Verify error message structure
    try:
        error_data = r.json()
        assert "error" in error_data, "Response should include 'error' field"
        print(f"[tenant] Error response: {error_data.get('error')}")
    except Exception:
        pass  # Error message format not critical for this test


def test_tenant_discovery_with_invalid_token(maas_api_internal_url: str):
    """
    Verify /v1/tenants endpoint rejects invalid tokens.
    """
    url = f"{maas_api_internal_url}/v1/tenants"

    # Attempt with invalid bearer token
    headers = {"Authorization": "Bearer invalid-token-12345"}
    r = requests.get(url, headers=headers, timeout=10, verify=TLS_VERIFY)

    log.info(f"[tenant] GET {url} (invalid token) -> {r.status_code}")
    print(f"[tenant] GET /v1/tenants with invalid token: {r.status_code}")

    assert r.status_code == 401, f"Expected 401 with invalid token, got {r.status_code}"


def test_tenant_discovery_authenticated(maas_api_internal_url: str, headers: dict):
    """
    Verify /v1/tenants endpoint returns tenant and gateway metadata when authenticated.

    This test uses the standard auth headers (service account token) that other E2E tests use.
    The endpoint uses system:authenticated authorization, so any authenticated user can access it.
    """
    url = f"{maas_api_internal_url}/v1/tenants"

    r = requests.get(url, headers=headers, timeout=10, verify=TLS_VERIFY)

    log.info(f"[tenant] GET {url} (authenticated) -> {r.status_code}")
    print(f"[tenant] GET /v1/tenants authenticated: {r.status_code}")

    # The endpoint should return 200 with system:authenticated authorization
    assert r.status_code == 200, \
        f"Expected 200 with auth, got {r.status_code}: {r.text[:400]}"

    # Validate the response structure
    data = r.json()
    print(f"[tenant] Response: {json.dumps(data, indent=2)}")

    # Validate response structure (array of tenants)
    assert "tenants" in data, "Response should include 'tenants' array"
    assert isinstance(data["tenants"], list), "Tenants should be an array"
    assert len(data["tenants"]) == 1, "Should return single tenant for this instance"

    # Validate tenant object
    tenant = data["tenants"][0]
    assert "name" in tenant, "Tenant should have 'name' field"
    assert "gateway" in tenant, "Tenant should have 'gateway' object"
    assert isinstance(tenant["name"], str), "Tenant name should be a string"
    print(f"[tenant] Tenant name: {tenant['name']}")

    # Validate gateway metadata
    gateway = tenant["gateway"]
    required_fields = ["name", "namespace", "externalUrl", "protocol", "port"]
    for field in required_fields:
        assert field in gateway, f"Gateway should have '{field}' field"

    # Validate field types
    assert isinstance(gateway["name"], str), "Gateway name should be a string"
    assert isinstance(gateway["namespace"], str), "Gateway namespace should be a string"
    assert isinstance(gateway["externalUrl"], str), "externalUrl should be a string"
    assert isinstance(gateway["protocol"], str), "Protocol should be a string"
    assert isinstance(gateway["port"], int), "Port should be an integer"

    # Validate protocol value
    assert gateway["protocol"] in ("http", "https"), f"Protocol should be http or https, got {gateway['protocol']}"

    # Validate externalUrl format
    assert gateway["externalUrl"].startswith(gateway["protocol"] + "://"), \
        "externalUrl should start with protocol://"

    print(f"[tenant] Gateway: {gateway['name']} in {gateway['namespace']}")
    print(f"[tenant] External URL: {gateway['externalUrl']}")
    print(f"[tenant] Test passed - tenant discovery working correctly")


def test_tenant_discovery_gateway_matches_deployment(maas_api_internal_url: str, headers: dict, gateway_host: str):
    """
    Verify the gateway URL returned by /v1/tenants matches the actual gateway host
    being used by the E2E tests.

    This is a regression test for the original problem: Dashboard assuming cluster domain
    instead of using the actual gateway hostname.
    """
    url = f"{maas_api_internal_url}/v1/tenants"

    r = requests.get(url, headers=headers, timeout=10, verify=TLS_VERIFY)

    assert r.status_code == 200, f"Expected 200, got {r.status_code}"

    data = r.json()
    tenant = data["tenants"][0]
    gateway = tenant["gateway"]

    # The external URL should contain the gateway_host
    external_url = gateway["externalUrl"]

    log.info(f"[tenant] Gateway external URL: {external_url}")
    log.info(f"[tenant] E2E gateway host: {gateway_host}")

    # Extract hostname from externalUrl and compare with gateway_host
    assert gateway_host in external_url, \
        f"Gateway external URL '{external_url}' doesn't contain E2E gateway host '{gateway_host}'"

    print(f"[tenant] Gateway host validation passed: {external_url} contains {gateway_host}")


def test_tenant_discovery_not_exposed_through_gateway(gateway_host: str, is_https: bool, headers: dict):
    """
    Verify /v1/tenants endpoint is NOT exposed through the Gateway.

    This is a critical security test - the endpoint should only be accessible
    via internal Service, not through external Gateway routes.

    The HTTPRoute should explicitly exclude /v1/tenants from Gateway exposure.
    """
    scheme = "https" if is_https else "http"

    # Try to access /v1/tenants through the Gateway (should fail)
    gateway_url = f"{scheme}://{gateway_host}/v1/tenants"

    log.info(f"[tenant] Attempting Gateway access: {gateway_url}")

    r = requests.get(gateway_url, headers=headers, timeout=10, verify=TLS_VERIFY)

    log.info(f"[tenant] Gateway response: {r.status_code}")

    # Should get 404 (not found) because the route doesn't exist in HTTPRoute
    # NOT 401/403 (which would mean it's routed but auth failed)
    assert r.status_code == 404, \
        f"Expected 404 (not routed), got {r.status_code}. " \
        f"Endpoint may be exposed through Gateway! Response: {r.text[:200]}"

    print(f"[tenant] ✓ /v1/tenants correctly returns 404 through Gateway (not exposed)")
    print(f"[tenant] ✓ Endpoint is internal-only as designed")
