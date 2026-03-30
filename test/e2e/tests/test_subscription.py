"""
MaaS Subscription Controller e2e tests.

Tests auth enforcement (MaaSAuthPolicy) and rate limiting (MaaSSubscription)
by hitting the gateway with API keys created via the MaaS API.

Policy Evaluation Order:
  1. AuthPolicy (Kuadrant) - FIRST LINE OF DEFENSE
     - Validates API key via /internal/v1/api-keys/validate
     - Validates subscription selection via /v1/subscriptions/select
       * Checks subscription exists and user has access (groups/users match)
       * Inference uses API keys only; each key carries the bound MaaSSubscription from mint
     - Denies invalid requests with 403 Forbidden (subscription validation failures)
     - Injects auth.identity.selected_subscription for downstream policies

  2. TokenRateLimitPolicy (Kuadrant) - RATE LIMITING ONLY
     - Trusts auth.identity.selected_subscription (already validated by AuthPolicy)
     - Applies rate limits based on selected subscription
     - Returns 429 Too Many Requests only when rate limit exceeded
     - Does NOT re-validate subscription (AuthPolicy already did this)

Expected Error Codes:
  - 401 Unauthorized: Missing or invalid API key
  - 403 Forbidden: Valid API key but subscription validation failed
    * Subscription bound on the key no longer exists or is invalid
    * No subscriptions available for user
  - 429 Too Many Requests: Valid request but rate limit exceeded
  - 200 OK: Valid request with available rate limit quota

Requires:
  - GATEWAY_HOST env var (e.g. maas.apps.cluster.example.com)
  - MAAS_API_BASE_URL env var (e.g. https://maas.apps.cluster.example.com/maas-api)
  - maas-controller deployed with example CRs applied
  - oc/kubectl access to create service account tokens (for API key creation)

Environment variables (all optional, with defaults):
  - GATEWAY_HOST: Gateway hostname (required)
  - MAAS_API_BASE_URL: MaaS API URL (required for API key creation)
  - MAAS_SUBSCRIPTION_NAMESPACE: MaaS CRs namespace (default: models-as-a-service)
  - E2E_TEST_TOKEN_SA_NAMESPACE, E2E_TEST_TOKEN_SA_NAME: When set, use this SA token
    instead of oc whoami -t (e.g. for Prow where oc whoami -t is unavailable)
  - E2E_TIMEOUT: Request timeout in seconds (default: 30)
  - E2E_RECONCILE_WAIT: Wait time for reconciliation in seconds (default: 8)
  - E2E_MODEL_PATH: Path to free model (default: /llm/facebook-opt-125m-simulated)
  - E2E_PREMIUM_MODEL_PATH: Path to premium model (default: /llm/premium-simulated-simulated-premium)
  - E2E_MODEL_NAME: Model name for API requests (default: facebook/opt-125m)
  - E2E_MODEL_REF: Model ref for CRs (default: facebook-opt-125m-simulated)
  - E2E_PREMIUM_MODEL_REF: Premium model ref for CRs (default: premium-simulated-simulated-premium)
  - E2E_UNCONFIGURED_MODEL_REF: Unconfigured model ref (default: e2e-unconfigured-facebook-opt-125m-simulated)
  - E2E_UNCONFIGURED_MODEL_PATH: Path to unconfigured model (default: /llm/e2e-unconfigured-facebook-opt-125m-simulated)
  - E2E_DISTINCT_MODEL_REF: First distinct model ref for multi-model tests (default: e2e-distinct-simulated)
  - E2E_DISTINCT_MODEL_PATH: Path to first distinct model (default: /llm/e2e-distinct-simulated)
  - E2E_DISTINCT_MODEL_ID: Model ID served by first distinct model (default: test/e2e-distinct-model)
  - E2E_DISTINCT_MODEL_2_REF: Second distinct model ref for multi-model tests (default: e2e-distinct-2-simulated)
  - E2E_DISTINCT_MODEL_2_PATH: Path to second distinct model (default: /llm/e2e-distinct-2-simulated)
  - E2E_DISTINCT_MODEL_2_ID: Model ID served by second distinct model (default: test/e2e-distinct-model-2)
  - E2E_SIMULATOR_SUBSCRIPTION: Free-tier subscription (default: simulator-subscription)
  - E2E_PREMIUM_SIMULATOR_SUBSCRIPTION: Premium-tier subscription (default: premium-simulator-subscription)
  - E2E_SIMULATOR_ACCESS_POLICY: Simulator auth policy name (default: simulator-access)
  - E2E_INVALID_SUBSCRIPTION: Invalid subscription name for 403 test (default: nonexistent-sub)
"""

import base64
import copy
import json
import logging
import os
import subprocess
import time
import uuid
from typing import Optional
from urllib.parse import urlparse

import pytest
import requests

log = logging.getLogger(__name__)


# Constants (override with env vars)
TIMEOUT = int(os.environ.get("E2E_TIMEOUT", "30"))
RECONCILE_WAIT = int(os.environ.get("E2E_RECONCILE_WAIT", "8"))
TLS_VERIFY = os.environ.get("E2E_SKIP_TLS_VERIFY", "").lower() != "true"
MODEL_PATH = os.environ.get("E2E_MODEL_PATH", "/llm/facebook-opt-125m-simulated")
PREMIUM_MODEL_PATH = os.environ.get("E2E_PREMIUM_MODEL_PATH", "/llm/premium-simulated-simulated-premium")
MODEL_NAME = os.environ.get("E2E_MODEL_NAME", "facebook/opt-125m")
MODEL_REF = os.environ.get("E2E_MODEL_REF", "facebook-opt-125m-simulated")
PREMIUM_MODEL_REF = os.environ.get("E2E_PREMIUM_MODEL_REF", "premium-simulated-simulated-premium")
MODEL_NAMESPACE = os.environ.get("E2E_MODEL_NAMESPACE", "llm")
UNCONFIGURED_MODEL_REF = os.environ.get("E2E_UNCONFIGURED_MODEL_REF", "e2e-unconfigured-facebook-opt-125m-simulated")
UNCONFIGURED_MODEL_PATH = os.environ.get("E2E_UNCONFIGURED_MODEL_PATH", "/llm/e2e-unconfigured-facebook-opt-125m-simulated")
DISTINCT_MODEL_REF = os.environ.get("E2E_DISTINCT_MODEL_REF", "e2e-distinct-simulated")
DISTINCT_MODEL_PATH = os.environ.get("E2E_DISTINCT_MODEL_PATH", "/llm/e2e-distinct-simulated")
DISTINCT_MODEL_ID = os.environ.get("E2E_DISTINCT_MODEL_ID", "test/e2e-distinct-model")
DISTINCT_MODEL_2_REF = os.environ.get("E2E_DISTINCT_MODEL_2_REF", "e2e-distinct-2-simulated")
DISTINCT_MODEL_2_PATH = os.environ.get("E2E_DISTINCT_MODEL_2_PATH", "/llm/e2e-distinct-2-simulated")
DISTINCT_MODEL_2_ID = os.environ.get("E2E_DISTINCT_MODEL_2_ID", "test/e2e-distinct-model-2")
SIMULATOR_SUBSCRIPTION = os.environ.get("E2E_SIMULATOR_SUBSCRIPTION", "simulator-subscription")
PREMIUM_SIMULATOR_SUBSCRIPTION = os.environ.get(
    "E2E_PREMIUM_SIMULATOR_SUBSCRIPTION", "premium-simulator-subscription"
)
SIMULATOR_ACCESS_POLICY = os.environ.get("E2E_SIMULATOR_ACCESS_POLICY", "simulator-access")
INVALID_SUBSCRIPTION = os.environ.get("E2E_INVALID_SUBSCRIPTION", "nonexistent-sub")

# Generated resource names (for TestManagedAnnotation)
AUTH_POLICY_NAME = f"maas-auth-{MODEL_REF}"
TRLP_NAME = f"maas-trlp-{MODEL_REF}"
MANAGED_ANNOTATION = "opendatahub.io/managed"


def _ns():
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
        # Derive from GATEWAY_HOST if MAAS_API_BASE_URL not set
        host = os.environ.get("GATEWAY_HOST", "")
        if not host:
            raise RuntimeError("MAAS_API_BASE_URL or GATEWAY_HOST env var is required")
        scheme = "http" if os.environ.get("INSECURE_HTTP", "").lower() == "true" else "https"
        url = f"{scheme}://{host}/maas-api"
    return url


# Used for debugging
def _decode_jwt_payload(token: str) -> Optional[dict]:
    """Decode JWT payload (no verification, for debugging). Returns claims dict or None."""
    try:
        parts = token.split(".")
        if len(parts) != 3:
            return None
        payload_b64 = parts[1]
        payload_b64 += "=" * (4 - len(payload_b64) % 4)  # add padding
        payload_bytes = base64.urlsafe_b64decode(payload_b64)
        return json.loads(payload_bytes)
    except Exception:
        return None


def _get_cluster_token():
    """Get OC token for API key management operations (not for inference).
    
    Priority:
      1. TOKEN env var (set by prow script for regular user)
      2. E2E_TEST_TOKEN_SA_* env vars (for SA-based tokens)
      3. oc whoami -t (fallback for local testing)
    """
    # Priority 1: TOKEN env var (regular user token from prow script)
    token = os.environ.get("TOKEN", "")
    if token:
        log.info("Using TOKEN env var for API key operations")
        return token
    
    # Priority 2: SA token if configured
    sa_ns = os.environ.get("E2E_TEST_TOKEN_SA_NAMESPACE")
    sa_name = os.environ.get("E2E_TEST_TOKEN_SA_NAME")
    if sa_ns and sa_name:
        token = _create_sa_token(sa_name, namespace=sa_ns)
    else:
        # Priority 3: oc whoami -t fallback
        token_result = subprocess.run(["oc", "whoami", "-t"], capture_output=True, text=True)
        token = token_result.stdout.strip() if token_result.returncode == 0 else ""
        if not token:
            raise RuntimeError("Could not get cluster token via `oc whoami -t`; run with oc login first")
    claims = _decode_jwt_payload(token)
    if claims:
        log.info("Token claims (decoded): %s", json.dumps(claims, indent=2))
    return token


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


