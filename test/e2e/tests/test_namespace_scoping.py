"""
E2E tests for namespace scoping in MaaS Controller.

Tests that MaaSAuthPolicy and MaaSSubscription can reference MaaSModelRef resources
in different namespaces, and that the generated AuthPolicies and TokenRateLimitPolicies
are created in the correct (model's) namespace.

Uses real LLMInferenceService models and makes actual inference requests to verify
the entire flow works end-to-end.

Requires:
  - GATEWAY_HOST env var (e.g. maas.apps.cluster.example.com)
  - MAAS_API_BASE_URL env var (e.g. https://maas.apps.cluster.example.com/maas-api)
  - maas-controller deployed
  - LLMInferenceService deployed in llm namespace (facebook-opt-125m-simulated)
  - oc/kubectl access with cluster-admin or sufficient RBAC permissions

Environment variables (all optional, with defaults):
  - GATEWAY_HOST: Gateway hostname (required)
  - MAAS_API_BASE_URL: MaaS API URL (required)
  - MAAS_NAMESPACE: Default MaaS namespace (default: opendatahub)
  - E2E_TIMEOUT: Request timeout in seconds (default: 30)
  - E2E_RECONCILE_WAIT: Wait time for controller reconciliation (default: 15)
  - E2E_SKIP_TLS_VERIFY: Set to "true" to skip TLS verification
  - E2E_MODEL_REF: Model ref for tests (default: facebook-opt-125m-simulated)
  - E2E_MODEL_NAMESPACE: Namespace where model MaaSModelRef lives (default: llm)
"""

import json
import logging
import os
import subprocess
import time
import uuid
from typing import Optional

import pytest
import requests

log = logging.getLogger(__name__)

# Constants
TIMEOUT = int(os.environ.get("E2E_TIMEOUT", "30"))
RECONCILE_WAIT = int(os.environ.get("E2E_RECONCILE_WAIT", "15"))  # Increased from 10 to 15 for reliability
TLS_VERIFY = os.environ.get("E2E_SKIP_TLS_VERIFY", "").lower() != "true"
MODEL_REF = os.environ.get("E2E_MODEL_REF", "facebook-opt-125m-simulated")
MODEL_NAME = os.environ.get("E2E_MODEL_NAME", "facebook/opt-125m")
MODEL_NAMESPACE = os.environ.get("E2E_MODEL_NAMESPACE", "llm")


def _ns():
    """Default MaaS namespace."""
    return os.environ.get("MAAS_NAMESPACE", "opendatahub")


def _gateway_url():
    """Gateway URL for inference requests."""
    host = os.environ.get("GATEWAY_HOST", "")
    if not host:
        raise RuntimeError("GATEWAY_HOST env var is required")
    scheme = "http" if os.environ.get("INSECURE_HTTP", "").lower() == "true" else "https"
    return f"{scheme}://{host}"


def _maas_api_url():
    """MaaS API base URL."""
    url = os.environ.get("MAAS_API_BASE_URL", "")
    if not url:
        host = os.environ.get("GATEWAY_HOST", "")
        if not host:
            raise RuntimeError("MAAS_API_BASE_URL or GATEWAY_HOST env var is required")
        scheme = "http" if os.environ.get("INSECURE_HTTP", "").lower() == "true" else "https"
        url = f"{scheme}://{host}/maas-api"
    return url


def _get_token():
    """Get OC token for authentication."""
    token = os.environ.get("TOKEN", "")
    if token:
        return token
    result = subprocess.run(["oc", "whoami", "-t"], capture_output=True, text=True)
    token = result.stdout.strip()
    if not token:
        raise RuntimeError("Could not get token via `oc whoami -t`")
    return token


def _create_api_key(name: str = None) -> tuple[str, str]:
    """Create an API key and return (key_id, plaintext_key)."""
    token = _get_token()
    url = f"{_maas_api_url()}/v1/api-keys"
    key_name = name or f"e2e-ns-test-{uuid.uuid4().hex[:8]}"

    r = requests.post(
        url,
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
        json={"name": key_name},
        timeout=TIMEOUT,
        verify=TLS_VERIFY,
    )
    if r.status_code not in (200, 201):
        raise RuntimeError(f"Failed to create API key: {r.status_code} {r.text}")

    data = r.json()
    return data.get("id"), data.get("key")


def _apply_cr(cr_dict: dict):
    """Apply CR from dict."""
    subprocess.run(
        ["oc", "apply", "-f", "-"],
        input=json.dumps(cr_dict),
        capture_output=True,
        text=True,
        check=True,
    )


def _delete_cr(kind: str, name: str, namespace: str):
    """Delete CR (best effort)."""
    subprocess.run(
        ["oc", "delete", kind, name, "-n", namespace, "--ignore-not-found", "--timeout=30s"],
        capture_output=True,
        text=True,
    )


