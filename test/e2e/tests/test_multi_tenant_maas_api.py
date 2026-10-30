"""
E2E tests for per-tenant maas-api infrastructure.

These tests validate the Phase 1 multi-tenant contract:
  - AITenant creates dedicated maas-api Deployment/Service/HTTPRoute/AuthPolicy
  - TENANT_NAME is set on each maas-api Deployment
  - HTTPRoutes attach to the tenant Gateway and route to the tenant Service
  - Default and multiple tenant maas-api instances coexist
"""

import os
import time

import pytest

from multitenancy_helpers import (
    AITENANT_KIND,
    AITENANT_NAMESPACE,
    GATEWAY_NAMESPACE,
    INFRA_NAMESPACE,
    apply_maas_auth_policy,
    bootstrap_aitenant_tenant,
    cleanup_discovery_case,
    deployment_env,
    get_json_or_none,
    http_route_backend_refs,
    http_route_parent_refs,
    list_json,
    new_named_tenant_case,
    per_tenant_maas_api_names,
    redact_mapping,
    require_aitenant_crd,
    wait_for_deployment_available,
    wait_for_gateway_authpolicy_ready,
    wait_for_json,
)

pytestmark = pytest.mark.xdist_group("mt_lifecycle")
AUTHORINO_NAMESPACE = os.environ.get("AUTHORINO_NAMESPACE", "kuadrant-system")
HTTPROUTE_RULE_AUTHCONFIG_ANNOTATION = "HTTPRouteRule.gateway.networking.k8s.io"
AUTHCONFIG_QUIET_WINDOW = 15
AUTHCONFIG_SETTLE_TIMEOUT = 180
WASM_GENERATION_STABILITY_WINDOW = int(
    os.environ.get("E2E_DUAL_LISTENER_WASM_STABILITY_WINDOW", "60")
)
POLL_INTERVAL = 3

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


@pytest.fixture(scope="module")
def dual_listener_tenant_case():
    """Create one tenant gateway whose HTTP and HTTPS listeners match the same route."""
    require_aitenant_crd()
    case = new_named_tenant_case("e2e-authcfg-http")
    try:
        bootstrap_aitenant_tenant(case, dual_http_listener=True)
        apply_maas_auth_policy(case["policy_name"], case["tenant_ns"])
        wait_for_gateway_authpolicy_ready(case["gateway_name"])
        yield case
    finally:
        cleanup_discovery_case(case)


def _route_accepted_by_gateway(route: dict, gateway_name: str) -> bool:
    for parent in (route.get("status") or {}).get("parents") or []:
        parent_ref = parent.get("parentRef") or {}
        parent_namespace = parent_ref.get("namespace") or GATEWAY_NAMESPACE
        if parent_ref.get("name") != gateway_name or parent_namespace != GATEWAY_NAMESPACE:
            continue
        if any(
            condition.get("type") == "Accepted" and condition.get("status") == "True"
            for condition in parent.get("conditions") or []
        ):
            return True
    return False


def _authconfig_condition(authconfig: dict, condition_type: str) -> str | None:
    for condition in (authconfig.get("status") or {}).get("conditions") or []:
        if condition.get("type") == condition_type:
            return condition.get("status")
    return None


def _authconfig_is_ready(authconfig: dict) -> bool:
    metadata = authconfig.get("metadata") or {}
    if metadata.get("deletionTimestamp"):
        return False
    return (
        _authconfig_condition(authconfig, "Ready") == "True"
        and _authconfig_condition(authconfig, "Available") == "True"
    )


def _gateway_listener_names_with_routes(gateway: dict) -> set[str]:
    """Return accepted Gateway listeners with at least one attached route."""
    attached = set()
    for listener in (gateway.get("status") or {}).get("listeners") or []:
        conditions = listener.get("conditions") or []
        accepted = any(
            condition.get("type") == "Accepted" and condition.get("status") == "True"
            for condition in conditions
        )
        if accepted and listener.get("attachedRoutes", 0) > 0:
            attached.add(listener.get("name"))
    return attached


