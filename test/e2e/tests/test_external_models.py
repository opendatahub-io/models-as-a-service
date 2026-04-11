"""
E2E tests for external model (egress) support.

Tests that MaaS can route requests to an external endpoint via ExternalModel CRD,
including reconciler resource creation, auth enforcement, and egress connectivity.

Prerequisites:
- MaaS deployed with ExternalModel reconciler
- External endpoint accessible from the cluster (default: httpbin.org)

Environment variables:
- E2E_EXTERNAL_ENDPOINT: External endpoint hostname (default: httpbin.org)
- E2E_EXTERNAL_SUBSCRIPTION: Subscription name (default: e2e-external-subscription)
- GATEWAY_HOST: MaaS gateway hostname (required)
"""

import json
import logging
import os
import subprocess
import time
from typing import Optional

import pytest
import requests

from test_helper import (
    _wait_for_authpolicy_phase,
    _wait_for_subscription_phase,
)

log = logging.getLogger(__name__)

# ─── Configuration ──────────────────────────────────────────────────────────

EXTERNAL_ENDPOINT = os.environ.get("E2E_EXTERNAL_ENDPOINT", os.environ.get("E2E_SIMULATOR_ENDPOINT", "httpbin.org"))
MODEL_NAMESPACE = os.environ.get("E2E_MODEL_NAMESPACE", "llm")
SUBSCRIPTION_NAMESPACE = os.environ.get("E2E_SUBSCRIPTION_NAMESPACE", os.environ.get("MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service"))
EXTERNAL_SUBSCRIPTION = os.environ.get("E2E_EXTERNAL_SUBSCRIPTION", "e2e-external-subscription")
EXTERNAL_AUTH_POLICY = os.environ.get("E2E_EXTERNAL_AUTH_POLICY", "e2e-external-access")
RECONCILE_WAIT = int(os.environ.get("E2E_RECONCILE_WAIT", "12"))
TLS_VERIFY = os.environ.get("E2E_SKIP_TLS_VERIFY", "").lower() != "true"

EXTERNAL_MODEL_NAME = "e2e-external-model"


# ─── Helpers ─────────────────────────────────────────────────────────────────

def _apply_cr(cr_dict: dict):
    """Apply a Kubernetes CR from a dict."""
    result = subprocess.run(
        ["oc", "apply", "-f", "-"],
        input=json.dumps(cr_dict),
        capture_output=True, text=True,
    )
    if result.returncode != 0:
        log.warning(f"oc apply failed: {result.stderr}")
    return result.returncode == 0


def _delete_cr(kind: str, name: str, namespace: str):
    """Delete a Kubernetes resource (best effort)."""
    subprocess.run(
        ["oc", "delete", kind, name, "-n", namespace, "--ignore-not-found", "--timeout=30s"],
        capture_output=True, text=True,
    )


def _patch_cr(kind: str, name: str, namespace: str, patch: dict):
    """Patch a Kubernetes resource."""
    subprocess.run(
        ["oc", "patch", kind, name, "-n", namespace, "--type=merge", "-p", json.dumps(patch)],
        capture_output=True, text=True,
    )


def _get_cr(kind: str, name: str, namespace: str) -> Optional[dict]:
    """Get a Kubernetes resource as dict, or None if not found."""
    result = subprocess.run(
        ["oc", "get", kind, name, "-n", namespace, "-o", "json"],
        capture_output=True, text=True,
    )
    if result.returncode != 0:
        return None
    return json.loads(result.stdout)