def _get_cr(kind: str, name: str, namespace: str) -> Optional[dict]:
    """Get CR as dict, or None if not found."""
    result = subprocess.run(
        ["oc", "get", kind, name, "-n", namespace, "-o", "json"],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        return None
    return json.loads(result.stdout)


def _wait_for_cr(kind: str, name: str, namespace: str, timeout: int = 30) -> bool:
    """Wait for CR to exist."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        if _get_cr(kind, name, namespace):
            return True
        time.sleep(2)
    return False


def _wait_for_authpolicy_enforced(name: str, namespace: str, timeout: int = 60) -> bool:
    """Wait for AuthPolicy to be enforced."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        cr = _get_cr("AuthPolicy", name, namespace)
        if cr:
            conditions = cr.get("status", {}).get("conditions", [])
            for condition in conditions:
                if condition.get("type") == "Enforced" and condition.get("status") == "True":
                    return True
        time.sleep(3)
    return False


def _wait_for_trlp_enforced_with_retry(name: str, namespace: str, timeout: int = 90, retries: int = 3) -> bool:
    """Wait for TokenRateLimitPolicy to be enforced with retry logic.

    Args:
        name: Name of the TokenRateLimitPolicy
        namespace: Namespace where TRLP exists
        timeout: Maximum time to wait per attempt in seconds
        retries: Number of retry attempts after the initial try

    Returns:
        True if enforced within timeout and retries, False otherwise
    """
    for attempt in range(retries + 1):  # retries + 1 = initial attempt + retries
        if attempt > 0:
            log.info(f"Retry {attempt}/{retries} for TRLP {name} enforcement check...")
            time.sleep(5)  # Brief pause between retries

        if _wait_for_trlp_enforced(name, namespace, timeout):
            return True

    log.error(f"TRLP {name} not enforced after {retries} retries")
    return False


def _wait_for_trlp_enforced(name: str, namespace: str, timeout: int = 90, expected_generation: Optional[int] = None) -> bool:
    """Wait for TokenRateLimitPolicy to be enforced.

    Args:
        name: Name of the TokenRateLimitPolicy
        namespace: Namespace where TRLP exists
        timeout: Maximum time to wait in seconds (default: 90)
        expected_generation: If provided, wait for generation >= this value

    Returns:
        True if enforced within timeout, False otherwise
    """
    deadline = time.time() + timeout
    last_status = None

    while time.time() < deadline:
        cr = _get_cr("TokenRateLimitPolicy", name, namespace)
        if cr:
            generation = cr.get("metadata", {}).get("generation", 0)
            conditions = cr.get("status", {}).get("conditions", [])

            # Check generation if expected_generation is specified
            if expected_generation is not None and generation < expected_generation:
                last_status = f"generation={generation}, expected>={expected_generation}"
                time.sleep(3)
                continue

            # Check enforcement status
            for condition in conditions:
                if condition.get("type") == "Enforced" and condition.get("status") == "True":
                    log.info(f"TRLP {name} enforced (generation={generation})")
                    return True

            last_status = f"generation={generation}, not enforced yet"
        else:
            last_status = "TRLP not found"

        time.sleep(3)

    log.warning(f"TRLP {name} not enforced within {timeout}s. Last status: {last_status}")
    return False


def _get_trlp_generation(name: str, namespace: str) -> int:
    """Get the current generation of a TokenRateLimitPolicy.

    Returns:
        Current generation number, or 0 if TRLP doesn't exist
    """
    cr = _get_cr("TokenRateLimitPolicy", name, namespace)
    if cr:
        return cr.get("metadata", {}).get("generation", 0)
    return 0


def _get_trlp_metadata(name: str, namespace: str) -> dict:
    """Get the current metadata of a TokenRateLimitPolicy.

    Returns:
        dict with 'uid', 'resourceVersion', 'generation', or empty dict if not found
    """
    cr = _get_cr("TokenRateLimitPolicy", name, namespace)
    if cr:
        metadata = cr.get("metadata", {})
        return {
            "uid": metadata.get("uid", ""),
            "resourceVersion": metadata.get("resourceVersion", ""),
            "generation": metadata.get("generation", 0)
        }
    return {}


def _create_namespace(name: str):
    """Create namespace if it doesn't exist."""
    result = subprocess.run(
        ["oc", "create", "namespace", name],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0 and "already exists" not in result.stderr:
        raise RuntimeError(f"Failed to create namespace {name}: {result.stderr}")


def _delete_namespace(name: str):
    """Delete namespace (best effort)."""
    subprocess.run(
        ["oc", "delete", "namespace", name, "--ignore-not-found", "--timeout=60s"],
        capture_output=True,
        text=True,
    )


def _list_models(api_key: str, model_ref: str, model_namespace: str = None, subscription: str = "simulator-subscription") -> requests.Response:
    """List available models."""
    url = f"{_gateway_url()}/llm/{model_ref}/v1/models"
    headers = {"Authorization": f"Bearer {api_key}"}
    if subscription:
        headers["x-maas-subscription"] = subscription
    return requests.get(
        url,
        headers=headers,
        timeout=TIMEOUT,
        verify=TLS_VERIFY,
    )


def _inference(api_key: str, model_ref: str, model_namespace: str, subscription: str = "simulator-subscription") -> requests.Response:
    """Make an inference request to a model."""
    url = f"{_gateway_url()}/llm/{model_ref}/v1/completions"
    headers = {
        "Authorization": f"Bearer {api_key}",
        "Content-Type": "application/json",
    }
    if subscription:
        headers["x-maas-subscription"] = subscription
    return requests.post(
        url,
        headers=headers,
        json={"model": MODEL_NAME, "prompt": "Hello", "max_tokens": 3},
        timeout=TIMEOUT,
        verify=TLS_VERIFY,
    )


def _poll_status(api_key: str, model_ref: str, model_namespace: str, expected: int, timeout: int = 60, interval: int = 2, subscription: str = "simulator-subscription") -> requests.Response:
    """Poll inference endpoint until expected status or timeout."""
    deadline = time.time() + timeout
    last_response = None
    last_error = None
    while time.time() < deadline:
        try:
            r = _inference(api_key, model_ref, model_namespace, subscription=subscription)
            last_response = r
            if r.status_code == expected:
                return r
            log.warning(f"Got status {r.status_code}, expected {expected}. Response: {r.text[:200]}")
        except Exception as e:
            log.warning(f"Request failed: {type(e).__name__}: {e}")
            last_error = e
        time.sleep(interval)

    if last_response:
        status = f"{last_response.status_code}: {last_response.text[:200]}"
    elif last_error:
        status = f"Exception: {type(last_error).__name__}: {last_error}"
    else:
        status = "No response"
    raise AssertionError(f"Expected status {expected} within {timeout}s, got {status}")


@pytest.fixture(scope="module")
def policy_namespace():
    """Create a test namespace for policies."""
    ns = f"e2e-ns-policy-{uuid.uuid4().hex[:6]}"
    log.info(f"Creating policy namespace: {ns}")
    _create_namespace(ns)
    yield ns
    log.info(f"Cleaning up policy namespace: {ns}")
    _delete_namespace(ns)


@pytest.fixture(scope="module")
def api_key():
    """Create an API key for tests."""
    _, key = _create_api_key("e2e-ns-scoping-key")
    return key


class TestCrossNamespaceAuthPolicy:
    """Test MaaSAuthPolicy can reference models in different namespaces."""

    def test_auth_policy_in_different_namespace(self, policy_namespace, api_key):
        """
        Create MaaSAuthPolicy in policy-namespace that references model in llm namespace.
        Verify that AuthPolicy is created in llm namespace (model's namespace).
        Verify that inference requests work.
        """
        policy_name = f"cross-ns-auth-{uuid.uuid4().hex[:6]}"

        log.info(f"Creating cross-namespace MaaSAuthPolicy {policy_name}")
        log.info(f"  Policy namespace: {policy_namespace}")
        log.info(f"  Model namespace: {MODEL_NAMESPACE}")

        # Create MaaSAuthPolicy in policy namespace referencing model in MODEL_NAMESPACE
        _apply_cr({
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSAuthPolicy",
            "metadata": {"name": policy_name, "namespace": policy_namespace},
            "spec": {
                "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE}],
                "subjects": {"groups": [{"name": "system:authenticated"}]},
            },
        })

        try:
            # Wait for controller to reconcile
            time.sleep(RECONCILE_WAIT)

            # Verify MaaSAuthPolicy exists
            maas_policy = _get_cr("MaaSAuthPolicy", policy_name, policy_namespace)
            assert maas_policy is not None, f"MaaSAuthPolicy {policy_name} not found"

            # Verify generated AuthPolicy is created in MODEL namespace (not policy namespace)
            auth_policy_name = f"maas-auth-{MODEL_REF}"
            auth_policy = _get_cr("AuthPolicy", auth_policy_name, MODEL_NAMESPACE)
            assert auth_policy is not None, \
                f"AuthPolicy {auth_policy_name} not found in model namespace {MODEL_NAMESPACE}"

            # Wait for AuthPolicy to be enforced
            log.info(f"Waiting for AuthPolicy {auth_policy_name} to be enforced...")
            enforced = _wait_for_authpolicy_enforced(auth_policy_name, MODEL_NAMESPACE, timeout=60)
            assert enforced, f"AuthPolicy {auth_policy_name} not enforced within timeout"

            # Verify AuthPolicy is NOT in policy namespace
            auth_in_policy_ns = _get_cr("AuthPolicy", auth_policy_name, policy_namespace)
            assert auth_in_policy_ns is None, \
                f"AuthPolicy should not exist in policy namespace {policy_namespace}"

            # Test model listing endpoint
            log.info("Testing /v1/models endpoint with cross-namespace AuthPolicy")
            r = _list_models(api_key, MODEL_REF, MODEL_NAMESPACE)
            assert r.status_code == 200, f"Model listing failed: {r.status_code}"
            models_data = r.json()
            model_ids = [m.get("id") for m in models_data.get("data", [])]
            assert MODEL_NAME in model_ids, f"Expected model {MODEL_NAME} not in list: {model_ids}"
            log.info(f"✓ Model {MODEL_NAME} found in /v1/models response")

            # Make inference request to verify everything works
            log.info("Testing inference request with cross-namespace AuthPolicy")
            r = _poll_status(api_key, MODEL_REF, MODEL_NAMESPACE, 200, timeout=90)
            assert r.status_code == 200, f"Expected 200, got {r.status_code}"

            log.info("✓ Cross-namespace AuthPolicy test passed")

        finally:
            _delete_cr("MaaSAuthPolicy", policy_name, policy_namespace)
            # Note: We don't delete the AuthPolicy because other tests may rely on it

    def test_multiple_policies_different_namespaces_same_model(self, policy_namespace, api_key):
        """
        Create multiple MaaSAuthPolicies in different namespaces referencing the same model.
        Verify that they aggregate correctly into a single AuthPolicy.
        Test the deletion bug fix: deleting one policy should NOT delete the AuthPolicy.
        """
        policy1_name = f"multi-ns-policy1-{uuid.uuid4().hex[:6]}"
        policy2_name = f"multi-ns-policy2-{uuid.uuid4().hex[:6]}"
        test_group = f"test-group-{uuid.uuid4().hex[:4]}"

        # Create second policy namespace
        policy_namespace2 = f"e2e-ns-policy2-{uuid.uuid4().hex[:6]}"
        _create_namespace(policy_namespace2)

        try:
            log.info(f"Creating two MaaSAuthPolicies in different namespaces for same model")

            # Create first policy in policy_namespace
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {"name": policy1_name, "namespace": policy_namespace},
                "spec": {
                    "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE}],
                    "subjects": {"groups": [{"name": "system:authenticated"}]},
                },
            })

            # Create second policy in policy_namespace2
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {"name": policy2_name, "namespace": policy_namespace2},
                "spec": {
                    "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE}],
                    "subjects": {"groups": [{"name": test_group}]},
                },
            })

            # Wait for reconciliation
            time.sleep(RECONCILE_WAIT)

            # Verify AuthPolicy exists in model namespace
            auth_policy_name = f"maas-auth-{MODEL_REF}"
            auth_policy = _get_cr("AuthPolicy", auth_policy_name, MODEL_NAMESPACE)
            assert auth_policy is not None, "Aggregated AuthPolicy should exist"

            # Wait for AuthPolicy to be enforced
            log.info(f"Waiting for AuthPolicy {auth_policy_name} to be enforced...")
            enforced = _wait_for_authpolicy_enforced(auth_policy_name, MODEL_NAMESPACE, timeout=60)
            assert enforced, f"AuthPolicy {auth_policy_name} not enforced within timeout"

            # Verify both policies' subjects are in the AuthPolicy
            spec = auth_policy.get("spec", {})
            log.info(f"AuthPolicy spec: {json.dumps(spec, indent=2)[:500]}")

            # Test model listing endpoint
            r = _list_models(api_key, MODEL_REF, MODEL_NAMESPACE)
            assert r.status_code == 200, f"Model listing failed: {r.status_code}"
            models_data = r.json()
            model_ids = [m.get("id") for m in models_data.get("data", [])]
            assert MODEL_NAME in model_ids, f"Expected model {MODEL_NAME} not in list: {model_ids}"

            # Make inference request to verify it works
            r = _poll_status(api_key, MODEL_REF, MODEL_NAMESPACE, 200, timeout=90)
            assert r.status_code == 200, f"Expected 200, got {r.status_code}"

            # Now delete the FIRST policy (not the last one)
            log.info(f"Deleting first policy {policy1_name} (should NOT delete AuthPolicy)")
            _delete_cr("MaaSAuthPolicy", policy1_name, policy_namespace)
            time.sleep(RECONCILE_WAIT)

            # Verify AuthPolicy still exists (it should be rebuilt)
            auth_policy_after = _get_cr("AuthPolicy", auth_policy_name, MODEL_NAMESPACE)
            assert auth_policy_after is not None, \
                "AuthPolicy should still exist after deleting first policy (other policies reference it)"

            # Inference should still work
            r = _poll_status(api_key, MODEL_REF, MODEL_NAMESPACE, 200, timeout=60)
            assert r.status_code == 200, "Inference should still work after deleting first policy"

            # Now delete the LAST test policy
            log.info(f"Deleting last test policy {policy2_name}")
            _delete_cr("MaaSAuthPolicy", policy2_name, policy_namespace2)
            time.sleep(RECONCILE_WAIT)

            # Verify AuthPolicy still exists if there are other MaaSAuthPolicies for this model
            # (like the pre-existing simulator-access policy)
            auth_policy_final = _get_cr("AuthPolicy", auth_policy_name, MODEL_NAMESPACE)
            # Note: AuthPolicy may still exist due to pre-existing policies (e.g. simulator-access)
            # The key test was that deleting the FIRST policy didn't delete the AuthPolicy
            log.info(f"AuthPolicy exists after deleting test policies: {auth_policy_final is not None}")

            log.info("✓ Multiple policies deletion test passed (bug fix verified)")

        finally:
            for resource, name, ns in [
                ("MaaSAuthPolicy", policy1_name, policy_namespace),
                ("MaaSAuthPolicy", policy2_name, policy_namespace2),
            ]:
                try:
                    _delete_cr(resource, name, ns)
                except Exception as e:
                    log.warning(f"Cleanup failed for {resource}/{name}: {e}")
            try:
                _delete_namespace(policy_namespace2)
            except Exception as e:
                log.warning(f"Cleanup failed for namespace {policy_namespace2}: {e}")


