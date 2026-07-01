import os
import socket
import subprocess
from urllib.parse import urlparse
import pytest
import requests

# TLS verification flag - set E2E_SKIP_TLS_VERIFY=true to disable cert verification
TLS_VERIFY = os.environ.get("E2E_SKIP_TLS_VERIFY", "").lower() != "true"

# When using port-forward (GATEWAY_HOST=127.0.0.1:PORT), set GATEWAY_ROUTE_HOST to the
# real route hostname so OpenShift gateway routing matches the Host header.
_GATEWAY_ROUTE_HOST = os.environ.get("GATEWAY_ROUTE_HOST", "")


def _maybe_inject_gateway_route_host(method, url, kwargs):
    if not _GATEWAY_ROUTE_HOST:
        return kwargs
    url_str = str(url)
    if "127.0.0.1" in url_str or "localhost" in url_str:
        headers = dict(kwargs.get("headers") or {})
        headers.setdefault("Host", _GATEWAY_ROUTE_HOST)
        kwargs["headers"] = headers
    return kwargs


if _GATEWAY_ROUTE_HOST:
    _orig_request = requests.api.request

    def _request(method, url, **kwargs):
        kwargs = _maybe_inject_gateway_route_host(method, url, kwargs)
        return _orig_request(method, url, **kwargs)

    requests.api.request = _request

    _orig_session_request = requests.Session.request

    def _session_request(self, method, url, **kwargs):
        kwargs = _maybe_inject_gateway_route_host(method, url, kwargs)
        return _orig_session_request(self, method, url, **kwargs)

    requests.Session.request = _session_request


@pytest.fixture(scope="session")
def gateway_host() -> str:
    """
    Gateway hostname. Primary source of truth for endpoint URLs.
    Can be set via GATEWAY_HOST or derived from MAAS_API_BASE_URL.
    """
    host = os.environ.get("GATEWAY_HOST", "")
    if host:
        return host
    
    # Fall back to deriving from MAAS_API_BASE_URL
    url = os.environ.get("MAAS_API_BASE_URL", "")
    if url:
        # Extract host from https://host/maas-api
        url = url.rstrip("/")
        if url.endswith("/maas-api"):
            url = url[:-len("/maas-api")]
        # Remove scheme
        if "://" in url:
            url = url.split("://", 1)[1]
        return url
    
    raise RuntimeError("GATEWAY_HOST or MAAS_API_BASE_URL env var is required")


@pytest.fixture(scope="session", autouse=True)
def _verify_gateway_dns(gateway_host: str):
    """Verify the gateway hostname resolves before running any tests.

    If DNS resolution fails, all tests are skipped. This catches CI
    infrastructure issues (e.g. cluster teardown, network isolation)
    early instead of producing cryptic ConnectionError stack traces
    in every test.
    """
    host_port = gateway_host.rsplit(":", 1)
    if len(host_port) == 2 and host_port[1].isdigit():
        host, port = host_port[0], int(host_port[1])
    else:
        host, port = gateway_host, None
    try:
        socket.getaddrinfo(host, port or 0)
    except socket.gaierror:
        pytest.skip(
            f"Gateway hostname '{gateway_host}' does not resolve "
            f"— CI infrastructure issue, skipping all tests"
        )


@pytest.fixture(scope="session")
def is_https() -> bool:
    """Check if we should use HTTPS (default) or HTTP."""
    return os.environ.get("INSECURE_HTTP", "").lower() != "true"


@pytest.fixture(scope="session")
def maas_api_base_url(gateway_host: str, is_https: bool) -> str:
    """
    MaaS API base URL. Derived from GATEWAY_HOST or MAAS_API_BASE_URL.
    """
    # If explicitly set, use it
    url = os.environ.get("MAAS_API_BASE_URL", "")
    if url:
        return url.rstrip("/")

    # Otherwise derive from gateway_host
    scheme = "https" if is_https else "http"
    return f"{scheme}://{gateway_host}/maas-api"


