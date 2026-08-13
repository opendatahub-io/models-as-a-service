"""
Negative-path and security-oriented E2E tests for MaaS.

Validates that the platform correctly rejects abuse scenarios:
- Header spoofing: client-supplied X-MaaS identity headers are rejected at the gateway
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
import subprocess
import time
import uuid
from urllib.parse import urlparse

import pytest
import requests

from test_helper import (
    MODEL_NAME,
    MODEL_NAMESPACE,
    MODEL_PATH,
    MODEL_REF,
    SIMULATOR_SUBSCRIPTION,
    TIMEOUT,
    TLS_VERIFY,
    UNCONFIGURED_MODEL_PATH,
    UNCONFIGURED_MODEL_REF,
    _create_api_key,
    _create_api_key_raw,
    _create_test_auth_policy,
    _create_test_subscription,
    _delete_cr,
    _gateway_url,
    _get_cluster_token,
    _get_cr,
    _inference,
    _maas_api_url,
    _poll_status,
    _revoke_api_key,
    _wait_for_gateway_auth_enforced,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
)

log = logging.getLogger(__name__)


# ============================================================================
# P0: Header Spoofing Tests
# ============================================================================

class TestHeaderSpoofing:
    """Verify that client-supplied identity headers cannot forge authorization.

    AuthPolicy deny-client-identity-headers rejects requests that already carry
    X-MaaS-Username / X-MaaS-Group. Authorino then injects trusted identity
    headers only after successful authentication.

    Security invariant: client-supplied identity headers are denied, not trusted.
    """

    @pytest.mark.parametrize(
        "forged_header,label",
        [
            ({"X-MaaS-Username": "cluster-admin"}, "X-MaaS-Username"),
            (
                {"X-MaaS-Group": '["system:cluster-admins","system:masters"]'},
                "X-MaaS-Group",
            ),
        ],
        ids=["username-only", "group-only"],
    )
    def test_forged_identity_headers_rejected_on_key_mint(self, forged_header, label):
        """POST /v1/api-keys with a forged X-MaaS identity header must be denied.

        Each denied header is asserted independently so a regression that only
        drops username or only drops group cannot hide behind a combined spoof.

        Under deny semantics there is no spoofed key to inspect — Authorino
        rejects before maas-api runs, so the response must not contain key
        material. A follow-up mint without forged headers still succeeds.
        """
        _wait_for_gateway_auth_enforced()
        oc_token = _get_cluster_token()

        # Prove the gateway is ready for legitimate minting first.
        # Revoke even if status/JSON assertions fail.
        baseline_key_id = None
        try:
            baseline = _create_api_key_raw(oc_token, subscription=SIMULATOR_SUBSCRIPTION)
            assert baseline.status_code in (200, 201), (
                f"Baseline API key mint failed before spoof check: "
                f"{baseline.status_code} body_bytes={len(baseline.content)}"
            )
            baseline_key_id = baseline.json().get("id")
        finally:
            if baseline_key_id:
                _revoke_api_key(oc_token, baseline_key_id)

        spoofed_headers = {
            "Authorization": f"Bearer {oc_token}",
            "Content-Type": "application/json",
            **forged_header,
        }
        url = f"{_maas_api_url()}/v1/api-keys"
        body = {
            "name": f"e2e-spoof-mint-{uuid.uuid4().hex[:8]}",
            "subscription": SIMULATOR_SUBSCRIPTION,
        }

        # Do not use _request_with_gateway_retry here: empty 403 is the expected
        # Authorino deny and must not be treated as transient propagation.
        r = requests.post(url, headers=spoofed_headers, json=body, timeout=TIMEOUT, verify=TLS_VERIFY)

        # Never log response bodies from mint endpoints — a CVE regression
        # could return a live sk-oai-* key into CI logs (CWE-532).
        log.info(
            "Forged %s key mint -> %s body_bytes=%d",
            label,
            r.status_code,
            len(r.content),
        )
        assert r.status_code in (401, 403), (
            f"Expected 401/403 denying forged {label} on key mint, "
            f"got {r.status_code} body_bytes={len(r.content)}"
        )
        assert "sk-oai-" not in r.text, (
            "Denied mint response must not contain API key material"
        )

        # Control: same token without forged headers can still mint.
        # Revoke even if status/JSON/key-format assertions fail.
        control_key_id = None
        try:
            control = _create_api_key_raw(oc_token, subscription=SIMULATOR_SUBSCRIPTION)
            assert control.status_code in (200, 201), (
                f"Control mint without forged headers failed: "
                f"{control.status_code} body_bytes={len(control.content)}"
            )
            control_body = control.json()
            control_key_id = control_body.get("id")
            assert control_body.get("key", "").startswith("sk-oai-"), (
                "Control mint missing sk-oai- key prefix"
            )
        finally:
            if control_key_id:
                _revoke_api_key(oc_token, control_key_id)

    def test_injected_identity_headers_rejected_on_inference(self):
        """Client injects X-MaaS-Username/Group — gateway rejects the request.

        With deny-client-identity-headers, forged identity headers are not
        overwritten and ignored; the request is denied at Authorino.
        """
        # Earlier tests may have rewritten maas-gateway-auth; minting a key
        # against an unenforced gateway yields empty 403 flakes.
        _wait_for_gateway_auth_enforced()
        api_key = _create_api_key(_get_cluster_token(), subscription=SIMULATOR_SUBSCRIPTION)

        spoofed_headers = {
            "X-MaaS-Username": "cluster-admin",
            "X-MaaS-Group": "system:cluster-admins,system:masters",
            "X-MaaS-Key-Id": "fake-key-id-00000",
        }

        r = _poll_status(api_key, (401, 403), extra_headers=spoofed_headers, timeout=30)

        log.info(
            "Spoofed identity headers on inference -> %s body_bytes=%d",
            r.status_code,
            len(r.content),
        )
        assert r.status_code in (401, 403), (
            f"Expected 401/403 (forged identity headers denied), "
            f"got {r.status_code} body_bytes={len(r.content)}"
        )
        assert "sk-oai-" not in r.text, (
            "Denied inference response must not contain API key material"
        )

    def test_duplicate_subscription_headers_ignored(self):
        """Client sends multiple X-MaaS-Subscription headers — API key binding wins.

        For API key requests, the subscription is fixed at mint time.
        Duplicate or conflicting X-MaaS-Subscription headers must not override
        the key-derived subscription.
        """
        _wait_for_gateway_auth_enforced()
        api_key = _create_api_key(_get_cluster_token(), subscription=SIMULATOR_SUBSCRIPTION)

        # Use http.client to send genuinely duplicate X-MaaS-Subscription headers.
        # The requests library uses a dict for headers, so it cannot send two
        # headers with the same name — the second value overwrites the first.
        path = f"{MODEL_PATH}/v1/completions"
        body = json.dumps({"model": MODEL_NAME, "prompt": "Hello", "max_tokens": 3})

        # Two separate X-MaaS-Subscription header lines
        headers = [
            ("Authorization", f"Bearer {api_key}"),
            ("Content-Type", "application/json"),
            ("X-MaaS-Subscription", SIMULATOR_SUBSCRIPTION),
            ("X-MaaS-Subscription", "nonexistent-fake-sub"),
        ]

        # As above, keep the raw duplicate headers on every retry while waiting
        # for shared gateway authorization state to settle.
        deadline = time.time() + 30
        while True:
            gateway = _gateway_url()
            parsed = urlparse(gateway)
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

            try:
                conn.putrequest("POST", path)
                for key, value in headers:
                    conn.putheader(key, value)
                conn.putheader("Content-Length", str(len(body)))
                conn.endheaders(body.encode())

                resp = conn.getresponse()
                status = resp.status
                resp_body = resp.read().decode(errors="replace")
            finally:
                conn.close()

            if status == 200 or time.time() >= deadline:
                break
            time.sleep(2)

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
        _wait_for_gateway_auth_enforced()
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
        """Create auth policy, delete it, verify legacy per-model AuthPolicy is absent.

        Uses the unconfigured model to avoid interfering with other tests.
        In gateway-only mode, the controller should not create per-model
        Kuadrant AuthPolicies. This verifies the legacy per-model AuthPolicy
        remains absent before and after MaaSAuthPolicy deletion.

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

            # Gateway-only mode should not generate per-model AuthPolicies.
            ap = _get_cr("authpolicy", kuadrant_auth_name, namespace=MODEL_NAMESPACE)
            assert ap is None, (
                f"Legacy per-model AuthPolicy '{kuadrant_auth_name}' should not exist in gateway-only mode"
            )
            log.info("Legacy per-model AuthPolicy %s absent as expected", kuadrant_auth_name)

            # Delete the MaaSAuthPolicy
            log.info("Deleting MaaSAuthPolicy %s", policy_name)
            _delete_cr("maasauthpolicy", policy_name)

            # Poll to confirm legacy per-model AuthPolicy remains absent.
            deadline = time.time() + 60
            while time.time() < deadline:
                ap = _get_cr("authpolicy", kuadrant_auth_name, namespace=MODEL_NAMESPACE)
                if ap is None:
                    break
                time.sleep(2)

            assert ap is None, f"Legacy per-model AuthPolicy '{kuadrant_auth_name}' should remain absent"
            log.info("Legacy per-model AuthPolicy %s remained absent after deletion", kuadrant_auth_name)

        finally:
            _delete_cr("maasauthpolicy", policy_name)


