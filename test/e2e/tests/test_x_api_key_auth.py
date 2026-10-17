"""
E2E tests for x-api-key inbound authentication.

Validates that the gateway AuthPolicy always accepts a MaaS API key through
either x-api-key or Authorization: Bearer, without an ExternalModel trigger.

Prerequisites:
- MaaS deployed with x-api-key support in the controller

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
    MODEL_PATH,
    TIMEOUT,
    TLS_VERIFY,
    _gateway_url,
    _inference,
    _poll_status,
    _wait_for_gateway_auth_enforced,
)

log = logging.getLogger(__name__)

GATEWAY_NAMESPACE = os.environ.get("GATEWAY_NAMESPACE", "openshift-ingress")
GATEWAY_AUTH_POLICY_NAME = "maas-gateway-auth"
IDENTITY_SOURCE_NAME = "api-keys-x-api-key"

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


def _wait_for_identity_source(identity_name, timeout=120):
    """Poll AuthPolicy until an identity source appears."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        identities = _get_authpolicy_identities()
        if identity_name in identities:
            log.info("Identity source %r appeared (identities: %s)", identity_name, identities)
            return True
        time.sleep(5)
    raise TimeoutError(
        f"Identity source {identity_name!r} did not appear "
        f"within {timeout}s. Current identities: {_get_authpolicy_identities()}"
    )


def _inference_x_api_key(api_key, path=None, extra_headers=None, model_name=None):
    """Send inference with x-api-key header instead of Authorization.

    Accepts the same kwargs as ``_inference`` so it can be passed to
    ``_poll_status(..., inference_fn=_inference_x_api_key)``.
    """
    path = path or MODEL_PATH
    url = f"{_gateway_url()}{path}/v1/completions"
    headers = {"x-api-key": api_key, "Content-Type": "application/json"}
    if extra_headers:
        headers.update(extra_headers)
    return requests.post(
        url, headers=headers,
        json={"model": model_name or MODEL_NAME, "prompt": "Hello", "max_tokens": 3},
        timeout=TIMEOUT, verify=TLS_VERIFY,
    )


pytestmark = [
    pytest.mark.xdist_group("api_keys"),
    pytest.mark.serial,
]


@pytest.fixture(scope="module")
def x_api_key_setup(api_key):
    """Wait for the always-on gateway identity source and auth enforcement."""
    _wait_for_identity_source(IDENTITY_SOURCE_NAME, timeout=120)

    # AuthPolicy YAML can list api-keys-x-api-key before Kuadrant/Authorino has
    # enforced it. Bearer still works via the existing api-keys identity, so a
    # Bearer-only readiness check misses empty-body 401s on x-api-key.
    _wait_for_gateway_auth_enforced(timeout=120)
    _poll_status(api_key, 200, timeout=60)
    _poll_status(api_key, 200, timeout=120, inference_fn=_inference_x_api_key)

    return api_key


class TestXAPIKeyAuthentication:
    """Validate x-api-key auth without creating any ExternalModel."""

    def test_x_api_key_authenticates(self, x_api_key_setup):
        """x-api-key header with valid API key returns 200."""
        api_key = x_api_key_setup
        r = _inference_x_api_key(api_key)
        assert r.status_code == 200, (
            f"Expected 200 with x-api-key header, got {r.status_code}: {r.text[:500]}"
        )

    def test_authorization_bearer_still_works(self, x_api_key_setup):
        """Authorization: Bearer authenticates the same key."""
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
