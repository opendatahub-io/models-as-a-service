"""
Shared helpers and constants for MaaS E2E tests.

This module centralizes common utilities used across multiple test files:
- Environment-based constants (timeouts, model refs, namespaces)
- Cluster authentication (OC tokens, service account tokens)
- API key management (create, revoke)
- Custom Resource management (apply, delete, get, list, snapshot)
- Inference helpers (send requests, poll for expected status)
- Wait/polling utilities (reconciliation, CR readiness, phase checks)
- CR creation helpers (MaaSAuthPolicy, MaaSSubscription)

Environment variables (all optional unless noted):
  - GATEWAY_HOST: Gateway hostname (required)
  - MAAS_API_BASE_URL: MaaS API URL (auto-derived from GATEWAY_HOST if not set)
  - MAAS_SUBSCRIPTION_NAMESPACE: MaaS CRs namespace (default: models-as-a-service)
  - E2E_TEST_TOKEN_SA_NAMESPACE, E2E_TEST_TOKEN_SA_NAME: SA token source for Prow
  - E2E_TIMEOUT: Request timeout in seconds (default: 45)
  - E2E_RECONCILE_WAIT: Wait time for reconciliation in seconds (default: 8)
  - E2E_SKIP_TLS_VERIFY: Set to "true" to skip TLS verification
  - E2E_MODEL_PATH: Path to free model (default: /llm/facebook-opt-125m-simulated)
  - E2E_MODEL_NAME: Model name for API requests (default: facebook/opt-125m)
  - E2E_MODEL_REF: Model ref for CRs (default: facebook-opt-125m-simulated)
  - E2E_MODEL_NAMESPACE: Namespace where models live (default: llm)
  - E2E_SIMULATOR_SUBSCRIPTION: Free-tier subscription (default: simulator-subscription)
  - E2E_PREMIUM_MODEL_REF: Premium model ref (default: premium-simulated-simulated-premium)
  - E2E_PREMIUM_SIMULATOR_SUBSCRIPTION: Premium subscription (default: premium-simulator-subscription)
  - E2E_SIMULATOR_ACCESS_POLICY: Simulator auth policy name (default: simulator-access)
  - E2E_UNCONFIGURED_MODEL_REF: Unconfigured model ref (default: e2e-unconfigured-facebook-opt-125m-simulated)
  - E2E_UNCONFIGURED_MODEL_PATH: Path to unconfigured model (default: /llm/e2e-unconfigured-facebook-opt-125m-simulated)
  - E2E_DISTINCT_MODEL_REF: First distinct model ref (default: e2e-distinct-simulated)
  - E2E_DISTINCT_MODEL_ID: Model ID for first distinct model (default: test/e2e-distinct-model)
  - E2E_DISTINCT_MODEL_2_REF: Second distinct model ref (default: e2e-distinct-2-simulated)
  - E2E_DISTINCT_MODEL_2_ID: Model ID for second distinct model (default: test/e2e-distinct-model-2)
  - E2E_TRLP_TEST_MODEL_REF: TRLP test model ref (default: e2e-trlp-test-simulated)                                                                                                   
  - E2E_TRLP_TEST_MODEL_PATH: Path to TRLP test model (default: /llm/e2e-trlp-test-simulated)
  - E2E_TRLP_TEST_MODEL_ID: Model ID for TRLP test model (default: test/e2e-trlp-test-model) 
"""

import base64
import json
import logging
import os
import subprocess
import time
import uuid
from typing import Optional

import requests

log = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Constants (override with env vars)
# ---------------------------------------------------------------------------
TIMEOUT = int(os.environ.get("E2E_TIMEOUT", "45"))
RECONCILE_WAIT = int(os.environ.get("E2E_RECONCILE_WAIT", "8"))
TLS_VERIFY = os.environ.get("E2E_SKIP_TLS_VERIFY", "").lower() != "true"
MODEL_PATH = os.environ.get("E2E_MODEL_PATH", "/llm/facebook-opt-125m-simulated")
MODEL_NAME = os.environ.get("E2E_MODEL_NAME", "facebook/opt-125m")
MODEL_REF = os.environ.get("E2E_MODEL_REF", "facebook-opt-125m-simulated")
MODEL_NAMESPACE = os.environ.get("E2E_MODEL_NAMESPACE", "llm")
SIMULATOR_SUBSCRIPTION = os.environ.get("E2E_SIMULATOR_SUBSCRIPTION", "simulator-subscription")
PREMIUM_MODEL_REF = os.environ.get("E2E_PREMIUM_MODEL_REF", "premium-simulated-simulated-premium")
PREMIUM_SIMULATOR_SUBSCRIPTION = os.environ.get("E2E_PREMIUM_SIMULATOR_SUBSCRIPTION", "premium-simulator-subscription")
SIMULATOR_ACCESS_POLICY = os.environ.get("E2E_SIMULATOR_ACCESS_POLICY", "simulator-access")
UNCONFIGURED_MODEL_REF = os.environ.get("E2E_UNCONFIGURED_MODEL_REF", "e2e-unconfigured-facebook-opt-125m-simulated")
UNCONFIGURED_MODEL_PATH = os.environ.get("E2E_UNCONFIGURED_MODEL_PATH", "/llm/e2e-unconfigured-facebook-opt-125m-simulated")
DISTINCT_MODEL_REF = os.environ.get("E2E_DISTINCT_MODEL_REF", "e2e-distinct-simulated")
DISTINCT_MODEL_ID = os.environ.get("E2E_DISTINCT_MODEL_ID", "test/e2e-distinct-model")
DISTINCT_MODEL_2_REF = os.environ.get("E2E_DISTINCT_MODEL_2_REF", "e2e-distinct-2-simulated")
DISTINCT_MODEL_2_ID = os.environ.get("E2E_DISTINCT_MODEL_2_ID", "test/e2e-distinct-model-2")
TRLP_TEST_MODEL_REF = os.environ.get("E2E_TRLP_TEST_MODEL_REF", "e2e-trlp-test-simulated")                                                                                            
TRLP_TEST_MODEL_PATH = os.environ.get("E2E_TRLP_TEST_MODEL_PATH", "/llm/e2e-trlp-test-simulated")                                                                                     
TRLP_TEST_MODEL_ID = os.environ.get("E2E_TRLP_TEST_MODEL_ID", "test/e2e-trlp-test-model") 