def _route_rule_authconfig_observations(
    route_namespace: str, route_name: str
) -> dict[str, list[tuple[str, str, bool]]]:
    """Return non-deleting managed AuthConfigs grouped by HTTPRoute rule."""
    route_annotation_prefix = (
        f"httproute.gateway.networking.k8s.io:{route_namespace}/{route_name}#"
    )
    by_rule = {}
    for authconfig in list_json(
        "authconfig", AUTHORINO_NAMESPACE, labels="kuadrant.io/auth=true"
    ):
        metadata = authconfig.get("metadata") or {}
        if metadata.get("deletionTimestamp"):
            continue
        annotations = metadata.get("annotations") or {}
        annotation = annotations.get(HTTPROUTE_RULE_AUTHCONFIG_ANNOTATION, "")
        if not annotation.startswith(route_annotation_prefix):
            continue
        rule = annotation.rsplit("#", 1)[-1]
        name = metadata.get("name", "<unnamed>")
        uid = metadata.get("uid")
        assert uid, f"Matching AuthConfig {name!r} has no metadata.uid"
        by_rule.setdefault(rule, []).append(
            (name, uid, _authconfig_is_ready(authconfig))
        )
    return by_rule


def _wait_for_stable_route_authconfigs(
    route: dict,
) -> dict[str, list[tuple[str, str, bool]]]:
    namespace = (route.get("metadata") or {}).get("namespace", INFRA_NAMESPACE)
    route_name = (route.get("metadata") or {}).get("name", "")
    rules = (route.get("spec") or {}).get("rules") or []
    expected_rules = {f"rule-{index}" for index in range(1, len(rules) + 1)}
    assert expected_rules, f"HTTPRoute {namespace}/{route_name} has no rules"

    deadline = time.monotonic() + AUTHCONFIG_SETTLE_TIMEOUT
    last_snapshot = None
    stable_since = None
    last_by_rule: dict[str, list[tuple[str, str, bool]]] = {}
    while time.monotonic() < deadline:
        last_by_rule = _route_rule_authconfig_observations(namespace, route_name)
        snapshot = {
            rule: tuple(sorted((name, uid) for name, uid, _ in observations))
            for rule, observations in sorted(last_by_rule.items())
        }
        complete = set(last_by_rule) == expected_rules and all(
            any(ready for _, _, ready in observations)
            for observations in last_by_rule.values()
        )
        now = time.monotonic()
        if complete and snapshot == last_snapshot:
            if stable_since is not None and now - stable_since >= AUTHCONFIG_QUIET_WINDOW:
                return last_by_rule
        elif complete:
            stable_since = now
        else:
            stable_since = None
        last_snapshot = snapshot
        time.sleep(POLL_INTERVAL)

    raise AssertionError(
        f"AuthConfig state for {namespace}/{route_name} did not settle within "
        f"{AUTHCONFIG_SETTLE_TIMEOUT}s; "
        f"expected rules={expected_rules!r}, observed={last_by_rule!r}"
    )


def _gateway_wasm_config(gateway_name: str) -> tuple[str, dict] | None:
    """Find Kuadrant's Gateway wasm config across supported RHCL resource kinds."""
    config_name = f"kuadrant-{gateway_name}"
    for kind in ("envoyfilter", "wasmplugin"):
        try:
            config = get_json_or_none(kind, config_name, GATEWAY_NAMESPACE)
        except RuntimeError as exc:
            if "doesn't have a resource type" in str(exc).lower():
                continue
            raise
        if config is not None:
            return kind, config
    return None


def _wait_for_gateway_wasm_config(
    gateway_name: str, timeout: int = 180
) -> tuple[str, dict]:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = _gateway_wasm_config(gateway_name)
        if result is not None:
            return result
        time.sleep(POLL_INTERVAL)
    raise AssertionError(
        f"Kuadrant wasm EnvoyFilter/WasmPlugin kuadrant-{gateway_name} "
        f"did not appear in {GATEWAY_NAMESPACE} within {timeout}s"
    )


