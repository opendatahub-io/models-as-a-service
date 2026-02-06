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
def token(maas_api_base_url: str) -> str:
    """
    MaaS token for API authentication.
    
    Token is typically provided via TOKEN env var (set by smoke.sh).
    
    Fallback minting via oc whoami -t works for:
    - Real users (Kubernetes identity tokens)
    - Service accounts from maas-ci-test namespace (CI bypass policy)
    
    Fallback will FAIL for other service accounts due to security policy
    that prevents SA tokens from minting new tokens.
    
    See: deployment/base/maas-api/policies/auth-policy-token-issuance.yaml
    See: test/e2e/scripts/ci-auth-policy-bypass.yaml (CI exception)
    """
    tok = os.environ.get("TOKEN", "")
    if tok:
        print(f"[token] using env TOKEN (len={len(tok)})")
        return tok

    # Fallback: try to mint token
    # Works for: real users, SAs from maas-ci-test namespace (CI bypass)
    # Fails for: other service accounts (security policy)
    print("[token] TOKEN env var not set, attempting fallback mint...")
    
    free = os.popen("oc whoami -t").read().strip()
    if not free:
        raise RuntimeError("Could not obtain cluster token via `oc whoami -t`")
    
    r = requests.post(
        f"{maas_api_base_url}/v1/tokens",
        headers={"Authorization": f"Bearer {free}", "Content-Type": "application/json"},
        json={"expiration": "10m"},
        timeout=30,
        verify=False,
    )
    if r.status_code == 403:
        raise RuntimeError(
            "Token minting failed with 403 Forbidden. "
            "SA tokens can only mint if from maas-ci-test namespace (CI bypass policy). "
            "For other SAs, ensure smoke.sh exports TOKEN before running pytest."
        )
    r.raise_for_status()
    data = r.json()
    return data["token"]

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