# ---------------------------------------------------------------------------
# Environment / URL helpers
# ---------------------------------------------------------------------------

def _ns():
    """Default MaaS subscription namespace."""
    return os.environ.get("MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service")


def _gateway_url():
    host = os.environ.get("GATEWAY_HOST", "")
    if not host:
        raise RuntimeError("GATEWAY_HOST env var is required")
    scheme = "http" if os.environ.get("INSECURE_HTTP", "").lower() == "true" else "https"
    return f"{scheme}://{host}"


def _maas_api_url():
    """Get the MaaS API base URL for API key operations."""
    url = os.environ.get("MAAS_API_BASE_URL", "")
    if not url:
        host = os.environ.get("GATEWAY_HOST", "")
        if not host:
            raise RuntimeError("MAAS_API_BASE_URL or GATEWAY_HOST env var is required")
        scheme = "http" if os.environ.get("INSECURE_HTTP", "").lower() == "true" else "https"
        url = f"{scheme}://{host}/maas-api"
    return url


# ---------------------------------------------------------------------------
# Authentication helpers
# ---------------------------------------------------------------------------

def _decode_jwt_payload(token: str) -> Optional[dict]:
    """Decode JWT payload (no verification, for debugging). Returns claims dict or None."""
    try:
        parts = token.split(".")
        if len(parts) != 3:
            return None
        payload_b64 = parts[1]
        payload_b64 += "=" * (4 - len(payload_b64) % 4)
        payload_bytes = base64.urlsafe_b64decode(payload_b64)
        return json.loads(payload_bytes)
    except Exception:
        return None


def _create_sa_token(sa_name, namespace=None, duration="10m"):
    namespace = namespace or _ns()
    sa_result = subprocess.run(
        ["oc", "create", "sa", sa_name, "-n", namespace], capture_output=True, text=True
    )
    if sa_result.returncode != 0 and "already exists" not in sa_result.stderr:
        raise RuntimeError(f"Failed to create SA {sa_name}: {sa_result.stderr}")
    result = subprocess.run(
        ["oc", "create", "token", sa_name, "-n", namespace, f"--duration={duration}"],
        capture_output=True, text=True,
    )
    token = result.stdout.strip()
    if not token:
        raise RuntimeError(f"Could not create token for SA {sa_name}: {result.stderr}")
    return token


def _delete_sa(sa_name, namespace=None):
    """Delete a service account (best-effort, for cleanup)."""
    namespace = namespace or _ns()
    result = subprocess.run(
        ["oc", "delete", "sa", sa_name, "-n", namespace, "--ignore-not-found"],
        capture_output=True,
        text=True,
        timeout=30,
    )
    if result.returncode != 0:
        log.warning(
            "Failed to delete serviceaccount/%s in %s: %s",
            sa_name,
            namespace,
            result.stderr.strip(),
        )


def _sa_to_user(sa_name, namespace=None):
    """Convert service account name to Kubernetes user principal."""
    namespace = namespace or _ns()
    return f"system:serviceaccount:{namespace}:{sa_name}"


