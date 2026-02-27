import logging
import json
import requests
from test_helper import chat, completions

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
            r = requests.get(f"{maas_api_base_url}{path}", timeout=10, verify=False)
            print(f"[health] GET {path} -> {r.status_code}")
            assert r.status_code in (200, 401, 404)
            return
        except Exception as e:
            print(f"[health] {path} error: {e}")
    # If both fail:
    assert False, "Neither /health nor /healthz responded as expected"

def test_tokens_endpoint_removed(maas_api_base_url: str):
    """
    Verify /v1/tokens endpoint has been removed (replaced by API key system).
    Expect 404 since the endpoint no longer exists.
    """
    url = f"{maas_api_base_url}/v1/tokens"
    r = requests.post(url, json={"expiration": "1m"}, timeout=20, verify=False)
    msg = f"[token] POST {url} (no auth) -> {r.status_code}"
    log.info(msg); print(msg)

    assert r.status_code == 404, f"Expected 404 (endpoint removed), got {r.status_code}: {r.text[:400]}"
    print("[token] Confirmed /v1/tokens endpoint has been removed (404)")

def test_models_catalog(model_catalog: dict):
    """
    Inventory: /v1/models returns a non-empty list with id/ready.
    """
    items = model_catalog.get("data") or model_catalog.get("models") or []
    print(f"[models] count={len(items)}")
    assert isinstance(items, list) and len(items) >= 1
    first = items[0]
    print(f"[models] first: {_pp(first)}")
    assert "id" in first and "ready" in first

def test_chat_completions_gateway_alive(model_v1: str, api_key_headers: dict, model_name: str):
    """
    Gateway: /chat/completions reachable for the deployed model URL.
    Uses API key for authentication (required for inference after JWT removal).
    Allowed: 200 (backend answers) or 404 (path present but not wired here).
    """
    r = chat("Say 'hello' in one word.", model_v1, api_key_headers, model_name=model_name)
    msg = f"[chat] POST /chat/completions -> {r.status_code}"
    log.info(msg); print(msg)
    assert r.status_code in (200, 404), f"unexpected {r.status_code}: {r.text[:500]}"

def test_legacy_completions_optionally(model_v1: str, api_key_headers: dict, model_name: str):
    """
    Compatibility: /completions (legacy). 200 or 404 both OK.
    Uses API key for authentication (required for inference after JWT removal).
    """
    r = completions("Say hello in one word.", model_v1, api_key_headers, model_name=model_name)
    msg = f"[legacy] POST /completions -> {r.status_code}"
    log.info(msg); print(msg)
    assert r.status_code in (200, 404), f"unexpected {r.status_code}: {r.text[:500]}"