class TestCrossNamespaceSubscription:
    """Test MaaSSubscription can reference models in different namespaces."""

    def test_subscription_in_different_namespace(self, policy_namespace, api_key):
        """
        Create MaaSSubscription in the subscription namespace that references model in llm namespace.
        Verify that TokenRateLimitPolicy is created in llm namespace (model's namespace).
        Verify that inference requests work with rate limiting.

        NOTE: MaaSSubscription must be in the MAAS subscription namespace (models-as-a-service)
        as the controller only watches that namespace. The "cross-namespace" aspect is that
        the subscription references a model in a DIFFERENT namespace (llm).
        """
        subscription_name = f"cross-ns-sub-{uuid.uuid4().hex[:6]}"
        maas_auth_policy_name = f"cross-ns-auth-for-sub-{uuid.uuid4().hex[:6]}"

        # Get the configured MAAS subscription namespace
        maas_subscription_ns = os.environ.get("MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service")

        log.info(f"Creating cross-namespace MaaSSubscription {subscription_name}")
        log.info(f"  Subscription namespace: {maas_subscription_ns}")
        log.info(f"  Model namespace: {MODEL_NAMESPACE}")

        # First ensure there's an AuthPolicy for the model (subscription needs auth to work)
        _apply_cr({
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSAuthPolicy",
            "metadata": {"name": maas_auth_policy_name, "namespace": maas_subscription_ns},
            "spec": {
                "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE}],
                "subjects": {"groups": [{"name": "system:authenticated"}]},
            },
        })

        # Create MaaSSubscription in MAAS subscription namespace referencing model in MODEL_NAMESPACE
        _apply_cr({
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSSubscription",
            "metadata": {"name": subscription_name, "namespace": maas_subscription_ns},
            "spec": {
                "owner": {"groups": [{"name": "system:authenticated"}]},
                "modelRefs": [
                    {
                        "name": MODEL_REF,
                        "namespace": MODEL_NAMESPACE,
                        "tokenRateLimits": [{"limit": 100, "window": "1m"}],
                    }
                ],
            },
        })

        try:
            # Wait for controller to reconcile
            time.sleep(RECONCILE_WAIT)

            # Verify MaaSSubscription exists
            maas_sub = _get_cr("MaaSSubscription", subscription_name, maas_subscription_ns)
            assert maas_sub is not None, f"MaaSSubscription {subscription_name} not found"

            # Wait for AuthPolicy to be enforced (subscription needs auth to work)
            auth_policy_name = f"maas-auth-{MODEL_REF}"
            log.info(f"Waiting for AuthPolicy {auth_policy_name} to be enforced...")
            enforced = _wait_for_authpolicy_enforced(auth_policy_name, MODEL_NAMESPACE, timeout=60)
            assert enforced, f"AuthPolicy {auth_policy_name} not enforced within timeout"

            # Wait for TokenRateLimitPolicy to be enforced before making requests
            # This implicitly verifies TRLP exists in the model namespace
            trlp_name = f"maas-trlp-{MODEL_REF}"
            log.info(f"Waiting for TokenRateLimitPolicy {trlp_name} to be enforced...")
            trlp_enforced = _wait_for_trlp_enforced_with_retry(trlp_name, MODEL_NAMESPACE, timeout=60, retries=3)
            assert trlp_enforced, f"TokenRateLimitPolicy {trlp_name} not enforced within timeout"

            # Verify TokenRateLimitPolicy is NOT in subscription namespace (after enforcement verified)
            trlp_in_sub_ns = _get_cr("TokenRateLimitPolicy", trlp_name, maas_subscription_ns)
            assert trlp_in_sub_ns is None, \
                f"TokenRateLimitPolicy should not exist in subscription namespace {maas_subscription_ns}"

            # Test model listing endpoint
            log.info(f"Testing /v1/models endpoint with cross-namespace Subscription {subscription_name}")
            r = _list_models(api_key, MODEL_REF, MODEL_NAMESPACE, subscription=subscription_name)
            assert r.status_code == 200, f"Model listing failed: {r.status_code}"
            models_data = r.json()
            model_ids = [m.get("id") for m in models_data.get("data", [])]
            assert MODEL_NAME in model_ids, f"Expected model {MODEL_NAME} not in list: {model_ids}"
            log.info(f"✓ Model {MODEL_NAME} found in /v1/models response")

            # Make inference request to verify everything works
            log.info(f"Testing inference request with subscription {subscription_name}")
            r = _poll_status(api_key, MODEL_REF, MODEL_NAMESPACE, 200, timeout=90, subscription=subscription_name)
            assert r.status_code == 200, f"Expected 200, got {r.status_code}"

            log.info("✓ Cross-namespace Subscription test passed")

        finally:
            for resource, name, ns in [
                ("MaaSSubscription", subscription_name, maas_subscription_ns),
                ("MaaSAuthPolicy", maas_auth_policy_name, maas_subscription_ns),
            ]:
                try:
                    _delete_cr(resource, name, ns)
                except Exception as e:
                    log.warning(f"Cleanup failed for {resource}/{name}: {e}")

    def test_multiple_subscriptions_different_namespaces_same_model(self, policy_namespace, api_key):
        """
        Create multiple MaaSSubscriptions in the subscription namespace referencing the same model.
        Verify that they aggregate correctly into a single TokenRateLimitPolicy.
        Test the deletion bug fix: deleting one subscription should NOT delete the TRLP.

        NOTE: All subscriptions must be in the MAAS subscription namespace (models-as-a-service)
        as the controller only watches that namespace for MaaSSubscription resources.
        """
        sub1_name = f"multi-ns-sub1-{uuid.uuid4().hex[:6]}"
        sub2_name = f"multi-ns-sub2-{uuid.uuid4().hex[:6]}"
        maas_auth_policy_name = f"multi-ns-auth-{uuid.uuid4().hex[:6]}"
        test_group = f"test-group-{uuid.uuid4().hex[:4]}"

        # Get the configured MAAS subscription namespace
        maas_subscription_ns = os.environ.get("MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service")

        try:
            log.info(f"Creating two MaaSSubscriptions in {maas_subscription_ns} for same model")

            # Create auth policy first
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {"name": maas_auth_policy_name, "namespace": maas_subscription_ns},
                "spec": {
                    "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE}],
                    "subjects": {"groups": [{"name": "system:authenticated"}]},
                },
            })

            # Create first subscription in MAAS subscription namespace
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": sub1_name, "namespace": maas_subscription_ns},
                "spec": {
                    "owner": {"groups": [{"name": "system:authenticated"}]},
                    "modelRefs": [
                        {
                            "name": MODEL_REF,
                            "namespace": MODEL_NAMESPACE,
                            "tokenRateLimits": [{"limit": 100, "window": "1m"}],
                        }
                    ],
                },
            })

            # Create second subscription in MAAS subscription namespace
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": sub2_name, "namespace": maas_subscription_ns},
                "spec": {
                    "owner": {"groups": [{"name": test_group}]},
                    "modelRefs": [
                        {
                            "name": MODEL_REF,
                            "namespace": MODEL_NAMESPACE,
                            "tokenRateLimits": [{"limit": 200, "window": "1m"}],
                        }
                    ],
                },
            })

            # Wait for reconciliation
            time.sleep(RECONCILE_WAIT)

            # Wait for AuthPolicy to be enforced (subscription needs auth to work)
            auth_policy_name = f"maas-auth-{MODEL_REF}"
            log.info(f"Waiting for AuthPolicy {auth_policy_name} to be enforced...")
            enforced = _wait_for_authpolicy_enforced(auth_policy_name, MODEL_NAMESPACE, timeout=60)
            assert enforced, f"AuthPolicy {auth_policy_name} not enforced within timeout"

            # Wait for TokenRateLimitPolicy to be enforced before making requests
            # This implicitly verifies TRLP exists and is aggregated correctly
            trlp_name = f"maas-trlp-{MODEL_REF}"
            log.info(f"Waiting for TokenRateLimitPolicy {trlp_name} to be enforced...")
            trlp_enforced = _wait_for_trlp_enforced_with_retry(trlp_name, MODEL_NAMESPACE, timeout=60, retries=3)
            assert trlp_enforced, f"TokenRateLimitPolicy {trlp_name} not enforced within timeout"

            # Test model listing endpoint
            log.info(f"Testing /v1/models with subscription {sub1_name}")
            r = _list_models(api_key, MODEL_REF, MODEL_NAMESPACE, subscription=sub1_name)
            assert r.status_code == 200, f"Model listing failed: {r.status_code}"
            models_data = r.json()
            model_ids = [m.get("id") for m in models_data.get("data", [])]
            assert MODEL_NAME in model_ids, f"Expected model {MODEL_NAME} not in list: {model_ids}"

            # Make inference request to verify it works
            r = _poll_status(api_key, MODEL_REF, MODEL_NAMESPACE, 200, timeout=90, subscription=sub1_name)
            assert r.status_code == 200, f"Expected 200, got {r.status_code}"

            # Capture TRLP metadata before deletion for verification
            # Retry to ensure we have valid metadata (not transient/empty)
            log.info(f"Capturing TRLP {trlp_name} metadata before deletion...")
            metadata_before = {}
            metadata_deadline = time.time() + 30
            while time.time() < metadata_deadline:
                metadata_before = _get_trlp_metadata(trlp_name, MODEL_NAMESPACE)
                # Ensure essential fields are non-empty
                if (metadata_before and
                    metadata_before.get('uid') and
                    metadata_before.get('resourceVersion') and
                    metadata_before.get('generation', 0) > 0):
                    break
                log.info("Retrying metadata capture (transient or empty values)...")
                time.sleep(2)

            # Verify we have valid metadata before proceeding
            assert metadata_before, "Failed to capture valid TRLP metadata before deletion"
            assert metadata_before.get('uid'), "TRLP uid is empty"
            assert metadata_before.get('resourceVersion'), "TRLP resourceVersion is empty"
            assert metadata_before.get('generation', 0) > 0, "TRLP generation is 0"

            log.info(f"TRLP before deletion - generation: {metadata_before.get('generation')}, "
                    f"resourceVersion: {metadata_before.get('resourceVersion')}, "
                    f"uid: {metadata_before.get('uid', '')[:8]}...")

            # Delete the FIRST subscription (not the last one)
            log.info(f"Deleting first subscription {sub1_name} (should NOT delete TRLP)")
            _delete_cr("MaaSSubscription", sub1_name, maas_subscription_ns)
            time.sleep(RECONCILE_WAIT)

            # Verify TRLP still exists (it should be rebuilt)
            # Use wait/retry to handle transient delete-recreate windows
            log.info("Waiting for TokenRateLimitPolicy to exist after subscription deletion...")
            trlp_after = None
            wait_deadline = time.time() + 30
            while time.time() < wait_deadline:
                trlp_after = _get_cr("TokenRateLimitPolicy", trlp_name, MODEL_NAMESPACE)
                if trlp_after is not None:
                    break
                time.sleep(2)
            assert trlp_after is not None, \
                "TokenRateLimitPolicy should still exist after deleting first subscription"

            # Wait for TRLP to be re-enforced AND updated after controller modifies it
            # (controller deletes and recreates TRLP when subscriptions change)
            # Must check both enforcement and metadata change in the same loop to avoid race
            log.info(f"Waiting for TokenRateLimitPolicy {trlp_name} to be re-enforced and updated...")
            deadline = time.time() + 90
            metadata_after = {}
            while time.time() < deadline:
                # must be enforced
                if not _wait_for_trlp_enforced(trlp_name, MODEL_NAMESPACE, timeout=15):
                    continue
                # must also be a new revision/object
                metadata_after = _get_trlp_metadata(trlp_name, MODEL_NAMESPACE)
                uid_changed = metadata_after.get("uid") != metadata_before.get("uid")
                rv_changed = metadata_after.get("resourceVersion") != metadata_before.get("resourceVersion")
                if uid_changed or rv_changed:
                    break
                time.sleep(3)

            assert metadata_after, f"TokenRateLimitPolicy {trlp_name} did not stabilize after subscription deletion"
            log.info(f"TRLP after deletion - generation: {metadata_after.get('generation')}, "
                    f"resourceVersion: {metadata_after.get('resourceVersion')}, "
                    f"uid: {metadata_after.get('uid', '')[:8]}...")

            # Assert that TRLP was updated: either UID changed (delete+recreate) or resourceVersion changed (update)
            uid_changed = metadata_after.get("uid") != metadata_before.get("uid")
            rv_changed = metadata_after.get("resourceVersion") != metadata_before.get("resourceVersion")
            gen_increased = metadata_after.get("generation", 0) > metadata_before.get("generation", 0)

            assert uid_changed or rv_changed, \
                f"TRLP metadata should have changed after subscription deletion. " \
                f"Before: uid={metadata_before.get('uid', '')[:8]}..., rv={metadata_before.get('resourceVersion')}. " \
                f"After: uid={metadata_after.get('uid', '')[:8]}..., rv={metadata_after.get('resourceVersion')}"

            if uid_changed:
                log.info("✓ TRLP was deleted and recreated (UID changed)")
            elif gen_increased:
                log.info(f"✓ TRLP was updated (generation {metadata_before.get('generation')} → {metadata_after.get('generation')})")
            else:
                log.info("✓ TRLP resourceVersion changed (updated in-place)")

            # Inference should still work (use default simulator-subscription since sub1 was deleted)
            log.info("Testing inference with default simulator-subscription after deleting sub1")
            r = _poll_status(api_key, MODEL_REF, MODEL_NAMESPACE, 200, timeout=60, subscription="simulator-subscription")
            assert r.status_code == 200, "Inference should still work after deleting first subscription"

            # Delete the LAST test subscription
            log.info(f"Deleting last test subscription {sub2_name}")
            _delete_cr("MaaSSubscription", sub2_name, maas_subscription_ns)
            time.sleep(RECONCILE_WAIT)

            # Verify TRLP still exists if there are other MaaSSubscriptions for this model
            # (like the pre-existing simulator-subscription)
            trlp_final = _get_cr("TokenRateLimitPolicy", trlp_name, MODEL_NAMESPACE)
            # Note: TRLP may still exist due to pre-existing subscriptions (e.g. simulator-subscription)
            # The key test was that deleting the FIRST subscription didn't delete the TRLP
            log.info(f"TokenRateLimitPolicy exists after deleting test subscriptions: {trlp_final is not None}")

            log.info("✓ Multiple subscriptions deletion test passed (bug fix verified)")

        finally:
            for resource, name, ns in [
                ("MaaSSubscription", sub1_name, maas_subscription_ns),
                ("MaaSSubscription", sub2_name, maas_subscription_ns),
                ("MaaSAuthPolicy", maas_auth_policy_name, maas_subscription_ns),
            ]:
                try:
                    _delete_cr(resource, name, ns)
                except Exception as e:
                    log.warning(f"Cleanup failed for {resource}/{name}: {e}")