def _get_cluster_token():
    """Get OC token for API key management operations (not for inference).

    Priority:
      1. TOKEN env var (set by prow script for regular user)
      2. E2E_TEST_TOKEN_SA_* env vars (for SA-based tokens)
      3. oc whoami -t (fallback for local testing)
    """
    token = os.environ.get("TOKEN", "")
    if token:
        log.info("Using TOKEN env var for API key operations")
        return token

    sa_ns = os.environ.get("E2E_TEST_TOKEN_SA_NAMESPACE")
    sa_name = os.environ.get("E2E_TEST_TOKEN_SA_NAME")
    if sa_ns and sa_name:
        token = _create_sa_token(sa_name, namespace=sa_ns)
    else:
        token_result = subprocess.run(["oc", "whoami", "-t"], capture_output=True, text=True)
        token = token_result.stdout.strip() if token_result.returncode == 0 else ""
        if not token:
            raise RuntimeError("Could not get cluster token via `oc whoami -t`; run with oc login first")
    claims = _decode_jwt_payload(token)
    if claims:
        safe_keys = {k: v for k, v in claims.items() if k in ("iss", "aud", "exp", "iat")}
        log.debug("Token claims (non-sensitive): %s", json.dumps(safe_keys))
    return token


# ---------------------------------------------------------------------------
# API Key Management
# ---------------------------------------------------------------------------

def _create_api_key_raw(oc_token: str, name: str = None, subscription: str = None):
    """Create an API key and return the raw response (for testing error cases).

    Args:
        oc_token: OC token for authentication with maas-api
        name: Optional name for the key (auto-generated if not provided)
        subscription: Optional MaaSSubscription name to bind (highest-priority auto-bind if omitted)

    Returns:
        requests.Response object
    """
    url = f"{_maas_api_url()}/v1/api-keys"
    key_name = name or f"e2e-test-{uuid.uuid4().hex[:8]}"

    body = {"name": key_name}
    if subscription:
        body["subscription"] = subscription

    return requests.post(
        url,
        headers={
            "Authorization": f"Bearer {oc_token}",
            "Content-Type": "application/json",
        },
        json=body,
        timeout=TIMEOUT,
        verify=TLS_VERIFY,
    )


def _create_api_key(oc_token: str, name: str = None, subscription: str = None) -> str:
    """Create an API key using the MaaS API and return the plaintext key.

    Args:
        oc_token: OC token for authentication with maas-api
        name: Optional name for the key (auto-generated if not provided)
        subscription: Optional MaaSSubscription name to bind (highest-priority auto-bind if omitted)

    Returns:
        The plaintext API key (sk-oai-xxx format)
    """
    r = _create_api_key_raw(oc_token, name, subscription)
    if r.status_code not in (200, 201):
        raise RuntimeError(f"Failed to create API key: {r.status_code} {r.text}")

    data = r.json()
    api_key = data.get("key")
    if not api_key:
        raise RuntimeError(f"API key response missing 'key' field: {data}")

    log.info("Created API key '%s' bound to subscription '%s'", name, subscription)
    return api_key


def _revoke_api_key(oc_token: str, key_id: str):
    """Revoke an API key (best-effort, for cleanup)."""
    url = f"{_maas_api_url()}/v1/api-keys/{key_id}"
    try:
        r = requests.delete(
            url,
            headers={"Authorization": f"Bearer {oc_token}"},
            timeout=TIMEOUT,
            verify=TLS_VERIFY,
        )
        if r.status_code not in (200, 204, 404):
            log.warning("Failed to revoke API key %s: %s %s", key_id, r.status_code, r.text[:200])
    except requests.RequestException as e:
        log.warning("Failed to revoke API key %s: %s", key_id, e)


# ---------------------------------------------------------------------------
# CR Management
# ---------------------------------------------------------------------------

def _apply_cr(cr_dict):
    subprocess.run(["oc", "apply", "-f", "-"], input=json.dumps(cr_dict), capture_output=True, text=True, check=True)


def _delete_cr(kind, name, namespace=None):
    namespace = namespace or _ns()
    result = subprocess.run(
        ["oc", "delete", kind, name, "-n", namespace, "--ignore-not-found", "--timeout=30s"],
        capture_output=True, text=True,
    )
    if result.returncode != 0:
        log.warning("Failed to delete %s/%s in %s: %s", kind, name, namespace, result.stderr.strip())


def _is_transient_kubectl_error(stderr):
    """Check if kubectl error is likely transient (network, timeout)."""
    transient_patterns = [
        "TLS handshake timeout",
        "connection refused",
        "connection reset",
        "i/o timeout",
        "dial tcp",
        "EOF",
        "temporary failure",
        "network is unreachable",
    ]
    stderr_lower = stderr.lower()
    return any(pattern.lower() in stderr_lower for pattern in transient_patterns)


def _is_not_found_error(stderr):
    """Check if kubectl error indicates the resource was not found."""
    stderr_lower = stderr.lower()
    return "notfound" in stderr_lower or "not found" in stderr_lower


