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
  - E2E_SIMULATOR_SUBSCRIPTION: Free-tier subscription (default: simulator-subscription)
  - E2E_PREMIUM_SIMULATOR_SUBSCRIPTION: Premium-tier subscription (default: premium-simulator-subscription)
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
UNCONFIGURED_MODEL_REF = os.environ.get("E2E_UNCONFIGURED_MODEL_REF", "e2e-unconfigured-facebook-opt-125m-simulated")
UNCONFIGURED_MODEL_PATH = os.environ.get("E2E_UNCONFIGURED_MODEL_PATH", "/llm/e2e-unconfigured-facebook-opt-125m-simulated")
SIMULATOR_SUBSCRIPTION = os.environ.get("E2E_SIMULATOR_SUBSCRIPTION", "simulator-subscription")
PREMIUM_SIMULATOR_SUBSCRIPTION = os.environ.get(
    "E2E_PREMIUM_SIMULATOR_SUBSCRIPTION", "premium-simulator-subscription"
)
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

# TODO: Move to helpers.sh and come up witha better way of creating a preimum token
def _get_cluster_token():
    # TODO: Consider always using SA for consistency; remove oc whoami -t fallback.
    sa_ns = os.environ.get("E2E_TEST_TOKEN_SA_NAMESPACE")
    sa_name = os.environ.get("E2E_TEST_TOKEN_SA_NAME")
    return _create_sa_token(sa_name, namespace=sa_ns)


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


def _subscription_for_path(path):
    """Return the X-MaaS-Subscription value for a given model path."""
    path = path or MODEL_PATH
    if path == PREMIUM_MODEL_PATH:
        return PREMIUM_SIMULATOR_SUBSCRIPTION
    if path == MODEL_PATH:
        return SIMULATOR_SUBSCRIPTION
    return None  # e.g. unconfigured model has no subscription


def _inference(token, path=None, extra_headers=None, subscription=None):
    path = path or MODEL_PATH
    url = f"{_gateway_url()}{path}/v1/completions"
    headers = {"Authorization": f"Bearer {token}", "Content-Type": "application/json"}
    # Add X-MaaS-Subscription: extra_headers overrides; else explicit subscription; else infer from path.
    # Pass subscription=False to explicitly omit the header (e.g. when testing no-subscription case).
    sub_header = "x-maas-subscription"
    if extra_headers and sub_header in extra_headers:
        pass  # extra_headers will set it
    elif subscription is False:
        pass  # explicitly omit
    elif subscription is not None:
        headers[sub_header] = subscription
    else:
        inferred = _subscription_for_path(path)
        if inferred:
            headers[sub_header] = inferred
    if extra_headers:
        headers.update(extra_headers)
    return requests.post(
        url, headers=headers,
        json={"model": MODEL_NAME, "prompt": "Hello", "max_tokens": 3},
        timeout=TIMEOUT, verify=False,
    )


def _wait_reconcile(seconds=None):
    time.sleep(seconds or RECONCILE_WAIT)


def _poll_status(token, expected, path=None, extra_headers=None, subscription=None, timeout=None, poll_interval=2):
    """Poll inference endpoint until expected HTTP status or timeout."""
    timeout = timeout or max(RECONCILE_WAIT * 3, 60)
    deadline = time.time() + timeout
    last = None
    last_err = None
    while time.time() < deadline:
        try:
            r = _inference(token, path=path, extra_headers=extra_headers, subscription=subscription)
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
        err_msg += f", no response (all requests may have raised non-RequestException)"
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


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