@pytest.fixture(scope="session")
def maas_api_internal_url() -> str:
    """
    MaaS API internal service URL (for internal-only endpoints like /v1/tenant).

    These endpoints are NOT exposed through the Gateway and must be called
    via the Kubernetes Service directly.

    Can be overridden with MAAS_API_INTERNAL_URL env var for custom deployments.
    """
    # Allow override
    url = os.environ.get("MAAS_API_INTERNAL_URL", "")
    if url:
        return url.rstrip("/")

    # Default: cluster-internal service URL
    # maas-api uses TLS on port 8443 (self-signed cert, use -k/verify=False)
    namespace = os.environ.get("MAAS_NAMESPACE", "opendatahub")
    service_name = os.environ.get("MAAS_API_SERVICE_NAME", "maas-api")
    port = os.environ.get("MAAS_API_SERVICE_PORT", "8443")

    return f"https://{service_name}.{namespace}.svc.cluster.local:{port}"

@pytest.fixture(scope="session")
def token() -> str:
    """
    Returns OC token for authenticating to MaaS API management endpoints.
    With the removal of /v1/tokens minting, OC tokens are used directly.
    """
    # Prefer TOKEN from environment (set by smoke.sh)
    tok = os.environ.get("TOKEN", "")
    if tok:
        print(f"[token] using env TOKEN (masked): {len(tok)}")
        return tok

    # Fallback: get OC token directly
    result = subprocess.run(["oc", "whoami", "-t"], capture_output=True, text=True)
    tok = result.stdout.strip()
    if not tok:
        raise RuntimeError("Could not obtain cluster token via `oc whoami -t`. Set TOKEN env var or login to OpenShift.")

    print(f"[token] using OC token from `oc whoami -t` (masked): {len(tok)}")
    return tok

@pytest.fixture(scope="session")
def headers(token: str):
    return {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}

@pytest.fixture(scope="session")
def model_catalog(maas_api_base_url: str, headers: dict):
    r = requests.get(f"{maas_api_base_url}/v1/models", headers=headers, timeout=45, verify=TLS_VERIFY)
    r.raise_for_status()
    return r.json()

@pytest.fixture(scope="session")
def model_id(model_catalog: dict):
    # Allow MODEL_NAME override
    override = os.environ.get("MODEL_NAME")
    if override:
        return override
    items = (model_catalog.get("data") or model_catalog.get("models") or [])
    if not items:
        raise RuntimeError("No models returned by catalog and MODEL_NAME not set")
    return items[0]["id"]

@pytest.fixture(scope="session")
def model_base_url(model_catalog: dict, model_id: str, gateway_url: str) -> str:
    items = (model_catalog.get("data") or model_catalog.get("models") or [])
    match = next((m for m in items if m.get("id") == model_id), None)
    if match:
        url = match.get("url")
        if url:
            path = urlparse(url).path
            return f"{gateway_url}{path}".rstrip("/")
    return f"{gateway_url}/llm/{model_id}".rstrip("/")

@pytest.fixture(scope="session")
def model_v1(model_base_url: str) -> str:
    return f"{model_base_url}/v1"

@pytest.fixture(scope="session")
def gateway_url(gateway_host: str, is_https: bool) -> str:
    """Full gateway URL (without path)."""
    scheme = "https" if is_https else "http"
    return f"{scheme}://{gateway_host}"

@pytest.fixture(scope="session")
def model_name(model_id: str) -> str:
    """Alias so tests can request `model_name` but we reuse model_id discovery."""
    return model_id

@pytest.fixture(scope="session")
def api_keys_base_url(maas_api_base_url: str) -> str:
    """Base URL for API Keys v1 endpoints."""
    return f"{maas_api_base_url}/v1/api-keys"

@pytest.fixture(scope="session")
def admin_token() -> str:
    """
    Admin token for authorization tests.
    If ADMIN_OC_TOKEN is not set, returns empty string and tests should skip.
    """
    tok = os.environ.get("ADMIN_OC_TOKEN", "")
    if tok:
        print(f"[admin_token] using env ADMIN_OC_TOKEN (masked): {len(tok)}")
    else:
        print("[admin_token] ADMIN_OC_TOKEN not set, admin tests will be skipped")
    return tok