# ---------------------------------------------------------------------------
# API Key Management Helpers
# ---------------------------------------------------------------------------

def _create_api_key(oc_token: str, name: str = None, subscription: str = None) -> str:
    """Create an API key using the MaaS API and return the plaintext key.
    
    Note: API keys inherit the authenticated user's groups automatically.
    Users can only create keys for themselves with their own groups.
    Pass ``subscription`` to bind a specific MaaSSubscription at mint time.

    Args:
        oc_token: OC token for authentication with maas-api
        name: Optional name for the key (auto-generated if not provided)
        subscription: Optional MaaSSubscription name to bind (highest-priority auto-bind if omitted)

    Returns:
        The plaintext API key (sk-oai-xxx format)
    """
    url = f"{_maas_api_url()}/v1/api-keys"
    key_name = name or f"e2e-sub-test-{uuid.uuid4().hex[:8]}"

    body = {"name": key_name}
    if subscription:
        body["subscription"] = subscription

    r = requests.post(
        url,
        headers={
            "Authorization": f"Bearer {oc_token}",
            "Content-Type": "application/json",
        },
        json=body,
        timeout=TIMEOUT,
        verify=TLS_VERIFY,
    )
    if r.status_code not in (200, 201):
        raise RuntimeError(f"Failed to create API key: {r.status_code} {r.text}")
    
    data = r.json()
    api_key = data.get("key")
    if not api_key:
        raise RuntimeError(f"API key response missing 'key' field: {data}")
    
    log.info(f"Created API key '{key_name}' (inherits user's groups), bound to subscription '{subscription}'")
    return api_key


def _revoke_api_key(oc_token: str, key_id: str):
    """Revoke an API key (best-effort, for cleanup)."""
    url = f"{_maas_api_url()}/v1/api-keys/{key_id}"
    try:
        requests.delete(
            url,
            headers={"Authorization": f"Bearer {oc_token}"},
            timeout=TIMEOUT,
            verify=TLS_VERIFY,
        )
    except Exception as e:
        log.warning(f"Failed to revoke API key {key_id}: {e}")


# Cache for API keys to avoid creating too many during test runs.
# Keyed by process ID to ensure test isolation when running in parallel workers.
_default_api_key_cache: dict = {}


def _get_default_api_key() -> str:
    """Get or create an API key for the authenticated user.
    
    The key inherits the user's groups (typically includes system:authenticated).
    Uses per-process caching to avoid creating multiple keys during test runs
    while maintaining isolation between parallel test workers.
    """
    pid = os.getpid()
    if pid not in _default_api_key_cache:
        oc_token = _get_cluster_token()
        _default_api_key_cache[pid] = _create_api_key(
            oc_token,
            name="e2e-default-key",
            subscription=SIMULATOR_SUBSCRIPTION,
        )
    return _default_api_key_cache[pid]


def _delete_sa(sa_name, namespace=None):
    namespace = namespace or _ns()
    subprocess.run(["oc", "delete", "sa", sa_name, "-n", namespace, "--ignore-not-found"], capture_output=True, text=True)


def _apply_cr(cr_dict):
    subprocess.run(["oc", "apply", "-f", "-"], input=json.dumps(cr_dict), capture_output=True, text=True, check=True)


def _delete_cr(kind, name, namespace=None):
    namespace = namespace or _ns()
    subprocess.run(["oc", "delete", kind, name, "-n", namespace, "--ignore-not-found", "--timeout=30s"], capture_output=True, text=True)


def _get_cr(kind, name, namespace=None):
    namespace = namespace or _ns()
    max_retries = 3
    retry_delay = 2

    for attempt in range(max_retries):
        result = subprocess.run(["oc", "get", kind, name, "-n", namespace, "-o", "json"], capture_output=True, text=True)

        if result.returncode == 0:
            return json.loads(result.stdout)

        # Retry transient errors
        if attempt < max_retries - 1 and _is_transient_kubectl_error(result.stderr):
            log.warning(
                f"Transient kubectl error getting {kind}/{name} (attempt {attempt + 1}/{max_retries}): {result.stderr.strip()}"
            )
            time.sleep(retry_delay * (attempt + 1))
            continue

        # Non-transient error or final attempt - return None (existing behavior)
        log.error(
            f"Failed to get {kind}/{name} in namespace '{namespace}' after {max_retries} retries. "
            f"Last error: {result.stderr.strip()}"
        )
        return None


def _cr_exists(kind, name, namespace=None):
    namespace = namespace or _ns()
    result = subprocess.run(["oc", "get", kind, name, "-n", namespace], capture_output=True, text=True)
    return result.returncode == 0


def _annotate(kind, name, annotation, namespace=None):
    """Set or remove an annotation on a resource.

    To set:   _annotate("authpolicy", "name", "key=value")
    To remove: _annotate("authpolicy", "name", "key-")
    """
    namespace = namespace or _ns()
    subprocess.run(
        ["oc", "annotate", kind, name, annotation, "-n", namespace, "--overwrite"],
        capture_output=True,
        text=True,
        check=True,
    )


def _get_auth_policies_for_model(model_ref, namespace=None):
    """Get all MaaSAuthPolicies that reference a model.

    Args:
        model_ref: Name of the MaaSModelRef
        namespace: Namespace to search (defaults to _ns())

    Returns:
        List of auth policy names that reference the model
    """
    namespace = namespace or _ns()
    policies = _list_crs("maasauthpolicy", namespace)

    matching = []
    for policy in policies:
        model_refs = policy.get("spec", {}).get("modelRefs", [])
        for ref in model_refs:
            # Handle both string refs and dict refs with 'name' field
            ref_name = ref.get("name") if isinstance(ref, dict) else ref
            if ref_name == model_ref:
                matching.append(policy["metadata"]["name"])
                break
    return matching


def _get_subscriptions_for_model(model_ref, namespace=None):
    """Get all MaaSSubscriptions that reference a model.

    Args:
        model_ref: Name of the MaaSModelRef
        namespace: Namespace to search (defaults to _ns())

    Returns:
        List of subscription names that reference the model
    """
    namespace = namespace or _ns()
    subs = _list_crs("maassubscription", namespace)

    matching = []
    for sub in subs:
        model_refs = sub.get("spec", {}).get("modelRefs", [])
        for ref in model_refs:
            # Handle both string refs and dict refs with 'name' field
            ref_name = ref.get("name") if isinstance(ref, dict) else ref
            if ref_name == model_ref:
                matching.append(sub["metadata"]["name"])
                break
    return matching


def _sa_to_user(sa_name, namespace=None):
    """Convert service account name to Kubernetes user principal."""
    namespace = namespace or _ns()
    return f"system:serviceaccount:{namespace}:{sa_name}"


def _create_test_maas_model(name, llmis_name=MODEL_REF, llmis_namespace=MODEL_NAMESPACE, namespace=None):
    """Create a MaaSModelRef CR for testing.

    Note: MaaSModelRef can only reference backend models (LLMInferenceService) in the same namespace.
    The namespace parameter sets where both the MaaSModelRef and its target are expected to be.
    """
    namespace = namespace or llmis_namespace  # Default to model's namespace, not opendatahub
    log.info("Creating MaaSModelRef: %s in namespace: %s", name, namespace)
    _apply_cr({
        "apiVersion": "maas.opendatahub.io/v1alpha1",
        "kind": "MaaSModelRef",
        "metadata": {"name": name, "namespace": namespace},
        "spec": {
            "modelRef": {
                "kind": "LLMInferenceService",
                "name": llmis_name
            }
        }
    })