def _get_cr(kind, name, namespace=None):
    """Get a CR as dict, or None if not found. Retries on transient errors.

    Returns None only when the resource genuinely does not exist (server NotFound).
    Raises RuntimeError for other failures (RBAC, missing CRD, transport errors
    that persist after retries) so callers can distinguish infrastructure issues
    from true absence.
    """
    namespace = namespace or _ns()
    max_retries = 3
    retry_delay = 2

    for attempt in range(max_retries):
        result = subprocess.run(["oc", "get", kind, name, "-n", namespace, "-o", "json"], capture_output=True, text=True)

        if result.returncode == 0:
            return json.loads(result.stdout)

        if attempt < max_retries - 1 and _is_transient_kubectl_error(result.stderr):
            log.warning(
                f"Transient kubectl error getting {kind}/{name} (attempt {attempt + 1}/{max_retries}): {result.stderr.strip()}"
            )
            time.sleep(retry_delay * (attempt + 1))
            continue

        # Terminal failure — distinguish not-found from other errors
        if _is_not_found_error(result.stderr):
            return None

        log.error(
            f"Failed to get {kind}/{name} in namespace '{namespace}' after {attempt + 1} attempts. "
            f"Last error: {result.stderr.strip()}"
        )
        raise RuntimeError(
            f"Failed to get {kind}/{name} in namespace '{namespace}': {result.stderr.strip()}"
        )


def _snapshot_cr(kind, name, namespace=None):
    """Capture a CR for later restoration (strips runtime metadata)."""
    cr = _get_cr(kind, name, namespace)
    if not cr:
        return None
    meta = cr.get("metadata", {})
    for key in ("resourceVersion", "uid", "creationTimestamp", "generation", "managedFields"):
        meta.pop(key, None)
    annotations = meta.get("annotations", {})
    annotations.pop("kubectl.kubernetes.io/last-applied-configuration", None)
    if not annotations:
        meta.pop("annotations", None)
    cr.pop("status", None)
    return cr


def _list_crs(kind, namespace=None):
    """List all CRs of a given kind.

    Args:
        kind: CR kind (e.g., 'maasmodelref', 'maasauthpolicy')
        namespace: Namespace to search (defaults to _ns())

    Returns:
        List of CR dictionaries

    Raises:
        RuntimeError: If kubectl command fails with contextual error details
    """
    namespace = namespace or _ns()
    plural = {
        "maasmodelref": "maasmodelrefs",
        "maasauthpolicy": "maasauthpolicies",
        "maassubscription": "maassubscriptions",
    }.get(kind, f"{kind}s")

    cmd = ["oc", "get", plural, "-n", namespace, "-o", "json"]

    # Retry transient network errors with exponential backoff
    max_retries = 3
    retry_delay = 2  # seconds

    for attempt in range(max_retries):
        result = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            check=False
        )

        if result.returncode == 0:
            return json.loads(result.stdout).get("items", [])

        # Check if error is transient and we have retries left
        if attempt < max_retries - 1 and _is_transient_kubectl_error(result.stderr):
            log.warning(
                f"Transient kubectl error (attempt {attempt + 1}/{max_retries}): {result.stderr.strip()}"
            )
            time.sleep(retry_delay * (attempt + 1))  # exponential backoff
            continue

        # Final attempt or non-transient error
        raise RuntimeError(
            f"Failed to list {plural} in namespace '{namespace}'.\n"
            f"Command: {' '.join(cmd)}\n"
            f"Exit code: {result.returncode}\n"
            f"Stderr: {result.stderr}\n"
            f"Guidance: Ensure the CRD exists, namespace is correct, and you have permissions."
        )

    # Unreachable: loop always exits via return or raise
    # Included for type checker and defensive programming
    return []


def _get_auth_policies_for_model(model_ref, namespace=None, model_namespace=None):
    """Get all MaaSAuthPolicies that reference a model.

    Args:
        model_ref: Name of the MaaSModelRef
        namespace: Namespace to search for policies (defaults to _ns())
        model_namespace: Expected namespace of the modelRef (defaults to MODEL_NAMESPACE)

    Returns:
        List of auth policy names that reference the model
    """
    namespace = namespace or _ns()
    model_namespace = model_namespace or MODEL_NAMESPACE
    policies = _list_crs("maasauthpolicy", namespace)

    matching = []
    for policy in policies:
        model_refs = policy.get("spec", {}).get("modelRefs", [])
        for ref in model_refs:
            if isinstance(ref, dict):
                ref_name = ref.get("name")
                ref_ns = ref.get("namespace")
            else:
                ref_name = ref
                ref_ns = None
            if ref_name == model_ref and ref_ns == model_namespace:
                matching.append(policy["metadata"]["name"])
                break
    return matching


def _get_subscriptions_for_model(model_ref, namespace=None, model_namespace=None):
    """Get all MaaSSubscriptions that reference a model.

    Args:
        model_ref: Name of the MaaSModelRef
        namespace: Namespace to search for subscriptions (defaults to _ns())
        model_namespace: Expected namespace of the modelRef (defaults to MODEL_NAMESPACE)

    Returns:
        List of subscription names that reference the model
    """
    namespace = namespace or _ns()
    model_namespace = model_namespace or MODEL_NAMESPACE
    subs = _list_crs("maassubscription", namespace)

    matching = []
    for sub in subs:
        model_refs = sub.get("spec", {}).get("modelRefs", [])
        for ref in model_refs:
            if isinstance(ref, dict):
                ref_name = ref.get("name")
                ref_ns = ref.get("namespace")
            else:
                ref_name = ref
                ref_ns = None
            if ref_name == model_ref and ref_ns == model_namespace:
                matching.append(sub["metadata"]["name"])
                break
    return matching