@pytest.fixture(scope="session")
def admin_headers(admin_token: str):
    """Headers with admin token. Returns None if admin_token is empty."""
    if not admin_token:
        return None
    return {"Authorization": f"Bearer {admin_token}", "Content-Type": "application/json"}

@pytest.fixture(scope="session")
def api_key(api_keys_base_url: str, headers: dict) -> str:
    """
    Create an API key for model inference tests.
    Returns the plaintext API key (show-once pattern).
    
    Note: The key inherits the authenticated user's groups, which should include
    system:authenticated to satisfy AuthPolicy requirements for model access.
    """
    from multitenancy_helpers import response_summary

    sim_sub = os.environ.get("E2E_SIMULATOR_SUBSCRIPTION", "simulator-subscription")
    print("[api_key] Creating API key for inference tests (subscription bound at mint)...")
    r = requests.post(
        api_keys_base_url,
        headers=headers,
        json={"name": "e2e-test-inference-key", "subscription": sim_sub},
        timeout=30,
        verify=TLS_VERIFY,
    )
    # Accept both 200 and 201 as success
    if r.status_code not in (200, 201):
        raise RuntimeError(f"Failed to create API key: {response_summary(r)}")

    data = r.json()
    key = data.get("key")
    if not key:
        raise RuntimeError("API key creation response missing 'key' field")

    print("[api_key] Created API key for inference tests")
    return key

@pytest.fixture(scope="session")
def api_key_headers(api_key: str):
    """Headers with API key for model inference requests."""
    return {"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"}


@pytest.fixture(scope="session")
def shared_test_tenants(gateway_host: str, is_https: bool):
    """
    Shared tenant infrastructure for multi-tenant tests.

    Creates two AITenant instances (tenant-a, tenant-b) that persist for the
    entire test session. Tests requiring multiple tenants should use this fixture
    instead of creating their own tenants.

    Each tenant gets its own Gateway with a unique route hostname.

    Returns:
        tuple: (tenant_a_dict, tenant_b_dict) with keys:
            - name: tenant label name (e.g., "e2e-shared-a-abc123")
            - namespace: tenant namespace (e.g., "ai-tenant-e2e-shared-a-abc123")
            - base_url: maas-api URL for this tenant (derived from tenant's gateway route)
            - gateway_name: name of the Gateway CR for this tenant
            - suffix: 6-character hex suffix for unique resource names
            - policy_name: default auth policy name for this tenant
            - subscription_name: default subscription name for this tenant

    Requires:
        - AITenant CRD installed
        - Tenant namespace discovery enabled on maas-controller
    """
    from multitenancy_helpers import (
        require_aitenant_crd,
        new_named_tenant_case,
        bootstrap_aitenant_tenant,
        cleanup_discovery_case,
        wait_for_route_admitted,
    )

    require_aitenant_crd()

    # Create two persistent tenants for the session
    case_a = new_named_tenant_case("e2e-shared-a")
    case_b = new_named_tenant_case("e2e-shared-b")

    try:
        # Bootstrap both tenants (creates gateway + AITenant CR)
        for case in (case_a, case_b):
            bootstrap_aitenant_tenant(case)

        scheme = "https"
        for case in (case_a, case_b):
            route = wait_for_route_admitted(f"{case['gateway_name']}-route")
            try:
                host = route["spec"]["host"]
            except KeyError as e:
                raise RuntimeError(
                    f"Route {case['gateway_name']}-route missing expected field: {e}. "
                    f"Route structure: {route}"
                ) from e
            case["base_url"] = f"{scheme}://{host}/maas-api"

        # Add aliases to match test expectations while keeping cleanup helper keys intact.
        for case in (case_a, case_b):
            case["name"] = case["tenant_label_name"]
            case["namespace"] = case["tenant_ns"]

        yield case_a, case_b

    finally:
        cleanup_discovery_case(case_a)
        cleanup_discovery_case(case_b)