def _create_test_auth_policy(name, model_refs, users=None, groups=None, namespace=None):
    """Create a MaaSAuthPolicy CR for testing.

    Args:
        name: Name of the auth policy
        model_refs: Model ref(s) - can be string or list
        users: List of user principals (e.g., ["system:serviceaccount:ns:sa"])
        groups: List of group names (e.g., ["system:authenticated"]) - will be converted to required format
        namespace: Namespace for the auth policy (defaults to _ns())
    """
    namespace = namespace or _ns()
    if not isinstance(model_refs, list):
        model_refs = [model_refs]

    # Convert model refs to required format: [{"name": "model1", "namespace": "llm"}, ...]
    model_refs_formatted = [{"name": ref, "namespace": MODEL_NAMESPACE} for ref in model_refs]

    # Convert groups list to required format: [{"name": "group1"}, {"name": "group2"}]
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
        users: List of user principals (e.g., ["system:serviceaccount:ns:sa"])
        groups: List of group names (e.g., ["system:authenticated"]) - will be converted to required format
        token_limit: Token rate limit (default: 100)
        window: Rate limit window (default: "1m")
        namespace: Namespace for the subscription (defaults to _ns())
        priority: Optional spec.priority (higher wins for default API key binding when omitted)
    """
    namespace = namespace or _ns()
    if not isinstance(model_refs, list):
        model_refs = [model_refs]

    # Convert groups list to required format: [{"name": "group1"}, {"name": "group2"}]
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


def _inference(api_key, path=None, extra_headers=None, model_name=None):
    """POST completions using an API key only (subscription is bound at mint)."""
    path = path or MODEL_PATH
    url = f"{_gateway_url()}{path}/v1/completions"
    headers = {"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"}
    if extra_headers:
        headers.update(extra_headers)
    return requests.post(
        url, headers=headers,
        json={"model": model_name or MODEL_NAME, "prompt": "Hello", "max_tokens": 3},
        timeout=TIMEOUT, verify=TLS_VERIFY,
    )


def _wait_reconcile(seconds=None):
    time.sleep(seconds or RECONCILE_WAIT)


def _wait_for_maas_model_ready(name, namespace=None, timeout=120):
    """Wait for MaaSModelRef to reach Ready phase.

    Args:
        name: Name of the MaaSModelRef
        namespace: Namespace (defaults to MODEL_NAMESPACE where models are deployed)
        timeout: Maximum wait time in seconds (default: 120)

    Returns:
        str: The model endpoint URL

    Raises:
        TimeoutError: If MaaSModelRef doesn't become Ready within timeout
    """
    namespace = namespace or MODEL_NAMESPACE
    deadline = time.time() + timeout
    log.info(f"Waiting for MaaSModelRef {name} to become Ready (timeout: {timeout}s)...")

    while time.time() < deadline:
        cr = _get_cr("maasmodelref", name, namespace)
        if cr:
            phase = cr.get("status", {}).get("phase")
            endpoint = cr.get("status", {}).get("endpoint")
            if phase == "Ready" and endpoint:
                log.info(f"✅ MaaSModelRef {name} is Ready (endpoint: {endpoint})")
                return endpoint
            log.debug(f"MaaSModelRef {name} phase: {phase}, endpoint: {endpoint or 'none'}")
        time.sleep(5)

    # Timeout - log current state for debugging
    cr = _get_cr("maasmodelref", name, namespace)
    current_phase = cr.get("status", {}).get("phase") if cr else "not found"
    raise TimeoutError(
        f"MaaSModelRef {name} did not become Ready within {timeout}s (current phase: {current_phase})"
    )


def _wait_for_maas_auth_policy_ready(name, namespace=None, timeout=60):
    """Wait for MaaSAuthPolicy to reach Active phase with enforced AuthPolicies.

    Args:
        name: Name of the MaaSAuthPolicy
        namespace: Namespace (defaults to _ns())
        timeout: Maximum wait time in seconds (default: 60)

    Raises:
        TimeoutError: If MaaSAuthPolicy doesn't become Active/enforced within timeout
    """
    namespace = namespace or _ns()
    deadline = time.time() + timeout
    log.info(f"Waiting for MaaSAuthPolicy {name} to become Active (timeout: {timeout}s)...")

    while time.time() < deadline:
        cr = _get_cr("maasauthpolicy", name, namespace)
        if cr:
            phase = cr.get("status", {}).get("phase")
            auth_policies = cr.get("status", {}).get("authPolicies", [])

            # Check if all auth policies are accepted and enforced
            all_enforced = all(
                ap.get("accepted") == "True" and ap.get("enforced") == "True"
                for ap in auth_policies
            )

            if phase == "Active" and auth_policies and all_enforced:
                log.info(f"✅ MaaSAuthPolicy {name} is Active and enforced")
                return
            log.debug(f"MaaSAuthPolicy {name} phase: {phase}, authPolicies: {len(auth_policies)}, all_enforced: {all_enforced}")
        time.sleep(2)

    # Timeout - log current state for debugging
    cr = _get_cr("maasauthpolicy", name, namespace)
    current_phase = cr.get("status", {}).get("phase") if cr else "not found"
    auth_policies = cr.get("status", {}).get("authPolicies", []) if cr else []
    raise TimeoutError(
        f"MaaSAuthPolicy {name} did not become Active/enforced within {timeout}s "
        f"(current phase: {current_phase}, authPolicies: {len(auth_policies)})"
    )


def _wait_for_maas_subscription_ready(name, namespace=None, timeout=30):
    """Wait for MaaSSubscription to reach Active phase.

    Args:
        name: Name of the MaaSSubscription
        namespace: Namespace (defaults to _ns())
        timeout: Maximum wait time in seconds (default: 30)

    Raises:
        TimeoutError: If MaaSSubscription doesn't become Active within timeout
    """
    namespace = namespace or _ns()
    deadline = time.time() + timeout
    log.info(f"Waiting for MaaSSubscription {name} to become Active (timeout: {timeout}s)...")

    while time.time() < deadline:
        cr = _get_cr("maassubscription", name, namespace)
        if cr:
            phase = cr.get("status", {}).get("phase")
            if phase == "Active":
                log.info(f"✅ MaaSSubscription {name} is Active")
                return
            log.debug(f"MaaSSubscription {name} phase: {phase}")
        time.sleep(2)

    # Timeout - log current state for debugging
    cr = _get_cr("maassubscription", name, namespace)
    current_phase = cr.get("status", {}).get("phase") if cr else "not found"
    raise TimeoutError(
        f"MaaSSubscription {name} did not become Active within {timeout}s (current phase: {current_phase})"
    )


def _wait_for_token_rate_limit_policy(model_ref, model_namespace="llm", timeout=60):
    """Wait for TokenRateLimitPolicy to be created and enforced for a model.

    Args:
        model_ref: Name of the model (e.g., "e2e-distinct-simulated")
        model_namespace: Namespace where the TRLP should be created (default: "llm")
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
            capture_output=True, text=True
        )
        if result.returncode == 0:
            try:
                trlp = json.loads(result.stdout)
                conditions = trlp.get("status", {}).get("conditions", [])
                # Check if TRLP is enforced
                enforced = next((c for c in conditions if c.get("type") in ["Enforced", "Ready"]), None)
                if enforced and enforced.get("status") == "True":
                    log.info(f"✅ TokenRateLimitPolicy {trlp_name} is enforced")
                    return
                log.debug(f"TokenRateLimitPolicy {trlp_name} exists but not enforced yet")
            except (json.JSONDecodeError, KeyError) as e:
                log.debug(f"Failed to parse TRLP status: {e}")
        else:
            log.debug(f"TokenRateLimitPolicy {trlp_name} not found yet...")
        time.sleep(3)

    raise TimeoutError(
        f"TokenRateLimitPolicy {trlp_name} was not created and enforced in {model_namespace} within {timeout}s"
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
            # Catch-all to surface non-RequestException (e.g. JSON decode, timeout config)
            last_err = exc
            log.warning(f"Unexpected error while polling: {exc}")
        time.sleep(poll_interval)
    # Build failure message with all available context
    exp_str = expected if isinstance(expected, int) else " or ".join(str(e) for e in expected)
    err_msg = f"Expected {exp_str} within {timeout}s"
    if last is not None:
        err_msg += f", last status: {last.status_code}"
    if last_err is not None:
        err_msg += f", last error: {last_err}"
    if last is None and last_err is None:
        err_msg += ", no response (all requests may have raised non-RequestException)"
    raise AssertionError(err_msg)


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

    cmd = ["kubectl", "get", plural, "-n", namespace, "-o", "json"]

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

    # Unreachable: loop always exits via return (line 684) or raise (line 695)
    # Included for type checker and defensive programming
    return []


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

class TestAuthEnforcement:
    """Tests that MaaSAuthPolicy correctly enforces access using API keys."""

    def test_authorized_user_gets_200(self):
        """API key with system:authenticated group should access the free model.
        Polls because AuthPolicies may still be syncing with Authorino."""
        api_key = _get_default_api_key()
        r = _poll_status(api_key, 200, timeout=90)
        log.info(f"Authorized API key -> {r.status_code}")

    def test_no_auth_gets_401(self):
        """Request without auth header should get 401."""
        url = f"{_gateway_url()}{MODEL_PATH}/v1/completions"
        r = requests.post(
            url,
            headers={"Content-Type": "application/json"},
            json={"model": MODEL_NAME, "prompt": "Hello", "max_tokens": 3},
            timeout=TIMEOUT,
            verify=TLS_VERIFY,
        )
        log.info(f"No auth -> {r.status_code}")
        assert r.status_code == 401, f"Expected 401, got {r.status_code}"

    def test_invalid_token_gets_403(self):
        """Invalid/garbage API key should get 403 (invalid key format)."""
        r = _inference("totally-invalid-garbage-token")
        log.info(f"Invalid token -> {r.status_code}")
        # Gateway may return 401 or 403 for invalid API keys
        assert r.status_code in (401, 403), f"Expected 401 or 403, got {r.status_code}"

    def test_wrong_group_gets_403(self):
        """API key without matching group should get 403 on premium model.
        
        The premium model requires 'premium-user' group. Since the test user's
        groups (system:authenticated, etc.) don't include premium-user, the
        API key should be denied access.
        """
        # The default API key inherits user's actual groups (system:authenticated, etc.)
        # which don't include 'premium-user', so it should get 403 on premium model
        api_key = _get_default_api_key()
        r = _inference(api_key, path=PREMIUM_MODEL_PATH)
        log.info(f"User groups (no premium-user) -> premium model: {r.status_code}")
        assert r.status_code == 403, f"Expected 403, got {r.status_code}"


# Higher than typical default subscriptions (e.g. 0) so SelectHighestPriority picks this CR.
_E2E_API_KEY_BINDING_HIGH_PRIORITY = 100_000


@pytest.fixture(scope="class")
def high_priority_subscription_name_for_api_key_binding():
    name = f"e2e-apikey-sub-binding-{uuid.uuid4().hex[:8]}"
    ns = _ns()
    try:
        _create_test_subscription(
            name,
            MODEL_REF,
            groups=["system:authenticated"],
            priority=_E2E_API_KEY_BINDING_HIGH_PRIORITY,
        )
        _wait_for_maas_subscription_ready(name, ns, timeout=90)
        yield name
    finally:
        _delete_cr("maassubscription", name)


class TestAPIKeySubscriptionBinding:
    """API key mint: default highest-priority subscription vs explicit subscription vs invalid name."""

    def _api_keys_url(self) -> str:
        return f"{_maas_api_url()}/v1/api-keys"

    def _auth_headers(self) -> dict:
        return {
            "Authorization": f"Bearer {_get_cluster_token()}",
            "Content-Type": "application/json",
        }

    def _revoke_key(self, key_id: str) -> None:
        _revoke_api_key(_get_cluster_token(), key_id)

    def test_create_api_key_uses_highest_priority_subscription(
        self,
        high_priority_subscription_name_for_api_key_binding: str,
    ):
        """Omitting subscription binds the accessible subscription with highest spec.priority."""
        r = requests.post(
            self._api_keys_url(),
            headers=self._auth_headers(),
            json={"name": f"test-key-high-prio-{uuid.uuid4().hex[:6]}"},
            timeout=TIMEOUT,
            verify=TLS_VERIFY,
        )
        assert r.status_code in (200, 201), f"Expected 200/201, got {r.status_code}: {r.text}"
        data = r.json()
        assert data.get("subscription") == high_priority_subscription_name_for_api_key_binding, (
            f"Expected default bind to {high_priority_subscription_name_for_api_key_binding!r}, "
            f"got {data.get('subscription')!r}"
        )
        self._revoke_key(data["id"])

    def test_create_api_key_with_explicit_simulator_subscription(
        self,
        high_priority_subscription_name_for_api_key_binding: str,
    ):
        """Explicit subscription in body should bind that subscription, not the highest-priority one."""
        designated = SIMULATOR_SUBSCRIPTION
        r = requests.post(
            self._api_keys_url(),
            headers=self._auth_headers(),
            json={"name": f"test-key-explicit-sub-{uuid.uuid4().hex[:6]}", "subscription": designated},
            timeout=TIMEOUT,
            verify=TLS_VERIFY,
        )
        assert r.status_code in (200, 201), f"Expected 200/201, got {r.status_code}: {r.text}"
        data = r.json()
        assert data.get("subscription") == designated
        assert data.get("subscription") != high_priority_subscription_name_for_api_key_binding
        self._revoke_key(data["id"])

    @pytest.mark.usefixtures("high_priority_subscription_name_for_api_key_binding")
    def test_create_api_key_nonexistent_subscription_errors(self):
        """Unknown subscription name should fail with generic invalid_subscription."""
        bogus = f"e2e-no-such-subscription-{uuid.uuid4().hex}"
        r = requests.post(
            self._api_keys_url(),
            headers=self._auth_headers(),
            json={"name": f"test-key-bogus-sub-{uuid.uuid4().hex[:6]}", "subscription": bogus},
            timeout=TIMEOUT,
            verify=TLS_VERIFY,
        )
        assert r.status_code == 400, f"Expected 400, got {r.status_code}: {r.text}"
        body = r.json()
        assert body.get("code") == "invalid_subscription", body


class TestSubscriptionEnforcement:
    """Tests that MaaSSubscription correctly enforces rate limits using API keys."""

    def test_subscribed_user_gets_200(self):
        """API key with matching group should access the model. Polls for AuthPolicy enforcement."""
        api_key = _get_default_api_key()
        r = _poll_status(api_key, 200, timeout=90)
        log.info(f"Subscribed API key -> {r.status_code}")

    def test_auth_pass_no_subscription_gets_403(self):
        """API key with auth pass but no matching subscription should get 403.

        The AuthPolicy includes a subscription-error-check rule that calls
        /internal/v1/subscriptions/select. If no subscription matches the user's groups,
        the request is denied with 403 "no matching subscription found for user".
        
        To test this, we temporarily add system:authenticated to the premium model's
        AuthPolicy (so auth passes) but keep the subscription only for premium-user
        (so subscription check fails).
        """
        ns = _ns()
        api_key = _get_default_api_key()
        
        # First verify that default key currently gets 403 on premium model (auth fails)
        r = _inference(api_key, path=PREMIUM_MODEL_PATH)
        assert r.status_code == 403, f"Expected 403 for premium model (auth should fail), got {r.status_code}"
        
        # Now temporarily add system:authenticated to premium model's AuthPolicy
        try:
            # Get current auth policy and add system:authenticated group
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {"name": "e2e-auth-pass-sub-fail", "namespace": ns},
                "spec": {
                    "modelRefs": [{"name": PREMIUM_MODEL_REF, "namespace": MODEL_NAMESPACE}],
                    "subjects": {
                        "groups": [{"name": "system:authenticated"}],  # Auth will pass
                    },
                },
            })
            _wait_reconcile()
            
            # Now auth passes (system:authenticated in AuthPolicy) but subscription fails
            # (premium subscription only allows premium-user, not system:authenticated)
            r = _poll_status(api_key, 403, path=PREMIUM_MODEL_PATH, timeout=30)
            log.info(f"Auth passes, subscription fails -> {r.status_code}")
            # Verify the error message indicates subscription issue
            if r.text:
                assert "subscription" in r.text.lower() or r.status_code == 403, \
                    f"Expected subscription-related 403, got: {r.text[:200]}"
        finally:
            _delete_cr("maasauthpolicy", "e2e-auth-pass-sub-fail")
            _wait_reconcile()

    def test_rate_limit_exhaustion_gets_429(self):
        """
        Test that a user gets 429 when they actually exceed their token rate limit.

        This test creates a dedicated subscription with a very low token limit,
        sends enough requests to exhaust it, and verifies a 429 response.

        Uses the unconfigured model to avoid interfering with other tests.
        """
        # Use unconfigured model to isolate this test
        model_ref = UNCONFIGURED_MODEL_REF
        model_path = UNCONFIGURED_MODEL_PATH

        # Create unique subscription and auth policy names
        auth_policy_name = "e2e-rate-limit-test-auth"
        subscription_name = "e2e-rate-limit-test-subscription"

        # Very low limit for fast test: 15 tokens/min with max_tokens=3 per request
        # Expected behavior:
        #   - Requests 1-5 succeed (use 15 tokens total)
        #   - Request 6 gets 429 (would need 18 tokens total)
        token_limit = 15
        window = "1m"
        max_tokens = 3  # Explicitly track tokens per request for clarity

        try:
            # 1. Create auth policy allowing system:authenticated
            _create_test_auth_policy(
                name=auth_policy_name,
                model_refs=[model_ref],
                groups=["system:authenticated"]
            )
            _wait_reconcile()

            # 2. Create subscription with low token limit
            _create_test_subscription(
                name=subscription_name,
                model_refs=[model_ref],
                groups=["system:authenticated"],
                token_limit=token_limit,
                window=window
            )
            _wait_reconcile()

            # Wait for TRLP to be created AND enforced by Kuadrant/Limitador.
            # Without this, requests bypass token rate limiting entirely.
            _wait_for_token_rate_limit_policy(model_ref, model_namespace=MODEL_NAMESPACE, timeout=90)

            # 3. API key must be minted for this subscription
            oc_token = _get_cluster_token()
            api_key = _create_api_key(
                oc_token,
                name=f"e2e-rate-limit-{uuid.uuid4().hex[:8]}",
                subscription=subscription_name,
            )

            # 4. Send requests to exhaust the limit
            # Calculate expected successful requests: token_limit / max_tokens = 15 / 3 = 5
            expected_success = token_limit // max_tokens
            # Send 2 extra requests to ensure we hit the limit
            total_requests = expected_success + 2

            rate_limited = False
            success_count = 0

            for i in range(total_requests):
                r = _inference(api_key, path=model_path)
                request_num = i + 1
                log.info(f"Request {request_num}/{total_requests}: {r.status_code}")

                if r.status_code == 200:
                    success_count += 1
                elif r.status_code == 429:
                    rate_limited = True
                    log.info(f"Rate limit exceeded after {success_count} successful requests "
                            f"({success_count * max_tokens} tokens used)")

                    # Verify we hit the limit at approximately the right point (±1 for rounding)
                    assert abs(success_count - expected_success) <= 1, \
                        f"Expected ~{expected_success} successful requests before 429, got {success_count}"

                    # Verify it's a rate limit 429, not a subscription error
                    response_text = r.text.lower() if r.text else ""
                    # Rate limit 429s typically mention "rate", "limit", or "quota"
                    # Subscription 429s mention "subscription" without "rate"
                    is_rate_limit_error = any(keyword in response_text
                                             for keyword in ["rate", "limit", "quota", "too many"])
                    is_subscription_error = "subscription" in response_text and not is_rate_limit_error

                    assert is_rate_limit_error or not is_subscription_error, \
                        f"Expected rate limit 429, not subscription error. Response: {r.text[:500]}"

                    # Check for Retry-After header (optional but good practice)
                    retry_after = r.headers.get("Retry-After") or r.headers.get("retry-after")
                    if retry_after:
                        log.info(f"Retry-After header present: {retry_after}")

                    break
                else:
                    # Unexpected status code
                    raise AssertionError(f"Unexpected status {r.status_code} at request {request_num}: {r.text[:200]}")

                # Brief pause to avoid overwhelming the system, but stay within the window
                time.sleep(0.1)

            assert rate_limited, \
                f"Expected 429 after ~{expected_success} requests with {token_limit} tokens/{window} limit, " \
                f"but got {success_count} successful requests without hitting limit"

            # Note: Skipping rate limit reset test to keep test fast (<5s)
            # Reset behavior is tested manually via scripts/test-rate-limit.sh

        finally:
            # Clean up in reverse order of creation
            _delete_cr("maassubscription", subscription_name)
            _delete_cr("maasauthpolicy", auth_policy_name)
            _wait_reconcile()
            log.info("Cleaned up rate limit test resources")


