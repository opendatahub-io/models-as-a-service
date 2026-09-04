"""
E2E tests for GET /v1/tenants gateway discovery endpoint.

Tests cover:
- Unauthenticated access (401)
- Authenticated access with valid service account token
- Response structure validation
- Gateway metadata accuracy

These tests use kubectl exec with curl to access the internal maas-api Service,
since /v1/tenants is not exposed through the Gateway and CI runs outside the cluster.
"""

import logging
import re
import shlex
import subprocess
import json
import os
import uuid
import pytest
import requests
from conftest import TLS_VERIFY
from test_helper import E2E_CURL_IMAGE, E2E_CURL_POD_NAMESPACE, _write_ca_to_pod, get_curl_ca_bundle

log = logging.getLogger(__name__)

pytestmark = pytest.mark.xdist_group("readonly")


def _curl_pod_namespace() -> str:
    return os.environ.get("E2E_CURL_POD_NAMESPACE", E2E_CURL_POD_NAMESPACE)


def _kubectl_curl(url: str, headers: dict = None, namespace: str = None) -> tuple[int, str]:
    """
    Execute curl request from inside the cluster using kubectl exec.

    The pod is created without credentials in its spec. Credentials are
    passed only via stdin to ``kubectl exec -i`` so that they never appear
    in the Pod object stored in the Kubernetes API.

    Returns (status_code, response_body)
    """
    namespace = namespace or _curl_pod_namespace()
    pod_name = f"test-curl-{os.getpid()}-{uuid.uuid4().hex[:6]}"
    ca_cert_path, ca_bundle_content = get_curl_ca_bundle(namespace)

    try:
        # 1. Create an ephemeral pod (no credentials in spec)
        create_cmd = [
            "kubectl", "run", pod_name,
            "--restart=Never",
            f"--image={E2E_CURL_IMAGE}",
            "-n", namespace,
            "--command", "--", "sleep", "300",
        ]
        subprocess.run(create_cmd, capture_output=True, text=True, timeout=30, check=True)

        # 2. Wait for pod readiness
        wait_cmd = [
            "kubectl", "wait", "--for=condition=Ready",
            f"pod/{pod_name}", "-n", namespace, "--timeout=30s",
        ]
        subprocess.run(wait_cmd, capture_output=True, text=True, timeout=45, check=True)

        # 2.5. Write custom CA bundle into the pod if configured
        if ca_bundle_content:
            _write_ca_to_pod(pod_name, namespace, ca_bundle_content)

        # 3. Build a shell script that reads credentials from stdin
        script_lines = []
        stdin_lines = []

        if headers:
            for i, (key, value) in enumerate(headers.items()):
                script_lines.append(f"IFS= read -r HDR{i}")
                stdin_lines.append(f"{key}: {value}")

        curl_parts = ["curl", "-s", "--proto", "=https", "--cacert", ca_cert_path, "-m", "10"]
        if headers:
            for i in range(len(headers)):
                curl_parts.append(f'-H "$HDR{i}"')
        curl_parts.append('-w "\\nHTTP_CODE:%{http_code}"')
        curl_parts.append(shlex.quote(url))

        script_lines.append(" ".join(curl_parts))
        script = "\n".join(script_lines)
        stdin_data = "\n".join(stdin_lines) + "\n" if stdin_lines else None

        # 4. Execute via kubectl exec -i; credentials travel through stdin
        exec_cmd = [
            "kubectl", "exec", "-i", pod_name, "-n", namespace,
            "--", "sh", "-c", script,
        ]
        result = subprocess.run(
            exec_cmd, capture_output=True, text=True, timeout=30,
            input=stdin_data,
        )

        # 5. Parse status code from output
        output = result.stdout
        if "HTTP_CODE:" in output:
            body, code_line = output.rsplit("HTTP_CODE:", 1)
            match = re.search(r'(\d{3})', code_line)
            if match:
                return int(match.group(1)), body.strip()
            else:
                log.error(f"Could not parse HTTP code from: {code_line}")
                return 0, body.strip()
        log.error(f"kubectl exec failed (returncode={result.returncode})")
        log.error(f"stdout: {output[:500]}")
        log.error(f"stderr: {result.stderr[:500]}")
        return 0, output
    except Exception as e:
        log.error(f"kubectl curl failed: {e}")
        return 0, str(e)
    finally:
        delete_cmd = [
            "kubectl", "delete", "pod", pod_name, "-n", namespace,
            "--grace-period=0", "--force", "--wait=false",
        ]
        subprocess.run(delete_cmd, capture_output=True, text=True, timeout=15)


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
    # Skip test when Gateway is deployed in unsupported ClusterIP + Route mode
    ingress_mode = os.environ.get("INGRESS_MODE", "clusterip")
    if ingress_mode == "clusterip":
        pytest.skip(
            "Skipping when Gateway uses ClusterIP + OpenShift Route (unsupported configuration). "
            "This mixes incompatible routing paradigms. "
            "Gateway has no external hostname in spec.listeners, so /v1/tenants returns an error. "
            "Supported configuration: LoadBalancer service with hostname in spec.listeners."
        )

    url = maas_api_internal_url + "/v1/tenants"

    status_code, body = _kubectl_curl(url, headers=headers)

    log.info(f"[tenant] GET {url} (authenticated) -> HTTP {status_code}")
    print(f"[tenant] GET /v1/tenants authenticated: HTTP {status_code}")

    # The endpoint should return 200 with system:authenticated authorization
    assert status_code == 200, \
        f"Expected 200 with auth, got {status_code}: {body[:400]}"

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

    Note: This test is skipped when the Gateway is deployed with ClusterIP service
    and OpenShift Route. This configuration is not supported - it mixes incompatible
    routing paradigms (OpenShift Routes with Gateway API). In this mode, the Gateway
    has no external hostname configured in spec.listeners, so /v1/tenants returns an
    error. The supported configuration is LoadBalancer service with hostname in spec.listeners.
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
