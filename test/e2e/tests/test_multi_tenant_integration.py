"""
Multi-tenant integration E2E scenarios for Phase 1.

These tests compose the S1/S10/S11 behavior:
  - Full tenant lifecycle from AITenant create to delete
  - Default tenant reconciliation remains intact
  - Same-named resources across tenant namespaces do not collide
  - Namespace discovery label changes trigger reconciliation

Requires maas-controller with --enable-tenant-namespace-discovery=true.
"""

import uuid

import pytest

from multitenancy_helpers import (
    ANNOTATION_AITENANT_NAME,
    ANNOTATION_AITENANT_NAMESPACE,
    AITENANT_KIND,
    AITENANT_NAMESPACE,
    DEFAULT_GATEWAY_NAME,
    FINALIZER_AUTHPOLICY,
    FINALIZER_SUBSCRIPTION,
    LABEL_MANAGED_BY_AITENANT,
    LABEL_TENANT_NAME,
    LABEL_TENANT_NAMESPACE,
    MODEL_NAMESPACE,
    MODEL_REF,
    TENANT_CR_NAME,
    TENANT_CONFIG_KIND,
    apply_discovery_labels,
    apply_maas_auth_policy,
    apply_maas_subscription,
    apply_tenant_cr,
    bootstrap_aitenant_tenant,
    cleanup_discovery_case,
    delete_best_effort,
    delete_maas_auth_policy,
    delete_maas_subscription,
    ensure_namespace,
    get_json_or_none,
    legacy_default_namespace,
    new_discovery_case,
    provision_tenant_model,
    remove_discovery_labels,
    require_aitenant_crd,
    require_tenant_namespace_discovery,
    wait_for_aitenant_cleanup_resources,
    wait_for_aitenant_cleanup_resources_deleted,
    wait_for_annotation_contains,
    wait_for_finalizer,
    wait_for_json,
    wait_for_not_found,
    wait_for_status_phase,
)
from test_helper import _wait_reconcile


@pytest.fixture(scope="module", autouse=True)
def _require_multitenancy_prerequisites():
    require_tenant_namespace_discovery()
    require_aitenant_crd()
    wait_for_json("namespace", AITENANT_NAMESPACE, timeout=180, interval=5)


