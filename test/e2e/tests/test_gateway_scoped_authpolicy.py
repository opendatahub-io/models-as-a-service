"""
E2E tests for gateway-scoped AuthPolicy (MT S10 / #912).

Validates that MaaSAuthPolicy reconciliation produces a singleton
maas-gateway-auth policy targeting the Gateway (not per-model HTTPRoute policies).

Runs in default CI (no tenant namespace discovery required).
"""

import json
import uuid

import pytest

from multitenancy_helpers import (
    DEFAULT_GATEWAY_NAME,
    GATEWAY_AUTH_POLICY_NAME,
    GATEWAY_NAMESPACE,
    assert_no_per_model_authpolicy,
    get_gateway_authpolicy,
    get_gateway_authpolicy_target_ref,
    get_json_or_none,
)
from test_helper import (
    MODEL_NAMESPACE,
    MODEL_REF,
    _create_test_auth_policy,
    _delete_cr,
    _wait_for_maas_auth_policy_phase,
    _wait_reconcile,
)


def _gateway_auth_rego() -> str:
    ap = get_gateway_authpolicy()
    if not ap:
        return ""
    authorization = (
        ((ap.get("spec") or {}).get("defaults") or {})
        .get("rules", {})
        .get("authorization")
        or {}
    )
    membership = authorization.get("require-group-membership") or {}
    return (membership.get("opa") or {}).get("rego") or ""


class TestGatewayAuthPolicyStructure:
    """S10: AuthPolicy targets Gateway; no legacy per-model policies."""

    def test_target_ref_points_to_gateway(self):
        """6.1: maas-gateway-auth targetRef must be Gateway, not HTTPRoute."""
        ap = get_gateway_authpolicy()
        assert ap is not None, (
            f"{GATEWAY_AUTH_POLICY_NAME} must exist in {GATEWAY_NAMESPACE} after prow fixtures reconcile"
        )

        target = get_gateway_authpolicy_target_ref()
        assert target.get("kind") == "Gateway", f"expected Gateway targetRef, got {target!r}"
        assert target.get("group") == "gateway.networking.k8s.io"
        assert target.get("name") == DEFAULT_GATEWAY_NAME
        target_ns = target.get("namespace") or GATEWAY_NAMESPACE
        assert target_ns == GATEWAY_NAMESPACE, f"expected gateway namespace {GATEWAY_NAMESPACE}, got {target_ns!r}"

        conditions = (ap.get("status") or {}).get("conditions") or []
        accepted = [c for c in conditions if c.get("type") == "Accepted"]
        assert accepted and accepted[0].get("status") == "True", (
            f"{GATEWAY_AUTH_POLICY_NAME} must be Accepted, got {conditions!r}"
        )

    def test_no_per_model_authpolicy_for_fixture_model(self):
        """6.2: Gateway-only mode must not create maas-auth-{model} in model namespace."""
        assert_no_per_model_authpolicy(MODEL_REF, MODEL_NAMESPACE)


class TestGatewayAuthPolicyLifecycle:
    """S10: Gateway auth is reconciled from MaaSAuthPolicy changes."""

    def test_gateway_auth_embeds_model_allowlist(self):
        """6.3: Aggregated subject allowlists appear in gateway auth rego."""
        suffix = uuid.uuid4().hex[:8]
        policy_name = f"e2e-gw-auth-{suffix}"
        unique_group = f"e2e-gw-group-{suffix}"

        try:
            _create_test_auth_policy(policy_name, MODEL_REF, groups=[unique_group])
            _wait_for_maas_auth_policy_phase(policy_name, timeout=120, require_auth_policies=False)

            rego = _gateway_auth_rego()
            assert unique_group in rego, (
                f"expected gateway auth rego to include group {unique_group!r}"
            )
            assert_no_per_model_authpolicy(MODEL_REF, MODEL_NAMESPACE)
        finally:
            _delete_cr("maasauthpolicy", policy_name)
            _wait_reconcile()

    def test_only_one_gateway_authpolicy_named_maas_gateway_auth(self):
        """6.2: Exactly one maas-gateway-auth exists in the gateway namespace."""
        ap = get_gateway_authpolicy()
        assert ap is not None

        from multitenancy_helpers import _oc_run

        result = _oc_run(
            [
                "get",
                "authpolicy",
                "-n",
                GATEWAY_NAMESPACE,
                "-l",
                "app.kubernetes.io/part-of=maas-gateway-auth",
                "-o",
                "json",
            ]
        )
        if result.returncode != 0:
            pytest.fail(result.stderr.strip() or result.stdout.strip())

        items = json.loads(result.stdout).get("items") or []
        names = [item.get("metadata", {}).get("name") for item in items]
        assert GATEWAY_AUTH_POLICY_NAME in names
        assert len(items) == 1, f"expected one gateway auth policy, got {names!r}"


