"""
E2E tests for GET /v1/tenants gateway discovery endpoint.

Tests cover:
- Unauthenticated access (401)
- Authenticated access with valid service account token
- Response structure validation
- Gateway metadata accuracy
- OpenShift Route fallback for ClusterIP Gateways

These tests use kubectl exec with curl to access the internal maas-api Service,
since /v1/tenants is not exposed through the Gateway and CI runs outside the cluster.
"""

import json
import logging
import os
from urllib.parse import urlsplit

import pytest
import requests
from test_helper import _request_with_gateway_retry
from test_helper import kubectl_curl as _kubectl_curl

log = logging.getLogger(__name__)

pytestmark = [pytest.mark.xdist_group("readonly"), pytest.mark.smoke]


def test_tenant_discovery_requires_auth(maas_api_internal_url: str):
    """
    Verify /v1/tenants endpoint requires authentication.
    Without a bearer token, the endpoint should return 401 Unauthorized.

    Note: This endpoint is internal-only (not exposed through Gateway),
    so we use kubectl run with curl to access it from inside the cluster.
    """
    url = maas_api_internal_url + "/v1/tenants"
    # Attempt without Authorization header
    status_code, body = _kubectl_curl(url)

    log.info(f"[tenant] GET {url} (no auth) -> HTTP {status_code}")
    print(f"[tenant] GET /v1/tenants without auth: HTTP {status_code}")

    assert status_code == 401, f"Expected 401 without auth, got {status_code}"

    # Verify error message structure if JSON
    try:
        error_data = json.loads(body)
        if "error" in error_data:
            print(f"[tenant] Error response: {error_data.get('error')}")
    except Exception:
        pass  # Error message format not critical for this test


def test_tenant_discovery_with_invalid_token(maas_api_internal_url: str):
    """
    Verify /v1/tenants endpoint rejects invalid tokens.
    """
    url = maas_api_internal_url + "/v1/tenants"
    # Attempt with invalid bearer token
    headers = {"Authorization": "Bearer invalid-token-12345"}
    status_code, body = _kubectl_curl(url, headers=headers)

    log.info(f"[tenant] GET {url} (invalid token) -> HTTP {status_code}")
    print(f"[tenant] GET /v1/tenants with invalid token: HTTP {status_code}")

    assert status_code == 401, f"Expected 401 with invalid token, got {status_code}"


def test_tenant_discovery_authenticated(maas_api_internal_url: str, headers: dict):
    """
    Verify /v1/tenants endpoint returns tenant and gateway metadata when authenticated.

    This test uses the standard auth headers (service account token) that other E2E tests use.
    The endpoint uses system:authenticated authorization, so any authenticated user can access it.
    """
    url = maas_api_internal_url + "/v1/tenants"

    status_code, body = _kubectl_curl(url, headers=headers)

    log.info(f"[tenant] GET {url} (authenticated) -> HTTP {status_code}")
    print(f"[tenant] GET /v1/tenants authenticated: HTTP {status_code}")

    # The endpoint should return 200 with system:authenticated authorization
    assert status_code == 200, (
        f"Expected 200 with auth, got {status_code}: {body[:400]}"
    )

    # Validate the response structure
    data = json.loads(body)
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
    assert gateway["protocol"] in ("http", "https"), (
        f"Protocol should be http or https, got {gateway['protocol']}"
    )

    # Validate externalUrl format
    assert gateway["externalUrl"].startswith(gateway["protocol"] + "://"), (
        "externalUrl should start with protocol://"
    )

    print(f"[tenant] Gateway: {gateway['name']} in {gateway['namespace']}")
    print(f"[tenant] External URL: {gateway['externalUrl']}")
    print("[tenant] Test passed - tenant discovery working correctly")


def test_tenant_discovery_openshift_route_fallback(
    maas_api_internal_url: str, headers: dict, gateway_host: str
):
    """
    Verify /v1/tenants resolves a cluster-local Gateway through its OpenShift Route.
    """
    if os.environ.get("INGRESS_MODE", "loadbalancer") != "ocproute":
        pytest.skip(
            "OpenShift Route fallback is exercised only in INGRESS_MODE=ocproute"
        )

    status_code, body = _kubectl_curl(
        maas_api_internal_url + "/v1/tenants", headers=headers
    )

    assert status_code == 200, (
        f"Expected 200 from tenant discovery, got {status_code}: {body[:400]}"
    )

    gateway = json.loads(body)["tenants"][0]["gateway"]
    external_host = urlsplit(gateway["externalUrl"]).hostname

    assert external_host == gateway_host, (
        f"Expected OpenShift Route host {gateway_host!r}, got {external_host!r}"
    )
    assert gateway["protocol"] == "https"
    assert gateway["port"] == 443
    assert not external_host.endswith(".svc.cluster.local")


def test_tenant_discovery_gateway_matches_deployment(
    maas_api_internal_url: str, headers: dict, gateway_host: str
):
    """
    Verify the gateway URL returned by /v1/tenants matches the actual gateway host
    being used by the E2E tests.

    This is a regression test for the original problem: Dashboard assuming cluster domain
    instead of using the actual gateway hostname.

    In ocproute mode, the Gateway has no external hostname in spec.listeners; tenant
    discovery must fall back to the admitted OpenShift Route that fronts the Gateway
    Service.
    """
    url = maas_api_internal_url + "/v1/tenants"

    status_code, body = _kubectl_curl(url, headers=headers)

    assert status_code == 200, f"Expected 200, got {status_code}"

    data = json.loads(body)
    tenant = data["tenants"][0]
    gateway = tenant["gateway"]

    # The external URL should contain the gateway_host
    external_url = gateway["externalUrl"]

    log.info(f"[tenant] Gateway external URL: {external_url}")
    log.info(f"[tenant] E2E gateway host: {gateway_host}")

    # Extract hostname from externalUrl and compare with gateway_host
    assert gateway_host in external_url, (
        f"Gateway external URL '{external_url}' doesn't contain E2E gateway host '{gateway_host}'"
    )

    print(
        f"[tenant] Gateway host validation passed: {external_url} contains {gateway_host}"
    )


def test_tenant_discovery_not_exposed_through_gateway(
    gateway_host: str, is_https: bool, headers: dict
):
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

    # A 404 is the expected answer (path not routed). Transient gateway 5xx
    # (empty/proxy 500-503 from Envoy or auth filters mid-reload) is a known
    # propagation flake — retry it rather than failing the security test.
    # Empty 401/403 is NOT retried: for this test it means the path is routed
    # and auth rejected the request, i.e. the exposure the test guards against.
    r = _request_with_gateway_retry(
        requests.get,
        gateway_url,
        headers=headers,
        timeout=10,
        auth_denial_is_transient=False,
    )

    log.info(f"[tenant] Gateway response: {r.status_code}")

    # Should get 404 (not found) because the route doesn't exist in HTTPRoute
    # NOT 401/403 (which would mean it's routed but auth failed)
    assert r.status_code == 404, (
        f"Expected 404 (not routed), got {r.status_code}. "
        f"Endpoint may be exposed through Gateway! Response: {r.text[:200]}"
    )

    print("[tenant] ✓ /v1/tenants correctly returns 404 through Gateway (not exposed)")
    print("[tenant] ✓ Endpoint is internal-only as designed")
