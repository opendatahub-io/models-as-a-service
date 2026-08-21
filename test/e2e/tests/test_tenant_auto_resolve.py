"""
E2E tests for MaaSModelRef tenant auto-resolution (RHOAIENG-87566).

Tests validate:
  - When spec.tenantRef is omitted, the controller auto-resolves the tenant
    from the HTTPRoute's gateway parentRef and populates status.resolvedTenantRef
  - When spec.tenantRef is set explicitly, status.resolvedTenantRef reflects
    the explicit value
  - When no AITenant matches the gateway, the MaaSModelRef enters Failed state
    with a descriptive message

Prerequisites:
  - AITenant CRD available
  - Tenant namespace discovery enabled on controller
  - KServe controller running (for HTTPRoute creation)
  - Gateway infrastructure (openshift-ingress)
"""

import logging

import pytest

from multitenancy_helpers import (
    GATEWAY_NAMESPACE,
    _apply,
    apply_gateway_fixture,
    bootstrap_aitenant_tenant,
    cleanup_discovery_case,
    delete_best_effort,
    get_json_or_none,
    new_named_tenant_case,
    require_aitenant_crd,
    require_tenant_namespace_discovery,
    wait_for_gateway_programmed,
    wait_for_httproute_accepted,
    wait_for_status_phase,
    apply_gateway_access_label,
)
from test_helper import (
    _create_llmis,
    _delete_cr,
)

pytestmark = pytest.mark.xdist_group("tenant_auto_resolve")


@pytest.fixture(scope="module", autouse=True)
def _require_prerequisites():
    require_tenant_namespace_discovery()
    require_aitenant_crd()


def _create_maas_model_ref_with_tenant(name, namespace, llmis_name, tenant_ref=None):
    """Create a MaaSModelRef, optionally setting spec.tenantRef."""
    spec = {
        "modelRef": {
            "kind": "LLMInferenceService",
            "name": llmis_name,
        },
    }
    if tenant_ref is not None:
        spec["tenantRef"] = tenant_ref
    _apply(
        {
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSModelRef",
            "metadata": {"name": name, "namespace": namespace},
            "spec": spec,
        }
    )


def _wait_for_maasmodelref_status(name, namespace, predicate, *, timeout=180, interval=5):
    """Wait for MaaSModelRef to satisfy a predicate on its status."""
    from multitenancy_helpers import wait_for_json

    def _pred(obj):
        return predicate(obj.get("status") or {})

    return wait_for_json("maasmodelref", name, namespace, predicate=_pred, timeout=timeout, interval=interval)