class TestGatewayAuthPolicyManagementEndpointAccess:
    """Verify gateway auth allows management endpoints without model context.

    The gateway-level AuthPolicy must allow requests to management endpoints
    (/v1/api-keys, /v1/subscriptions, /v1/models, /maas-api/*) even when no
    model identity is present. This prevents the API Keys page from 403-ing
    on clusters with zero subscriptions.
    """

    def test_gateway_auth_rego_allows_empty_model_identity(self):
        """OPA rego must allow requests where model_identity is empty (management endpoints)."""
        rego = _gateway_auth_rego()
        assert rego, "gateway auth rego must not be empty"
        assert 'model_identity == ""' in rego, (
            "gateway auth rego must contain an allow rule for empty model_identity "
            "(management endpoints like /v1/api-keys, /maas-api/*). "
            f"Got rego:\n{rego}"
        )

    def test_gateway_auth_subscription_check_gated_by_model_identity(self):
        """subscription-valid authorization must only run when a model is targeted."""
        ap = get_gateway_authpolicy()
        assert ap is not None

        defaults = (ap.get("spec") or {}).get("defaults") or {}
        authorization = defaults.get("rules", {}).get("authorization") or {}
        sub_valid = authorization.get("subscription-valid") or {}
        when_list = sub_valid.get("when") or []
        assert len(when_list) > 0, (
            "subscription-valid must have a 'when' predicate so it only runs for "
            "model inference, not management endpoints"
        )
        predicate = when_list[0].get("predicate", "")
        assert 'request.path.split' in predicate and 'x-gateway-model-name' in predicate, (
            "subscription-valid 'when' predicate must use the model-identity CEL expression "
            f"(path-based + header-based check), got: {predicate}"
        )

    def test_gateway_default_auth_scoped_if_present(self):
        """If gateway-default-auth exists, it must scope deny-all to model paths only."""
        default_auth = get_json_or_none(
            "authpolicy", "gateway-default-auth", GATEWAY_NAMESPACE
        )
        if default_auth is None:
            pytest.skip(
                "gateway-default-auth not present (maas-gateway-auth is active); "
                "scoping is validated by unit tests"
            )

        defaults = (default_auth.get("spec") or {}).get("defaults") or {}
        when_list = defaults.get("when") or []
        assert len(when_list) > 0, (
            "gateway-default-auth must have a 'when' predicate to exclude "
            "management endpoints (/v1/*, /maas-api/*) from deny-all"
        )
        predicate = when_list[0].get("predicate", "")
        assert predicate, "gateway-default-auth 'when' predicate must not be empty"
        assert 'request.path.split' in predicate, (
            "gateway-default-auth predicate must use path-based model identity CEL, "
            f"got: {predicate}"
        )
        assert '"v1"' in predicate and '"maas-api"' in predicate, (
            "gateway-default-auth predicate must exclude /v1/* and /maas-api/* paths "
            f"via CEL expression, got: {predicate}"
        )
        assert 'x-gateway-model-name' in predicate, (
            "gateway-default-auth predicate must include header-based model identity check, "
            f"got: {predicate}"
        )
