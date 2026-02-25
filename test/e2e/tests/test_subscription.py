"""
MaaS Subscription Controller e2e tests.

Tests auth enforcement (MaaSAuthPolicy) and rate limiting (MaaSSubscription)
by hitting the gateway directly with raw Kubernetes tokens (not minted MaaS tokens).

Requires:
  - GATEWAY_HOST env var (e.g. maas.apps.cluster.example.com)
  - maas-controller deployed with example CRs applied
  - oc/kubectl access to create service account tokens

Environment variables (all optional, with defaults):
  - GATEWAY_HOST: Gateway hostname (required)
  - MAAS_NAMESPACE: MaaS namespace (default: opendatahub)
  - E2E_TIMEOUT: Request timeout in seconds (default: 30)
  - E2E_RECONCILE_WAIT: Wait time for reconciliation in seconds (default: 8)
  - E2E_MODEL_PATH: Path to free model (default: /llm/facebook-opt-125m-simulated)
  - E2E_PREMIUM_MODEL_PATH: Path to premium model (default: /llm/premium-simulated-simulated-premium)
  - E2E_MODEL_NAME: Model name for API requests (default: facebook/opt-125m)
  - E2E_MODEL_REF: Model ref for CRs (default: facebook-opt-125m-simulated)
  - E2E_PREMIUM_MODEL_REF: Premium model ref for CRs (default: premium-simulated-simulated-premium)
  - E2E_SIMULATOR_SUBSCRIPTION: Simulator subscription name (default: simulator-subscription)
  - E2E_SIMULATOR_ACCESS_POLICY: Simulator auth policy name (default: simulator-access)
  - E2E_INVALID_SUBSCRIPTION: Invalid subscription name for 429 test (default: nonexistent-sub)
"""

import logging
import os
import subprocess
import json
import time
import pytest
import requests

log = logging.getLogger(__name__)

# Constants (override with env vars)
TIMEOUT = int(os.environ.get("E2E_TIMEOUT", "30"))
RECONCILE_WAIT = int(os.environ.get("E2E_RECONCILE_WAIT", "8"))
MODEL_PATH = os.environ.get("E2E_MODEL_PATH", "/llm/facebook-opt-125m-simulated")
PREMIUM_MODEL_PATH = os.environ.get("E2E_PREMIUM_MODEL_PATH", "/llm/premium-simulated-simulated-premium")
MODEL_NAME = os.environ.get("E2E_MODEL_NAME", "facebook/opt-125m")
MODEL_REF = os.environ.get("E2E_MODEL_REF", "facebook-opt-125m-simulated")
PREMIUM_MODEL_REF = os.environ.get("E2E_PREMIUM_MODEL_REF", "premium-simulated-simulated-premium")
SIMULATOR_SUBSCRIPTION = os.environ.get("E2E_SIMULATOR_SUBSCRIPTION", "simulator-subscription")
SIMULATOR_ACCESS_POLICY = os.environ.get("E2E_SIMULATOR_ACCESS_POLICY", "simulator-access")
INVALID_SUBSCRIPTION = os.environ.get("E2E_INVALID_SUBSCRIPTION", "nonexistent-sub")


def _ns():
    return os.environ.get("MAAS_NAMESPACE", "opendatahub")


def _gateway_url():
    host = os.environ.get("GATEWAY_HOST", "")
    if not host:
        raise RuntimeError("GATEWAY_HOST env var is required")
    scheme = "http" if os.environ.get("INSECURE_HTTP", "").lower() == "true" else "https"
    return f"{scheme}://{host}"


def _get_cluster_token():
    result = subprocess.run(["oc", "whoami", "-t"], capture_output=True, text=True)
    token = result.stdout.strip()
    if not token:
        raise RuntimeError("Could not get cluster token via 'oc whoami -t'")
    return token


def _create_sa_token(sa_name, namespace=None, duration="10m"):
    namespace = namespace or _ns()
    sa_result = subprocess.run(["oc", "create", "sa", sa_name, "-n", namespace], capture_output=True, text=True)
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
    namespace = namespace or _ns()
    subprocess.run(["oc", "delete", "sa", sa_name, "-n", namespace, "--ignore-not-found"], capture_output=True, text=True)


def _apply_cr(cr_dict):
    subprocess.run(["oc", "apply", "-f", "-"], input=json.dumps(cr_dict), capture_output=True, text=True, check=True)