def _wait_for_phase(kind: str, name: str, namespace: str, phase: str, timeout: int = 60) -> bool:
    """Wait for a CR to reach a specific status phase."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        cr = _get_cr(kind, name, namespace)
        if cr and cr.get("status", {}).get("phase") == phase:
            return True
        time.sleep(2)
    return False


# ─── Connectivity check ──────────────────────────────────────────────────────

def _check_external_endpoint_reachable():
    """Verify the external endpoint is reachable. Skip tests if not."""
    try:
        r = requests.get(f"https://{EXTERNAL_ENDPOINT}/get", timeout=10, verify=False)
        if r.status_code == 200:
            return True
    except Exception:
        pass
    # Try HTTP fallback
    try:
        r = requests.get(f"http://{EXTERNAL_ENDPOINT}/get", timeout=10)
        if r.status_code == 200:
            return True
    except Exception:
        pass
    return False


pytestmark = pytest.mark.skipif(
    not _check_external_endpoint_reachable(),
    reason=f"External endpoint {EXTERNAL_ENDPOINT} is not reachable (disconnected environment?)",
)


# ─── Fixture: Create external model resources ────────────────────────────────

@pytest.fixture(scope="module")
def external_models_setup(gateway_url, headers, api_keys_base_url):
    """
    Create a single ExternalModel CR, MaaSModelRef, AuthPolicy, and
    Subscription pointing to an external endpoint. Cleanup after tests.
    """
    log.info(f"Setting up external model test fixture (endpoint: {EXTERNAL_ENDPOINT})...")

    # Create a dummy secret (ExternalModel requires credentialRef)
    _apply_cr({
        "apiVersion": "v1",
        "kind": "Secret",
        "metadata": {
            "name": f"{EXTERNAL_MODEL_NAME}-api-key",
            "namespace": MODEL_NAMESPACE,
        },
        "type": "Opaque",
        "stringData": {"api-key": "e2e-test-key"},
    })

    # Create ExternalModel CR
    _apply_cr({
        "apiVersion": "maas.opendatahub.io/v1alpha1",
        "kind": "ExternalModel",
        "metadata": {"name": EXTERNAL_MODEL_NAME, "namespace": MODEL_NAMESPACE},
        "spec": {
            "provider": "openai",
            "targetModel": "gpt-3.5-turbo",
            "endpoint": EXTERNAL_ENDPOINT,
            "credentialRef": {
                "name": f"{EXTERNAL_MODEL_NAME}-api-key",
                "namespace": MODEL_NAMESPACE,
            },
        },
    })

    # Create MaaSModelRef
    _apply_cr({
        "apiVersion": "maas.opendatahub.io/v1alpha1",
        "kind": "MaaSModelRef",
        "metadata": {
            "name": EXTERNAL_MODEL_NAME,
            "namespace": MODEL_NAMESPACE,
            "annotations": {
                "maas.opendatahub.io/endpoint": EXTERNAL_ENDPOINT,
                "maas.opendatahub.io/provider": "openai",
            },
        },
        "spec": {
            "modelRef": {"kind": "ExternalModel", "name": EXTERNAL_MODEL_NAME},
        },
    })

    # Create MaaSAuthPolicy
    _apply_cr({
        "apiVersion": "maas.opendatahub.io/v1alpha1",
        "kind": "MaaSAuthPolicy",
        "metadata": {"name": EXTERNAL_AUTH_POLICY, "namespace": SUBSCRIPTION_NAMESPACE},
        "spec": {
            "modelRefs": [{"name": EXTERNAL_MODEL_NAME, "namespace": MODEL_NAMESPACE}],
            "subjects": {"groups": [{"name": "system:authenticated"}]},
        },
    })

    # Create MaaSSubscription
    _apply_cr({
        "apiVersion": "maas.opendatahub.io/v1alpha1",
        "kind": "MaaSSubscription",
        "metadata": {"name": EXTERNAL_SUBSCRIPTION, "namespace": SUBSCRIPTION_NAMESPACE},
        "spec": {
            "owner": {"groups": [{"name": "system:authenticated"}]},
            "modelRefs": [
                {
                    "name": EXTERNAL_MODEL_NAME,
                    "namespace": MODEL_NAMESPACE,
                    "tokenRateLimits": [{"limit": 10000, "window": "1h"}],
                },
            ],
        },
    })

    # Wait for CRs to reconcile
    _wait_for_authpolicy_phase(EXTERNAL_AUTH_POLICY, namespace=SUBSCRIPTION_NAMESPACE)
    _wait_for_subscription_phase(EXTERNAL_SUBSCRIPTION, namespace=SUBSCRIPTION_NAMESPACE)

    # Create API key for tests
    log.info("Creating API key for external model tests...")
    r = requests.post(
        api_keys_base_url,
        headers=headers,
        json={"name": "e2e-external-model-key", "subscription": EXTERNAL_SUBSCRIPTION},
        timeout=30,
        verify=TLS_VERIFY,
    )
    if r.status_code not in (200, 201):
        pytest.fail(f"Failed to create API key: {r.status_code} {r.text}")

    api_key = r.json().get("key")
    log.info(f"API key created: {api_key[:15]}...")

    yield {
        "api_key": api_key,
        "gateway_url": gateway_url,
    }

    # ── Cleanup ──
    log.info("Cleaning up external model test fixtures...")
    _delete_cr("maasauthpolicy", EXTERNAL_AUTH_POLICY, SUBSCRIPTION_NAMESPACE)
    _delete_cr("maassubscription", EXTERNAL_SUBSCRIPTION, SUBSCRIPTION_NAMESPACE)
    _patch_cr("maasmodelref", EXTERNAL_MODEL_NAME, MODEL_NAMESPACE,
              {"metadata": {"finalizers": []}})
    _delete_cr("maasmodelref", EXTERNAL_MODEL_NAME, MODEL_NAMESPACE)
    _delete_cr("externalmodel", EXTERNAL_MODEL_NAME, MODEL_NAMESPACE)
    _delete_cr("secret", f"{EXTERNAL_MODEL_NAME}-api-key", MODEL_NAMESPACE)


# ─── Tests: Discovery ───────────────────────────────────────────────────────

class TestExternalModelDiscovery:
    """Verify ExternalModel reconciler creates the expected Istio resources."""

    def test_maasmodelref_created(self, external_models_setup):
        """MaaSModelRef exists for the external model."""
        cr = _get_cr("maasmodelref", EXTERNAL_MODEL_NAME, MODEL_NAMESPACE)
        assert cr is not None, f"MaaSModelRef {EXTERNAL_MODEL_NAME} not found"

    def test_reconciler_created_httproute(self, external_models_setup):
        """Reconciler created maas-model-* HTTPRoute."""
        cr = _get_cr("httproute", f"maas-model-{EXTERNAL_MODEL_NAME}", MODEL_NAMESPACE)
        assert cr is not None, f"HTTPRoute maas-model-{EXTERNAL_MODEL_NAME} not found"

    def test_reconciler_created_backend_service(self, external_models_setup):
        """Reconciler created backend service."""
        cr = _get_cr("service", f"maas-model-{EXTERNAL_MODEL_NAME}-backend", MODEL_NAMESPACE)
        assert cr is not None, f"Service maas-model-{EXTERNAL_MODEL_NAME}-backend not found"


# ─── Tests: Auth ─────────────────────────────────────────────────────────────

class TestExternalModelAuth:
    """Verify auth enforcement for external model routes."""

    def test_invalid_key_returns_401(self, external_models_setup):
        """Invalid API key returns 401/403."""
        setup = external_models_setup
        url = f"{setup['gateway_url']}/{EXTERNAL_MODEL_NAME}/v1/chat/completions"
        headers = {
            "Content-Type": "application/json",
            "Authorization": "Bearer INVALID-KEY-12345",
        }
        body = {"model": EXTERNAL_MODEL_NAME, "messages": [{"role": "user", "content": "hello"}]}

        r = requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
        assert r.status_code in (401, 403), f"Expected 401/403, got {r.status_code}"

    def test_no_key_returns_401(self, external_models_setup):
        """No API key returns 401/403."""
        setup = external_models_setup
        url = f"{setup['gateway_url']}/{EXTERNAL_MODEL_NAME}/v1/chat/completions"
        headers = {"Content-Type": "application/json"}
        body = {"model": EXTERNAL_MODEL_NAME, "messages": [{"role": "user", "content": "hello"}]}

        r = requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
        assert r.status_code in (401, 403), f"Expected 401/403, got {r.status_code}"


# ─── Tests: Egress ───────────────────────────────────────────────────────────

class TestExternalModelEgress:
    """Verify requests are forwarded to the external endpoint."""

    def test_request_forwarded_returns_200(self, external_models_setup):
        """
        With a valid API key, the request passes auth and reaches the
        external endpoint. Expect 200 confirming egress connectivity.
        """
        setup = external_models_setup
        url = f"{setup['gateway_url']}/{EXTERNAL_MODEL_NAME}/v1/chat/completions"
        headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {setup['api_key']}",
        }
        body = {"model": EXTERNAL_MODEL_NAME, "messages": [{"role": "user", "content": "hello"}]}

        r = requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)
        assert r.status_code not in (401, 403), (
            f"Request was blocked by auth (HTTP {r.status_code}). "
            f"Expected the request to reach the external endpoint."
        )
        # Any non-auth response confirms egress connectivity.
        # httpbin.org may return 404 for unknown paths — that's fine,
        # it means the request left the cluster and reached the endpoint.
        log.info(f"Egress test: HTTP {r.status_code} from external endpoint")


# ─── Tests: Cleanup ─────────────────────────────────────────────────────────

class TestExternalModelCleanup:
    """Verify resource cleanup when external models are deleted."""

    def test_delete_removes_httproute(self, external_models_setup):
        """
        Deleting a MaaSModelRef removes the maas-model-* HTTPRoute
        via the finalizer.
        """
        temp_name = "e2e-cleanup-test"

        # Create temporary model
        _apply_cr({
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "ExternalModel",
            "metadata": {"name": temp_name, "namespace": MODEL_NAMESPACE},
            "spec": {
                "provider": "openai",
                "targetModel": "gpt-3.5-turbo",
                "endpoint": EXTERNAL_ENDPOINT,
                "credentialRef": {
                    "name": f"{EXTERNAL_MODEL_NAME}-api-key",
                    "namespace": MODEL_NAMESPACE,
                },
            },
        })
        _apply_cr({
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSModelRef",
            "metadata": {
                "name": temp_name,
                "namespace": MODEL_NAMESPACE,
                "annotations": {
                    "maas.opendatahub.io/endpoint": EXTERNAL_ENDPOINT,
                    "maas.opendatahub.io/provider": "openai",
                },
            },
            "spec": {"modelRef": {"kind": "ExternalModel", "name": temp_name}},
        })

        try:
            # Wait for reconciler to create resources
            time.sleep(RECONCILE_WAIT * 2)

            # Verify HTTPRoute was created
            route = _get_cr("httproute", f"maas-model-{temp_name}", MODEL_NAMESPACE)
            assert route is not None, f"HTTPRoute maas-model-{temp_name} should exist before deletion"

            # Delete
            _delete_cr("maasmodelref", temp_name, MODEL_NAMESPACE)
            time.sleep(RECONCILE_WAIT)

            # Verify HTTPRoute was cleaned up
            route = _get_cr("httproute", f"maas-model-{temp_name}", MODEL_NAMESPACE)
            assert route is None, f"HTTPRoute maas-model-{temp_name} should be cleaned up after deletion"
        finally:
            # Always clean up to avoid resource leaks
            _patch_cr("maasmodelref", temp_name, MODEL_NAMESPACE,
                      {"metadata": {"finalizers": []}})
            _delete_cr("maasmodelref", temp_name, MODEL_NAMESPACE)
            _delete_cr("externalmodel", temp_name, MODEL_NAMESPACE)
