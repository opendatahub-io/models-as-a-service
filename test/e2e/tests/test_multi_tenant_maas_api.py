"""
E2E tests for per-tenant maas-api infrastructure.

These tests validate the Phase 1 multi-tenant contract:
  - AITenant creates dedicated maas-api Deployment/Service/HTTPRoute/AuthPolicy
  - TENANT_NAME is set on each maas-api Deployment
  - HTTPRoutes attach to the tenant Gateway and route to the tenant Service
  - Default and multiple tenant maas-api instances coexist
"""

import pytest

from multitenancy_helpers import (
    AITENANT_KIND,
    AITENANT_NAMESPACE,
    DEPLOYMENT_NAMESPACE,
    GATEWAY_NAMESPACE,
    INFRA_NAMESPACE,
    apply_maas_auth_policy,
    bootstrap_aitenant_tenant,
    cleanup_discovery_case,
    deployment_env,
    env_bool,
    get_json_or_none,
    http_route_backend_refs,
    http_route_parent_refs,
    new_named_tenant_case,
    per_tenant_maas_api_names,
    redact_mapping,
    require_aitenant_crd,
    wait_for_deployment_available,
    wait_for_json,
)


# Multi-tenant maas-api tests are enabled by default (Phase 1 implementation)


@pytest.fixture(scope="module")
def tenant_cases():
    require_aitenant_crd()
    case_a = new_named_tenant_case("e2e-api-a")
    case_b = new_named_tenant_case("e2e-api-b")
    try:
        for case in (case_a, case_b):
            bootstrap_aitenant_tenant(case)
            # Create MaaSAuthPolicy to trigger gateway AuthPolicy creation
            apply_maas_auth_policy(f"e2e-{case['tenant_label_name']}-policy", case["tenant_ns"])
        yield case_a, case_b
    finally:
        cleanup_discovery_case(case_a)
        cleanup_discovery_case(case_b)


def _tenant_name(case: dict[str, str]) -> str:
    return case["tenant_label_name"]


def _tenant_api_names(case: dict[str, str]) -> dict[str, str]:
    return per_tenant_maas_api_names(_tenant_name(case))


class TestPerTenantMaaSAPI:
    """S27 section 2 — per-tenant maas-api infrastructure."""

    def test_aitenant_creates_dedicated_maas_api_infrastructure(self, tenant_cases):
        """2.1: AITenant creates dedicated maas-api Deployment, Service, HTTPRoute, and gateway AuthPolicy."""
        for case in tenant_cases:
            names = _tenant_api_names(case)
            deployment = wait_for_deployment_available(names["deployment"], INFRA_NAMESPACE, timeout=240)
            assert deployment["metadata"]["name"] == names["deployment"]

            service = wait_for_json("service", names["service"], INFRA_NAMESPACE, timeout=180)
            assert service["metadata"]["name"] == names["service"]

            route = wait_for_json("httproute", names["httproute"], INFRA_NAMESPACE, timeout=180)
            assert route["metadata"]["name"] == names["httproute"]

            # Gateway-level AuthPolicy is in the gateway namespace, named {gateway-name}-maas-auth
            gateway_auth_policy_name = f"{case['gateway_name']}-maas-auth"
            auth = wait_for_json("authpolicy", gateway_auth_policy_name, GATEWAY_NAMESPACE, timeout=180)
            assert auth["metadata"]["name"] == gateway_auth_policy_name
            # Verify it targets the tenant Gateway
            assert auth["spec"]["targetRef"]["name"] == case["gateway_name"]

            aitenant = wait_for_json(AITENANT_KIND, case["tenant_label_name"], AITENANT_NAMESPACE, timeout=180)
            assert aitenant["status"]["gatewayRef"]["name"] == case["gateway_name"]

    def test_tenant_name_environment_variable_set(self, tenant_cases):
        """2.2: TENANT_NAME env var identifies the tenant served by each maas-api Deployment."""
        for case in tenant_cases:
            names = _tenant_api_names(case)
            env = deployment_env(names["deployment"], INFRA_NAMESPACE)
            assert env.get("TENANT_NAME") == _tenant_name(case), (
                f"{names['deployment']} should set TENANT_NAME={_tenant_name(case)!r}, got {redact_mapping(env)!r}"
            )

    def test_service_routing_isolation(self, tenant_cases):
        """2.3: Each tenant HTTPRoute points at that tenant's dedicated Service."""
        service_names = set()
        for case in tenant_cases:
            names = _tenant_api_names(case)
            service = get_json_or_none("service", names["service"], INFRA_NAMESPACE)
            assert service is not None
            service_names.add(service["metadata"]["name"])

            backend_refs = http_route_backend_refs(names["httproute"], INFRA_NAMESPACE)
            backend_names = {ref.get("name") for ref in backend_refs}
            assert names["service"] in backend_names, (
                f"{names['httproute']} should route to {names['service']}, got {backend_refs!r}"
            )

        assert len(service_names) == len(tenant_cases), f"expected isolated services, got {service_names!r}"

    def test_httproute_tenant_attachment(self, tenant_cases):
        """2.4: Tenant maas-api HTTPRoute attaches to the tenant Gateway."""
        for case in tenant_cases:
            names = _tenant_api_names(case)
            parent_refs = http_route_parent_refs(names["httproute"], INFRA_NAMESPACE)
            assert any(
                ref.get("name") == case["gateway_name"]
                and (ref.get("namespace") or GATEWAY_NAMESPACE) == GATEWAY_NAMESPACE
                for ref in parent_refs
            ), f"{names['httproute']} parentRefs do not attach to {GATEWAY_NAMESPACE}/{case['gateway_name']}: {parent_refs!r}"

    def test_default_and_multiple_tenants_coexist(self, tenant_cases):
        """2.5: Default maas-api remains while multiple tenant maas-api instances exist."""
        assert get_json_or_none("deployment", "maas-api", INFRA_NAMESPACE) is not None
        assert get_json_or_none("service", "maas-api", INFRA_NAMESPACE) is not None
        assert get_json_or_none("httproute", "maas-api-route", INFRA_NAMESPACE) is not None

        tenant_deployments = [
            get_json_or_none("deployment", _tenant_api_names(case)["deployment"], INFRA_NAMESPACE)
            for case in tenant_cases
        ]
        assert all(deployment is not None for deployment in tenant_deployments)
        assert len({deployment["metadata"]["name"] for deployment in tenant_deployments if deployment}) == len(tenant_cases)