def _delete_cr(kind, name, namespace=None):
    namespace = namespace or _ns()
    subprocess.run(["oc", "delete", kind, name, "-n", namespace, "--ignore-not-found", "--timeout=30s"], capture_output=True, text=True)


def _get_cr(kind, name, namespace=None):
    namespace = namespace or _ns()
    result = subprocess.run(["oc", "get", kind, name, "-n", namespace, "-o", "json"], capture_output=True, text=True)
    if result.returncode != 0:
        return None
    return json.loads(result.stdout)


def _cr_exists(kind, name, namespace=None):
    namespace = namespace or _ns()
    result = subprocess.run(["oc", "get", kind, name, "-n", namespace], capture_output=True, text=True)
    return result.returncode == 0


def _inference(token, path=None, extra_headers=None):
    path = path or MODEL_PATH
    url = f"{_gateway_url()}{path}/v1/completions"
    headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}
    if extra_headers:
        headers.update(extra_headers)
    return requests.post(
        url, headers=headers,
        json={"model": MODEL_NAME, "prompt": "Hello", "max_tokens": 3},
        timeout=TIMEOUT, verify=False,
    )


def _wait_reconcile(seconds=None):
    time.sleep(seconds or RECONCILE_WAIT)


def _poll_status(token, expected, path=None, extra_headers=None, timeout=None, poll_interval=2):
    """Poll inference endpoint until expected HTTP status or timeout."""
    timeout = timeout or max(RECONCILE_WAIT * 3, 30)
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        r = _inference(token, path=path, extra_headers=extra_headers)
        if r.status_code == expected:
            return r
        last = r
        time.sleep(poll_interval)
    actual = last.status_code if last else "no response"
    raise AssertionError(
        f"Expected {expected} within {timeout}s, last status: {actual}"
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


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

class TestAuthEnforcement:
    """Tests that MaaSAuthPolicy correctly enforces access."""

    def test_authorized_user_gets_200(self):
        """Admin user (in system:authenticated) should access the free model."""
        token = _get_cluster_token()
        r = _inference(token)
        log.info(f"Authorized user -> {r.status_code}")
        assert r.status_code == 200, f"Expected 200, got {r.status_code}: {r.text[:200]}"

    def test_no_auth_gets_401(self):
        """Request without auth header should get 401."""
        url = f"{_gateway_url()}{MODEL_PATH}/v1/completions"
        r = requests.post(
            url,
            headers={"Content-Type": "application/json"},
            json={"model": MODEL_NAME, "prompt": "Hello", "max_tokens": 3},
            timeout=TIMEOUT,
            verify=False,
        )
        log.info(f"No auth -> {r.status_code}")
        assert r.status_code == 401, f"Expected 401, got {r.status_code}"

    def test_invalid_token_gets_401(self):
        """Garbage token should get 401."""
        r = _inference("totally-invalid-garbage-token")
        log.info(f"Invalid token -> {r.status_code}")
        assert r.status_code == 401, f"Expected 401, got {r.status_code}"

    def test_wrong_group_gets_403(self):
        """SA not in premium-user group should get 403 on premium model."""
        sa = "e2e-test-wrong-group"
        try:
            token = _create_sa_token(sa)
            r = _inference(token, path=PREMIUM_MODEL_PATH)
            log.info(f"Wrong group -> premium model: {r.status_code}")
            assert r.status_code == 403, f"Expected 403, got {r.status_code}"
        finally:
            _delete_sa(sa)


class TestSubscriptionEnforcement:
    """Tests that MaaSSubscription correctly enforces rate limits."""

    def test_subscribed_user_gets_200(self):
        token = _get_cluster_token()
        r = _inference(token)
        assert r.status_code == 200, f"Expected 200, got {r.status_code}: {r.text[:200]}"

    def test_auth_pass_no_subscription_gets_429(self):
        """SA granted auth but with no subscription should get 429."""
        sa = "e2e-test-no-sub"
        ns = _ns()
        try:
            token = _create_sa_token(sa, namespace=ns)
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {"name": "e2e-test-nosub-auth", "namespace": ns},
                "spec": {
                    "modelRefs": [PREMIUM_MODEL_REF],
                    "subjects": {"groups": [{"name": f"system:serviceaccounts:{ns}"}]},
                },
            })
            r = _poll_status(token, 429, path=PREMIUM_MODEL_PATH)
            log.info(f"Auth pass, no subscription -> {r.status_code}")
        finally:
            _delete_cr("maasauthpolicy", "e2e-test-nosub-auth")
            _delete_sa(sa, namespace=ns)

    def test_invalid_subscription_header_gets_429(self):
        token = _get_cluster_token()
        r = _inference(token, extra_headers={"x-maas-subscription": INVALID_SUBSCRIPTION})
        assert r.status_code == 429, f"Expected 429, got {r.status_code}"

    def test_explicit_subscription_header_works(self):
        token = _get_cluster_token()
        r = _inference(token, extra_headers={"x-maas-subscription": SIMULATOR_SUBSCRIPTION})
        assert r.status_code == 200, f"Expected 200, got {r.status_code}: {r.text[:200]}"


