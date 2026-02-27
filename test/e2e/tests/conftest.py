import os
import pytest
import requests

@pytest.fixture(scope="session")
def maas_api_base_url() -> str:
    url = os.environ.get("MAAS_API_BASE_URL")
    if not url:
        raise RuntimeError("MAAS_API_BASE_URL env var is required")
    return url.rstrip("/")

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
    tok = os.popen("oc whoami -t").read().strip()
    if not tok:
        raise RuntimeError("Could not obtain cluster token via `oc whoami -t`. Set TOKEN env var or login to OpenShift.")

    print(f"[token] using OC token from `oc whoami -t` (masked): {len(tok)}")
    return tok

@pytest.fixture(scope="session")
def headers(token: str):
    return {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}

@pytest.fixture(scope="session")
def model_catalog(maas_api_base_url: str, headers: dict):
    r = requests.get(f"{maas_api_base_url}/v1/models", headers=headers, timeout=45, verify=False)
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
def model_base_url(model_catalog: dict, model_id: str, maas_api_base_url: str) -> str:
    items = (model_catalog.get("data") or model_catalog.get("models") or [])
    first = items[0] if items else {}
    url = (first or {}).get("url")
    if not url:
        # Build from gateway host derived from MAAS_API_BASE_URL
        base = maas_api_base_url[:-len("/maas-api")]
        url = f"{base}/llm/{model_id}"
    return url.rstrip("/")

@pytest.fixture(scope="session")
def model_v1(model_base_url: str) -> str:
    return f"{model_base_url}/v1"

@pytest.fixture(scope="session")
def is_https(maas_api_base_url: str) -> bool:
    return maas_api_base_url.lower().startswith("https://")

@pytest.fixture(scope="session")
def model_name(model_id: str) -> str:
    """Alias so tests can request `model_name` but we reuse model_id discovery."""
    return model_id

@pytest.fixture(scope="session")
def api_keys_base_url(maas_api_base_url: str) -> str:
    """Base URL for API Keys v1 endpoints."""
    return f"{maas_api_base_url}/v1/api-keys"

@pytest.fixture(scope="session")
def api_keys_validation_url(maas_api_base_url: str) -> str:
    """URL for internal API key validation endpoint."""
    return f"{maas_api_base_url}/internal/v1/api-keys/validate"

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
    """
    print("[api_key] Creating API key for inference tests...")
    r = requests.post(
        api_keys_base_url,
        headers=headers,
        json={"name": "e2e-test-inference-key"},
        timeout=30,
        verify=False,
    )
    if r.status_code != 201:
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