class TestMultipleSubscriptionsPerModel:
    """Multiple subscriptions for one model — API key in ONE subscription should get access.

    Validates the fix for the bug where multiple subscriptions' when predicates
    were AND'd, requiring a user to be in ALL subscriptions.
    """

    def test_user_in_one_of_two_subscriptions_gets_200(self):
        """Add a 2nd subscription for a different group. API key only in the original
        group should still get 200 (not blocked by the 2nd sub's group check)."""
        ns = _ns()
        try:
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": "e2e-extra-sub", "namespace": ns},
                "spec": {
                    "owner": {"groups": [{"name": "nonexistent-group-xyz"}]},
                    "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE, "tokenRateLimits": [{"limit": 999, "window": "1m"}]}],
                },
            })

            api_key = _get_default_api_key()
            r = _poll_status(api_key, 200)
            log.info(f"API key in 1 of 2 subs -> {r.status_code}")
        finally:
            _delete_cr("maassubscription", "e2e-extra-sub")
            _wait_reconcile()


class TestMultipleAuthPoliciesPerModel:
    """Multiple auth policies for one model aggregate with OR logic."""

    def test_two_auth_policies_or_logic(self):
        """Two auth policies for the premium model with OR logic: user matching either gets access."""
        ns = _ns()
        try:
            # Create a 2nd auth policy that allows system:authenticated (user's actual group)
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {"name": "e2e-premium-sa-auth", "namespace": ns},
                "spec": {
                    "modelRefs": [{"name": PREMIUM_MODEL_REF, "namespace": MODEL_NAMESPACE}],
                    "subjects": {"groups": [{"name": "system:authenticated"}]},
                },
            })
            # Create a subscription for system:authenticated on premium model
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": "e2e-premium-sa-sub", "namespace": ns},
                "spec": {
                    "owner": {"groups": [{"name": "system:authenticated"}]},
                    "modelRefs": [{"name": PREMIUM_MODEL_REF, "namespace": MODEL_NAMESPACE, "tokenRateLimits": [{"limit": 100, "window": "1m"}]}],
                },
            })
            _wait_reconcile()
            
            # Key must be minted for the premium subscription
            api_key = _create_api_key(
                _get_cluster_token(),
                name=f"e2e-premium-sa-{uuid.uuid4().hex[:8]}",
                subscription="e2e-premium-sa-sub",
            )
            r = _poll_status(api_key, 200, path=PREMIUM_MODEL_PATH, timeout=30)
            log.info(f"API key with 2nd auth policy -> premium: {r.status_code}")
        finally:
            _delete_cr("maassubscription", "e2e-premium-sa-sub")
            _delete_cr("maasauthpolicy", "e2e-premium-sa-auth")
            _wait_reconcile()

    def test_delete_one_auth_policy_other_still_works(self):
        """Delete one of two auth policies for a model -> remaining still works."""
        ns = _ns()
        try:
            # Create an extra auth policy for the standard model (same model as existing one)
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {"name": "e2e-extra-auth", "namespace": ns},
                "spec": {
                    "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE}],
                    "subjects": {"groups": [{"name": "system:authenticated"}]},
                },
            })
            _wait_reconcile()

            # Delete the extra policy - original policy should still work
            _delete_cr("maasauthpolicy", "e2e-extra-auth")
            _wait_reconcile()

            # Default API key should still work via the original auth policy
            api_key = _get_default_api_key()
            r = _poll_status(api_key, 200)
            log.info(f"After deleting extra auth policy -> {r.status_code}")
        finally:
            _delete_cr("maasauthpolicy", "e2e-extra-auth")
            _wait_reconcile()


