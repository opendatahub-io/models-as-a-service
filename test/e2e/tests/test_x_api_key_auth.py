"""
E2E tests for x-api-key inbound authentication.

Validates that the gateway AuthPolicy dynamically adds an x-api-key identity
source when an IPP ExternalModel CR with apiFormat=messages exists, allowing
Anthropic SDK clients to authenticate via x-api-key header instead of
Authorization: Bearer.

Prerequisites:
- MaaS deployed with x-api-key support in the controller
- IPP ExternalModel CRD (externalmodels.inference.opendatahub.io) installed

Environment variables:
  See test_helper.py module docstring for shared environment variables.
  Additional:
  - GATEWAY_NAMESPACE: Gateway namespace (default: openshift-ingress)
"""

import json
import logging
import os
import subprocess
import time

import pytest
import requests

from test_helper import (
    MODEL_NAME,
    MODEL_NAMESPACE,
    MODEL_PATH,
    TIMEOUT,
    TLS_VERIFY,
    _apply_cr,
    _delete_cr,
    _gateway_url,
    _inference,
    _poll_status,
    _wait_reconcile,
)

log = logging.getLogger(__name__)

GATEWAY_NAMESPACE = os.environ.get("GATEWAY_NAMESPACE", "openshift-ingress")
GATEWAY_AUTH_POLICY_NAME = "maas-gateway-auth"
IDENTITY_SOURCE_NAME = "api-keys-x-api-key"

IPP_EXTERNAL_MODEL_CRD = "externalmodels.inference.opendatahub.io"
IPP_EXTERNAL_MODEL_NAME = "e2e-x-api-key-trigger"

IPP_EXTERNAL_MODEL_CR = {
    "apiVersion": "inference.opendatahub.io/v1alpha1",
    "kind": "ExternalModel",
    "metadata": {"name": IPP_EXTERNAL_MODEL_NAME, "namespace": MODEL_NAMESPACE},
    "spec": {
        "modelName": "claude-test",
        "externalProviderRefs": [{
            "ref": {"name": "dummy-anthropic"},
            "targetModel": "claude-sonnet-4-20250514",
            "apiFormat": "messages",
        }],
    },
}


def _crd_installed(crd_name):
    """Check if a CRD is installed on the cluster."""
    result = subprocess.run(
        ["oc", "get", "crd", crd_name],
        capture_output=True, text=True, timeout=30,
    )
    return result.returncode == 0


def _get_authpolicy_identities():
    """Get the list of identity source names from the gateway AuthPolicy."""
    result = subprocess.run(
        ["oc", "get", "authpolicy", GATEWAY_AUTH_POLICY_NAME,
         "-n", GATEWAY_NAMESPACE, "-o", "json"],
        capture_output=True, text=True, timeout=30,
    )
    if result.returncode != 0:
        return []

    ap = json.loads(result.stdout)
    defaults = (ap.get("spec") or {}).get("defaults") or {}
    rules = defaults.get("rules") or {}
    authentication = rules.get("authentication") or {}
    return list(authentication.keys())


