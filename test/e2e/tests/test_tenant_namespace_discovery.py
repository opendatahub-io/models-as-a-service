"""
E2E tests for tenant namespace discovery (MT S1 / RHOAIENG-62761).

Validates maas-controller behavior when --enable-tenant-namespace-discovery=true:
  - Labeled tenant namespaces are reconciled
  - Unlabeled namespaces are ignored
  - Label removal stops reconciliation
  - Per-tenant OIDC from namespace-local Tenant/default-tenant
  - Namespace-qualified policy contributor tracking
  - Webhook rejects CRs without Tenant CR (MT S6 / RHOAIENG-62766)

Requires:
  - AITenant CRD (S11)
  - maas-controller with --enable-tenant-namespace-discovery=true
  - LLMInferenceService + MaaSModelRef for MODEL_REF in MODEL_NAMESPACE (prow fixtures)
  - oc access

Environment variables:
  ENABLE_TENANT_NAMESPACE_DISCOVERY - when "true", require the controller discovery flag
  See multitenancy_helpers.py and test_helper.py for namespace/gateway defaults.
"""

import os
import uuid

import pytest

from multitenancy_helpers import (
    AITENANT_NAMESPACE,
    DEFAULT_GATEWAY_NAME,
    FINALIZER_AUTHPOLICY,
    FINALIZER_SUBSCRIPTION,
    GATEWAY_AUTH_POLICY_NAME,
    GATEWAY_NAMESPACE,
    apply_discovery_labels,
    apply_gateway_fixture,
    apply_maas_auth_policy,
    apply_maas_subscription,
    apply_tenant_cr,
    assert_no_per_model_authpolicy,
    auth_can_create_maassubscription,
    bootstrap_aitenant_tenant,
    cleanup_discovery_case,
    controller_has_tenant_namespace_discovery,
    delete_maas_auth_policy,
    delete_maas_subscription,
    delete_namespace_best_effort,
    ensure_namespace,
    get_gateway_authpolicy,
    get_gateway_authpolicy_issuer,
    get_json_or_none,
    legacy_default_namespace,
    new_discovery_case,
    patch_controller_tenant_namespace_discovery,
    remove_discovery_labels,
    require_aitenant_crd,
    require_tenant_namespace_discovery,
    wait_for_annotation_contains,
    wait_for_finalizer,
    wait_for_json,
    wait_for_status_phase,
    _create_expect_failure,
    _oc_run,
)
from test_helper import MODEL_NAMESPACE, MODEL_REF, _wait_for_maas_auth_policy_phase, _wait_reconcile


@pytest.fixture(scope="module", autouse=True)
def _require_multitenancy_prerequisites():
    require_tenant_namespace_discovery()
    require_aitenant_crd()
    wait_for_json("namespace", AITENANT_NAMESPACE, timeout=180, interval=5)


