"""
Negative-path and security-oriented E2E tests for MaaS.

Validates that the platform correctly rejects abuse scenarios:
- Header spoofing: client-supplied identity headers are stripped
- Expired API keys: rejected at gateway level
- Cross-model access: subscription-model binding enforced
- AuthPolicy removal: access revoked when policy deleted
- Missing resources: CRs referencing non-existent models

Requires:
  - GATEWAY_HOST env var
  - MAAS_API_BASE_URL env var (for API key creation)
  - oc/kubectl access to manage CRs
  - Pre-deployed test models (free-tier simulator)

Environment variables:
  See test_helper.py module docstring for shared environment variables
  (GATEWAY_HOST, MAAS_API_BASE_URL, MAAS_SUBSCRIPTION_NAMESPACE, etc.).
  This file uses no additional file-specific environment variables.
"""

import http.client
import json
import logging
import ssl
import time
import uuid
from urllib.parse import urlparse

import pytest
import requests

from test_helper import (
    MODEL_NAME,
    MODEL_NAMESPACE,
    MODEL_PATH,
    SIMULATOR_SUBSCRIPTION,
    TIMEOUT,
    TLS_VERIFY,
    UNCONFIGURED_MODEL_PATH,
    UNCONFIGURED_MODEL_REF,
    _create_api_key,
    _create_test_auth_policy,
    _create_test_subscription,
    _delete_cr,
    _gateway_url,
    _get_cluster_token,
    _get_cr,
    _inference,
    _maas_api_url,
    _poll_status,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
)

log = logging.getLogger(__name__)


# ============================================================================
# P0: Header Spoofing Tests
# ============================================================================

class TestHeaderSpoofing:
    """Verify that client-supplied identity headers cannot influence authorization.

    The AuthPolicy is configured to strip identity headers (X-MaaS-Username,
    X-MaaS-Group, X-MaaS-Key-Id) before forwarding to the model backend.
    Only X-MaaS-Subscription is injected (from key-derived identity, not client).

    Security invariant: key-derived identity always wins over client-supplied headers.
    """

    def test_injected_identity_headers_ignored(self):
        """Client injects X-MaaS-Username/Group/Key-Id — platform ignores them.

        Validates that Authorino strips attacker-controlled identity headers.
        The request should succeed (200) using the real key-derived identity,
        proving the spoofed headers had no effect on authorization.
        """
        api_key = _create_api_key(_get_cluster_token(), subscription=SIMULATOR_SUBSCRIPTION)

        spoofed_headers = {
            "X-MaaS-Username": "cluster-admin",
            "X-MaaS-Group": "system:cluster-admins,system:masters",
            "X-MaaS-Key-Id": "fake-key-id-00000",
        }

        r = _inference(api_key, extra_headers=spoofed_headers)

        # Request succeeds with the REAL identity (API key owner), not the spoofed one.
        # If spoofed headers were honored, the test user would gain cluster-admin access.
        log.info("Spoofed identity headers -> %s", r.status_code)
        assert r.status_code == 200, (
            f"Expected 200 (spoofed headers stripped, real identity used), "
            f"got {r.status_code}: {r.text[:500]}"
        )

    def test_duplicate_subscription_headers_ignored(self):
        """Client sends multiple X-MaaS-Subscription headers — API key binding wins.

        For API key requests, the subscription is fixed at mint time.
        Duplicate or conflicting X-MaaS-Subscription headers must not override
        the key-derived subscription.
        """
        api_key = _create_api_key(_get_cluster_token(), subscription=SIMULATOR_SUBSCRIPTION)

        # Use http.client to send genuinely duplicate X-MaaS-Subscription headers.
        # The requests library uses a dict for headers, so it cannot send two
        # headers with the same name — the second value overwrites the first.
        gateway = _gateway_url()
        parsed = urlparse(gateway)
        path = f"{MODEL_PATH}/v1/completions"
        body = json.dumps({"model": MODEL_NAME, "prompt": "Hello", "max_tokens": 3})

        if parsed.scheme == "https":
            ctx = ssl.create_default_context()
            if not TLS_VERIFY:
                ctx.check_hostname = False
                ctx.verify_mode = ssl.CERT_NONE
            conn = http.client.HTTPSConnection(
                parsed.hostname, parsed.port or 443, timeout=TIMEOUT, context=ctx,
            )
        else:
            conn = http.client.HTTPConnection(
                parsed.hostname, parsed.port or 80, timeout=TIMEOUT,
            )

        # Two separate X-MaaS-Subscription header lines
        headers = [
            ("Authorization", f"Bearer {api_key}"),
            ("Content-Type", "application/json"),
            ("X-MaaS-Subscription", SIMULATOR_SUBSCRIPTION),
            ("X-MaaS-Subscription", "nonexistent-fake-sub"),
        ]

        conn.putrequest("POST", path)
        for key, value in headers:
            conn.putheader(key, value)
        conn.putheader("Content-Length", str(len(body)))
        conn.endheaders(body.encode())

        resp = conn.getresponse()
        status = resp.status
        resp_body = resp.read().decode(errors="replace")
        conn.close()

        # API key binding wins — request succeeds with key-derived subscription.
        log.info("Duplicate X-MaaS-Subscription headers -> %s", status)
        assert status == 200, (
            f"Expected 200 (API key subscription binding wins over duplicate headers), "
            f"got {status}: {resp_body[:500]}"
        )