# ---------------------------------------------------------------------------
# CR Creation Helpers
# ---------------------------------------------------------------------------

def _create_test_auth_policy(name, model_refs, users=None, groups=None, namespace=None):
    """Create a MaaSAuthPolicy CR for testing.

    Args:
        name: Name of the auth policy
        model_refs: Model ref(s) - can be string or list
        users: List of user principals (e.g., ["system:serviceaccount:ns:sa"])
        groups: List of group names (e.g., ["system:authenticated"])
        namespace: Namespace for the auth policy (defaults to _ns())
    """
    namespace = namespace or _ns()
    if not isinstance(model_refs, list):
        model_refs = [model_refs]

    model_refs_formatted = [{"name": ref, "namespace": MODEL_NAMESPACE} for ref in model_refs]
    groups_formatted = [{"name": g} for g in (groups or [])]

    log.info("Creating MaaSAuthPolicy: %s", name)
    _apply_cr({
        "apiVersion": "maas.opendatahub.io/v1alpha1",
        "kind": "MaaSAuthPolicy",
        "metadata": {"name": name, "namespace": namespace},
        "spec": {
            "modelRefs": model_refs_formatted,
            "subjects": {
                "users": users or [],
                "groups": groups_formatted
            }
        }
    })


def _create_test_subscription(
    name,
    model_refs,
    users=None,
    groups=None,
    token_limit=100,
    window="1m",
    namespace=None,
    priority=None,
):
    """Create a MaaSSubscription CR for testing.

    Args:
        name: Name of the subscription
        model_refs: Model ref(s) - can be string or list
        users: List of user principals
        groups: List of group names
        token_limit: Token rate limit (default: 100)
        window: Rate limit window (default: "1m")
        namespace: Namespace for the subscription (defaults to _ns())
        priority: Optional spec.priority (higher wins for default API key binding)
    """
    namespace = namespace or _ns()
    if not isinstance(model_refs, list):
        model_refs = [model_refs]

    groups_formatted = [{"name": g} for g in (groups or [])]

    spec = {
        "owner": {
            "users": users or [],
            "groups": groups_formatted,
        },
        "modelRefs": [
            {
                "name": ref,
                "namespace": MODEL_NAMESPACE,
                "tokenRateLimits": [{"limit": token_limit, "window": window}],
            }
            for ref in model_refs
        ],
    }
    if priority is not None:
        spec["priority"] = int(priority)

    log.info("Creating MaaSSubscription: %s", name)
    _apply_cr(
        {
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSSubscription",
            "metadata": {"name": name, "namespace": namespace},
            "spec": spec,
        }
    )


# ---------------------------------------------------------------------------
# Inference Helpers
# ---------------------------------------------------------------------------

def _inference(api_key, path=None, extra_headers=None, model_name=None, max_tokens=3):
    """POST completions using an API key only (subscription is bound at mint)."""
    path = path or MODEL_PATH
    url = f"{_gateway_url()}{path}/v1/completions"
    headers = {"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"}
    if extra_headers:
        headers.update(extra_headers)
    return requests.post(
        url, headers=headers,
        json={"model": model_name or MODEL_NAME, "prompt": "Hello", "max_tokens": max_tokens},
        timeout=TIMEOUT, verify=TLS_VERIFY,
    )


def _poll_status(api_key, expected, path=None, extra_headers=None, model_name=None, timeout=None, poll_interval=2):
    """Poll inference endpoint until expected HTTP status or timeout."""
    timeout = timeout or max(RECONCILE_WAIT * 3, 60)
    deadline = time.time() + timeout
    last = None
    last_err = None
    while time.time() < deadline:
        try:
            r = _inference(api_key, path=path, extra_headers=extra_headers, model_name=model_name)
            last_err = None
            ok = r.status_code == expected if isinstance(expected, int) else r.status_code in expected
            if ok:
                return r
            last = r
        except requests.RequestException as exc:
            last_err = exc
            log.debug(f"Transient request error while polling: {exc}")
        except Exception as exc:
            log.exception(f"Non-transient error while polling, failing fast: {exc}")
            raise
        time.sleep(poll_interval)
    exp_str = expected if isinstance(expected, int) else " or ".join(str(e) for e in expected)
    err_msg = f"Expected {exp_str} within {timeout}s"
    if last is not None:
        err_msg += f", last status: {last.status_code}"
    if last_err is not None:
        err_msg += f", last error: {last_err}"
    if last is None and last_err is None:
        err_msg += ", no response (all requests may have raised non-RequestException)"
    raise AssertionError(err_msg)


# ---------------------------------------------------------------------------
# HTTP helpers (used by test_smoke.py)
# ---------------------------------------------------------------------------