class TestTenantNamespaceDiscovery:
    """S27 section 1 — tenant namespace discovery (S1 / PR #975)."""

    def test_labeled_tenant_namespace_is_discovered(self):
        """1.1: Controller discovers labeled tenant namespaces and reconciles CRs."""
        case = new_discovery_case(use_default_gateway=True)
        try:
            apply_discovery_labels(case["tenant_ns"], case["tenant_label_name"])
            apply_tenant_cr(case["tenant_ns"], DEFAULT_GATEWAY_NAME)
            apply_maas_auth_policy(case["policy_name"], case["tenant_ns"])
            apply_maas_subscription(case["subscription_name"], case["tenant_ns"])

            wait_for_finalizer("maasauthpolicy", case["policy_name"], case["tenant_ns"], FINALIZER_AUTHPOLICY)
            wait_for_finalizer("maassubscription", case["subscription_name"], case["tenant_ns"], FINALIZER_SUBSCRIPTION)
            auth = wait_for_status_phase(
                "maasauthpolicy",
                case["policy_name"],
                case["tenant_ns"],
                expected_phase="Active",
            )
            sub = wait_for_status_phase(
                "maassubscription",
                case["subscription_name"],
                case["tenant_ns"],
                expected_phase=("Active", "Degraded"),
            )
            assert auth.get("status", {}).get("phase") == "Active"
            assert sub.get("status", {}).get("phase") in ("Active", "Degraded")
        finally:
            cleanup_discovery_case(case, delete_gateway=False)

    def test_label_removal_stops_reconciliation(self):
        """1.2: Removing discovery labels stops new reconciliation activity."""
        case = new_discovery_case(use_default_gateway=True)
        try:
            apply_discovery_labels(case["tenant_ns"], case["tenant_label_name"])
            apply_tenant_cr(case["tenant_ns"], DEFAULT_GATEWAY_NAME)
            apply_maas_auth_policy(case["policy_name"], case["tenant_ns"])
            wait_for_finalizer("maasauthpolicy", case["policy_name"], case["tenant_ns"], FINALIZER_AUTHPOLICY)

            remove_discovery_labels(case["tenant_ns"])
            _wait_reconcile(10)

            new_policy = f"e2e-post-label-{case['suffix']}"
            apply_maas_auth_policy(new_policy, case["tenant_ns"])
            _wait_reconcile(15)

            obj = get_json_or_none("maasauthpolicy", new_policy, case["tenant_ns"])
            assert obj is not None
            finalizers = (obj.get("metadata") or {}).get("finalizers") or []
            assert FINALIZER_AUTHPOLICY not in finalizers, (
                "CR created after label removal should not receive controller finalizer"
            )
            assert not (obj.get("status") or {}).get("phase"), (
                "CR created after label removal should not be reconciled"
            )
        finally:
            delete_maas_auth_policy(f"e2e-post-label-{case['suffix']}", case["tenant_ns"])
            cleanup_discovery_case(case, delete_gateway=False)

    def test_unlabeled_namespace_ignored(self):
        """1.3: CRs in namespaces without discovery labels are ignored (Tenant CR satisfies webhook)."""
        suffix = uuid.uuid4().hex[:8]
        unlabeled_ns = f"e2e-unlabeled-{suffix}"
        policy_name = f"e2e-unlabeled-policy-{suffix}"
        sub_name = f"e2e-unlabeled-sub-{suffix}"
        try:
            ensure_namespace(unlabeled_ns)
            apply_tenant_cr(unlabeled_ns, DEFAULT_GATEWAY_NAME)
            apply_maas_auth_policy(policy_name, unlabeled_ns)
            apply_maas_subscription(sub_name, unlabeled_ns)
            _wait_reconcile(15)

            auth = get_json_or_none("maasauthpolicy", policy_name, unlabeled_ns)
            sub = get_json_or_none("maassubscription", sub_name, unlabeled_ns)
            assert auth is not None and sub is not None
            assert not (auth.get("metadata") or {}).get("finalizers"), "unlabeled namespace auth policy should have no finalizers"
            assert not (sub.get("metadata") or {}).get("finalizers"), "unlabeled namespace subscription should have no finalizers"
            assert not (auth.get("status") or {}).get("phase")
            assert not (sub.get("status") or {}).get("phase")
        finally:
            delete_maas_auth_policy(policy_name, unlabeled_ns)
            delete_maas_subscription(sub_name, unlabeled_ns)
            delete_namespace_best_effort(unlabeled_ns)

    def test_dynamic_discovery_after_label_added(self):
        """1.2 variant: Pre-existing CRs reconcile after namespace gains discovery labels."""
        case = new_discovery_case(use_default_gateway=True)
        try:
            ensure_namespace(case["tenant_ns"])
            apply_tenant_cr(case["tenant_ns"], DEFAULT_GATEWAY_NAME)
            apply_maas_auth_policy(case["policy_name"], case["tenant_ns"])
            _wait_reconcile(10)
            before = get_json_or_none("maasauthpolicy", case["policy_name"], case["tenant_ns"])
            assert before is not None
            assert FINALIZER_AUTHPOLICY not in ((before.get("metadata") or {}).get("finalizers") or [])

            apply_discovery_labels(case["tenant_ns"], case["tenant_label_name"])
            wait_for_finalizer("maasauthpolicy", case["policy_name"], case["tenant_ns"], FINALIZER_AUTHPOLICY)
            wait_for_status_phase(
                "maasauthpolicy",
                case["policy_name"],
                case["tenant_ns"],
                expected_phase="Active",
            )
        finally:
            cleanup_discovery_case(case, delete_gateway=False)

    def test_per_tenant_oidc_configuration(self):
        """1.4: Gateway-scoped maas-gateway-auth issuerUrl reflects Tenant externalOIDC (#912)."""
        if os.environ.get("EXTERNAL_OIDC") != "true" or not os.environ.get("OIDC_ISSUER_URL"):
            pytest.skip("OIDC_ISSUER_URL not set; per-tenant OIDC E2E requires external OIDC deploy")

        issuer = os.environ["OIDC_ISSUER_URL"]
        case = new_discovery_case(use_default_gateway=True)

        try:
            apply_discovery_labels(case["tenant_ns"], case["tenant_label_name"])
            apply_tenant_cr(
                case["tenant_ns"],
                DEFAULT_GATEWAY_NAME,
                external_oidc={"issuerUrl": issuer, "clientId": os.environ.get("OIDC_CLIENT_ID", "test-client")},
            )
            apply_maas_auth_policy(case["policy_name"], case["tenant_ns"])
            _wait_for_maas_auth_policy_phase(case["policy_name"], namespace=case["tenant_ns"], timeout=180)

            gw_auth = get_gateway_authpolicy()
            assert gw_auth is not None, f"{GATEWAY_AUTH_POLICY_NAME} should exist in {GATEWAY_NAMESPACE}"
            assert_no_per_model_authpolicy(MODEL_REF, MODEL_NAMESPACE)

            live_issuer = get_gateway_authpolicy_issuer()
            assert live_issuer.rstrip("/") == issuer.rstrip("/"), (
                f"expected gateway AuthPolicy issuer {issuer}, got {live_issuer!r}"
            )
        finally:
            cleanup_discovery_case(case, delete_gateway=False)

    def test_namespace_qualified_collision_prevention(self):
        """1.5: Same-named CRs in two tenant namespaces use namespace-qualified TRLP tracking."""
        case_a = new_discovery_case(use_default_gateway=True)
        case_b = new_discovery_case(use_default_gateway=True)
        shared_policy_name = f"e2e-shared-policy-{case_a['suffix']}"
        shared_sub_name = f"e2e-shared-sub-{case_a['suffix']}"
        case_a["policy_name"] = shared_policy_name
        case_a["subscription_name"] = shared_sub_name
        case_b["policy_name"] = shared_policy_name
        case_b["subscription_name"] = shared_sub_name

        try:
            for case in (case_a, case_b):
                apply_discovery_labels(case["tenant_ns"], case["tenant_label_name"])
                apply_tenant_cr(case["tenant_ns"], DEFAULT_GATEWAY_NAME)
                apply_maas_auth_policy(shared_policy_name, case["tenant_ns"])
                apply_maas_subscription(shared_sub_name, case["tenant_ns"])
                wait_for_finalizer("maasauthpolicy", shared_policy_name, case["tenant_ns"], FINALIZER_AUTHPOLICY)
                wait_for_finalizer("maassubscription", shared_sub_name, case["tenant_ns"], FINALIZER_SUBSCRIPTION)
                _wait_for_maas_auth_policy_phase(shared_policy_name, namespace=case["tenant_ns"], timeout=120)

            assert_no_per_model_authpolicy(MODEL_REF, MODEL_NAMESPACE)
            assert get_gateway_authpolicy() is not None

            expected_a_sub = f"{case_a['tenant_ns']}/{shared_sub_name}"
            expected_b_sub = f"{case_b['tenant_ns']}/{shared_sub_name}"
            sub_contributors = wait_for_annotation_contains(
                "tokenratelimitpolicy",
                f"maas-trlp-{MODEL_REF}",
                MODEL_NAMESPACE,
                "maas.opendatahub.io/subscriptions",
                [expected_a_sub, expected_b_sub],
            )

            assert expected_a_sub in sub_contributors, f"missing {expected_a_sub} in {sub_contributors}"
            assert expected_b_sub in sub_contributors, f"missing {expected_b_sub} in {sub_contributors}"
        finally:
            cleanup_discovery_case(case_a, delete_gateway=False)
            cleanup_discovery_case(case_b, delete_gateway=False)

    def test_tenant_admin_rbac_is_namespace_scoped(self):
        """1.6: Tenant-admin Role from AITenant bootstrap is scoped to the tenant namespace."""
        case = new_discovery_case()
        sa_name = f"e2e-mt-rbac-{case['suffix']}"
        role_name = f"aitenant-{case['tenant_label_name']}-tenant-admin"
        other_ns = f"e2e-mt-other-{case['suffix']}"
        try:
            apply_gateway_fixture(case["gateway_name"], fixture_label=case["tenant_label_name"])
            bootstrap_aitenant_tenant(case)

            role = get_json_or_none("role", role_name, case["tenant_ns"])
            binding = get_json_or_none("rolebinding", role_name, case["tenant_ns"])
            assert role is not None, "tenant-admin Role should exist in tenant namespace"
            assert binding is None, "AITenant should not create tenant-admin RoleBindings"
            rules = role.get("rules") or []
            assert any("maassubscriptions" in (rule.get("resources") or []) for rule in rules)

            sa_result = _oc_run(["create", "sa", sa_name, "-n", case["tenant_ns"]])
            if sa_result.returncode != 0 and "already exists" not in (sa_result.stderr or "").lower():
                raise RuntimeError(sa_result.stderr.strip() or sa_result.stdout.strip())
            rb_result = _oc_run(
                [
                    "create",
                    "rolebinding",
                    f"{sa_name}-binding",
                    f"--role={role_name}",
                    f"--serviceaccount={case['tenant_ns']}:{sa_name}",
                    "-n",
                    case["tenant_ns"],
                ]
            )
            if rb_result.returncode != 0 and "already exists" not in (rb_result.stderr or "").lower():
                raise RuntimeError(rb_result.stderr.strip() or rb_result.stdout.strip())

            sa_user = f"system:serviceaccount:{case['tenant_ns']}:{sa_name}"
            ensure_namespace(other_ns)
            apply_tenant_cr(other_ns, DEFAULT_GATEWAY_NAME)

            assert auth_can_create_maassubscription(sa_user, case["tenant_ns"]), (
                f"tenant-admin SA should manage subscriptions in its tenant namespace"
            )
            assert not auth_can_create_maassubscription(sa_user, other_ns), (
                f"tenant-admin SA should not manage subscriptions in another tenant namespace"
            )
        finally:
            delete_namespace_best_effort(other_ns)
            _oc_run(["delete", "rolebinding", f"{sa_name}-binding", "-n", case["tenant_ns"], "--ignore-not-found"])
            _oc_run(["delete", "sa", sa_name, "-n", case["tenant_ns"], "--ignore-not-found"])
            cleanup_discovery_case(case)


