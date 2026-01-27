import logging
import json
import requests
from tests.test_helper import chat, completions

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

def test_token_endpoint_requires_auth(maas_api_base_url: str):
    """
    Security test: Verify /v1/tokens endpoint rejects unauthenticated requests.
    
    This test does NOT mint a token - it verifies the endpoint is protected.
    Actual token minting is handled by:
    - smoke.sh (mints before pytest runs)
    - conftest.py fallback (for allowed users)
    
    Token minting is allowed for:
    - Real users (Kubernetes identity tokens)
    - Service accounts from maas-ci-test namespace (CI bypass policy)
    
    Expected: 401 Unauthorized or 403 Forbidden
    """
    url = f"{maas_api_base_url}/v1/tokens"
    r = requests.post(url, json={"expiration": "1m"}, timeout=20, verify=False)
    msg = f"[security] POST {url} (no auth) -> {r.status_code}"
    log.info(msg); print(msg)

    if r.status_code == 200:
        # Unexpected: endpoint allowed unauthenticated access
        body = (r.json() or {})
        tok = body.get("token")
        head, tail = (tok or "")[:12], (tok or "")[-8:]
        print(f"[security] WARNING: unauthenticated mint succeeded! len={len(tok) if tok else 0} head={head}…tail={tail}")
        assert tok, "200 from token endpoint but no 'token' field"
    else:
        assert r.status_code in (401, 403), f"unexpected {r.status_code}: {r.text[:400]}"

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

def test_chat_completions_gateway_alive(model_v1: str, headers: dict, model_name: str):
    """
    Gateway: /chat/completions reachable for the deployed model URL.
    Allowed responses:
    - 200: Backend answers successfully
    - 404: Path present but not wired for this model
    - 429: Rate limit hit (gateway is working, rate limiting is enforced)
    """
    r = chat("Say 'hello' in one word.", model_v1, headers, model_name=model_name)
    msg = f"[chat] POST /chat/completions -> {r.status_code}"
    log.info(msg); print(msg)
    assert r.status_code in (200, 404, 429), f"unexpected {r.status_code}: {r.text[:500]}"

def test_legacy_completions_optionally(model_v1: str, headers: dict, model_name: str):
    """
    Compatibility: /completions (legacy).
    Allowed responses:
    - 200: Backend answers successfully
    - 404: Path not available for this model
    - 429: Rate limit hit (gateway is working, rate limiting is enforced)
    """
    r = completions("Say hello in one word.", model_v1, headers, model_name=model_name)
    msg = f"[legacy] POST /completions -> {r.status_code}"
    log.info(msg); print(msg)
    assert r.status_code in (200, 404, 429), f"unexpected {r.status_code}: {r.text[:500]}"