# ============================================================================
# P1: Expired Key Rejection
# ============================================================================

class TestExpiredKeyRejection:
    """Verify that expired API keys are rejected at the gateway."""

    def test_expired_key_rejected_at_gateway(self):
        """Create a short-lived API key, wait for expiration, assert 403.

        This validates that Authorino's apiKeyValidation metadata evaluator
        calls /internal/v1/api-keys/validate which returns valid=false for
        expired keys, causing the auth-valid OPA rule to deny the request.
        """
        oc_token = _get_cluster_token()

        # Create key with shortest supported expiration
        url = f"{_maas_api_url()}/v1/api-keys"
        r = requests.post(
            url,
            headers={"Authorization": f"Bearer {oc_token}", "Content-Type": "application/json"},
            json={
                "name": f"e2e-expired-{uuid.uuid4().hex[:8]}",
                "subscription": SIMULATOR_SUBSCRIPTION,
                "expiresIn": "1s",
            },
            timeout=TIMEOUT,
            verify=TLS_VERIFY,
        )
        if r.status_code not in (200, 201):
            pytest.skip(f"Could not create short-lived key: {r.status_code} {r.text}")

        expired_key = r.json().get("key")
        if not expired_key:
            pytest.skip("API key response missing 'key' field")

        # Wait for expiration + cache TTL propagation
        time.sleep(5)

        # Expired key should be rejected at gateway
        r = _poll_status(expired_key, (401, 403), timeout=30)
        log.info("Expired API key -> %s", r.status_code)
        assert r.status_code in (401, 403), (
            f"Expected 401 or 403 for expired key, got {r.status_code}: {r.text[:500]}"
        )


# ============================================================================
# P1: Cross-Model Access
# ============================================================================

class TestCrossModelAccess:
    """Verify subscription-model binding is enforced at gateway.

    A key bound to subscription S (which grants access to model A) must NOT
    be able to access model B (not in subscription S).
    """

    def test_key_cannot_access_model_outside_subscription(self):
        """Key for model A cannot infer on model B outside its subscription.

        Uses the pre-deployed unconfigured model (a model with no subscription
        granting access to it) to test cross-model access denial.
        """
        api_key = _create_api_key(_get_cluster_token(), subscription=SIMULATOR_SUBSCRIPTION)

        # The unconfigured model exists but has no subscription granting access.
        # Using the same API key (bound to simulator-subscription which covers MODEL_REF)
        # should fail because the subscription doesn't cover UNCONFIGURED_MODEL_REF.
        r = _inference(api_key, path=UNCONFIGURED_MODEL_PATH)

        log.info("Cross-model access (model outside subscription) -> %s", r.status_code)
        assert r.status_code in (401, 403), (
            f"Expected 401 or 403 for model outside subscription scope, "
            f"got {r.status_code}: {r.text[:500]}"
        )