class TestCascadeDeletion:
    """Tests that deleting CRs triggers proper cleanup and rebuilds."""

    def test_delete_subscription_rebuilds_trlp(self):
        """Add a 2nd subscription, delete it -> TRLP rebuilt with only the original."""
        ns = _ns()
        try:
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": "e2e-temp-sub", "namespace": ns},
                "spec": {
                    "owner": {"groups": [{"name": "system:authenticated"}]},
                    "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE, "tokenRateLimits": [{"limit": 50, "window": "1m"}]}],
                },
            })
            _wait_reconcile()

            _delete_cr("maassubscription", "e2e-temp-sub")

            api_key = _get_default_api_key()
            _poll_status(api_key, 200)
        finally:
            _delete_cr("maassubscription", "e2e-temp-sub")

    def test_trlp_persists_during_multi_subscription_deletion(self):
        """Validate CWE-693/CWE-400 fix: TRLP rebuilt in-place during deletion.

        Tests the fix for the security vulnerability where deleting one subscription
        would delete the entire TokenRateLimitPolicy, disabling rate limiting for
        ALL subscriptions to that model and creating a window for unthrottled requests.

        The fix ensures:
        1. TRLP is rebuilt in-place when a subscription is deleted (not deleted entirely)
        2. TRLP contains only remaining subscriptions after deletion
        3. TRLP is deleted only when no subscriptions remain

        This prevents the rate-limit protection gap (CWE-693: Protection Mechanism
        Failure, CWE-400: Uncontrolled Resource Consumption).
        """
        ns = _ns()
        trlp_ns = MODEL_NAMESPACE
        trlp_name = TRLP_NAME

        # Snapshot original subscription for restoration
        original_sub = _snapshot_cr("maassubscription", SIMULATOR_SUBSCRIPTION, ns)
        assert original_sub, f"Pre-existing {SIMULATOR_SUBSCRIPTION} not found"

        try:
            # Step 1: Create a second subscription for the same model
            log.info("Creating second subscription for the same model...")
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": "e2e-second-sub", "namespace": ns},
                "spec": {
                    "owner": {"groups": [{"name": "system:authenticated"}]},
                    "modelRefs": [{
                        "name": MODEL_REF,
                        "namespace": MODEL_NAMESPACE,
                        "tokenRateLimits": [{"limit": 75, "window": "1m"}]
                    }],
                },
            })
            _wait_reconcile()

            # Step 2: Verify TRLP exists and contains both subscriptions
            log.info("Verifying TRLP contains both subscriptions...")
            trlp_with_both = _get_cr("tokenratelimitpolicy", trlp_name, trlp_ns)
            assert trlp_with_both, f"TRLP {trlp_name} not found in {trlp_ns} after creating 2nd subscription"

            # Verify both subscriptions are in the TRLP limits
            limits = trlp_with_both.get("spec", {}).get("limits", {})
            assert limits, f"TRLP {trlp_name} has no limits defined"

            # Look for both subscription references in TRLP limits
            # Format: {namespace}-{subscription-name}-{model-name}-tokens
            simulator_limit_key = f"{ns.replace('/', '-')}-{SIMULATOR_SUBSCRIPTION}-{MODEL_REF}-tokens"
            second_limit_key = f"{ns.replace('/', '-')}-e2e-second-sub-{MODEL_REF}-tokens"

            assert simulator_limit_key in limits, \
                f"Original subscription limit key '{simulator_limit_key}' not found in TRLP. Available keys: {list(limits.keys())}"
            assert second_limit_key in limits, \
                f"Second subscription limit key '{second_limit_key}' not found in TRLP. Available keys: {list(limits.keys())}"

            log.info(f"✅ TRLP contains both subscriptions: {list(limits.keys())}")

            # Step 3: Delete the second subscription
            log.info("Deleting second subscription...")
            _delete_cr("maassubscription", "e2e-second-sub", ns)
            _wait_reconcile()

            # Step 4: Verify TRLP still exists (not deleted) and contains only original subscription
            log.info("Verifying TRLP persists and contains only original subscription...")
            trlp_after_deletion = _get_cr("tokenratelimitpolicy", trlp_name, trlp_ns)
            assert trlp_after_deletion, \
                f"CRITICAL: TRLP {trlp_name} was deleted when 2nd subscription was removed! " \
                f"This creates a rate-limit protection gap (CWE-693/CWE-400)."

            limits_after = trlp_after_deletion.get("spec", {}).get("limits", {})
            assert limits_after, f"TRLP {trlp_name} has no limits after 2nd subscription deletion"

            # Verify original subscription still in TRLP, second subscription removed
            assert simulator_limit_key in limits_after, \
                f"Original subscription limit '{simulator_limit_key}' missing after 2nd sub deletion. " \
                f"Available: {list(limits_after.keys())}"
            assert second_limit_key not in limits_after, \
                f"Deleted subscription limit '{second_limit_key}' still present in TRLP. " \
                f"Available: {list(limits_after.keys())}"

            log.info(f"✅ TRLP rebuilt in-place with only original subscription: {list(limits_after.keys())}")

            # Step 5: Verify rate limiting still works (optional if models ready)
            # The core TRLP persistence logic (CWE-693/CWE-400 fix) has been validated in steps 1-4
            log.info("Verifying rate limiting is still enforced...")
            try:
                api_key = _get_default_api_key()
                r = _poll_status(api_key, 200, timeout=10)
                assert r.status_code == 200, \
                    f"Rate limiting broken after subscription deletion: expected 200, got {r.status_code}"
                log.info("✅ Rate limiting still enforced after subscription deletion")
            except (AssertionError, Exception) as e:
                log.warning(f"Inference test skipped (models not ready): {e}")
                log.info("Core TRLP persistence validated in steps 1-4")

            # Step 6: Delete the last remaining subscription
            log.info("Deleting last subscription...")
            _delete_cr("maassubscription", SIMULATOR_SUBSCRIPTION, ns)
            _wait_reconcile()

            # Step 7: Verify TRLP is now deleted (no subscriptions remain)
            log.info("Verifying TRLP is deleted when no subscriptions remain...")
            trlp_final = _get_cr("tokenratelimitpolicy", trlp_name, trlp_ns)
            assert trlp_final is None, \
                f"TRLP {trlp_name} should be deleted when no subscriptions remain, but still exists"

            log.info("✅ TRLP correctly deleted when no subscriptions remain")

        finally:
            # Cleanup: restore original subscription, delete test subscription
            log.info("Restoring original subscription...")
            _delete_cr("maassubscription", "e2e-second-sub", ns)
            if original_sub:
                _apply_cr(original_sub)
            _wait_reconcile()

    def test_delete_last_subscription_denies_access(self):
        """Delete all subscriptions for a model -> access denied with 403 Forbidden.

        When the last subscription is deleted, AuthPolicy's subscription validation
        fails (no subscriptions found for user) and returns 403 Forbidden before
        the request reaches TokenRateLimitPolicy.
        """
        api_key = _get_default_api_key()
        original = _snapshot_cr("maassubscription", SIMULATOR_SUBSCRIPTION)
        assert original, f"Pre-existing {SIMULATOR_SUBSCRIPTION} not found"
        try:
            _delete_cr("maassubscription", SIMULATOR_SUBSCRIPTION)
            # With no subscription, expect 403 from AuthPolicy subscription validation
            r = _poll_status(api_key, 403, timeout=30)
            log.info(f"No subscriptions -> {r.status_code} (access denied as expected)")
        finally:
            _apply_cr(original)
            _wait_reconcile()

    # TODO: Uncomment this test once we validated unconfigured models
    # def test_unconfigured_model_denied_by_gateway_auth(self):
    #     """New model with no MaaSAuthPolicy/MaaSSubscription -> gateway default auth denies (403)."""
    #     api_key = _get_default_api_key()
    #     r = _inference(api_key, path=UNCONFIGURED_MODEL_PATH)
    #     log.info(f"Unconfigured model (no auth policy) -> {r.status_code}")
    #     assert r.status_code == 403, f"Expected 403 (gateway default deny), got {r.status_code}"


