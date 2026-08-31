"""
E2E tests for tenant-gateway attachment of inference ExternalModel HTTPRoutes.

Verifies that ExternalModels created in AITenant-managed namespaces attach their
HTTPRoute parentRef to the tenant gateway (not the cluster default gateway).

Prerequisites:
- AITenant CRD and tenant namespace discovery enabled on maas-controller
- IPP ExternalModel reconciler with tenant gateway resolution deployed
- Per-tenant gateway fixtures (same as other multi-tenancy E2E)
"""

from __future__ import annotations

import logging
import time

import pytest

from multitenancy_helpers import (
    GATEWAY_NAMESPACE,
    bootstrap_aitenant_tenant,
    cleanup_discovery_case,
    http_route_parent_refs,
    new_named_tenant_case,
    require_aitenant_crd,
    require_tenant_namespace_discovery,
    wait_for_json,
)
from test_helper import (
    _apply_cr,
    _delete_cr,
    _get_cr,
    _wait_reconcile,
)

log = logging.getLogger(__name__)

EXTERNAL_MODEL_KIND = "externalmodels.inference.opendatahub.io"
EXTERNAL_PROVIDER_KIND = "externalproviders.inference.opendatahub.io"
RECONCILE_WAIT = int(__import__("os").environ.get("E2E_RECONCILE_WAIT", "12"))


@pytest.fixture(scope="module")
def tenant_external_model_case():
    require_aitenant_crd()
    require_tenant_namespace_discovery()

    case = new_named_tenant_case("ext-gw")
    model_name = f"{case['tenant_label_name']}-external"
    provider_name = f"{case['tenant_label_name']}-provider"
    case["model_name"] = model_name
    case["provider_name"] = provider_name

    bootstrap_aitenant_tenant(case)

    endpoint = __import__("os").environ.get(
        "E2E_EXTERNAL_ENDPOINT",
        __import__("os").environ.get("E2E_SIMULATOR_ENDPOINT", "httpbingo.org"),
    )

    _apply_cr(
        {
            "apiVersion": "v1",
            "kind": "Secret",
            "metadata": {
                "name": f"{model_name}-api-key",
                "namespace": case["tenant_ns"],
                "labels": {"inference.llm-d.ai/ipp-managed": "true"},
            },
            "type": "Opaque",
            "stringData": {"api-key": "e2e-test-key"},
        }
    )

    _apply_cr(
        {
            "apiVersion": "inference.opendatahub.io/v1alpha1",
            "kind": "ExternalProvider",
            "metadata": {"name": provider_name, "namespace": case["tenant_ns"]},
            "spec": {
                "provider": "openai",
                "endpoint": endpoint,
                "auth": {
                    "type": "simple",
                    "secretRef": {"name": f"{model_name}-api-key"},
                },
            },
        }
    )

    _apply_cr(
        {
            "apiVersion": "inference.opendatahub.io/v1alpha1",
            "kind": "ExternalModel",
            "metadata": {"name": model_name, "namespace": case["tenant_ns"]},
            "spec": {
                "externalProviderRefs": [
                    {
                        "ref": {"name": provider_name},
                        "targetModel": "gpt-3.5-turbo",
                        "apiFormat": "openai-chat",
                        "path": "/post",
                    },
                ],
            },
        }
    )

    _wait_reconcile(RECONCILE_WAIT)

    deadline = time.time() + 180
    while time.time() < deadline:
        em = _get_cr(EXTERNAL_MODEL_KIND, model_name, case["tenant_ns"])
        if em and (em.get("status") or {}).get("phase") == "Ready":
            break
        time.sleep(5)
    else:
        pytest.fail(f"ExternalModel {model_name} in {case['tenant_ns']} did not reach Ready phase")

    yield case

    _delete_cr(EXTERNAL_MODEL_KIND, model_name, case["tenant_ns"])
    _delete_cr(EXTERNAL_PROVIDER_KIND, provider_name, case["tenant_ns"])
    _delete_cr("secret", f"{model_name}-api-key", case["tenant_ns"])
    cleanup_discovery_case(case)


class TestExternalModelTenantGateway:
    """ExternalModel HTTPRoutes in tenant namespaces attach to tenant gateways."""

    def test_httproute_attaches_to_tenant_gateway(self, tenant_external_model_case):
        case = tenant_external_model_case
        model_name = case["model_name"]
        expected_gateway = case["gateway_name"]

        route = wait_for_json("httproute", model_name, case["tenant_ns"], timeout=120)
        assert route is not None, f"HTTPRoute {model_name} not found in {case['tenant_ns']}"

        parent_refs = http_route_parent_refs(model_name, case["tenant_ns"])
        assert parent_refs, f"HTTPRoute {model_name} has no parentRefs"

        attached = any(
            ref.get("name") == expected_gateway
            and (ref.get("namespace") or GATEWAY_NAMESPACE) == GATEWAY_NAMESPACE
            for ref in parent_refs
        )
        assert attached, (
            f"HTTPRoute {model_name} parentRefs {parent_refs!r} do not attach to "
            f"{GATEWAY_NAMESPACE}/{expected_gateway}"
        )

    @pytest.mark.skipif(
        __import__("os").environ.get("E2E_SKIP_DEFAULT_TENANT_EXTERNAL_GATEWAY_CHECK", "").lower()
        in ("1", "true", "yes"),
        reason="Default-tenant external model fixture check skipped by env",
    )
    def test_default_gateway_route_unchanged_for_default_tenant(self):
        """Sanity: default-tenant external models still use the default gateway."""
        from test_helper import MODEL_NAMESPACE

        route = _get_cr("httproute", "e2e-external-model", MODEL_NAMESPACE)
        if route is None:
            pytest.skip("Default-tenant external model fixture not present in this run")

        parent_refs = http_route_parent_refs("e2e-external-model", MODEL_NAMESPACE)
        assert any(ref.get("name") == "maas-default-gateway" for ref in parent_refs), (
            f"Expected default gateway parentRef, got {parent_refs!r}"
        )