def _wait_for_identity_source(identity_name, present=True, timeout=120):
    """Poll AuthPolicy until an identity source appears or disappears."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        identities = _get_authpolicy_identities()
        found = identity_name in identities
        if found == present:
            log.info(
                "Identity source %r %s (identities: %s)",
                identity_name, "appeared" if present else "removed", identities,
            )
            return True
        time.sleep(5)
    raise TimeoutError(
        f"Identity source {identity_name!r} did not {'appear' if present else 'disappear'} "
        f"within {timeout}s. Current identities: {_get_authpolicy_identities()}"
    )


def _trigger_reconcile():
    """Annotate all MaaSAuthPolicies to trigger controller reconciliation."""
    ns = os.environ.get("MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service")
    result = subprocess.run(
        ["oc", "get", "maasauthpolicy", "-n", ns, "-o", "name"],
        capture_output=True, text=True, timeout=30,
    )
    if result.returncode != 0:
        log.warning("Failed to list MaaSAuthPolicies: %s", result.stderr.strip())
        return

    for resource in result.stdout.strip().splitlines():
        if not resource:
            continue
        subprocess.run(
            ["oc", "annotate", resource, "-n", ns,
             f"e2e.maas/reconcile-trigger={int(time.time())}", "--overwrite"],
            capture_output=True, text=True, timeout=30,
        )


def _inference_x_api_key(api_key, path=None, model_name=None):
    """Send inference with x-api-key header instead of Authorization."""
    path = path or MODEL_PATH
    url = f"{_gateway_url()}{path}/v1/completions"
    headers = {"x-api-key": api_key, "Content-Type": "application/json"}
    return requests.post(
        url, headers=headers,
        json={"model": model_name or MODEL_NAME, "prompt": "Hello", "max_tokens": 3},
        timeout=TIMEOUT, verify=TLS_VERIFY,
    )


pytestmark = pytest.mark.skipif(
    not _crd_installed(IPP_EXTERNAL_MODEL_CRD),
    reason=f"IPP ExternalModel CRD ({IPP_EXTERNAL_MODEL_CRD}) not installed",
)


@pytest.fixture(scope="module")
def x_api_key_setup(api_key):
    """Create IPP ExternalModel with apiFormat=messages to enable x-api-key identity source.

    Setup:
      1. Apply IPP ExternalModel CR
      2. Trigger reconciliation
      3. Wait for api-keys-x-api-key identity to appear in gateway AuthPolicy

    Teardown:
      1. Delete IPP ExternalModel CR
      2. Trigger reconciliation
      3. Wait for api-keys-x-api-key identity to be removed
    """
    log.info("Setting up x-api-key auth test fixture...")

    _apply_cr(IPP_EXTERNAL_MODEL_CR)
    _wait_reconcile()
    _trigger_reconcile()

    try:
        _wait_for_identity_source(IDENTITY_SOURCE_NAME, present=True, timeout=120)
    except TimeoutError:
        _delete_cr("externalmodel.inference.opendatahub.io", IPP_EXTERNAL_MODEL_NAME, MODEL_NAMESPACE)
        raise

    _poll_status(api_key, 200, timeout=60)

    log.info("x-api-key identity source is active, running tests...")
    yield api_key

    log.info("Cleaning up x-api-key auth test fixture...")
    _delete_cr("externalmodel.inference.opendatahub.io", IPP_EXTERNAL_MODEL_NAME, MODEL_NAMESPACE)
    _wait_reconcile()
    _trigger_reconcile()

    try:
        _wait_for_identity_source(IDENTITY_SOURCE_NAME, present=False, timeout=120)
    except TimeoutError:
        log.warning("Identity source %s did not disappear during cleanup", IDENTITY_SOURCE_NAME)


class TestXAPIKeyAuthentication:
    """Validate x-api-key header authentication when IPP ExternalModel with apiFormat=messages exists."""

    def test_x_api_key_authenticates(self, x_api_key_setup):
        """x-api-key header with valid API key returns 200."""
        api_key = x_api_key_setup
        r = _inference_x_api_key(api_key)
        assert r.status_code == 200, (
            f"Expected 200 with x-api-key header, got {r.status_code}: {r.text[:500]}"
        )

    def test_authorization_bearer_still_works(self, x_api_key_setup):
        """Authorization: Bearer still works when x-api-key identity source is active."""
        api_key = x_api_key_setup
        r = _inference(api_key)
        assert r.status_code == 200, (
            f"Expected 200 with Authorization: Bearer, got {r.status_code}: {r.text[:500]}"
        )

    def test_invalid_x_api_key_rejected(self, x_api_key_setup):
        """Invalid API key in x-api-key header is rejected."""
        r = _inference_x_api_key("sk-oai-invalid-not-a-real-key-12345")
        assert r.status_code in (401, 403), (
            f"Expected 401/403 for invalid x-api-key, got {r.status_code}: {r.text[:500]}"
        )

    def test_x_api_key_without_prefix_rejected(self, x_api_key_setup):
        """Random value without sk-oai- prefix in x-api-key header is rejected."""
        r = _inference_x_api_key("random-value-no-prefix")
        assert r.status_code in (401, 403), (
            f"Expected 401/403 for x-api-key without valid prefix, got {r.status_code}: {r.text[:500]}"
        )

    def test_both_headers_no_conflict(self, x_api_key_setup):
        """Sending both Authorization: Bearer and x-api-key does not cause conflicts."""
        api_key = x_api_key_setup
        r = _inference(api_key, extra_headers={"x-api-key": api_key})
        assert r.status_code == 200, (
            f"Expected 200 with both auth headers, got {r.status_code}: {r.text[:500]}"
        )