def _post(url: str, payload: dict, headers: dict, timeout_sec: int = 45) -> requests.Response:
    return requests.post(
        url,
        headers=headers,
        json=payload,
        timeout=(timeout_sec, timeout_sec),
        verify=TLS_VERIFY,
        stream=False,
    )


def chat(prompt: str, model_v1: str, headers: dict, model_name: str):
    url = f"{model_v1}/chat/completions"
    body = {"model": model_name, "messages": [{"role": "user", "content": prompt}]}
    return requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)


def completions(prompt: str, model_v1: str, headers: dict, model_name: str):
    url = f"{model_v1}/completions"
    body = {"model": model_name, "prompt": prompt, "max_tokens": 16}
    return requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)


# ---------------------------------------------------------------------------
# Wait / Polling Helpers
# ---------------------------------------------------------------------------

def _wait_reconcile(seconds=None):
    time.sleep(seconds or RECONCILE_WAIT)


def _wait_for_token_rate_limit_policy(model_ref, model_namespace=MODEL_NAMESPACE, timeout=60):
    """Wait for TokenRateLimitPolicy to be created and enforced for a model.

    Args:
        model_ref: Name of the model (e.g., "e2e-distinct-simulated")
        model_namespace: Namespace where the TRLP should be created (default: MODEL_NAMESPACE)
        timeout: Maximum wait time in seconds (default: 60)

    Raises:
        TimeoutError: If TRLP isn't created and enforced within timeout
    """
    trlp_name = f"maas-trlp-{model_ref}"
    deadline = time.time() + timeout
    log.info(f"Waiting for TokenRateLimitPolicy {trlp_name} in {model_namespace} (timeout: {timeout}s)...")

    while time.time() < deadline:
        result = subprocess.run(
            ["oc", "get", "tokenratelimitpolicy", trlp_name, "-n", model_namespace, "-o", "json"],
            capture_output=True,
            text=True,
            timeout=30,
        )
        if result.returncode == 0:
            try:
                trlp = json.loads(result.stdout)
                conditions = trlp.get("status", {}).get("conditions", [])
                enforced = next((c for c in conditions if c.get("type") == "Enforced"), None)
                if enforced and enforced.get("status") == "True":
                    log.info(f"TokenRateLimitPolicy {trlp_name} is enforced")
                    return
                log.debug(f"TokenRateLimitPolicy {trlp_name} exists but not enforced yet")
            except (json.JSONDecodeError, KeyError) as e:
                log.debug(f"Failed to parse TRLP status: {e}")
        elif _is_not_found_error(result.stderr):
            log.debug(f"TokenRateLimitPolicy {trlp_name} not found yet...")
        elif _is_transient_kubectl_error(result.stderr):
            log.debug(
                f"Transient error while reading TokenRateLimitPolicy {trlp_name}: {result.stderr.strip()}"
            )
        else:
            raise RuntimeError(
                f"Failed to get TokenRateLimitPolicy {trlp_name} in namespace '{model_namespace}': "
                f"{result.stderr.strip()}"
            )
        time.sleep(3)

    raise TimeoutError(
        f"TokenRateLimitPolicy {trlp_name} was not created and enforced in {model_namespace} within {timeout}s"
    )


def _wait_for_maas_subscription_phase(name, expected_phase="Active", namespace=None, timeout=60, require_model_statuses=False):
    """Wait for MaaSSubscription to reach a specific phase.

    Args:
        name: Name of the MaaSSubscription
        expected_phase: Phase to wait for (default: "Active")
        namespace: Namespace (defaults to _ns())
        timeout: Maximum wait time in seconds (default: 60)
        require_model_statuses: If True, also requires modelRefStatuses to be populated
                                (default: False). Set to True for status reporting tests.

    Returns:
        The subscription CR dict when the expected phase is reached

    Raises:
        TimeoutError: If MaaSSubscription doesn't reach expected phase within timeout
    """
    namespace = namespace or _ns()
    deadline = time.time() + timeout
    log.info(f"Waiting for MaaSSubscription {name} to reach phase '{expected_phase}' (timeout: {timeout}s)...")

    while time.time() < deadline:
        cr = _get_cr("maassubscription", name, namespace)
        if cr:
            status = cr.get("status", {})
            phase = status.get("phase")
            model_statuses = status.get("modelRefStatuses", [])

            if phase == expected_phase:
                if require_model_statuses:
                    expected_count = len(cr.get("spec", {}).get("modelRefs", []))
                    if len(model_statuses) >= expected_count:
                        log.info(f"MaaSSubscription {name} reached phase '{expected_phase}' with {len(model_statuses)}/{expected_count} modelRefStatuses")
                        return cr
                else:
                    log.info(f"MaaSSubscription {name} reached phase '{expected_phase}'")
                    return cr
            log.debug(f"MaaSSubscription {name}: phase={phase}, modelRefStatuses={len(model_statuses)}")
        time.sleep(2)

    # Timeout - return current state for debugging
    cr = _get_cr("maassubscription", name, namespace)
    status = cr.get("status", {}) if cr else {}
    raise TimeoutError(
        f"MaaSSubscription {name} did not reach phase '{expected_phase}' within {timeout}s "
        f"(current: phase={status.get('phase')}, modelRefStatuses={len(status.get('modelRefStatuses', []))})"
    )