class TestMultiTenantIntegration:
    """S27 section 7 — multi-tenant integration scenarios."""

    def test_full_tenant_lifecycle_create_to_delete(self):
        """7.1: Full tenant lifecycle from create through policy/subscription reconcile to delete."""
        case = new_discovery_case()
        role_name = f"aitenant-{case['tenant_label_name']}-tenant-admin"
        try:
            bootstrap_aitenant_tenant(case)

            tenant = wait_for_json(TENANT_CONFIG_KIND, TENANT_CR_NAME, case["tenant_ns"], timeout=180)
            tenant_labels = tenant["metadata"].get("labels") or {}
            tenant_annotations = tenant["metadata"].get("annotations") or {}
            assert tenant_labels[LABEL_MANAGED_BY_AITENANT] == "true"
            assert tenant_labels[LABEL_TENANT_NAME] == case["tenant_label_name"]
            assert tenant_labels[LABEL_TENANT_NAMESPACE] == case["tenant_ns"]
            assert tenant_annotations[ANNOTATION_AITENANT_NAME] == case["tenant_label_name"]
            assert tenant_annotations[ANNOTATION_AITENANT_NAMESPACE] == AITENANT_NAMESPACE
            aitenant = wait_for_json(AITENANT_KIND, case["tenant_label_name"], AITENANT_NAMESPACE, timeout=180)
            assert aitenant["status"]["gatewayRef"]["name"] == case["gateway_name"]
            assert get_json_or_none("role", role_name, case["tenant_ns"]) is not None

            model_name = f"e2e-lifecycle-model-{case['suffix']}"
            provision_tenant_model(model_name, case["tenant_ns"], case["gateway_name"])

            apply_maas_auth_policy(
                case["policy_name"],
                case["tenant_ns"],
                model_ref=model_name,
                model_namespace=case["tenant_ns"],
            )
            apply_maas_subscription(
                case["subscription_name"],
                case["tenant_ns"],
                model_ref=model_name,
                model_namespace=case["tenant_ns"],
            )
            wait_for_status_phase("maasauthpolicy", case["policy_name"], case["tenant_ns"], expected_phase="Active")
            wait_for_status_phase(
                "maassubscription",
                case["subscription_name"],
                case["tenant_ns"],
                expected_phase="Active",
            )
            wait_for_aitenant_cleanup_resources(case)

            delete_best_effort(AITENANT_KIND, case["tenant_label_name"], AITENANT_NAMESPACE, timeout="180s")
            wait_for_not_found(TENANT_CONFIG_KIND, TENANT_CR_NAME, case["tenant_ns"], timeout=180)
            wait_for_not_found("role", role_name, case["tenant_ns"], timeout=180)
            wait_for_not_found("namespace", case["tenant_ns"], timeout=180)
            wait_for_aitenant_cleanup_resources_deleted(case)
        finally:
            cleanup_discovery_case(case)

    def test_default_tenant_unaffected_by_multitenancy_enablement(self):
        """7.2: Default tenant namespace still reconciles while discovery mode is enabled."""
        ns = legacy_default_namespace()
        suffix = uuid.uuid4().hex[:6]
        policy_name = f"e2e-default-int-auth-{suffix}"
        subscription_name = f"e2e-default-int-sub-{suffix}"
        try:
            apply_maas_auth_policy(policy_name, ns)
            apply_maas_subscription(subscription_name, ns)
            wait_for_status_phase("maasauthpolicy", policy_name, ns, expected_phase="Active")
            wait_for_status_phase("maassubscription", subscription_name, ns, expected_phase=("Active", "Degraded"))
        finally:
            delete_maas_auth_policy(policy_name, ns)
            delete_maas_subscription(subscription_name, ns)

    def test_same_named_resources_across_tenants(self):
        """7.3: Same-named MaaS resources in separate tenant namespaces both contribute safely."""
        case_a = new_discovery_case(use_default_gateway=True)
        case_b = new_discovery_case(use_default_gateway=True)
        shared_policy = f"e2e-shared-int-policy-{case_a['suffix']}"
        shared_sub = f"e2e-shared-int-sub-{case_a['suffix']}"
        for case in (case_a, case_b):
            case["policy_name"] = shared_policy
            case["subscription_name"] = shared_sub

        try:
            for case in (case_a, case_b):
                apply_discovery_labels(case["tenant_ns"], case["tenant_label_name"])
                apply_tenant_cr(case["tenant_ns"], DEFAULT_GATEWAY_NAME)
                apply_maas_auth_policy(shared_policy, case["tenant_ns"])
                apply_maas_subscription(shared_sub, case["tenant_ns"])
                wait_for_finalizer("maasauthpolicy", shared_policy, case["tenant_ns"], FINALIZER_AUTHPOLICY)
                wait_for_finalizer("maassubscription", shared_sub, case["tenant_ns"], FINALIZER_SUBSCRIPTION)
                wait_for_status_phase("maasauthpolicy", shared_policy, case["tenant_ns"], expected_phase="Active")

            expected_subs = [f"{case_a['tenant_ns']}/{shared_sub}", f"{case_b['tenant_ns']}/{shared_sub}"]
            wait_for_annotation_contains(
                "tokenratelimitpolicy",
                f"maas-trlp-{MODEL_REF}",
                MODEL_NAMESPACE,
                "maas.opendatahub.io/subscriptions",
                expected_subs,
            )
        finally:
            cleanup_discovery_case(case_a, delete_gateway=False)
            cleanup_discovery_case(case_b, delete_gateway=False)

    def test_tenant_namespace_label_change_triggers_reconciliation(self):
        """7.4: Adding and removing discovery labels changes reconciliation behavior."""
        case = new_discovery_case(use_default_gateway=True)
        first_policy = case["policy_name"]
        second_policy = f"e2e-label-return-{case['suffix']}"
        try:
            ensure_namespace(case["tenant_ns"])
            apply_tenant_cr(case["tenant_ns"], DEFAULT_GATEWAY_NAME)
            apply_maas_auth_policy(first_policy, case["tenant_ns"])
            _wait_reconcile(10)
            first = get_json_or_none("maasauthpolicy", first_policy, case["tenant_ns"])
            assert first is not None
            assert FINALIZER_AUTHPOLICY not in ((first.get("metadata") or {}).get("finalizers") or [])

            apply_discovery_labels(case["tenant_ns"], case["tenant_label_name"])
            wait_for_finalizer("maasauthpolicy", first_policy, case["tenant_ns"], FINALIZER_AUTHPOLICY)
            wait_for_status_phase("maasauthpolicy", first_policy, case["tenant_ns"], expected_phase="Active")

            remove_discovery_labels(case["tenant_ns"])
            _wait_reconcile(10)
            apply_maas_auth_policy(second_policy, case["tenant_ns"])
            _wait_reconcile(10)
            second = get_json_or_none("maasauthpolicy", second_policy, case["tenant_ns"])
            assert second is not None
            assert FINALIZER_AUTHPOLICY not in ((second.get("metadata") or {}).get("finalizers") or [])
        finally:
            delete_maas_auth_policy(second_policy, case["tenant_ns"])
            cleanup_discovery_case(case, delete_gateway=False)