class TestMultipleSubscriptionsPerModel:
    """Multiple subscriptions for one model — user in ONE subscription should get access.

    Validates the fix for the bug where multiple subscriptions' when predicates
    were AND'd, requiring a user to be in ALL subscriptions.
    """

    def test_user_in_one_of_two_subscriptions_gets_200(self):
        """Add a 2nd subscription for a different group. User only in the original
        group should still get 200 (not blocked by the 2nd sub's group check)."""
        ns = _ns()
        try:
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": "e2e-extra-sub", "namespace": ns},
                "spec": {
                    "owner": {"groups": [{"name": "nonexistent-group-xyz"}]},
                    "modelRefs": [{"name": MODEL_REF, "tokenRateLimits": [{"limit": 999, "window": "1m"}]}],
                },
            })

            token = _get_cluster_token()
            r = _poll_status(token, 200)
            log.info(f"User in 1 of 2 subs -> {r.status_code}")
        finally:
            _delete_cr("maassubscription", "e2e-extra-sub")
            _wait_reconcile()

    def test_sa_selects_sub_it_does_not_belong_to_gets_429(self):
        """SA explicitly selects a subscription it's not a member of -> 429."""
        ns = _ns()
        sa = "e2e-test-wrongsub"
        try:
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": "e2e-premium-sub", "namespace": ns},
                "spec": {
                    "owner": {"groups": [{"name": "premium-user"}]},
                    "modelRefs": [{"name": MODEL_REF, "tokenRateLimits": [{"limit": 500, "window": "1m"}]}],
                },
            })
            token = _create_sa_token(sa)
            r = _poll_status(token, 429, extra_headers={"x-maas-subscription": "e2e-premium-sub"})
            log.info(f"SA selects wrong sub -> {r.status_code}")
        finally:
            _delete_cr("maassubscription", "e2e-premium-sub")
            _delete_sa(sa)
            _wait_reconcile()

    def test_multi_tier_auto_select_highest(self):
        """With 2 tiers for the same model, user in both should still get access.
        (Verifies multiple overlapping subscriptions don't break routing.)"""
        ns = _ns()
        try:
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": "e2e-high-tier", "namespace": ns},
                "spec": {
                    "owner": {"groups": [{"name": "system:authenticated"}]},
                    "modelRefs": [{"name": MODEL_REF, "tokenRateLimits": [{"limit": 9999, "window": "1m"}]}],
                },
            })

            token = _get_cluster_token()
            r = _poll_status(token, 200, extra_headers={"x-maas-subscription": "e2e-high-tier"})

            r2 = _inference(token)
            assert r2.status_code == 200, f"Expected 200 with auto-select, got {r2.status_code}"
        finally:
            _delete_cr("maassubscription", "e2e-high-tier")
            _wait_reconcile()