class TestTenantWebhookValidation:
    """S6 webhook — reject MaaSSubscription / MaaSAuthPolicy without Tenant CR (PR #942)."""

    def test_maassubscription_rejected_without_tenant_cr(self):
        suffix = uuid.uuid4().hex[:8]
        ns = f"e2e-webhook-sub-{suffix}"
        try:
            ensure_namespace(ns)
            output = _create_expect_failure(
                {
                    "apiVersion": "maas.opendatahub.io/v1alpha1",
                    "kind": "MaaSSubscription",
                    "metadata": {"name": f"e2e-webhook-sub-{suffix}", "namespace": ns},
                    "spec": {
                        "owner": {"groups": [{"name": "system:authenticated"}]},
                        "modelRefs": [
                            {
                                "name": MODEL_REF,
                                "namespace": MODEL_NAMESPACE,
                                "tokenRateLimits": [{"limit": 1, "window": "1m"}],
                            }
                        ],
                    },
                }
            )
            assert "not enabled for MaaS tenant resources" in output or "denied" in output.lower()
        finally:
            delete_namespace_best_effort(ns)

    def test_maasauthpolicy_rejected_without_tenant_cr(self):
        suffix = uuid.uuid4().hex[:8]
        ns = f"e2e-webhook-auth-{suffix}"
        try:
            ensure_namespace(ns)
            output = _create_expect_failure(
                {
                    "apiVersion": "maas.opendatahub.io/v1alpha1",
                    "kind": "MaaSAuthPolicy",
                    "metadata": {"name": f"e2e-webhook-auth-{suffix}", "namespace": ns},
                    "spec": {
                        "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE}],
                        "subjects": {"groups": [{"name": "system:authenticated"}]},
                    },
                }
            )
            assert "not enabled for MaaS tenant resources" in output or "denied" in output.lower()
        finally:
            delete_namespace_best_effort(ns)