# ============================================================================
# P1: AuthPolicy Removal
# ============================================================================

class TestAuthPolicyRemoval:
    """Verify that deleting a MaaSAuthPolicy revokes gateway access.

    When an AuthPolicy is removed, the generated Kuadrant AuthPolicy is also
    deleted, and subsequent requests with the API key should be denied.
    """

    def test_authpolicy_deletion_revokes_access(self):
        """Create auth policy, delete it, verify Kuadrant AuthPolicy is removed.

        Uses the unconfigured model to avoid interfering with other tests.
        Creates a MaaSAuthPolicy, waits for the generated Kuadrant AuthPolicy
        to appear, then deletes the MaaSAuthPolicy and verifies the controller
        removes the downstream Kuadrant AuthPolicy.

        This tests the controller's cleanup logic. Gateway enforcement of
        AuthPolicy is already covered by other tests (e.g. test_wrong_group_gets_403).
        """
        suffix = uuid.uuid4().hex[:8]
        policy_name = f"e2e-neg-policy-{suffix}"
        model_ref = UNCONFIGURED_MODEL_REF
        kuadrant_auth_name = f"maas-auth-{model_ref}"

        try:
            # Create auth policy granting access
            _create_test_auth_policy(
                policy_name,
                model_ref,
                groups=["system:authenticated"],
            )

            _wait_for_maas_auth_policy_phase(policy_name)

            # Verify Kuadrant AuthPolicy was generated
            ap = _get_cr("authpolicy", kuadrant_auth_name, namespace=MODEL_NAMESPACE)
            assert ap is not None, (
                f"Kuadrant AuthPolicy '{kuadrant_auth_name}' should exist after MaaSAuthPolicy creation"
            )
            log.info("Kuadrant AuthPolicy %s exists in %s", kuadrant_auth_name, MODEL_NAMESPACE)

            # Delete the MaaSAuthPolicy
            log.info("Deleting MaaSAuthPolicy %s", policy_name)
            _delete_cr("maasauthpolicy", policy_name)

            # Poll until the Kuadrant AuthPolicy is removed by the controller
            deadline = time.time() + 60
            while time.time() < deadline:
                ap = _get_cr("authpolicy", kuadrant_auth_name, namespace=MODEL_NAMESPACE)
                if ap is None:
                    break
                time.sleep(2)

            assert ap is None, (
                f"Kuadrant AuthPolicy '{kuadrant_auth_name}' should be removed "
                f"after MaaSAuthPolicy deletion"
            )
            log.info("Kuadrant AuthPolicy %s removed after MaaSAuthPolicy deletion", kuadrant_auth_name)

        finally:
            _delete_cr("maasauthpolicy", policy_name)


# ============================================================================
# P2: Missing MaaSModelRef References
# ============================================================================