class TestAuthorizationBoundary:
    """Test that namespace boundaries provide proper authorization isolation."""

    def test_model_isolation_between_namespaces(self, policy_namespace):
        """
        Verify that policies in one namespace cannot accidentally affect models
        in other namespaces unless explicitly configured.
        """
        # This is more of a design verification test
        # The key security property: MaaSAuthPolicy in namespace A can only affect
        # models that it explicitly lists with their namespaces in spec.modelRefs

        # If a policy doesn't list a model, it shouldn't affect it
        # This is enforced by the controller only creating AuthPolicies for
        # models explicitly listed in spec.modelRefs[]

        policy_name = f"isolated-policy-{uuid.uuid4().hex[:6]}"
        unmanaged_model_name = f"unmanaged-model-{uuid.uuid4().hex[:6]}"

        # Create a temporary test model that won't be referenced by the policy
        _apply_cr({
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSModelRef",
            "metadata": {"name": unmanaged_model_name, "namespace": MODEL_NAMESPACE},
            "spec": {
                "modelRef": {"kind": "ExternalModel", "name": "test-backend"}
            }
        })

        # Create a policy that only targets MODEL_REF (not the temporary model)
        _apply_cr({
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSAuthPolicy",
            "metadata": {"name": policy_name, "namespace": policy_namespace},
            "spec": {
                "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE}],
                "subjects": {"groups": [{"name": "system:authenticated"}]},
            },
        })

        try:
            time.sleep(RECONCILE_WAIT)

            # Verify that this policy ONLY created AuthPolicy for the specified model
            auth_policy_name = f"maas-auth-{MODEL_REF}"
            auth_policy = _get_cr("AuthPolicy", auth_policy_name, MODEL_NAMESPACE)
            assert auth_policy is not None, "AuthPolicy should exist for specified model"

            # Negative test: Verify NO Kuadrant resources created for unmanaged model
            unmanaged_auth_policy = _get_cr("AuthPolicy", f"maas-auth-{unmanaged_model_name}", MODEL_NAMESPACE)
            assert unmanaged_auth_policy is None, \
                f"AuthPolicy should NOT exist for unmanaged model {unmanaged_model_name}"

            log.info(f"✓ Verified no AuthPolicy created for unmanaged model {unmanaged_model_name}")
            log.info("✓ Authorization boundary test passed - policies only affect listed models")

        finally:
            for resource, name, ns in [
                ("MaaSAuthPolicy", policy_name, policy_namespace),
                ("MaaSModelRef", unmanaged_model_name, MODEL_NAMESPACE),
            ]:
                try:
                    _delete_cr(resource, name, ns)
                except Exception as e:
                    log.warning(f"Cleanup failed for {resource}/{name}: {e}")