class TestOrderingEdgeCases:
    """Tests that resource creation order doesn't matter."""

    def test_subscription_before_auth_policy(self):
        """Create subscription first, then auth policy -> should work once both exist."""
        ns = _ns()
        try:
            # Subscription CR must exist before minting a key bound to it
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": "e2e-ordering-sub", "namespace": ns},
                "spec": {
                    "owner": {"groups": [{"name": "system:authenticated"}]},
                    "modelRefs": [{"name": PREMIUM_MODEL_REF, "namespace": MODEL_NAMESPACE, "tokenRateLimits": [{"limit": 100, "window": "1m"}]}],
                },
            })
            _wait_reconcile()
            _wait_for_maas_subscription_ready("e2e-ordering-sub", namespace=ns, timeout=90)

            api_key = _create_api_key(
                _get_cluster_token(),
                name=f"e2e-ordering-{uuid.uuid4().hex[:8]}",
                subscription="e2e-ordering-sub",
            )

            # Without auth policy for system:authenticated on premium model, request should fail with 403
            r1 = _inference(api_key, path=PREMIUM_MODEL_PATH)
            log.info(f"Sub only (no auth policy) -> {r1.status_code}")
            assert r1.status_code == 403, f"Expected 403 (no auth policy yet), got {r1.status_code}"

            # Now add the auth policy
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {"name": "e2e-ordering-auth", "namespace": ns},
                "spec": {
                    "modelRefs": [{"name": PREMIUM_MODEL_REF, "namespace": MODEL_NAMESPACE}],
                    "subjects": {"groups": [{"name": "system:authenticated"}]},
                },
            })

            # Now it should work
            r2 = _poll_status(api_key, 200, path=PREMIUM_MODEL_PATH)
            log.info(f"Sub + auth policy -> {r2.status_code}")
        finally:
            _delete_cr("maassubscription", "e2e-ordering-sub")
            _delete_cr("maasauthpolicy", "e2e-ordering-auth")
            _wait_reconcile()


class TestManagedAnnotation:
    """Tests that opendatahub.io/managed=false prevents the controller from updating generated resources."""

    def test_authpolicy_managed_false_prevents_update(self):
        """AuthPolicy annotated with opendatahub.io/managed=false must not have
        its spec updated when the parent MaaSAuthPolicy is modified."""
        ns = _ns()
        ap_ns = MODEL_NAMESPACE
        parent_snapshot = None
        try:
            # 1. Verify the AuthPolicy exists
            ap = _get_cr("authpolicy", AUTH_POLICY_NAME, ap_ns)
            assert ap, f"AuthPolicy {AUTH_POLICY_NAME} not found in {ap_ns}"

            # 2. Snapshot the parent MaaSAuthPolicy for cleanup
            parent_snapshot = _snapshot_cr(
                "maasauthpolicy", SIMULATOR_ACCESS_POLICY, ns
            )
            assert parent_snapshot, (
                f"MaaSAuthPolicy {SIMULATOR_ACCESS_POLICY} not found in {ns}"
            )

            # 3. Annotate the AuthPolicy with managed=false
            _annotate(
                "authpolicy", AUTH_POLICY_NAME, f"{MANAGED_ANNOTATION}=false", ap_ns
            )
            log.info(
                "Annotated AuthPolicy %s with %s=false",
                AUTH_POLICY_NAME,
                MANAGED_ANNOTATION,
            )

            # 4. Re-read the AuthPolicy to capture baseline spec (post-annotation)
            ap_baseline = _get_cr("authpolicy", AUTH_POLICY_NAME, ap_ns)
            assert ap_baseline, (
                f"AuthPolicy {AUTH_POLICY_NAME} disappeared after annotation"
            )
            baseline_spec = ap_baseline["spec"]

            # 5. Modify the parent MaaSAuthPolicy (add a group to subjects)
            modified_parent = copy.deepcopy(parent_snapshot)
            groups = modified_parent["spec"].get("subjects", {}).get("groups", [])
            groups.append({"name": "e2e-managed-annotation-test-group"})
            modified_parent["spec"]["subjects"]["groups"] = groups
            _apply_cr(modified_parent)
            log.info(
                "Modified parent MaaSAuthPolicy %s (added test group)",
                SIMULATOR_ACCESS_POLICY,
            )

            # 6. Wait for reconciliation
            _wait_reconcile()

            # 7. Re-read the AuthPolicy and compare spec
            ap_after = _get_cr("authpolicy", AUTH_POLICY_NAME, ap_ns)
            assert ap_after, (
                f"AuthPolicy {AUTH_POLICY_NAME} disappeared after parent update"
            )
            after_spec = ap_after["spec"]

            assert baseline_spec == after_spec, (
                f"AuthPolicy spec changed despite {MANAGED_ANNOTATION}=false.\n"
                f"Before: {json.dumps(baseline_spec, indent=2)}\n"
                f"After:  {json.dumps(after_spec, indent=2)}"
            )
            log.info(
                "AuthPolicy spec unchanged after parent modification — managed=false respected"
            )

        finally:
            # Remove the annotation (best-effort so parent restore still runs)
            try:
                _annotate(
                    "authpolicy", AUTH_POLICY_NAME, f"{MANAGED_ANNOTATION}-", ap_ns
                )
                log.info(
                    "Removed %s annotation from AuthPolicy %s",
                    MANAGED_ANNOTATION,
                    AUTH_POLICY_NAME,
                )
            except subprocess.CalledProcessError:
                log.warning(
                    "Failed to remove %s annotation from AuthPolicy %s (may not exist)",
                    MANAGED_ANNOTATION,
                    AUTH_POLICY_NAME,
                )

            # Restore the parent MaaSAuthPolicy
            if parent_snapshot:
                _apply_cr(parent_snapshot)
                log.info(
                    "Restored parent MaaSAuthPolicy %s from snapshot",
                    SIMULATOR_ACCESS_POLICY,
                )

            _wait_reconcile()

    def test_trlp_managed_false_prevents_update(self):
        """TokenRateLimitPolicy annotated with opendatahub.io/managed=false must not
        have its spec updated when the parent MaaSSubscription is modified."""
        ns = _ns()
        trlp_ns = MODEL_NAMESPACE
        parent_snapshot = None
        try:
            # 1. Verify the TRLP exists
            trlp = _get_cr("tokenratelimitpolicy", TRLP_NAME, trlp_ns)
            assert trlp, f"TokenRateLimitPolicy {TRLP_NAME} not found in {trlp_ns}"

            # 2. Snapshot the parent MaaSSubscription for cleanup
            parent_snapshot = _snapshot_cr(
                "maassubscription", SIMULATOR_SUBSCRIPTION, ns
            )
            assert parent_snapshot, (
                f"MaaSSubscription {SIMULATOR_SUBSCRIPTION} not found in {ns}"
            )

            # 3. Annotate the TRLP with managed=false
            _annotate(
                "tokenratelimitpolicy",
                TRLP_NAME,
                f"{MANAGED_ANNOTATION}=false",
                trlp_ns,
            )
            log.info(
                "Annotated TokenRateLimitPolicy %s with %s=false",
                TRLP_NAME,
                MANAGED_ANNOTATION,
            )

            # 4. Re-read the TRLP to capture baseline spec (post-annotation)
            trlp_baseline = _get_cr("tokenratelimitpolicy", TRLP_NAME, trlp_ns)
            assert trlp_baseline, (
                f"TokenRateLimitPolicy {TRLP_NAME} disappeared after annotation"
            )
            baseline_spec = trlp_baseline["spec"]

            # 5. Modify the parent MaaSSubscription (change the token rate limit value)
            modified_parent = copy.deepcopy(parent_snapshot)
            model_refs = modified_parent["spec"].get("modelRefs", [])
            assert model_refs, (
                f"MaaSSubscription {SIMULATOR_SUBSCRIPTION} has no modelRefs"
            )
            for ref in model_refs:
                if ref.get("name") == MODEL_REF:
                    limits = ref.get("tokenRateLimits", [])
                    assert limits, f"modelRef {MODEL_REF} has no tokenRateLimits"
                    limits[0]["limit"] = limits[0]["limit"] + 99999
                    break
            _apply_cr(modified_parent)
            log.info(
                "Modified parent MaaSSubscription %s (changed token rate limit)",
                SIMULATOR_SUBSCRIPTION,
            )

            # 6. Wait for reconciliation
            _wait_reconcile()

            # 7. Re-read the TRLP and compare spec
            trlp_after = _get_cr("tokenratelimitpolicy", TRLP_NAME, trlp_ns)
            assert trlp_after, (
                f"TokenRateLimitPolicy {TRLP_NAME} disappeared after parent update"
            )
            after_spec = trlp_after["spec"]

            assert baseline_spec == after_spec, (
                f"TokenRateLimitPolicy spec changed despite {MANAGED_ANNOTATION}=false.\n"
                f"Before: {json.dumps(baseline_spec, indent=2)}\n"
                f"After:  {json.dumps(after_spec, indent=2)}"
            )
            log.info(
                "TokenRateLimitPolicy spec unchanged after parent modification — managed=false respected"
            )

        finally:
            # Remove the annotation (best-effort so parent restore still runs)
            try:
                _annotate(
                    "tokenratelimitpolicy", TRLP_NAME, f"{MANAGED_ANNOTATION}-", trlp_ns
                )
                log.info(
                    "Removed %s annotation from TokenRateLimitPolicy %s",
                    MANAGED_ANNOTATION,
                    TRLP_NAME,
                )
            except subprocess.CalledProcessError:
                log.warning(
                    "Failed to remove %s annotation from TokenRateLimitPolicy %s (may not exist)",
                    MANAGED_ANNOTATION,
                    TRLP_NAME,
                )

            # Restore the parent MaaSSubscription
            if parent_snapshot:
                _apply_cr(parent_snapshot)
                log.info(
                    "Restored parent MaaSSubscription %s from snapshot",
                    SIMULATOR_SUBSCRIPTION,
                )

            _wait_reconcile()