class TestTenantAutoResolve:
    """Tenant auto-resolution from HTTPRoute gateway parentRef."""

    def test_auto_resolve_populates_resolved_tenant_ref(self):
        """When tenantRef is omitted, resolvedTenantRef is populated from the gateway."""
        case = new_named_tenant_case("e2e-autores")
        model_name = f"autores-model-{case['suffix']}"
        try:
            bootstrap_aitenant_tenant(case)

            _create_llmis(model_name, case["tenant_ns"], case["gateway_name"], GATEWAY_NAMESPACE)
            wait_for_httproute_accepted(
                f"{model_name}-kserve-route",
                case["tenant_ns"],
                case["gateway_name"],
                timeout=180,
            )

            _create_maas_model_ref_with_tenant(model_name, case["tenant_ns"], model_name)

            obj = _wait_for_maasmodelref_status(
                model_name,
                case["tenant_ns"],
                lambda s: s.get("resolvedTenantRef") == case["tenant_label_name"],
                timeout=120,
            )
            status = obj.get("status", {})
            assert status["resolvedTenantRef"] == case["tenant_label_name"], (
                f"Expected resolvedTenantRef={case['tenant_label_name']}, "
                f"got {status.get('resolvedTenantRef')}"
            )
            assert status.get("httpRouteGatewayName") == case["gateway_name"]
            assert status.get("httpRouteGatewayNamespace") == GATEWAY_NAMESPACE

        finally:
            _delete_cr("maasmodelref", model_name, case["tenant_ns"])
            _delete_cr("llminferenceservice", model_name, case["tenant_ns"])
            cleanup_discovery_case(case)

    def test_explicit_tenant_ref_preserved(self):
        """When tenantRef is set explicitly, resolvedTenantRef reflects the explicit value."""
        case = new_named_tenant_case("e2e-explicit")
        model_name = f"explicit-model-{case['suffix']}"
        try:
            bootstrap_aitenant_tenant(case)

            _create_llmis(model_name, case["tenant_ns"], case["gateway_name"], GATEWAY_NAMESPACE)
            wait_for_httproute_accepted(
                f"{model_name}-kserve-route",
                case["tenant_ns"],
                case["gateway_name"],
                timeout=180,
            )

            _create_maas_model_ref_with_tenant(
                model_name, case["tenant_ns"], model_name,
                tenant_ref=case["tenant_label_name"],
            )

            obj = _wait_for_maasmodelref_status(
                model_name,
                case["tenant_ns"],
                lambda s: s.get("resolvedTenantRef") == case["tenant_label_name"],
                timeout=120,
            )
            status = obj.get("status", {})
            assert status["resolvedTenantRef"] == case["tenant_label_name"]
            spec = obj.get("spec", {})
            assert spec.get("tenantRef") == case["tenant_label_name"], (
                "spec.tenantRef should be preserved as-is"
            )

        finally:
            _delete_cr("maasmodelref", model_name, case["tenant_ns"])
            _delete_cr("llminferenceservice", model_name, case["tenant_ns"])
            cleanup_discovery_case(case)

    def test_no_matching_tenant_enters_failed(self):
        """MaaSModelRef enters Failed state when no AITenant matches the gateway."""
        case = new_named_tenant_case("e2e-noten")
        model_name = f"noten-model-{case['suffix']}"
        fixture_label = f"noten-{case['suffix']}"
        try:
            apply_gateway_fixture(case["gateway_name"], fixture_label=fixture_label)
            wait_for_gateway_programmed(case["gateway_name"])
            apply_gateway_access_label(case["tenant_ns"], case["gateway_name"])

            from multitenancy_helpers import ensure_namespace
            ensure_namespace(case["tenant_ns"])
            apply_gateway_access_label(case["tenant_ns"], case["gateway_name"])

            _create_llmis(model_name, case["tenant_ns"], case["gateway_name"], GATEWAY_NAMESPACE)
            wait_for_httproute_accepted(
                f"{model_name}-kserve-route",
                case["tenant_ns"],
                case["gateway_name"],
                timeout=180,
            )

            _create_maas_model_ref_with_tenant(model_name, case["tenant_ns"], model_name)

            obj = _wait_for_maasmodelref_status(
                model_name,
                case["tenant_ns"],
                lambda s: s.get("phase") == "Failed",
                timeout=120,
            )
            status = obj.get("status", {})
            assert status["phase"] == "Failed"
            assert status.get("resolvedTenantRef", "") == "", (
                "resolvedTenantRef should be empty when no tenant matches"
            )
            conditions = status.get("conditions") or []
            has_error_condition = any(
                c.get("status") == "False" or "no AITenant found" in (c.get("message") or "")
                for c in conditions
            )
            assert has_error_condition, (
                f"Expected a condition indicating failed tenant resolution, got: {conditions}"
            )

        finally:
            _delete_cr("maasmodelref", model_name, case["tenant_ns"])
            _delete_cr("llminferenceservice", model_name, case["tenant_ns"])
            delete_best_effort("gateway", case["gateway_name"], GATEWAY_NAMESPACE)
            delete_best_effort("configmap", f"{case['gateway_name']}-gw-options", GATEWAY_NAMESPACE)
            from multitenancy_helpers import delete_namespace_best_effort, remove_gateway_access_label, INFRA_NAMESPACE
            try:
                remove_gateway_access_label(INFRA_NAMESPACE, case["gateway_name"])
            except Exception as exc:  # noqa: BLE001 - cleanup must not mask test results
                logging.warning(
                    "failed to remove gateway access label %s from %s: %s",
                    case["gateway_name"], INFRA_NAMESPACE, exc,
                )
            delete_namespace_best_effort(case["tenant_ns"])