class TestMissingModelRef:
    """Verify CRs referencing non-existent MaaSModelRefs don't generate gateway resources.

    The MaaS controller uses optimistic reconciliation: CRs referencing models
    that don't exist yet are accepted and set to Active phase (the model could
    be created later). However, the controller should NOT generate downstream
    Kuadrant resources (AuthPolicy, TokenRateLimitPolicy) until the referenced
    MaaSModelRef actually exists.

    These tests create CRs pointing at a non-existent model, confirm the CR
    reaches Active, then verify that no Kuadrant gateway resources were generated.
    """

    def test_subscription_with_nonexistent_model_ref(self):
        """MaaSSubscription with ghost model goes Active but no TRLP is generated.

        Creates a MaaSSubscription referencing a model that doesn't exist,
        waits for the CR to reach Active phase (optimistic reconciliation),
        then asserts that no TokenRateLimitPolicy was created in the model
        namespace — proving the controller doesn't create rate-limit
        enforcement for non-existent models.
        """
        suffix = uuid.uuid4().hex[:8]
        sub_name = f"e2e-neg-ghost-sub-{suffix}"
        ghost_model = f"nonexistent-model-{suffix}"

        try:
            _create_test_subscription(
                sub_name,
                ghost_model,
                groups=["system:authenticated"],
            )

            _wait_for_maas_subscription_phase(sub_name)

            # CR becomes Active, but no TRLP should exist for the ghost model
            trlp_name = f"maas-trlp-{ghost_model}"
            trlp = _get_cr("tokenratelimitpolicy", trlp_name, namespace=MODEL_NAMESPACE)
            log.info("MaaSSubscription with ghost model -> TRLP exists: %s", trlp is not None)
            assert trlp is None, (
                f"TokenRateLimitPolicy '{trlp_name}' should not exist for non-existent model"
            )
        finally:
            _delete_cr("maassubscription", sub_name)

    def test_authpolicy_with_nonexistent_model_ref(self):
        """MaaSAuthPolicy with ghost model goes Active but no AuthPolicy is generated.

        Creates a MaaSAuthPolicy referencing a model that doesn't exist,
        waits for the CR to reach Active phase (optimistic reconciliation),
        then asserts that no Kuadrant AuthPolicy was created in the model
        namespace — proving the controller doesn't create auth enforcement
        for non-existent models.

        Note: we use _wait_for_maas_auth_policy_phase with require_auth_policies=False
        because the default (require_auth_policies=True, require_enforced=True)
        would timeout — there are no authPolicies status entries when the
        referenced model doesn't exist.
        """
        suffix = uuid.uuid4().hex[:8]
        policy_name = f"e2e-neg-ghost-policy-{suffix}"
        ghost_model = f"nonexistent-model-{suffix}"

        try:
            _create_test_auth_policy(
                policy_name,
                ghost_model,
                groups=["system:authenticated"],
            )

            # Wait for phase==Active only (not enforced authPolicies — there are
            # none for a ghost model ref).
            _wait_for_maas_auth_policy_phase(policy_name, "Active", timeout=30, require_auth_policies=False)

            # CR becomes Active, but no Kuadrant AuthPolicy should exist for the ghost model
            auth_name = f"maas-auth-{ghost_model}"
            ap = _get_cr("authpolicy", auth_name, namespace=MODEL_NAMESPACE)
            log.info("MaaSAuthPolicy with ghost model -> AuthPolicy exists: %s", ap is not None)
            assert ap is None, (
                f"AuthPolicy '{auth_name}' should not exist for non-existent model"
            )
        finally:
            _delete_cr("maasauthpolicy", policy_name)


# ============================================================================
# P2: Header Abuse
# ============================================================================

class TestHeaderAbuse:
    """Verify malicious header values are handled safely."""

    def test_special_characters_in_subscription_header(self):
        """Injection-style characters in X-MaaS-Subscription header.

        Ensures the platform returns a clean 403 (subscription not found)
        without leaking errors, stack traces, or SQL/NoSQL injection.
        """
        api_key = _create_api_key(_get_cluster_token(), subscription=SIMULATOR_SUBSCRIPTION)

        injection_payloads = [
            "'; DROP TABLE subscriptions; --",
            '{"$gt": ""}',
            "../../../etc/passwd",
            "<script>alert(1)</script>",
        ]

        for payload in injection_payloads:
            r = _inference(api_key, extra_headers={"X-MaaS-Subscription": payload})

            log.info("Injection payload %r -> %s", payload, r.status_code)
            # API key binding wins — request should succeed (200) because
            # the spoofed header is ignored for API key requests.
            # If the platform processes the header, it should return 403, not 500.
            assert r.status_code != 500, (
                f"Server error with injection payload '{payload}': {r.text[:500]}"
            )
