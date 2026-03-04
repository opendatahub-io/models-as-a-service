import os
import subprocess

import pytest
import requests


def _obtain_token(maas_api_base_url: str) -> str:
    """Obtain a bearer token using the best available method.

    Priority: TOKEN env > SA token (E2E_TEST_TOKEN_SA_*) > oc whoami -t with mint.
    """
    tok = os.environ.get("TOKEN", "")
    if tok:
        print(f"[token] using env TOKEN (len={len(tok)})")
        return tok

    sa_ns = os.environ.get("E2E_TEST_TOKEN_SA_NAMESPACE", "")
    sa_name = os.environ.get("E2E_TEST_TOKEN_SA_NAME", "")
    if sa_ns or sa_name:
        if not (sa_ns and sa_name):
            raise RuntimeError(
                "Set both E2E_TEST_TOKEN_SA_NAMESPACE and E2E_TEST_TOKEN_SA_NAME"
            )
        result = subprocess.run(
            ["oc", "create", "token", sa_name, "-n", sa_ns, "--duration=30m"],
            capture_output=True, text=True, timeout=30,
        )
        tok = result.stdout.strip()
        if tok:
            print(f"[token] using SA token {sa_ns}/{sa_name} (len={len(tok)})")
            return tok
        raise RuntimeError(
            f"Failed to create SA token for {sa_ns}/{sa_name}: {result.stderr}"
        )

    result = subprocess.run(
        ["oc", "whoami", "-t"], capture_output=True, text=True, timeout=30
    )
    cluster_token = result.stdout.strip() if result.returncode == 0 else ""
    if not cluster_token:
        raise RuntimeError(
            "Could not obtain token: set TOKEN env, "
            "E2E_TEST_TOKEN_SA_NAMESPACE + E2E_TEST_TOKEN_SA_NAME, "
            "or login with `oc login`"
        )

    r = requests.post(
        f"{maas_api_base_url}/v1/tokens",
        headers={"Authorization": f"Bearer {cluster_token}", "Content-Type": "application/json"},
        json={"expiration": "10m"},
        timeout=30,
        verify=False,
    )
    r.raise_for_status()
    data = r.json()
    return data["token"]


@pytest.fixture(scope="session")
def maas_api_base_url() -> str:
    url = os.environ.get("MAAS_API_BASE_URL")
    if not url:
        raise RuntimeError("MAAS_API_BASE_URL env var is required")
    return url.rstrip("/")

@pytest.fixture(scope="session")
def token(maas_api_base_url: str) -> str:
    return _obtain_token(maas_api_base_url)

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