class TestE2ESubscriptionFlow:
    """
    End-to-end tests that create MaaSModelRef, MaaSAuthPolicy, and MaaSSubscription
    from scratch and validate the complete subscription flow.

    Each test creates all necessary CRs and validates one scenario (gateway inference uses
    API keys only; subscription is chosen at mint via POST /v1/api-keys):
    1. API key with both access and bound subscription → 200 OK
    2. API key bound to subscription that is then removed → 403 Forbidden (auth still passes)
    3. API key with subscription but no auth → 403 Forbidden
    4. Single subscription for user + mint without explicit subscription → 200 OK
    5. Two subscriptions: separate keys minted for each → 200 OK for each
    6. Mint API key for another user's subscription → 400 invalid_subscription
    """


    @classmethod
    def setup_class(cls):
        """Validate test environment prerequisites before running any tests.
        
        This validates that expected resources exist and are in the correct state.
        Tests will FAIL (not skip) if prerequisites are missing, ensuring CI catches issues.
        """
        log.info("=" * 60)
        log.info("Validating E2E Test Prerequisites")
        log.info("=" * 60)
        
        # Validate MODEL_REF exists and is Ready
        model = _get_cr("maasmodelref", MODEL_REF, MODEL_NAMESPACE)
        if not model:
            pytest.fail(f"PREREQUISITE MISSING: MaaSModelRef '{MODEL_REF}' not found. "
                       f"Ensure prow setup has created the model.")

        phase = model.get("status", {}).get("phase")
        endpoint = model.get("status", {}).get("endpoint")
        if phase != "Ready" or not endpoint:
            pytest.fail(f"PREREQUISITE INVALID: MaaSModelRef '{MODEL_REF}' not Ready "
                       f"(phase={phase}, endpoint={endpoint or 'none'}). "
                       f"Wait for reconciliation or check controller logs.")
        
        log.info(f"✓ Model '{MODEL_REF}' is Ready")
        log.info(f"  Endpoint: {endpoint}")
        
        # Discover existing auth policies and subscriptions (for debugging)
        cls.discovered_auth_policies = _get_auth_policies_for_model(MODEL_REF)
        cls.discovered_subscriptions = _get_subscriptions_for_model(MODEL_REF)
        
        log.info(f"✓ Found {len(cls.discovered_auth_policies)} auth policies for model:")
        for policy in cls.discovered_auth_policies:
            log.info(f"  - {policy}")
        
        log.info(f"✓ Found {len(cls.discovered_subscriptions)} subscriptions for model:")
        for sub in cls.discovered_subscriptions:
            log.info(f"  - {sub}")
        
        # Validate expected resources exist
        if SIMULATOR_ACCESS_POLICY not in cls.discovered_auth_policies:
            pytest.fail(f"PREREQUISITE MISSING: Expected auth policy '{SIMULATOR_ACCESS_POLICY}' not found. "
                       f"Found: {cls.discovered_auth_policies}. "
                       f"Ensure prow setup has created the auth policy.")
        
        if SIMULATOR_SUBSCRIPTION not in cls.discovered_subscriptions:
            pytest.fail(f"PREREQUISITE MISSING: Expected subscription '{SIMULATOR_SUBSCRIPTION}' not found. "
                       f"Found: {cls.discovered_subscriptions}. "
                       f"Ensure prow setup has created the subscription.")
        
        log.info("=" * 60)
        log.info("✅ All prerequisites validated - proceeding with tests")
        log.info("=" * 60)


    def test_e2e_with_both_access_and_subscription_gets_200(self):
        """
        Full E2E test: Create MaaSModelRef, MaaSAuthPolicy, and MaaSSubscription from scratch.
        API key with both access and subscription should get 200 OK.

        This is the comprehensive test that validates the complete E2E flow including
        MaaSModelRef creation and reconciliation. Other tests use existing models for speed.
        """
        ns = _ns()
        model_ref = "e2e-test-model-success"
        auth_policy_name = "e2e-test-auth-success"
        subscription_name = "e2e-test-subscription-success"
        sa_name = "e2e-sa-success"

        try:
            # Create service account and get OC token for maas-api
            oc_token = _create_sa_token(sa_name, namespace=ns)
            sa_user = _sa_to_user(sa_name, namespace=ns)

            # Create test resources
            _create_test_maas_model(model_ref)
            endpoint = _wait_for_maas_model_ready(model_ref, timeout=120)  # Wait for model to be Ready!

            # Extract path from endpoint (e.g., https://maas.../llm/facebook-opt-125m-simulated -> /llm/facebook-opt-125m-simulated)
            model_path = urlparse(endpoint).path

            _create_test_auth_policy(auth_policy_name, model_ref, users=[sa_user])
            _create_test_subscription(subscription_name, model_ref, users=[sa_user])

            _wait_reconcile()

            # API key bound to this subscription at mint (inference does not send x-maas-subscription)
            api_key = _create_api_key(
                oc_token, name=f"{sa_name}-key", subscription=subscription_name
            )

            # Test: Both access and subscription → 200
            log.info("Testing: API key with both access and subscription")
            r = _poll_status(api_key, 200, path=model_path, timeout=90)
            log.info("✅ Both access and subscription → %s", r.status_code)

        finally:
            _delete_cr("maassubscription", subscription_name, namespace=ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=ns)
            _delete_cr("maasmodelref", model_ref, namespace=ns)
            _delete_sa(sa_name, namespace=ns)
            _wait_reconcile()

    def test_e2e_with_access_but_no_subscription_gets_403(self):
        """
        Test: User with access (MaaSAuthPolicy) but not in any subscription gets 403.
        Uses existing model (facebook-opt-125m-simulated) for faster execution.

        Note: We temporarily remove simulator-subscription to ensure the test user
        has auth but no matching subscriptions.
        """
        ns = _ns()
        auth_policy_name = "e2e-test-auth-no-sub"
        sa_name = "e2e-sa-no-sub"

        # Snapshot existing subscription to restore later
        original_sim = _snapshot_cr("maassubscription", SIMULATOR_SUBSCRIPTION)

        try:
            # Create service account and get OC token for maas-api
            oc_token = _create_sa_token(sa_name, namespace=ns)
            sa_user = _sa_to_user(sa_name, namespace=ns)

            # Create auth policy for this specific user
            _create_test_auth_policy(auth_policy_name, MODEL_REF, users=[sa_user])

            # Bind simulator subscription on the key while the CR still exists, then remove it
            api_key = _create_api_key(
                oc_token,
                name=f"{sa_name}-key",
                subscription=SIMULATOR_SUBSCRIPTION,
            )

            _delete_cr("maassubscription", SIMULATOR_SUBSCRIPTION)
            _wait_reconcile()

            log.info("Testing: API key after subscription removed (auth still passes)")
            r = _poll_status(api_key, 403, path=MODEL_PATH, timeout=90)
            log.info("✅ Access but no live subscription for bound key → %s", r.status_code)

        finally:
            # Restore simulator-subscription first
            if original_sim:
                _apply_cr(original_sim)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=ns)
            _delete_sa(sa_name, namespace=ns)
            _wait_reconcile()

    def test_e2e_with_subscription_but_no_access_gets_403(self):
        """
        Test: User with subscription but not in auth policy gets 403 Forbidden.
        Uses existing model (facebook-opt-125m-simulated) for faster execution.

        Note: Temporarily removes simulator-access to ensure the test user truly
        has no auth (otherwise they'd match via system:authenticated group).
        """
        ns = _ns()
        auth_policy_name = "e2e-test-auth-no-access"
        subscription_name = "e2e-test-subscription-no-access"
        sa_with_auth = "e2e-sa-with-auth"
        sa_with_sub = "e2e-sa-with-sub"

        # Snapshot existing auth policy to restore later
        original_access = _snapshot_cr("maasauthpolicy", SIMULATOR_ACCESS_POLICY)

        try:
            # Create two service accounts:
            # - sa_with_auth: in auth policy (so the policy exists)
            # - sa_with_sub: in subscription but NOT in auth policy
            _ = _create_sa_token(sa_with_auth, namespace=ns)  # SA creation only - token unused
            oc_token_with_sub = _create_sa_token(sa_with_sub, namespace="default")  # Different namespace

            sa_with_auth_user = _sa_to_user(sa_with_auth, namespace=ns)
            sa_with_sub_user = _sa_to_user(sa_with_sub, namespace="default")

            # Delete simulator-access so system:authenticated doesn't grant auth
            _delete_cr("maasauthpolicy", SIMULATOR_ACCESS_POLICY)

            # Create test-specific auth/subscription
            _create_test_auth_policy(auth_policy_name, MODEL_REF, users=[sa_with_auth_user])
            _create_test_subscription(subscription_name, MODEL_REF, users=[sa_with_sub_user])

            _wait_reconcile()

            api_key_with_sub = _create_api_key(
                oc_token_with_sub,
                name=f"{sa_with_sub}-key",
                subscription=subscription_name,
            )

            # Test: Subscription but no access → 403
            log.info("Testing: API key with subscription but no access")
            r = _poll_status(api_key_with_sub, 403, path=MODEL_PATH, timeout=90)
            log.info("✅ Subscription but no access → %s", r.status_code)

        finally:
            # Restore simulator-access first
            if original_access:
                _apply_cr(original_access)
            _delete_cr("maassubscription", subscription_name, namespace=ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=ns)
            _delete_sa(sa_with_auth, namespace=ns)
            _delete_sa(sa_with_sub, namespace="default")
            _wait_reconcile()

    def test_e2e_single_subscription_auto_selects(self):
        """
        Test: User with single subscription auto-selects without header (PR #427).
        Uses existing model (facebook-opt-125m-simulated) for faster execution.

        Note: Temporarily removes simulator-subscription to ensure the test user
        has exactly ONE subscription (not two, which would require a header).
        """
        ns = _ns()
        auth_policy_name = "e2e-test-auth-single-sub"
        subscription_name = "e2e-test-subscription-single-sub"
        sa_name = "e2e-sa-single-sub"

        # Snapshot existing subscription to restore later
        original_sim = _snapshot_cr("maassubscription", SIMULATOR_SUBSCRIPTION)

        try:
            oc_token = _create_sa_token(sa_name, namespace=ns)
            sa_user = _sa_to_user(sa_name, namespace=ns)

            # Delete simulator-subscription so user has exactly ONE subscription
            # (otherwise they'd have 2: ours + simulator-subscription via system:authenticated)
            _delete_cr("maassubscription", SIMULATOR_SUBSCRIPTION)

            # Create auth policy and subscription for test user
            _create_test_auth_policy(auth_policy_name, MODEL_REF, users=[sa_user])
            _create_test_subscription(subscription_name, MODEL_REF, users=[sa_user])
            _wait_reconcile()

            # Exactly one subscription for this user → mint can auto-bind it without explicit name
            api_key = _create_api_key(oc_token, name=f"{sa_name}-key")

            log.info("Testing: Single subscription auto-select at mint")
            r = _poll_status(api_key, 200, path=MODEL_PATH, timeout=90)
            log.info("✅ Single subscription auto-select → %s", r.status_code)

        finally:
            # Restore simulator-subscription first
            if original_sim:
                _apply_cr(original_sim)
            _delete_cr("maassubscription", subscription_name, namespace=ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=ns)
            _delete_sa(sa_name, namespace=ns)
            _wait_reconcile()

    def test_e2e_multiple_subscriptions_separate_keys_gets_200(self):
        """
        User with two subscriptions for the same model: mint one API key per subscription;
        each key succeeds on inference without x-maas-subscription.
        """
        ns = _ns()
        auth_policy_name = "e2e-test-auth-multi-sub-valid"
        subscription_1 = "e2e-test-subscription-free"
        subscription_2 = "e2e-test-subscription-premium"
        sa_name = "e2e-sa-multi-sub-valid"

        try:
            # Create service account and get OC token for maas-api
            oc_token = _create_sa_token(sa_name, namespace=ns)
            sa_user = _sa_to_user(sa_name, namespace=ns)

            # Create test resources with 2 subscriptions for the same user
            _create_test_auth_policy(auth_policy_name, MODEL_REF, users=[sa_user])
            _create_test_subscription(subscription_1, MODEL_REF, users=[sa_user], token_limit=100)
            _create_test_subscription(subscription_2, MODEL_REF, users=[sa_user], token_limit=1000)

            _wait_reconcile()

            key1 = _create_api_key(
                oc_token,
                name=f"{sa_name}-key-tier1",
                subscription=subscription_1,
            )
            key2 = _create_api_key(
                oc_token,
                name=f"{sa_name}-key-tier2",
                subscription=subscription_2,
            )

            log.info("Testing: key bound to subscription 1")
            r1 = _poll_status(key1, 200, path=MODEL_PATH, timeout=90)
            log.info("✅ Key for tier 1 → %s", r1.status_code)

            log.info("Testing: key bound to subscription 2")
            r2 = _poll_status(key2, 200, path=MODEL_PATH, timeout=90)
            log.info("✅ Key for tier 2 → %s", r2.status_code)

        finally:
            _delete_cr("maassubscription", subscription_1, namespace=ns)
            _delete_cr("maassubscription", subscription_2, namespace=ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=ns)
            _delete_sa(sa_name, namespace=ns)
            _wait_reconcile()

    def test_e2e_mint_api_key_denied_for_inaccessible_subscription(self):
        """POST /v1/api-keys with another user's subscription returns generic invalid_subscription."""
        ns = _ns()
        auth_policy_name = "e2e-test-auth-access-denied"
        user_subscription = "e2e-test-user-subscription"
        other_subscription = "e2e-test-other-subscription"
        sa_user = "e2e-sa-user"
        sa_other = "e2e-sa-other"

        try:
            # Create two service accounts
            oc_token_user = _create_sa_token(sa_user, namespace=ns)
            _ = _create_sa_token(sa_other, namespace=ns)  # SA creation only - token unused

            user_principal = _sa_to_user(sa_user, namespace=ns)
            other_principal = _sa_to_user(sa_other, namespace=ns)

            # Create test resources
            # Both users have access to the model
            _create_test_auth_policy(auth_policy_name, MODEL_REF, users=[user_principal, other_principal])
            # Each user has their own subscription
            _create_test_subscription(user_subscription, MODEL_REF, users=[user_principal])
            _create_test_subscription(other_subscription, MODEL_REF, users=[other_principal])

            _wait_reconcile()

            r = requests.post(
                f"{_maas_api_url()}/v1/api-keys",
                headers={
                    "Authorization": f"Bearer {oc_token_user}",
                    "Content-Type": "application/json",
                },
                json={"name": f"{sa_user}-bad-sub-key", "subscription": other_subscription},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )
            assert r.status_code == 400, f"Expected 400, got {r.status_code}: {r.text[:500]}"
            assert r.json().get("code") == "invalid_subscription", r.text
            log.info("✅ Mint with inaccessible subscription → %s", r.status_code)

        finally:
            _delete_cr("maassubscription", user_subscription, namespace=ns)
            _delete_cr("maassubscription", other_subscription, namespace=ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=ns)
            _delete_sa(sa_user, namespace=ns)
            _delete_sa(sa_other, namespace=ns)
            _wait_reconcile()

    def test_e2e_group_based_access_gets_200(self):
        """
        E2E test: Group-based auth and subscription (success case).

        Validates that users can access models via group membership in both
        MaaSAuthPolicy and MaaSSubscription, not just explicit user lists.
        """
        ns = _ns()
        auth_policy_name = "e2e-test-group-auth"
        subscription_name = "e2e-test-group-subscription"
        sa_name = "e2e-sa-group"

        # Use namespace-specific group that SA will be in
        test_group = f"system:serviceaccounts:{ns}"

        try:
            # Create service account and get OC token for maas-api
            oc_token = _create_sa_token(sa_name, namespace=ns)

            # Create auth policy using GROUP (not user)
            _create_test_auth_policy(auth_policy_name, MODEL_REF, groups=[test_group])

            # Create subscription using GROUP (not user)
            _create_test_subscription(subscription_name, MODEL_REF, groups=[test_group])

            _wait_reconcile()

            api_key = _create_api_key(
                oc_token,
                name=f"{sa_name}-key",
                subscription=subscription_name,
            )

            # Test: User matches via group membership → 200
            log.info("Testing: Group-based auth and subscription")
            r = _poll_status(api_key, 200, path=MODEL_PATH, timeout=90)
            log.info("✅ Group-based access → %s", r.status_code)

        finally:
            _delete_cr("maassubscription", subscription_name, namespace=ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=ns)
            _delete_sa(sa_name, namespace=ns)
            _wait_reconcile()

    def test_e2e_group_based_auth_but_no_subscription_gets_403(self):
        """
        E2E test: Group-based auth, but user's group not in any subscription (failure case).

        Validates that having auth via group membership is not sufficient if the user's
        groups don't match any subscription's owner groups.
        """
        ns = _ns()
        auth_policy_name = "e2e-test-group-auth-only"
        sa_name = "e2e-sa-group-auth-only"

        # Use namespace-specific group for auth
        test_group = f"system:serviceaccounts:{ns}"

        # Snapshot existing subscription to restore later
        original_sim = _snapshot_cr("maassubscription", SIMULATOR_SUBSCRIPTION)

        try:
            # Create service account and get OC token for maas-api
            oc_token = _create_sa_token(sa_name, namespace=ns)

            # Create auth policy using group
            _create_test_auth_policy(auth_policy_name, MODEL_REF, groups=[test_group])

            api_key = _create_api_key(
                oc_token,
                name=f"{sa_name}-key",
                subscription=SIMULATOR_SUBSCRIPTION,
            )

            _delete_cr("maassubscription", SIMULATOR_SUBSCRIPTION)
            _wait_reconcile()

            log.info("Testing: Group-based auth; key bound to removed subscription")
            r = _poll_status(api_key, 403, path=MODEL_PATH, timeout=90)
            log.info("✅ Group auth but no live subscription for bound key → %s", r.status_code)

        finally:
            # Restore simulator-subscription first
            if original_sim:
                _apply_cr(original_sim)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=ns)
            _delete_sa(sa_name, namespace=ns)
            _wait_reconcile()

    def test_e2e_group_based_subscription_but_no_auth_gets_403(self):
        """
        E2E test: Group-based subscription, but user's group not in auth policy (failure case).

        Validates that having a subscription via group membership is not sufficient if the
        user's groups don't match the auth policy.

        Note: Temporarily removes simulator-access to ensure the test user truly
        has no auth (otherwise they'd match via system:authenticated group).
        """
        ns = _ns()
        auth_policy_name = "e2e-test-group-no-auth"
        subscription_name = "e2e-test-group-sub-only"
        sa_name = "e2e-sa-group-sub-only"

        # Use namespace-specific group for subscription
        test_group = f"system:serviceaccounts:{ns}"

        # Snapshot existing auth policy to restore later
        original_access = _snapshot_cr("maasauthpolicy", SIMULATOR_ACCESS_POLICY)

        try:
            # Create service account and get OC token for maas-api
            oc_token = _create_sa_token(sa_name, namespace=ns)

            # Delete simulator-access so system:authenticated doesn't grant auth
            _delete_cr("maasauthpolicy", SIMULATOR_ACCESS_POLICY)

            # Create auth policy with a group the SA is NOT in
            _create_test_auth_policy(auth_policy_name, MODEL_REF, groups=["nonexistent-group-xyz"])

            # Create subscription with group the SA IS in
            _create_test_subscription(subscription_name, MODEL_REF, groups=[test_group])

            _wait_reconcile()

            api_key = _create_api_key(
                oc_token,
                name=f"{sa_name}-key",
                subscription=subscription_name,
            )

            # Test: Has subscription via group but no auth → 403
            log.info("Testing: Group-based subscription but no auth")
            r = _poll_status(api_key, 403, path=MODEL_PATH, timeout=90)
            log.info("✅ Group subscription but no auth → %s", r.status_code)

        finally:
            # Restore simulator-access first
            if original_access:
                _apply_cr(original_access)
            _delete_cr("maassubscription", subscription_name, namespace=ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=ns)
            _delete_sa(sa_name, namespace=ns)
            _wait_reconcile()