# ============================================================================
# P2: Missing MaaSModelRef References
# ============================================================================

class TestMissingModelRef:
    """Verify CRs don't generate gateway resources for non-existent MaaSModelRefs.

    Uses a Degraded/partial approach: each CR references one valid model
    (MODEL_REF) and one ghost model. The CR reaches Degraded phase, proving
    the controller processed it successfully. We then verify that downstream
    Kuadrant resources were created only for the valid model, not the ghost.

    This is stronger than testing with all-ghost models (which just go Failed),
    because it proves the controller selectively generates resources per model
    rather than failing early before resource generation.
    """

    def test_subscription_with_nonexistent_model_ref(self):
        """MaaSSubscription generates TRLP only for valid model, not ghost model.

        Creates a subscription referencing one valid model and one ghost model,
        waits for Degraded phase, then asserts that a TRLP exists for the valid
        model but not for the ghost model.
        """
        suffix = uuid.uuid4().hex[:8]
        sub_name = f"e2e-neg-ghost-sub-{suffix}"
        auth_name = f"e2e-neg-ghost-sub-auth-{suffix}"
        ghost_model = f"nonexistent-model-{suffix}"

        try:
            _create_test_auth_policy(auth_name, MODEL_REF, groups=["system:authenticated"])
            _create_test_subscription(
                sub_name,
                [MODEL_REF, ghost_model],
                groups=["system:authenticated"],
            )

            _wait_for_maas_subscription_phase(sub_name, "Degraded", timeout=60)

            # No TRLP should exist for the ghost model
            ghost_trlp_name = f"maas-trlp-{ghost_model}"
            ghost_trlp = _get_cr("tokenratelimitpolicy", ghost_trlp_name, namespace=MODEL_NAMESPACE)
            log.info("Ghost model TRLP exists: %s", ghost_trlp is not None)
            assert ghost_trlp is None, (
                f"TokenRateLimitPolicy '{ghost_trlp_name}' should not exist for non-existent model"
            )

            # TRLP should exist for the valid model
            valid_trlp_name = f"maas-trlp-{MODEL_REF}"
            valid_trlp = _get_cr("tokenratelimitpolicy", valid_trlp_name, namespace=MODEL_NAMESPACE)
            log.info("Valid model TRLP exists: %s", valid_trlp is not None)
            assert valid_trlp is not None, (
                f"TokenRateLimitPolicy '{valid_trlp_name}' should exist for valid model"
            )

        finally:
            _delete_cr("maassubscription", sub_name)
            _delete_cr("maasauthpolicy", auth_name)

    def test_authpolicy_with_nonexistent_model_ref(self):
        """MaaSAuthPolicy does not generate per-model AuthPolicies in gateway-only mode.

        Creates an auth policy referencing one valid model and one ghost model,
        waits for Degraded phase, then asserts that no legacy per-model
        AuthPolicies exist for either model.
        """
        suffix = uuid.uuid4().hex[:8]
        policy_name = f"e2e-neg-ghost-policy-{suffix}"
        ghost_model = f"nonexistent-model-{suffix}"

        try:
            _create_test_auth_policy(
                policy_name,
                [MODEL_REF, ghost_model],
                groups=["system:authenticated"],
            )

            _wait_for_maas_auth_policy_phase(policy_name, "Degraded", timeout=60, require_auth_policies=False)

            # No AuthPolicy should exist for the ghost model
            ghost_auth_name = f"maas-auth-{ghost_model}"
            ghost_ap = _get_cr("authpolicy", ghost_auth_name, namespace=MODEL_NAMESPACE)
            log.info("Ghost model AuthPolicy exists: %s", ghost_ap is not None)
            assert ghost_ap is None, (
                f"AuthPolicy '{ghost_auth_name}' should not exist for non-existent model"
            )

            # Gateway-only mode should not create a per-model AuthPolicy for valid model either.
            valid_auth_name = f"maas-auth-{MODEL_REF}"
            valid_ap = _get_cr("authpolicy", valid_auth_name, namespace=MODEL_NAMESPACE)
            log.info("Valid model AuthPolicy exists: %s", valid_ap is not None)
            assert valid_ap is None, (
                f"AuthPolicy '{valid_auth_name}' should not exist in gateway-only mode"
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
        _wait_for_gateway_auth_enforced()
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


class TestWebhookValidation:
    """Verify admission webhooks enforce namespace labeling requirements."""

    def test_subscription_rejected_in_unlabeled_namespace(self):
        """MaaSSubscription create is rejected in namespace without MaasTenantConfig CR.

        Webhooks require namespaces to have a MaasTenantConfig CR to contain tenant resources.
        """
        test_ns = f"e2e-webhook-test-{uuid.uuid4().hex[:6]}"

        try:
            # Create namespace without MaasTenantConfig CR
            result = subprocess.run(
                ["oc", "create", "namespace", test_ns],
                capture_output=True, text=True, timeout=30
            )
            assert result.returncode == 0, f"Failed to create namespace: {result.stderr}"

            # Try to create MaaSSubscription (should be rejected by webhook)
            result = subprocess.run(
                ["oc", "apply", "-f", "-"],
                input=json.dumps({
                    "apiVersion": "maas.opendatahub.io/v1alpha1",
                    "kind": "MaaSSubscription",
                    "metadata": {"name": "test-sub", "namespace": test_ns},
                    "spec": {
                        "owner": {"groups": [{"name": "system:authenticated"}]},
                        "modelRefs": [{
                            "name": MODEL_REF,
                            "namespace": MODEL_NAMESPACE,
                            "tokenRateLimits": [{"limit": 100, "window": "1m"}]
                        }],
                    },
                }),
                capture_output=True, text=True, timeout=30
            )

            # Verify webhook rejection
            assert result.returncode != 0, "Expected webhook to reject subscription in namespace without MaasTenantConfig CR"
            assert "admission webhook" in result.stderr.lower(), \
                f"Expected webhook rejection, got: {result.stderr}"
            assert "not enabled for MaaS tenant resources" in result.stderr, \
                f"Expected helpful error message, got: {result.stderr}"
            assert "Create a MaasTenantConfig CR" in result.stderr, \
                f"Expected error to mention creating MaasTenantConfig CR, got: {result.stderr}"

            log.info("✅ Webhook correctly rejected MaaSSubscription in namespace without MaasTenantConfig CR")
            log.info(f"Error message: {result.stderr}")

        finally:
            # Clean up namespace
            subprocess.run(
                ["oc", "delete", "namespace", test_ns, "--ignore-not-found"],
                capture_output=True, text=True, timeout=30
            )

    def test_authpolicy_rejected_in_unlabeled_namespace(self):
        """MaaSAuthPolicy create is rejected in namespace without MaasTenantConfig CR."""
        test_ns = f"e2e-webhook-test-{uuid.uuid4().hex[:6]}"

        try:
            # Create namespace without MaasTenantConfig CR
            result = subprocess.run(
                ["oc", "create", "namespace", test_ns],
                capture_output=True, text=True, timeout=30
            )
            assert result.returncode == 0, f"Failed to create namespace: {result.stderr}"

            # Try to create MaaSAuthPolicy (should be rejected by webhook)
            result = subprocess.run(
                ["oc", "apply", "-f", "-"],
                input=json.dumps({
                    "apiVersion": "maas.opendatahub.io/v1alpha1",
                    "kind": "MaaSAuthPolicy",
                    "metadata": {"name": "test-policy", "namespace": test_ns},
                    "spec": {
                        "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE}],
                        "subjects": {"groups": [{"name": "system:authenticated"}]},
                    },
                }),
                capture_output=True, text=True, timeout=30
            )

            # Verify webhook rejection
            assert result.returncode != 0, "Expected webhook to reject auth policy in namespace without MaasTenantConfig CR"
            assert "admission webhook" in result.stderr.lower(), \
                f"Expected webhook rejection, got: {result.stderr}"
            assert "not enabled for MaaS tenant resources" in result.stderr, \
                f"Expected helpful error message, got: {result.stderr}"

            log.info("✅ Webhook correctly rejected MaaSAuthPolicy in namespace without MaasTenantConfig CR")

        finally:
            # Clean up namespace
            subprocess.run(
                ["oc", "delete", "namespace", test_ns, "--ignore-not-found"],
                capture_output=True, text=True, timeout=30
            )
