"""
E2E tests for /v1/tenants endpoint data isolation.

Verifies that each tenant's maas-api instance returns its own configuration
and does not leak data from other tenants. With system:authenticated authorization,
any authenticated user can call any tenant's endpoint, but each endpoint must
return only its own tenant's data.

These tests use kubectl run with curl to access internal Service URLs.
"""

import logging
import pytest
import subprocess
import json
import os

from conftest import TLS_VERIFY
from test_helper import E2E_CURL_POD_NAMESPACE, MAAS_API_DEPLOYMENT_NAMESPACE, _get_cluster_token

log = logging.getLogger(__name__)


def _kubectl_curl(url: str, headers: dict = None, namespace: str = None) -> tuple[int, str]:
    """Execute curl from inside cluster. Returns (status_code, response_body)"""
    namespace = namespace or os.environ.get("E2E_CURL_POD_NAMESPACE", E2E_CURL_POD_NAMESPACE)
    curl_args = ["-sk", "-m", "10"]
    if headers:
        for key, value in headers.items():
            curl_args.extend(["-H", f"{key}: {value}"])
    curl_args.extend(["-w", "\\nHTTP_CODE:%{http_code}", url])

    cmd = [
        "kubectl", "run", f"test-curl-{os.getpid()}-{id(url)}",
        "--rm", "-i", "--restart=Never",
        "--image=curlimages/curl:latest",
        "-n", namespace,
        "--", "curl"
    ] + curl_args

    try:
        result = subprocess.run(cmd, capture_output=True, text=True, timeout=30)
        output = result.stdout
        if "HTTP_CODE:" in output:
            body, code_line = output.rsplit("HTTP_CODE:", 1)
            # Extract just the numeric status code (kubectl deletion message may be appended)
            import re
            match = re.search(r'(\d{3})', code_line)
            if match:
                return int(match.group(1)), body.strip()
            else:
                log.error(f"Could not parse HTTP code from: {code_line}")
                return 0, body.strip()
        # No HTTP_CODE in output - kubectl run likely failed
        log.error(f"kubectl run failed (returncode={result.returncode})")
        log.error(f"stdout: {output[:500]}")
        log.error(f"stderr: {result.stderr[:500]}")
        return 0, output
    except Exception as e:
        log.error(f"kubectl curl failed: {e}")
        return 0, str(e)


@pytest.fixture
def tenant_service_urls(shared_test_tenants):
    """
    Get internal service URLs for each tenant's maas-api.

    Each tenant has its own maas-api deployment and service in MAAS_API_DEPLOYMENT_NAMESPACE.
    The /v1/tenants endpoint must be called via the Service (not Gateway).
    """
    tenant_a, tenant_b = shared_test_tenants

    # Construct internal service URLs
    # Format: https://{service-name}.{namespace}.svc.cluster.local:{port}
    # AITenant maas-api services are deployed in MAAS_API_DEPLOYMENT_NAMESPACE,
    # NOT in the tenant-specific namespace (ai-tenant-xxx)
    def service_url(tenant):
        # Service name follows pattern: maas-api-{tenant-name}
        service_name = f"maas-api-{tenant['name']}"
        port = "8443"  # HTTPS port - bearer tokens must not be sent over cleartext
        return f"https://{service_name}.{MAAS_API_DEPLOYMENT_NAMESPACE}.svc.cluster.local:{port}"

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
    # Skip test when Gateway is deployed in unsupported ClusterIP + Route mode
    ingress_mode = os.environ.get("INGRESS_MODE", "clusterip")
    if ingress_mode == "clusterip":
        pytest.skip(
            "Skipping when Gateway uses ClusterIP + OpenShift Route (unsupported configuration). "
            "This mixes incompatible routing paradigms. "
            "Gateway has no external hostname in spec.listeners, so /v1/tenants returns an error. "
            "Supported configuration: LoadBalancer service with hostname in spec.listeners."
        )

    for tenant_key in ["tenant_a", "tenant_b"]:
        tenant = tenant_service_urls[tenant_key]
        token = tenant_tokens["cluster"]  # Using cluster token (has access)

        url = f"{tenant['service_url']}/v1/tenants"
        headers = {"Authorization": f"Bearer {token}"}

        log.info(f"[isolation] Testing {tenant['name']} can access own endpoint")

        status_code, body = _kubectl_curl(url, headers=headers)

        # Should succeed (200) with system:authenticated authorization
        assert status_code == 200, \
            f"{tenant['name']} should access endpoint with system:authenticated, got {status_code}: {body[:400]}"

        data = json.loads(body)
        assert "tenants" in data and len(data["tenants"]) == 1, "Should return single tenant in array"
        tenant_data = data["tenants"][0]
        assert tenant_data["name"] == tenant["aitenant_name"], \
            f"Expected tenant name {tenant['aitenant_name']}, got {tenant_data['name']}"
        print(f"[isolation] ✓ {tenant['name']} can access /v1/tenants endpoint (system:authenticated)")


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
    # Skip test when Gateway is deployed in unsupported ClusterIP + Route mode
    ingress_mode = os.environ.get("INGRESS_MODE", "clusterip")
    if ingress_mode == "clusterip":
        pytest.skip(
            "Skipping when Gateway uses ClusterIP + OpenShift Route (unsupported configuration). "
            "This mixes incompatible routing paradigms. "
            "Gateway has no external hostname in spec.listeners, so /v1/tenants returns an error. "
            "Supported configuration: LoadBalancer service with hostname in spec.listeners."
        )

    tenant_a = tenant_service_urls["tenant_a"]
    tenant_b = tenant_service_urls["tenant_b"]
    cluster_token = tenant_tokens["cluster"]

    # Test: Call tenant A's endpoint
    url_a = f"{tenant_a['service_url']}/v1/tenants"
    headers = {"Authorization": f"Bearer {cluster_token}"}

    status_a, body_a = _kubectl_curl(url_a, headers=headers)

    assert status_a == 200, f"Tenant A endpoint should return 200 (system:authenticated), got {status_a}"
    data_a = json.loads(body_a)
    tenant_a_data = data_a["tenants"][0]

    # Test: Call tenant B's endpoint (same token - should also work)
    url_b = f"{tenant_b['service_url']}/v1/tenants"

    status_b, body_b = _kubectl_curl(url_b, headers=headers)

    assert status_b == 200, f"Tenant B endpoint should return 200 (system:authenticated), got {status_b}"
    data_b = json.loads(body_b)
    tenant_b_data = data_b["tenants"][0]

    # Critical: Verify no data leakage
    # Each endpoint should return its OWN tenant data, not the other's
    assert tenant_a_data["name"] == tenant_a["aitenant_name"], \
        f"Tenant A endpoint should return tenant A data, got {tenant_a_data['name']}"

    assert tenant_b_data["name"] == tenant_b["aitenant_name"], \
        f"Tenant B endpoint should return tenant B data, got {tenant_b_data['name']}"

    assert tenant_a_data["name"] != tenant_b_data["name"], \
        "Tenant A and B should return different tenant names"

    # Verify gateway isolation
    assert tenant_a_data["gateway"]["name"] != tenant_b_data["gateway"]["name"], \
        "Each tenant should have its own gateway"

    assert tenant_a_data["gateway"]["externalUrl"] != tenant_b_data["gateway"]["externalUrl"], \
        "Each tenant should have its own gateway URL"

    print(f"[isolation] ✓ Tenant A: {tenant_a_data['name']} / Gateway: {tenant_a_data['gateway']['name']}")
    print(f"[isolation] ✓ Tenant B: {tenant_b_data['name']} / Gateway: {tenant_b_data['gateway']['name']}")
    print(f"[isolation] ✓ No data leakage - each tenant returns own data")


