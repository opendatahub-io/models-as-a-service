"""
Shared test helpers/fixtures for MaaS billing tests.

What this file provides:
- http(requests.Session)       -> respects REQUESTS_VERIFY and INGRESS_CA_PATH (default verify=True)
- base_url                     -> from $MAAS_API_BASE_URL (skips suite if not set)
- model_name                   -> from $MODEL_NAME
- bearer(token)                -> {"Authorization": f"Bearer <token>"}
- ensure_free_key/ensure_premium_key:
    Try to mint a MaaS JWT via /v1/tokens or /tokens (with/without body).
    If minting isn't available, fall back to the OC token so tests still run.
- mint_maas_key/revoke_maas_key for explicit control in tests
- parse_usage_headers()        -> reads x-odhu-usage-* headers
- get_limit(env_name, fallback_key, default_val):
    env override -> cluster CR discovery (RLP/TRLP) -> default
- Simple http_get/http_post wrappers (optional convenience)
"""

from __future__ import annotations
import os, json, base64, subprocess
import pytest, requests

# -------------------------- Env & constants --------------------------

BASE_URL         = os.getenv("MAAS_API_BASE_URL", "").rstrip("/")
MODEL_NAME       = os.getenv("MODEL_NAME")
FREE_OC_TOKEN    = os.getenv("FREE_OC_TOKEN", "")
PREMIUM_OC_TOKEN = os.getenv("PREMIUM_OC_TOKEN", "")
USAGE_API_BASE   = os.getenv("USAGE_API_BASE", BASE_URL)

# Optional custom CA for HTTPS clusters; not required for HTTP
INGRESS_CA_PATH  = os.getenv("INGRESS_CA_PATH", "")

USAGE_HEADERS = [
    "x-odhu-usage-input-tokens",
    "x-odhu-usage-output-tokens",
    "x-odhu-usage-total-tokens",
]

def _env_bool(name: str, default: bool) -> bool:
    v = os.getenv(name)
    if v is None:
        return default
    return str(v).strip().lower() not in ("0", "false", "no", "off")

# -------------------------- Pytest fixtures --------------------------

@pytest.fixture(scope="session")
def base_url():
    if not BASE_URL:
        pytest.skip("MAAS_API_BASE_URL not set; skipping MaaS billing tests", allow_module_level=True)
    return BASE_URL

@pytest.fixture(scope="session")
def model_name():
    return MODEL_NAME

@pytest.fixture(scope="session")
def http() -> requests.Session:
    s = requests.Session()
    # Default verify=True; allow opt-out via env; allow custom CA path if provided
    verify_default = True
    verify_env = _env_bool("REQUESTS_VERIFY", True)
    verify: bool | str = verify_env
    if INGRESS_CA_PATH and os.path.exists(INGRESS_CA_PATH):
        # If a CA bundle is provided, prefer it (typical for OpenShift HTTPS ingress)
        verify = INGRESS_CA_PATH
    else:
        verify = verify_env if verify_env is not None else verify_default
    s.verify = verify
    return s

# -------------------------- HTTP helpers -----------------------------

def http_get(http: requests.Session, url: str, headers=None, timeout=60):
    r = http.get(url, headers=headers or {}, timeout=timeout)
    try:
        body = r.json()
    except Exception:
        body = r.text
    return r.status_code, body, r

def http_post(http: requests.Session, url: str, headers=None, json=None, data=None, timeout=60):
    r = http.post(url, headers=headers or {}, json=json, data=data, timeout=timeout)
    try:
        body = r.json()
    except Exception:
        body = r.text
    return r.status_code, body, r

def bearer(tok: str) -> dict:
    return {"Authorization": f"Bearer {tok}"} if tok else {}

# -------------------------- Token mint/revoke ------------------------

def _looks_like_jwt(tok: str) -> bool:
    parts = tok.split(".")
    if len(parts) != 3:
        return False
    try:
        base64.urlsafe_b64decode(parts[0] + "===")
        base64.urlsafe_b64decode(parts[1] + "===")
        return True
    except Exception:
        return False

def _try_mint_maas_key(http: requests.Session, base_url: str, oc_user_token: str, minutes=10) -> str | None:
    """
    Try several permutations commonly seen across clusters:
    - POST /v1/tokens and /tokens
    - with bodies: {"ttl": "10m"}, {"expiration": "10m"}, {}, and no body
    Return the minted token or None if not available.
    """
    eps = ["/v1/tokens", "/tokens"]
    bodies = [{"ttl": f"{minutes}m"}, {"expiration": f"{minutes}m"}, {}, None]
    for ep in eps:
        url = f"{base_url.rstrip('/')}{ep}"
        for body in bodies:
            try:
                r = http.post(url, headers=bearer(oc_user_token), json=body, timeout=60)
            except Exception:
                continue
            if r.status_code in (200, 201):
                try:
                    j = r.json()
                    tok = j.get("token") or j.get("access_token")
                    if tok:
                        return tok
                except Exception:
                    pass
            if r.status_code in (404, 405):
                # endpoint not present or method not allowed → try next ep
                break
    return None

def mint_maas_key(http: requests.Session, base_url: str, oc_user_token: str, minutes=10) -> str:
    tok = _try_mint_maas_key(http, base_url, oc_user_token, minutes=minutes)
    if tok:
        return tok
    raise AssertionError("Could not mint a MaaS key with any common body/endpoint variant")

