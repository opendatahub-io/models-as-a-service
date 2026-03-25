"""
E2E tests for TokenRateLimitPolicy merge strategy (RHOAIENG-53869).

Tests that multiple MaaSModelRefs pointing to the same HTTPRoute can coexist
using defaults.strategy: merge, enabling independent rate limits per model.
"""

import json
import logging
import os
import shutil
import subprocess
import time
import uuid
from typing import Optional

import pytest

log = logging.getLogger(__name__)

# Constants (override with env vars)
TIMEOUT = int(os.environ.get("E2E_TIMEOUT", "30"))
RECONCILE_WAIT = int(os.environ.get("E2E_RECONCILE_WAIT", "8"))
MODEL_NAMESPACE = os.environ.get("E2E_MODEL_NAMESPACE", "llm")
MAAS_NAMESPACE = os.environ.get("MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service")

# Default backend model to use for testing
DEFAULT_LLMIS_NAME = os.environ.get("E2E_DEFAULT_LLMIS", "facebook-opt-125m-simulated")
DEFAULT_LLMIS_NAMESPACE = os.environ.get("E2E_DEFAULT_LLMIS_NAMESPACE", "llm")

# kubectl binary path and timeout for all subprocess calls
KUBECTL_BIN = shutil.which("kubectl")
if not KUBECTL_BIN or not os.path.isabs(KUBECTL_BIN):
    raise RuntimeError(f"kubectl not found in PATH or not absolute: {KUBECTL_BIN}")
KUBECTL_TIMEOUT = int(os.environ.get("E2E_KUBECTL_TIMEOUT", "60"))


# ---------------------------------------------------------------------------
# Helper Function
# ---------------------------------------------------------------------------

def _ns():
    """Get the MaaS CRs namespace."""
    return MAAS_NAMESPACE


def _apply_cr(cr_dict):
    """Apply a custom resource using kubectl."""
    subprocess.run(
        [KUBECTL_BIN, "apply", "-f", "-"],
        input=json.dumps(cr_dict),
        capture_output=True,
        text=True,
        check=True,
        timeout=KUBECTL_TIMEOUT,
    )


def _delete_cr(kind, name, namespace=None):
    """Delete a custom resource."""
    namespace = namespace or _ns()
    subprocess.run(
        [KUBECTL_BIN, "delete", kind, name, "-n", namespace, "--ignore-not-found", "--timeout=30s"],
        capture_output=True,
        text=True,
        timeout=KUBECTL_TIMEOUT,
    )


def _get_cr(kind, name, namespace=None):
    """Get a custom resource as a dictionary."""
    namespace = namespace or _ns()
    result = subprocess.run(
        [KUBECTL_BIN, "get", kind, name, "-n", namespace, "-o", "json"],
        capture_output=True,
        text=True,
        timeout=KUBECTL_TIMEOUT,
    )
    if result.returncode != 0:
        return None
    return json.loads(result.stdout)


def _wait_reconcile(seconds=None):
    """Wait for controller reconciliation."""
    time.sleep(seconds or RECONCILE_WAIT)