class TestTenantDiscoveryDormantMode:
    """Verify dormant mode when discovery flag is disabled (regression guard)."""

    def test_dormant_mode_ignores_labeled_namespace(self):
        if os.environ.get("ENABLE_TENANT_DISCOVERY_DORMANT_E2E", "").lower() != "true":
            pytest.skip("Dormant-mode test mutates controller flags; set ENABLE_TENANT_DISCOVERY_DORMANT_E2E=true")

        if not controller_has_tenant_namespace_discovery():
            pytest.skip("Controller already in dormant mode")

        case = new_discovery_case(use_default_gateway=True)
        policy_name = f"e2e-dormant-{case['suffix']}"
        try:
            apply_discovery_labels(case["tenant_ns"], case["tenant_label_name"])
            apply_tenant_cr(case["tenant_ns"], DEFAULT_GATEWAY_NAME)

            patch_controller_tenant_namespace_discovery(enabled=False)
            apply_maas_auth_policy(policy_name, case["tenant_ns"])
            _wait_reconcile(15)

            obj = get_json_or_none("maasauthpolicy", policy_name, case["tenant_ns"])
            assert obj is not None
            assert FINALIZER_AUTHPOLICY not in ((obj.get("metadata") or {}).get("finalizers") or [])
            assert not (obj.get("status") or {}).get("phase")
        finally:
            delete_maas_auth_policy(policy_name, case["tenant_ns"])
            delete_namespace_best_effort(case["tenant_ns"])
            patch_controller_tenant_namespace_discovery(enabled=True)


class TestLegacyDefaultNamespaceStillWorks:
    """Default tenant namespace continues to reconcile with discovery enabled."""

    def test_models_as_a_service_namespace_reconciles(self):
        ns = legacy_default_namespace()
        suffix = uuid.uuid4().hex[:8]
        policy_name = f"e2e-default-ns-{suffix}"
        try:
            apply_maas_auth_policy(policy_name, ns)
            wait_for_finalizer("maasauthpolicy", policy_name, ns, FINALIZER_AUTHPOLICY)
            wait_for_status_phase("maasauthpolicy", policy_name, ns, expected_phase="Active")
        finally:
            delete_maas_auth_policy(policy_name, ns)
