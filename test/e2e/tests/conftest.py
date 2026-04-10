import os
import subprocess
import pytest
import requests

# TLS verification flag - set E2E_SKIP_TLS_VERIFY=true to disable cert verification
TLS_VERIFY = os.environ.get("E2E_SKIP_TLS_VERIFY", "").lower() != "true"


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
            return url.rstrip("/")
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
        raise RuntimeError(f"Failed to create API key: {r.status_code} {r.text}")

    data = r.json()
    key = data.get("key")
    if not key:
        raise RuntimeError("API key creation response missing 'key' field")

    print(f"[api_key] Created API key id={data.get('id')}, key prefix={key[:15]}...")
    return key

@pytest.fixture(scope="session")
def api_key_headers(api_key: str):
    """Headers with API key for model inference requests."""
    return {"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"}