def _create_test_maas_model(name, llmis_name=None, llmis_namespace=None, namespace=None):
    """Create a MaaSModelRef CR for testing.

    Args:
        name: Name of the MaaSModelRef to create
        llmis_name: Name of the LLMInferenceService to reference
        llmis_namespace: Namespace of the LLMInferenceService
        namespace: Namespace for the MaaSModelRef (defaults to same as LLMIS)
    """
    llmis_name = llmis_name or DEFAULT_LLMIS_NAME
    llmis_namespace = llmis_namespace or DEFAULT_LLMIS_NAMESPACE
    namespace = namespace or llmis_namespace

    log.info(f"Creating MaaSModelRef: {name} in namespace: {namespace}")
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
        model_refs: List of model refs (strings or dicts with name/namespace)
        users: List of user principals
        groups: List of group names (will be converted to required format)
        namespace: Namespace for the auth policy
    """
    namespace = namespace or _ns()
    if not isinstance(model_refs, list):
        model_refs = [model_refs]

    # Convert model refs to required format: [{"name": "model1", "namespace": "llm"}, ...]
    model_refs_formatted = []
    for ref in model_refs:
        if isinstance(ref, dict):
            model_refs_formatted.append(ref)
        else:
            model_refs_formatted.append({"name": ref, "namespace": MODEL_NAMESPACE})

    # Convert groups list to required format: [{"name": "group1"}, {"name": "group2"}]
    groups_formatted = [{"name": g} for g in (groups or [])]

    log.info(f"Creating MaaSAuthPolicy: {name}")
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


def _create_test_subscription(name, model_refs, users=None, groups=None, token_limit=100, window="1m", namespace=None):
    """Create a MaaSSubscription CR for testing.

    Args:
        name: Name of the subscription
        model_refs: List of model refs (strings)
        users: List of user principals
        groups: List of group names (will be converted to required format)
        token_limit: Token rate limit (default: 100)
        window: Rate limit window (default: "1m")
        namespace: Namespace for the subscription
    """
    namespace = namespace or _ns()
    if not isinstance(model_refs, list):
        model_refs = [model_refs]

    # Convert groups list to required format: [{"name": "group1"}, {"name": "group2"}]
    groups_formatted = [{"name": g} for g in (groups or [])]

    log.info(f"Creating MaaSSubscription: {name}")
    _apply_cr({
        "apiVersion": "maas.opendatahub.io/v1alpha1",
        "kind": "MaaSSubscription",
        "metadata": {"name": name, "namespace": namespace},
        "spec": {
            "owner": {
                "users": users or [],
                "groups": groups_formatted
            },
            "modelRefs": [{
                "name": ref,
                "namespace": MODEL_NAMESPACE,
                "tokenRateLimits": [{"limit": token_limit, "window": window}]
            } for ref in model_refs]
        }
    })


def _wait_for_maas_model_ready(name, namespace=None, timeout=120):
    """Wait for MaaSModelRef to reach Ready phase.

    Args:
        name: Name of the MaaSModelRef
        namespace: Namespace (defaults to MODEL_NAMESPACE)
        timeout: Maximum wait time in seconds

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
            http_route = cr.get("status", {}).get("httpRouteName")
            if phase == "Ready" and endpoint:
                log.info(f"MaaSModelRef {name} is Ready (endpoint: {endpoint}, route: {http_route})")
                return endpoint
            log.debug(f"MaaSModelRef {name} phase: {phase}, endpoint: {endpoint or 'none'}")
        time.sleep(5)

    # Timeout - log current state for debugging
    cr = _get_cr("maasmodelref", name, namespace)
    current_phase = cr.get("status", {}).get("phase") if cr else "not found"
    raise TimeoutError(
        f"MaaSModelRef {name} did not become Ready within {timeout}s (current phase: {current_phase})"
    )


def _wait_for_authpolicy_enforced(name: str, namespace: str, timeout: int = 60) -> bool:
    """Wait for AuthPolicy to be enforced.

    Args:
        name: Name of the AuthPolicy
        namespace: Namespace where AuthPolicy exists
        timeout: Maximum time to wait in seconds

    Returns:
        True if enforced within timeout, False otherwise
    """
    deadline = time.time() + timeout
    log.info(f"Waiting for AuthPolicy {name} to be enforced (timeout: {timeout}s)...")

    while time.time() < deadline:
        cr = _get_cr("AuthPolicy", name, namespace)
        if cr:
            conditions = cr.get("status", {}).get("conditions", [])
            for condition in conditions:
                if condition.get("type") == "Enforced" and condition.get("status") == "True":
                    log.info(f"AuthPolicy {name} is enforced")
                    return True
        time.sleep(3)

    log.warning(f"AuthPolicy {name} not enforced within {timeout}s")
    return False


def _wait_for_trlp_enforced(name: str, namespace: str, timeout: int = 90) -> bool:
    """Wait for TokenRateLimitPolicy to be enforced.

    Args:
        name: Name of the TokenRateLimitPolicy
        namespace: Namespace where TRLP exists
        timeout: Maximum time to wait in seconds

    Returns:
        True if enforced within timeout, False otherwise

    Raises:
        AssertionError: If TRLP shows Overridden status (merge strategy failed)
    """
    deadline = time.time() + timeout
    last_status = None
    log.info(f"Waiting for TokenRateLimitPolicy {name} to be enforced (timeout: {timeout}s)...")

    while time.time() < deadline:
        cr = _get_cr("TokenRateLimitPolicy", name, namespace)
        if cr:
            generation = cr.get("metadata", {}).get("generation", 0)
            conditions = cr.get("status", {}).get("conditions", [])

            # Check for Enforced condition
            for condition in conditions:
                if condition.get("type") == "Enforced":
                    status = condition.get("status")
                    reason = condition.get("reason")

                    # CRITICAL: Fail fast if Overridden (merge strategy not working)
                    if reason == "Overridden":
                        raise AssertionError(
                            f"TRLP {name} is Overridden (merge strategy failed): {condition}"
                        )

                    if status == "True":
                        log.info(f"TRLP {name} is enforced (generation={generation})")
                        return True

                    last_status = f"generation={generation}, status={status}, reason={reason}"
        else:
            last_status = "TRLP not found"

        time.sleep(3)

    log.warning(f"TRLP {name} not enforced within {timeout}s. Last status: {last_status}")
    return False