def test_tenant_discovery_unauthorized_access(tenant_service_urls):
    """
    Verify completely unauthorized access is rejected.

    Without any token or with an invalid token, all tenant endpoints should return 401.
    """
    for tenant_key in ["tenant_a", "tenant_b"]:
        tenant = tenant_service_urls[tenant_key]
        url = f"{tenant['service_url']}/v1/tenants"

        # Test 1: No auth header
        status_code, _ = _kubectl_curl(url)
        assert status_code == 401, \
            f"{tenant['name']} should reject no-auth request with 401, got {status_code}"

        # Test 2: Invalid token
        headers = {"Authorization": "Bearer invalid-token-12345"}
        status_code, _ = _kubectl_curl(url, headers=headers)
        assert status_code == 401, \
            f"{tenant['name']} should reject invalid token with 401, got {status_code}"

    print("[isolation] ✓ Both tenants properly reject unauthorized access")


def test_tenant_discovery_each_tenant_returns_own_gateway(tenant_service_urls, tenant_tokens):
    """
    Verify each tenant's endpoint returns metadata for its own configured gateway.

    This validates that the implementation uses instance configuration (GATEWAY_NAME env var)
    rather than hardcoding a specific gateway name.
    """
    # Skip test when Gateway is deployed in unsupported ClusterIP + Route mode
    ingress_mode = os.environ.get("INGRESS_MODE", "clusterip")
    if ingress_mode == "clusterip":
        pytest.skip(
            "Skipping when Gateway uses ClusterIP + OpenShift Route (unsupported configuration). "
            "This mixes incompatible routing paradigms. "
            "Gateway has no external hostname in spec.listeners, so /v1/tenants returns an error. "
            "Supported configuration: LoadBalancer service with hostname in spec.listeners."
        )

    cluster_token = tenant_tokens["cluster"]

    for tenant_key in ["tenant_a", "tenant_b"]:
        tenant = tenant_service_urls[tenant_key]
        url = f"{tenant['service_url']}/v1/tenants"
        headers = {"Authorization": f"Bearer {cluster_token}"}

        status_code, body = _kubectl_curl(url, headers=headers)

        # With system:authenticated authorization, 403 indicates auth/RBAC regression
        assert status_code == 200, \
            f"Expected 200 (system:authenticated), got {status_code}: {body[:400]}"

        data = json.loads(body)
        tenant_data = data["tenants"][0]

        # The gateway name should match this tenant's gateway
        # (not hardcoded to 'maas-default-gateway')
        gateway_name = tenant_data["gateway"]["name"]

        # Each tenant's gateway name should contain their tenant name or suffix
        # to prove it's NOT using a hardcoded default
        assert tenant["name"] in gateway_name or "default" not in gateway_name, \
            f"Gateway name '{gateway_name}' should be tenant-specific, not default"

        print(f"[isolation] ✓ {tenant['name']} returns own gateway: {gateway_name}")