def _assert_gateway_wasm_generation_stable(gateway_name: str) -> None:
    kind, initial = _wait_for_gateway_wasm_config(gateway_name)
    initial_metadata = initial.get("metadata") or {}
    initial_generation = initial_metadata.get("generation")
    assert initial_generation is not None, (
        f"{kind} kuadrant-{gateway_name} has no metadata.generation"
    )
    initial_uid = initial_metadata.get("uid")
    assert initial_uid is not None, f"{kind} kuadrant-{gateway_name} has no metadata.uid"

    started_at = time.monotonic()
    deadline = started_at + WASM_GENERATION_STABILITY_WINDOW
    observations = [(0, initial_uid, initial_generation)]
    while time.monotonic() < deadline:
        time.sleep(POLL_INTERVAL)
        current = _gateway_wasm_config(gateway_name)
        elapsed = round(time.monotonic() - started_at, 1)
        assert current is not None, (
            f"{kind} kuadrant-{gateway_name} disappeared while checking generation stability; "
            f"observations={observations!r}"
        )
        current_kind, config = current
        metadata = config.get("metadata") or {}
        uid = metadata.get("uid")
        generation = metadata.get("generation")
        observations.append((elapsed, uid, generation))
        assert current_kind == kind and uid == initial_uid and generation == initial_generation, (
            f"{kind} kuadrant-{gateway_name} identity or generation changed during the "
            f"{WASM_GENERATION_STABILITY_WINDOW}s quiet window: "
            f"uid/generation {initial_uid}/{initial_generation} -> {uid}/{generation}; "
            f"observations={observations!r}"
        )


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

    @pytest.mark.nightly
    @pytest.mark.serial
    def test_dual_listeners_keep_one_authconfig_per_route_rule(self, dual_listener_tenant_case):
        """One stable Authorino AuthConfig per rule when both listeners attach the route."""
        case = dual_listener_tenant_case
        gateway = wait_for_json(
            "gateway",
            case["gateway_name"],
            GATEWAY_NAMESPACE,
            predicate=lambda obj: {"http", "https"}
            <= _gateway_listener_names_with_routes(obj),
            timeout=180,
        )
        listeners = {
            listener.get("name"): listener
            for listener in (gateway.get("spec") or {}).get("listeners") or []
        }
        assert {"http", "https"} <= set(listeners), (
            f"{case['gateway_name']} must have both HTTP and HTTPS listeners, got {listeners!r}"
        )
        assert listeners["http"].get("protocol") == "HTTP" and listeners["http"].get("port") == 80
        assert listeners["https"].get("protocol") == "HTTPS" and listeners["https"].get("port") == 443
        assert listeners["http"].get("hostname") == listeners["https"].get("hostname"), (
            f"Test setup must use a shared hostname across listeners: {listeners!r}"
        )

        route_name = _tenant_api_names(case)["httproute"]
        route = wait_for_json(
            "httproute",
            route_name,
            INFRA_NAMESPACE,
            predicate=lambda obj: _route_accepted_by_gateway(obj, case["gateway_name"]),
            timeout=180,
        )
        authconfigs_by_rule = _wait_for_stable_route_authconfigs(route)
        rules = (route.get("spec") or {}).get("rules") or []
        expected_rules = {f"rule-{index}" for index in range(1, len(rules) + 1)}
        assert set(authconfigs_by_rule) == expected_rules
        assert all(
            len(observations) == 1 and observations[0][2]
            for observations in authconfigs_by_rule.values()
        ), (
            f"Expected exactly one Ready and Available non-deleting Authorino "
            f"AuthConfig per rule for "
            f"{INFRA_NAMESPACE}/{route_name}; got {authconfigs_by_rule!r}"
        )

    @pytest.mark.nightly
    @pytest.mark.serial
    def test_dual_listener_wasm_envoyfilter_generation_stays_stable(
        self, dual_listener_tenant_case
    ):
        """The overlapping listeners must not trigger Kuadrant wasm config churn."""
        _assert_gateway_wasm_generation_stable(dual_listener_tenant_case["gateway_name"])