def revoke_maas_key(http: requests.Session, base_url: str, oc_user_token: str, token: str | None = None):
    # Some clusters revoke by calling DELETE on the token endpoint (token not always needed)
    last = None
    for ep in ("/v1/tokens", "/tokens"):
        url = f"{base_url.rstrip('/')}{ep}"
        try:
            last = http.delete(url, headers=bearer(oc_user_token), timeout=60)
        except Exception:
            continue
        if last.status_code in (200, 202, 204):
            return last
    return last

def ensure_free_key(http: requests.Session) -> str:
    """
    Preferred: a minted MaaS JWT. Fallback: the OC token (if cluster accepts Bearer OC).
    """
    assert FREE_OC_TOKEN, "FREE_OC_TOKEN not set (export your current user's oc token)"
    assert BASE_URL, "MAAS_API_BASE_URL not set"
    minted = _try_mint_maas_key(http, BASE_URL, FREE_OC_TOKEN, minutes=10)
    if minted and _looks_like_jwt(minted):
        return minted
    return FREE_OC_TOKEN

def ensure_premium_key(http: requests.Session) -> str:
    """
    Preferred: a minted MaaS JWT. Fallback: the OC token (if cluster accepts Bearer OC).
    """
    assert PREMIUM_OC_TOKEN, "PREMIUM_OC_TOKEN not set (export your premium user's oc token)"
    assert BASE_URL, "MAAS_API_BASE_URL not set"
    minted = _try_mint_maas_key(http, BASE_URL, PREMIUM_OC_TOKEN, minutes=10)
    if minted and _looks_like_jwt(minted):
        return minted
    return PREMIUM_OC_TOKEN

@pytest.fixture
def maas_key(http: requests.Session):
    return ensure_free_key(http)

# -------------------------- Usage headers helper ---------------------

def parse_usage_headers(resp) -> dict:
    out = {}
    for h in USAGE_HEADERS:
        v = resp.headers.get(h) or resp.headers.get(h.title())
        if v is not None:
            try:
                out[h] = int(v)
            except Exception:
                out[h] = v
    return out

# -------------------------- Cluster policy discovery -----------------

def _get_json(ns: str, kind: str, name: str):
    try:
        out = subprocess.run(
            ["oc", "-n", ns, "get", kind, name, "-o", "json"],
            capture_output=True, text=True, check=True
        ).stdout
        return json.loads(out)
    except Exception:
        return {}

def _first_existing(ns: str, kinds: list[str], name: str):
    for k in kinds:
        d = _get_json(ns, k, name)
        if d:
            return d
    return {}

def policy_from_cluster():
    # Try both API groups for each CRD
    rlp = _first_existing(
        "openshift-ingress",
        ["ratelimitpolicies.gateway.networking.k8s.io",
         "ratelimitpolicies.kuadrant.io"],
        "gateway-rate-limits",
    )
    trlp = _first_existing(
        "openshift-ingress",
        ["tokenratelimitpolicies.gateway.networking.k8s.io",
         "tokenratelimitpolicies.kuadrant.io"],
        "gateway-default-deny",
    )
    return {
        "free_burst":     (rlp or {}).get("spec", {}).get("limits", {}).get("free", {}).get("rates", [{}])[0].get("limit"),
        "premium_burst":  (rlp or {}).get("spec", {}).get("limits", {}).get("premium", {}).get("rates", [{}])[0].get("limit"),
        "free_tokens":    (trlp or {}).get("spec", {}).get("limits", {}).get("free-user-tokens", {}).get("rates", [{}])[0].get("limit"),
        "premium_tokens": (trlp or {}).get("spec", {}).get("limits", {}).get("premium-user-tokens", {}).get("rates", [{}])[0].get("limit"),
    }

POLICY = policy_from_cluster()

def get_limit(env_name: str, fallback_key: str, default_val):
    """
    Prefer env override → then cluster policy (POLICY[fallback_key]) → default.
    Examples:
      get_limit("RATE_LIMIT_BURST_FREE", "free_burst", 16)
      get_limit("RATE_LIMIT_BURST_PREMIUM", "premium_burst", 32)
      get_limit("TOKEN_LIMIT_FREE", "free_tokens", 1000)
      get_limit("TOKEN_LIMIT_PREMIUM", "premium_tokens", 2000)
    """
    v = os.getenv(env_name)
    if v:
        try:
            return int(v)
        except Exception:
            return default_val
    return POLICY.get(fallback_key) or default_val

# --- new: tool-calling -

@pytest.fixture(scope="session")
def tool_calling_enabled() -> bool:
    return os.getenv("TOOL_CALLING_ENABLED", "false").lower() in ("1","true","yes")

# @pytest.fixture(scope="session")
# def tools_spec():
#     return [
#         {
#             "type": "function",
#             "function": {
#                 "name": "get_weather",
#                 "description": "Get current weather for a city/location.",
#                 "parameters": {
#                     "type": "object",
#                     "properties": {
#                         "city": {"type": "string"},
#                         "location": {"type": "string"},
#                         "unit": {"type": "string", "enum": ["celsius", "fahrenheit"]},
#                     },
#                     "required": ["location"],
#                     "additionalProperties": False,
#                 },
#             },
#         }
#     ]

@pytest.fixture(scope="session")
def tools_spec():
    return [
        {
            "type": "function",
            "function": {
                "name": "get_weather",
                "description": "Get current weather for a city/location.",
                "parameters": {
                    "type": "object",
                    "properties": {
                        "location": {"type": "string", "description": "City name, e.g. Boston, MA"},
                        "city": {"type": "string", "description": "City name (legacy alias)"},
                        "unit": {"type": "string", "enum": ["celsius", "fahrenheit"]}
                    },
                    "required": ["location"],
                    "additionalProperties": False
                },
            },
        }
    ]