def _get_trlp_strategy(name: str, namespace: str) -> Optional[str]:
    """Get the strategy field from TRLP spec.defaults.strategy.

    Args:
        name: Name of the TokenRateLimitPolicy
        namespace: Namespace where TRLP exists

    Returns:
        Strategy value ("merge", "atomic", etc.) or None if not found
    """
    trlp = _get_cr("TokenRateLimitPolicy", name, namespace)
    if not trlp:
        return None
    return trlp.get("spec", {}).get("defaults", {}).get("strategy")


def _get_trlp_target_ref(name: str, namespace: str) -> Optional[dict]:
    """Get the targetRef from TRLP spec.

    Args:
        name: Name of the TokenRateLimitPolicy
        namespace: Namespace where TRLP exists

    Returns:
        TargetRef dict with kind/name/namespace or None if not found
    """
    trlp = _get_cr("TokenRateLimitPolicy", name, namespace)
    if not trlp:
        return None
    return trlp.get("spec", {}).get("targetRef")


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

class TestTRLPMergeStrategy:
    """
    Tests for TokenRateLimitPolicy merge strategy allowing multiple
    models to share the same HTTPRoute with independent rate limits.
    """

    def test_multiple_models_shared_route_both_enforced(self):
        """
        Scenario: Two MaaSModelRefs → same LLMInferenceService → same HTTPRoute

        Expected:
        - Both TokenRateLimitPolicies created
        - Both have defaults.strategy: merge
        - Both show Enforced: True (not Overridden)
        - Both target the same HTTPRoute

        This is the core test validating RHOAIENG-53869 fix.
        """
        # Generate unique names for test resources
        test_id = uuid.uuid4().hex[:8]
        model_ref_a_name = f"e2e-merge-model-a-{test_id}"
        model_ref_b_name = f"e2e-merge-model-b-{test_id}"
        auth_a_name = f"e2e-merge-auth-a-{test_id}"
        auth_b_name = f"e2e-merge-auth-b-{test_id}"
        sub_a_name = f"e2e-merge-sub-a-{test_id}"
        sub_b_name = f"e2e-merge-sub-b-{test_id}"

        # Expected TRLP names (generated by controller)
        trlp_a_name = f"maas-trlp-{model_ref_a_name}"
        trlp_b_name = f"maas-trlp-{model_ref_b_name}"

        try:
            # ============================================================
            # SETUP PHASE: Create test resources
            # ============================================================

            log.info("=" * 70)
            log.info("SETUP: Creating two MaaSModelRefs pointing to same LLMInferenceService")
            log.info("=" * 70)

            # 1. Create two MaaSModelRef CRs pointing to same backend model
            _create_test_maas_model(
                model_ref_a_name,
                llmis_name=DEFAULT_LLMIS_NAME,
                llmis_namespace=DEFAULT_LLMIS_NAMESPACE,
                namespace=MODEL_NAMESPACE,
            )
            _create_test_maas_model(
                model_ref_b_name,
                llmis_name=DEFAULT_LLMIS_NAME,
                llmis_namespace=DEFAULT_LLMIS_NAMESPACE,
                namespace=MODEL_NAMESPACE,
            )

            # 2. Wait for both models to reach Ready phase
            log.info("Waiting for both MaaSModelRefs to become Ready...")
            endpoint_a = _wait_for_maas_model_ready(model_ref_a_name, MODEL_NAMESPACE, timeout=120)
            endpoint_b = _wait_for_maas_model_ready(model_ref_b_name, MODEL_NAMESPACE, timeout=120)

            log.info(f"Model A endpoint: {endpoint_a}")
            log.info(f"Model B endpoint: {endpoint_b}")

            # 3. Verify both models resolve to same HTTPRoute
            model_a = _get_cr("maasmodelref", model_ref_a_name, MODEL_NAMESPACE)
            model_b = _get_cr("maasmodelref", model_ref_b_name, MODEL_NAMESPACE)

            # Guard against None or missing keys
            assert model_a is not None, f"MaaSModelRef {model_ref_a_name} not found"
            assert model_b is not None, f"MaaSModelRef {model_ref_b_name} not found"

            assert "status" in model_a, f"MaaSModelRef {model_ref_a_name} missing status"
            assert "status" in model_b, f"MaaSModelRef {model_ref_b_name} missing status"

            route_a = model_a.get("status", {}).get("httpRouteName")
            route_b = model_b.get("status", {}).get("httpRouteName")

            assert route_a is not None, (
                f"MaaSModelRef {model_ref_a_name} missing status.httpRouteName"
            )
            assert route_b is not None, (
                f"MaaSModelRef {model_ref_b_name} missing status.httpRouteName"
            )

            log.info(f"Model A HTTPRoute: {route_a}")
            log.info(f"Model B HTTPRoute: {route_b}")

            assert route_a == route_b, (
                f"Models should share the same HTTPRoute. "
                f"Got route_a={route_a}, route_b={route_b}"
            )
            log.info(f"VERIFIED: Both models share HTTPRoute: {route_a}")

            # 4. Create auth policies for each model
            log.info("Creating MaaSAuthPolicies for both models...")
            _create_test_auth_policy(
                auth_a_name,
                model_refs=[{"name": model_ref_a_name, "namespace": MODEL_NAMESPACE}],
                groups=["system:authenticated"],
                namespace=MAAS_NAMESPACE,
            )
            _create_test_auth_policy(
                auth_b_name,
                model_refs=[{"name": model_ref_b_name, "namespace": MODEL_NAMESPACE}],
                groups=["system:authenticated"],
                namespace=MAAS_NAMESPACE,
            )

            # Wait for auth policies to be enforced
            # Note: We attempt to wait for enforcement but don't fail if it times out,
            # as the test focus is TRLP merge strategy. AuthPolicy enforcement may
            # complete asynchronously before TRLP validation.
            _wait_for_authpolicy_enforced(auth_a_name, MAAS_NAMESPACE)
            _wait_for_authpolicy_enforced(auth_b_name, MAAS_NAMESPACE)

            # 5. Create subscriptions with different rate limits
            log.info("Creating MaaSSubscriptions with different rate limits...")
            _create_test_subscription(
                sub_a_name,
                model_refs=[model_ref_a_name],
                groups=["system:authenticated"],
                token_limit=1000,
                window="1m",
                namespace=MAAS_NAMESPACE,
            )
            _create_test_subscription(
                sub_b_name,
                model_refs=[model_ref_b_name],
                groups=["system:authenticated"],
                token_limit=5000,
                window="1m",
                namespace=MAAS_NAMESPACE,
            )

            # Wait for controller to reconcile subscriptions
            log.info("Waiting for controller to reconcile subscriptions...")
            _wait_reconcile(10)

            # ============================================================
            # VALIDATION PHASE: Verify merge strategy works
            # ============================================================

            log.info("=" * 70)
            log.info("VALIDATION: Verifying TRLP merge strategy")
            log.info("=" * 70)

            # 6. Verify both TRLPs exist
            log.info("Checking if both TokenRateLimitPolicies exist...")
            trlp_a = _get_cr("TokenRateLimitPolicy", trlp_a_name, MODEL_NAMESPACE)
            trlp_b = _get_cr("TokenRateLimitPolicy", trlp_b_name, MODEL_NAMESPACE)

            assert trlp_a is not None, f"TRLP-A not found: {trlp_a_name}"
            assert trlp_b is not None, f"TRLP-B not found: {trlp_b_name}"
            log.info("VERIFIED: Both TRLPs exist")

            # 7. Verify both have merge strategy
            log.info("Verifying both TRLPs have merge strategy...")
            strategy_a = _get_trlp_strategy(trlp_a_name, MODEL_NAMESPACE)
            strategy_b = _get_trlp_strategy(trlp_b_name, MODEL_NAMESPACE)

            assert strategy_a == "merge", (
                f"TRLP-A strategy should be 'merge', got '{strategy_a}'"
            )
            assert strategy_b == "merge", (
                f"TRLP-B strategy should be 'merge', got '{strategy_b}'"
            )
            log.info("VERIFIED: Both TRLPs have strategy='merge'")

            # 8. Verify both target same HTTPRoute
            log.info("Verifying both TRLPs target the same HTTPRoute...")
            target_a = _get_trlp_target_ref(trlp_a_name, MODEL_NAMESPACE)
            target_b = _get_trlp_target_ref(trlp_b_name, MODEL_NAMESPACE)

            assert target_a is not None, "TRLP-A targetRef not found"
            assert target_b is not None, "TRLP-B targetRef not found"

            target_a_name = target_a.get("name")
            target_b_name = target_b.get("name")

            assert target_a_name == target_b_name, (
                f"TRLPs should target same HTTPRoute. "
                f"Got target_a={target_a_name}, target_b={target_b_name}"
            )
            assert target_a_name == route_a, (
                f"TRLP target should match HTTPRoute from MaaSModelRef. "
                f"Got target={target_a_name}, expected={route_a}"
            )
            log.info(f"VERIFIED: Both TRLPs target HTTPRoute: {target_a_name}")

            # 9. CRITICAL: Verify both TRLPs are Enforced (not Overridden)
            log.info("CRITICAL CHECK: Verifying both TRLPs are Enforced...")
            enforced_a = _wait_for_trlp_enforced(trlp_a_name, MODEL_NAMESPACE, timeout=90)
            enforced_b = _wait_for_trlp_enforced(trlp_b_name, MODEL_NAMESPACE, timeout=90)

            assert enforced_a, f"TRLP-A ({trlp_a_name}) not enforced within timeout"
            assert enforced_b, f"TRLP-B ({trlp_b_name}) not enforced within timeout"

            # Double-check status conditions to ensure neither is Overridden
            trlp_a_fresh = _get_cr("TokenRateLimitPolicy", trlp_a_name, MODEL_NAMESPACE)
            trlp_b_fresh = _get_cr("TokenRateLimitPolicy", trlp_b_name, MODEL_NAMESPACE)

            # Guard against None CRs
            assert trlp_a_fresh is not None, (
                f"TokenRateLimitPolicy {trlp_a_name} not found after enforcement check"
            )
            assert trlp_b_fresh is not None, (
                f"TokenRateLimitPolicy {trlp_b_name} not found after enforcement check"
            )

            # Safely extract conditions
            conditions_a = trlp_a_fresh.get("status", {}).get("conditions", [])
            conditions_b = trlp_b_fresh.get("status", {}).get("conditions", [])

            enforced_condition_a = next(
                (c for c in conditions_a if c.get("type") == "Enforced"),
                None
            )
            enforced_condition_b = next(
                (c for c in conditions_b if c.get("type") == "Enforced"),
                None
            )

            assert enforced_condition_a is not None, "TRLP-A missing Enforced condition"
            assert enforced_condition_b is not None, "TRLP-B missing Enforced condition"

            assert enforced_condition_a["status"] == "True", (
                f"TRLP-A not enforced: {enforced_condition_a}"
            )
            assert enforced_condition_b["status"] == "True", (
                f"TRLP-B not enforced: {enforced_condition_b}"
            )

            # Verify neither is Overridden
            assert enforced_condition_a["reason"] != "Overridden", (
                f"TRLP-A should not be Overridden. This means merge strategy failed. "
                f"Condition: {enforced_condition_a}"
            )
            assert enforced_condition_b["reason"] != "Overridden", (
                f"TRLP-B should not be Overridden. This means merge strategy failed. "
                f"Condition: {enforced_condition_b}"
            )

            log.info("VERIFIED: Both TRLPs are Enforced (not Overridden)")
            log.info("=" * 70)
            log.info("SUCCESS: TRLP merge strategy test passed!")
            log.info("=" * 70)

        finally:
            # ============================================================
            # CLEANUP PHASE: Delete all created resources
            # ============================================================

            log.info("=" * 70)
            log.info("CLEANUP: Deleting test resources")
            log.info("=" * 70)

            # Delete subscriptions (triggers TRLP cleanup by controller)
            log.info("Deleting subscriptions...")
            _delete_cr("maassubscription", sub_a_name, MAAS_NAMESPACE)
            _delete_cr("maassubscription", sub_b_name, MAAS_NAMESPACE)

            # Delete auth policies
            log.info("Deleting auth policies...")
            _delete_cr("maasauthpolicy", auth_a_name, MAAS_NAMESPACE)
            _delete_cr("maasauthpolicy", auth_b_name, MAAS_NAMESPACE)

            # Delete MaaSModelRefs
            log.info("Deleting MaaSModelRefs...")
            _delete_cr("maasmodelref", model_ref_a_name, MODEL_NAMESPACE)
            _delete_cr("maasmodelref", model_ref_b_name, MODEL_NAMESPACE)

            # Note: TRLPs should auto-delete when subscriptions are deleted by the controller
            log.info("Cleanup complete (TRLPs should auto-delete)")