def _wait_for_subscription_trlp_status(name, expected_ready=True, namespace=None, timeout=60):
    """Wait for MaaSSubscription's TokenRateLimitPolicy status to reach expected ready state.

    Args:
        name: Name of the MaaSSubscription
        expected_ready: Expected ready state for all TRLPs (True or False)
        namespace: Namespace (defaults to _ns())
        timeout: Maximum wait time in seconds (default: 60)

    Returns:
        The subscription CR dict when all TRLPs reach the expected ready state

    Raises:
        TimeoutError: If TRLPs don't reach expected state within timeout
    """
    namespace = namespace or _ns()
    deadline = time.time() + timeout
    log.info(f"Waiting for MaaSSubscription {name} TRLP ready={expected_ready} (timeout: {timeout}s)...")

    while time.time() < deadline:
        cr = _get_cr("maassubscription", name, namespace)
        if cr:
            status = cr.get("status", {})
            trlp_statuses = status.get("tokenRateLimitStatuses", [])

            # If we expect ready and there are no TRLPs yet, keep waiting
            if expected_ready and len(trlp_statuses) == 0:
                log.debug(f"MaaSSubscription {name}: waiting for TRLP statuses to appear")
                time.sleep(2)
                continue

            # Check if all TRLPs match expected ready state
            if len(trlp_statuses) > 0:
                all_match = all(trlp.get("ready") == expected_ready for trlp in trlp_statuses)
                if all_match:
                    log.info(f"✅ MaaSSubscription {name} has {len(trlp_statuses)} TRLP(s) with ready={expected_ready}")
                    return cr
                log.debug(f"MaaSSubscription {name}: TRLP statuses={trlp_statuses}")

        time.sleep(2)

    # Timeout - return current state for debugging
    cr = _get_cr("maassubscription", name, namespace)
    status = cr.get("status", {}) if cr else {}
    trlp_statuses = status.get("tokenRateLimitStatuses", [])
    raise TimeoutError(
        f"MaaSSubscription {name} TRLPs did not reach ready={expected_ready} within {timeout}s "
        f"(current TRLPs: {trlp_statuses})"
    )


def _wait_for_maas_auth_policy_phase(name, expected_phase="Active", namespace=None, timeout=60,
                                require_auth_policies=True, require_enforced=True):
    """Wait for MaaSAuthPolicy to reach a specific phase.

    Args:
        name: Name of the MaaSAuthPolicy
        expected_phase: Phase to wait for (default: "Active")
        namespace: Namespace (defaults to _ns())
        timeout: Maximum wait time in seconds (default: 60)
        require_auth_policies: If True, requires authPolicies to be populated (default: True).
                               Set to False for Failed phase with missing models.
        require_enforced: If True, requires all authPolicies to have ready=True
                          (default: True). Only applies when require_auth_policies is True.

    Returns:
        The auth policy CR dict when the expected phase is reached

    Raises:
        TimeoutError: If MaaSAuthPolicy doesn't reach expected phase within timeout
    """
    namespace = namespace or _ns()
    deadline = time.time() + timeout
    log.info(f"Waiting for MaaSAuthPolicy {name} to reach phase '{expected_phase}' (timeout: {timeout}s)...")

    while time.time() < deadline:
        cr = _get_cr("maasauthpolicy", name, namespace)
        if cr:
            status = cr.get("status", {})
            phase = status.get("phase")
            auth_policies = status.get("authPolicies", [])

            if phase == expected_phase:
                # No auth policies required — phase match is sufficient
                if not require_auth_policies:
                    log.info(f"MaaSAuthPolicy {name} reached phase '{expected_phase}'")
                    return cr

                # Auth policies required — check they exist
                if len(auth_policies) > 0:
                    if require_enforced:
                        all_enforced = all(
                            ap.get("ready") is True
                            for ap in auth_policies
                        )
                        if all_enforced:
                            log.info(f"MaaSAuthPolicy {name} reached phase '{expected_phase}' and all enforced")
                            return cr
                    else:
                        log.info(f"MaaSAuthPolicy {name} reached phase '{expected_phase}' with {len(auth_policies)} auth policy status(es)")
                        return cr

            log.debug(f"MaaSAuthPolicy {name}: phase={phase}, authPolicies={len(auth_policies)}")
        time.sleep(2)

    # Timeout - return current state for debugging
    cr = _get_cr("maasauthpolicy", name, namespace)
    status = cr.get("status", {}) if cr else {}
    raise TimeoutError(
        f"MaaSAuthPolicy {name} did not reach phase '{expected_phase}' within {timeout}s "
        f"(current: phase={status.get('phase')}, authPolicies={len(status.get('authPolicies', []))})"
    )


# ---------------------------------------------------------------------------
# Controller scaling utilities
# ---------------------------------------------------------------------------