class TestAuthEnforcement:
    """Tests that MaaSAuthPolicy correctly enforces access."""

    def test_authorized_user_gets_200(self):
        """Admin user (in system:authenticated) should access the free model.
        Polls because AuthPolicies may still be syncing with Authorino."""
        token = _get_cluster_token()
        r = _poll_status(token, 200, timeout=90)
        log.info(f"Authorized user -> {r.status_code}")

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
        """Subscribed user should access the model. Polls for AuthPolicy enforcement."""
        token = _get_cluster_token()
        r = _poll_status(token, 200, timeout=90)
        log.info(f"Subscribed user -> {r.status_code}")

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
            _wait_reconcile()  # allow auth policy to propagate before polling
            r = _poll_status(token, 429, path=PREMIUM_MODEL_PATH, subscription=False)
            log.info(f"Auth pass, no subscription -> {r.status_code}")
        finally:
            _delete_cr("maasauthpolicy", "e2e-test-nosub-auth")
            _delete_sa(sa, namespace=ns)

    def test_invalid_subscription_header_gets_429(self):
        token = _get_cluster_token()
        r = _inference(token, extra_headers={"x-maas-subscription": INVALID_SUBSCRIPTION})
        # Gateway may return 429 (rate limited) or 403 (forbidden) for invalid subscription
        assert r.status_code in (429, 403), f"Expected 429 or 403, got {r.status_code}"

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
        sa_user = f"system:serviceaccount:{ns}:{sa}"
        try:
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {"name": "e2e-premium-sa-auth", "namespace": ns},
                "spec": {
                    "modelRefs": [PREMIUM_MODEL_REF],
                    "subjects": {"users": [sa_user], "groups": []},
                },
            })
            _apply_cr({
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {"name": "e2e-premium-sa-sub", "namespace": ns},
                "spec": {
                    "owner": {"users": [sa_user], "groups": []},
                    "modelRefs": [{"name": PREMIUM_MODEL_REF, "tokenRateLimits": [{"limit": 100, "window": "1m"}]}],
                },
            })
            token = _create_sa_token(sa)
            r = _poll_status(token, 200, path=PREMIUM_MODEL_PATH, subscription="e2e-premium-sa-sub")
            log.info(f"SA with 2nd auth policy -> premium: {r.status_code}")

            # Original premium-user policy should still work for the cluster admin
            admin_token = _get_cluster_token()
            r2 = _inference(admin_token, path=PREMIUM_MODEL_PATH)  # uses premium-simulator-subscription
            assert r2.status_code == 200, f"Expected 200 (admin via original policy), got {r2.status_code}"
        finally:
            _delete_cr("maassubscription", "e2e-premium-sa-sub")
            _delete_cr("maasauthpolicy", "e2e-premium-sa-auth")
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

    def test_delete_last_subscription_allows_unrestricted(self):
        """Delete all subscriptions for a model -> no rate limit, auth still passes (200)."""
        token = _get_cluster_token()
        original = _snapshot_cr("maassubscription", SIMULATOR_SUBSCRIPTION)
        assert original, f"Pre-existing {SIMULATOR_SUBSCRIPTION} not found"
        try:
            _delete_cr("maassubscription", SIMULATOR_SUBSCRIPTION)
            r = _poll_status(token, 200, subscription=False, timeout=30)  # no sub header: sub was deleted
            log.info(f"No subscriptions -> {r.status_code}")
        finally:
            _apply_cr(original)
            _wait_reconcile()

    def test_unconfigured_model_denied_by_gateway_auth(self):
        """New model with no MaaSAuthPolicy/MaaSSubscription -> gateway default auth denies (403)."""
        token = _get_cluster_token()
        r = _inference(token, path=UNCONFIGURED_MODEL_PATH)
        log.info(f"Unconfigured model (no auth policy) -> {r.status_code}")
        assert r.status_code == 403, f"Expected 403 (gateway default deny), got {r.status_code}"


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

            r1 = _inference(token, path=PREMIUM_MODEL_PATH, subscription="e2e-ordering-sub")
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

            r2 = _poll_status(token, 200, path=PREMIUM_MODEL_PATH, subscription="e2e-ordering-sub")
            log.info(f"Sub + auth policy -> {r2.status_code}")
        finally:
            _delete_cr("maassubscription", "e2e-ordering-sub")
            _delete_cr("maasauthpolicy", "e2e-ordering-auth")
            _delete_sa(sa)
            _wait_reconcile()
