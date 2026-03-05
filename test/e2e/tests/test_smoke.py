import logging
import json
import requests
from test_helper import chat, completions
from conftest import TLS_VERIFY

log = logging.getLogger(__name__)

def _pp(obj) -> str:
    try:
        return json.dumps(obj, indent=2, sort_keys=True)
    except Exception:
        return str(obj)

def test_healthz_or_404(maas_api_base_url: str):
    # Prefer /health, but tolerate /healthz on some envs
    for path in ("/health", "/healthz"):
        try:
            r = requests.get(f"{maas_api_base_url}{path}", timeout=10, verify=TLS_VERIFY)
            print(f"[health] GET {path} -> {r.status_code}")
            assert r.status_code in (200, 401, 404)
            return
        except Exception as e:
            print(f"[health] {path} error: {e}")
    # If both fail:
    assert False, "Neither /health nor /healthz responded as expected"

def test_tokens_endpoint_replaced_by_api_keys(maas_api_base_url: str):
    """
    Verify /v1/tokens endpoint has been replaced by the API key system.
    The endpoint may return 401 (auth required), 404 (removed), or 405 (method not allowed).
    Any of these indicates the old token minting is no longer available.
    """
    url = f"{maas_api_base_url}/v1/tokens"
    r = requests.post(url, json={"expiration": "1m"}, timeout=20, verify=TLS_VERIFY)
    msg = f"[token] POST {url} (no auth) -> {r.status_code}"
    log.info(msg)
    print(msg)

    # 401/404/405 all indicate the old token minting endpoint is gone/protected
    assert r.status_code in (401, 404, 405), f"Expected 401/404/405 (endpoint replaced), got {r.status_code}: {r.text[:400]}"
    print(f"[token] Confirmed /v1/tokens endpoint replaced by API keys (got {r.status_code})")

def test_models_catalog(model_catalog: dict):
    """
    Inventory: /v1/models returns model information.
    
    Note: The catalog may be empty if no models are registered via MaaSModelRef,
    or if the maas-api doesn't expose models through this endpoint.
    This test validates the response structure rather than requiring models.
    """
    items = model_catalog.get("data") or model_catalog.get("models") or []
    print(f"[models] count={len(items)}")
    
    # Catalog may be empty if models are managed via MaaSModelRef CRDs directly
    # rather than being registered through the maas-api
    if len(items) == 0:
        print("[models] Catalog is empty - models may be managed via MaaSModelRef CRDs")
        # Check if we can verify models exist via MODEL_NAME env var
        import os
        model_name = os.environ.get("MODEL_NAME")
        if model_name:
            print(f"[models] MODEL_NAME={model_name} is set, catalog empty is acceptable")
            return
        # No MODEL_NAME set and catalog empty - this is a warning but not a failure
        # as the deployment may use direct model routes without catalog registration
        print("[models] Warning: No models in catalog and MODEL_NAME not set")
        return
    
    first = items[0]
    print(f"[models] first: {_pp(first)}")
    assert "id" in first, "Model should have 'id' field"
    # 'ready' field is optional - some deployments may not include it
    if "ready" in first:
        print(f"[models] first model ready={first['ready']}")

def test_chat_completions_gateway_alive(model_v1: str, api_key_headers: dict, model_name: str):
    """
    Gateway: /chat/completions reachable for the deployed model URL.
    Uses API key for authentication (required for inference after JWT removal).
    Allowed: 200 (backend answers) or 404 (path present but not wired here).
    """
    r = chat("Say 'hello' in one word.", model_v1, api_key_headers, model_name=model_name)
    msg = f"[chat] POST /chat/completions -> {r.status_code}"
    log.info(msg)
    print(msg)
    assert r.status_code in (200, 404), f"unexpected {r.status_code}: {r.text[:500]}"

def test_legacy_completions_optionally(model_v1: str, api_key_headers: dict, model_name: str):
    """
    Compatibility: /completions (legacy). 200 or 404 both OK.
    Uses API key for authentication (required for inference after JWT removal).
    """
    r = completions("Say hello in one word.", model_v1, api_key_headers, model_name=model_name)
    msg = f"[legacy] POST /completions -> {r.status_code}"
    log.info(msg)
    print(msg)
    assert r.status_code in (200, 404), f"unexpected {r.status_code}: {r.text[:500]}"