def _scale_controller(replicas, namespace=None, timeout=60):
    """
    Scale the maas-controller deployment.

    Args:
        replicas: Target replica count (0 to disable, 1+ to enable)
        namespace: Deployment namespace (defaults to DEPLOYMENT_NAMESPACE env or 'opendatahub')
        timeout: Max seconds to wait for scaling operation (default: 60)

    Raises:
        subprocess.CalledProcessError: If kubectl scale fails
        TimeoutError: If pods don't reach desired state within timeout
    """
    namespace = namespace or os.environ.get("DEPLOYMENT_NAMESPACE", "opendatahub")

    log.info(f"Scaling maas-controller to {replicas} replicas in namespace {namespace}...")

    # Scale the deployment
    result = subprocess.run(
        ["oc", "scale", "deployment", "maas-controller",
         f"--replicas={replicas}", "-n", namespace],
        check=True,
        capture_output=True,
        text=True,
        timeout=timeout
    )

    # Wait for pods to reach desired state
    if replicas == 0:
        # Wait for all pods to terminate
        log.debug(f"Waiting for maas-controller pods to terminate (timeout: {timeout}s)...")
        subprocess.run(
            ["oc", "wait", "--for=delete", "pod",
             "-l", "app=maas-controller", "-n", namespace,
             f"--timeout={timeout}s"],
            check=False,  # Don't fail if no pods exist
            capture_output=True,
            text=True
        )
        log.info("✓ maas-controller scaled down to 0 replicas")
    else:
        # Wait for pods to become ready
        log.debug(f"Waiting for maas-controller pods to become ready (timeout: {timeout}s)...")
        try:
            subprocess.run(
                ["oc", "wait", "--for=condition=ready", "pod",
                 "-l", "app=maas-controller", "-n", namespace,
                 f"--timeout={timeout}s"],
                check=True,
                capture_output=True,
                text=True
            )
            log.info(f"✓ maas-controller scaled to {replicas} replica(s)")
        except subprocess.CalledProcessError as e:
            # Log but don't fail - sometimes pods need extra time
            log.warning(f"Pods may not be ready yet: {e.stderr}")
            time.sleep(5)  # Give it a bit more time


def _scale_controller_down(namespace=None, timeout=60):
    """Scale maas-controller to 0 replicas (convenience wrapper)."""
    _scale_controller(0, namespace, timeout)


def _scale_controller_up(namespace=None, timeout=60):
    """Scale maas-controller to 1 replica (convenience wrapper)."""
    _scale_controller(1, namespace, timeout)


def _scale_kuadrant_controller(replicas, namespace="kuadrant-system", timeout=60):
    """
    Scale the kuadrant-operator deployment.

    Args:
        replicas: Target replica count (0 to disable, 1+ to enable)
        namespace: Deployment namespace (default: kuadrant-system)
        timeout: Max seconds to wait for scaling operation (default: 60)

    Raises:
        subprocess.CalledProcessError: If kubectl scale fails
        TimeoutError: If pods don't reach desired state within timeout
    """
    log.info(f"Scaling kuadrant-operator to {replicas} replicas in namespace {namespace}...")

    # Scale the deployment
    result = subprocess.run(
        ["oc", "scale", "deployment", "kuadrant-operator-controller-manager",
         f"--replicas={replicas}", "-n", namespace],
        check=True,
        capture_output=True,
        text=True,
        timeout=timeout
    )

    # Wait for pods to reach desired state
    if replicas == 0:
        # Wait for all pods to terminate
        log.debug(f"Waiting for kuadrant-operator pods to terminate (timeout: {timeout}s)...")
        subprocess.run(
            ["oc", "wait", "--for=delete", "pod",
             "-l", "control-plane=controller-manager", "-n", namespace,
             f"--timeout={timeout}s"],
            check=False,  # Don't fail if no pods exist
            capture_output=True,
            text=True
        )
        log.info("✓ kuadrant-operator scaled down to 0 replicas")
    else:
        # Wait for pods to become ready
        log.debug(f"Waiting for kuadrant-operator pods to become ready (timeout: {timeout}s)...")
        try:
            subprocess.run(
                ["oc", "wait", "--for=condition=ready", "pod",
                 "-l", "control-plane=controller-manager", "-n", namespace,
                 f"--timeout={timeout}s"],
                check=True,
                capture_output=True,
                text=True
            )
            log.info(f"✓ kuadrant-operator scaled to {replicas} replica(s)")
        except subprocess.CalledProcessError:
            log.warning(f"Pods may not be ready yet (timeout: {timeout}s)")
            raise


def _scale_kuadrant_controller_down(namespace="kuadrant-system", timeout=60):
    """Scale kuadrant-operator to 0 replicas (convenience wrapper)."""
    _scale_kuadrant_controller(0, namespace, timeout)


def _scale_kuadrant_controller_up(namespace="kuadrant-system", timeout=60):
    """Scale kuadrant-operator to 1 replica (convenience wrapper)."""
    _scale_kuadrant_controller(1, namespace, timeout)