class TestMultipleAuthPoliciesPerModel:
    """Multiple auth policies for one model aggregate with OR logic."""

    def test_two_auth_policies_or_logic(self):
        """Two auth policies for the premium model: SA matching the 2nd should get access,
        and the original premium-user policy should still work for the admin."""
        ns = _ns()
        sa = "e2e-test-multiauth"
        try:
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {"name": "e2e-sa-auth", "namespace": ns},
                "spec": {
                    "modelRefs": [PREMIUM_MODEL_REF],
                    "subjects": {"groups": [{"name": f"system:serviceaccounts:{ns}"}]},
                },
            })
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": "e2e-sa-sub", "namespace": ns},
                "spec": {
                    "owner": {"groups": [{"name": f"system:serviceaccounts:{ns}"}]},
                    "modelRefs": [{"name": PREMIUM_MODEL_REF, "tokenRateLimits": [{"limit": 100, "window": "1m"}]}],
                },
            })
            token = _create_sa_token(sa)
            r = _poll_status(token, 200, path=PREMIUM_MODEL_PATH)
            log.info(f"SA with 2nd auth policy -> premium: {r.status_code}")

            # Original premium-user policy should still work for the cluster admin
            admin_token = _get_cluster_token()
            r2 = _inference(admin_token, path=PREMIUM_MODEL_PATH)
            assert r2.status_code == 200, f"Expected 200 (admin via original policy), got {r2.status_code}"
        finally:
            _delete_cr("maassubscription", "e2e-sa-sub")
            _delete_cr("maasauthpolicy", "e2e-sa-auth")
            _delete_sa(sa)
            _wait_reconcile()

    def test_delete_one_auth_policy_other_still_works(self):
        """Delete one of two auth policies for premium model -> remaining still works."""
        ns = _ns()
        try:
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {"name": "e2e-extra-auth", "namespace": ns},
                "spec": {
                    "modelRefs": [PREMIUM_MODEL_REF],
                    "subjects": {"groups": [{"name": "system:authenticated"}]},
                },
            })
            _wait_reconcile()

            _delete_cr("maasauthpolicy", "e2e-extra-auth")

            token = _get_cluster_token()
            r = _poll_status(token, 200, path=PREMIUM_MODEL_PATH)
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
                    "modelRefs": [{"name": MODEL_REF, "tokenRateLimits": [{"limit": 50, "window": "1m"}]}],
                },
            })
            _wait_reconcile()

            _delete_cr("maassubscription", "e2e-temp-sub")

            token = _get_cluster_token()
            r = _poll_status(token, 200)
        finally:
            _delete_cr("maassubscription", "e2e-temp-sub")

    def test_delete_last_subscription_falls_back_to_deny(self):
        """Delete all subscriptions for a model -> gateway default deny (429)."""
        token = _get_cluster_token()
        original = _snapshot_cr("maassubscription", SIMULATOR_SUBSCRIPTION)
        assert original, f"Pre-existing {SIMULATOR_SUBSCRIPTION} not found"
        try:
            _delete_cr("maassubscription", SIMULATOR_SUBSCRIPTION)
            r = _poll_status(token, 429, timeout=30)
            log.info(f"No subscriptions -> {r.status_code}")
        finally:
            _apply_cr(original)
            _wait_reconcile()

    def test_delete_last_auth_policy_falls_back_to_gateway_deny(self):
        """Delete the auth policy for a model -> gateway default auth (403)."""
        token = _get_cluster_token()
        original = _snapshot_cr("maasauthpolicy", SIMULATOR_ACCESS_POLICY)
        assert original, f"Pre-existing {SIMULATOR_ACCESS_POLICY} not found"
        try:
            _delete_cr("maasauthpolicy", SIMULATOR_ACCESS_POLICY)
            r = _poll_status(token, 403, timeout=30)
            log.info(f"No auth policy -> {r.status_code}")
        finally:
            _apply_cr(original)
            _wait_reconcile()


class TestOrderingEdgeCases:
    """Tests that resource creation order doesn't matter."""

    def test_subscription_before_auth_policy(self):
        """Create subscription first, then auth policy -> should work once both exist."""
        ns = _ns()
        sa = "e2e-test-ordering"
        try:
            token = _create_sa_token(sa)

            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": "e2e-ordering-sub", "namespace": ns},
                "spec": {
                    "owner": {"groups": [{"name": f"system:serviceaccounts:{ns}"}]},
                    "modelRefs": [{"name": PREMIUM_MODEL_REF, "tokenRateLimits": [{"limit": 100, "window": "1m"}]}],
                },
            })
            _wait_reconcile()

            r1 = _inference(token, path=PREMIUM_MODEL_PATH)
            log.info(f"Sub only (no auth policy) -> {r1.status_code}")
            assert r1.status_code == 403, f"Expected 403 (no auth policy yet), got {r1.status_code}"

            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {"name": "e2e-ordering-auth", "namespace": ns},
                "spec": {
                    "modelRefs": [PREMIUM_MODEL_REF],
                    "subjects": {"groups": [{"name": f"system:serviceaccounts:{ns}"}]},
                },
            })

            r2 = _poll_status(token, 200, path=PREMIUM_MODEL_PATH)
            log.info(f"Sub + auth policy -> {r2.status_code}")
        finally:
            _delete_cr("maassubscription", "e2e-ordering-sub")
            _delete_cr("maasauthpolicy", "e2e-ordering-auth")
            _delete_sa(sa)
            _wait_reconcile()
