"""Consumer portal stack convergence E2E tests — RHAI-31081.

Asserts the portal stack ai-gateway-controller deploys when the portal
enablement is on actually converged: the HTTPRoutes are Accepted with all
references resolved, and the auth proxy Deployment is available. These
assertions sit one level below test_consumer_portal_coexistence (which
exercises the sign-in flow) and would have caught both real wiring failures
observed on a dev cluster: an auth proxy deployed with empty client
configuration, and portal routes rejected by the Gateway.

The portal-to-backend (BFF) data path is deliberately not tested here: the
lane's maas-portal backend is a placeholder that serves nothing, so there is
no real UI/BFF to exchange data with. That coverage belongs with the UI
team's lane once a real portal UI is deployable.

Prerequisites: EXTERNAL_OIDC=true and MAAS_PORTAL=true (the default when
EXTERNAL_OIDC=true; see test/e2e/scripts/deploy-maas-portal.sh).
"""

from __future__ import annotations

import json
import os

import pytest

from multitenancy_helpers import (
    _oc_run,
    get_json_or_none,
    wait_for_deployment_available,
    wait_for_json,
)
from test_helper import MAAS_API_DEPLOYMENT_NAMESPACE

pytestmark = [
    pytest.mark.nightly,
    pytest.mark.external,
    pytest.mark.full_suite,
    pytest.mark.skipif(
        os.environ.get("EXTERNAL_OIDC", "").lower() != "true",
        reason="EXTERNAL_OIDC is not true",
    ),
    pytest.mark.skipif(
        os.environ.get("MAAS_PORTAL", os.environ.get("EXTERNAL_OIDC", "")).lower() != "true",
        reason="MAAS_PORTAL is not true (consumer portal enablement is off)",
    ),
    pytest.mark.xdist_group("oidc_portal_stack"),
]

PORTAL_ROUTE_NAME = "maas-consumer-portal"
OAUTH2_ROUTE_NAME = "maas-consumer-portal-oauth2"
AUTH_PROXY_DEPLOYMENT_LABEL = "app.kubernetes.io/name=maas-consumer-portal-auth-proxy"


def _route_condition(route: dict, condition_type: str) -> str | None:
    for parent in (route.get("status") or {}).get("parents") or []:
        for condition in parent.get("conditions") or []:
            if condition.get("type") == condition_type:
                return condition.get("status")
    return None


def _wait_for_route_accepted(name: str) -> dict:
    """Wait until the route is Accepted with all backend references resolved."""
    return wait_for_json(
        "httproute",
        name,
        MAAS_API_DEPLOYMENT_NAMESPACE,
        predicate=lambda route: (
            _route_condition(route, "Accepted") == "True"
            and _route_condition(route, "ResolvedRefs") == "True"
        ),
        timeout=300,
    )


def _list_deployments_by_label(namespace: str) -> list[dict]:
    result = _oc_run(
        ["get", "deployments", "-n", namespace, "-l", AUTH_PROXY_DEPLOYMENT_LABEL, "-o", "json"],
        timeout=60,
    )
    assert result.returncode == 0, (
        f"Failed to list auth proxy deployments in {namespace}: "
        f"{result.stderr.strip() or result.stdout.strip()}"
    )
    return json.loads(result.stdout).get("items") or []


class TestPortalStackConvergence:
    def test_portal_routes_accepted(self):
        """Both portal HTTPRoutes attach to the gateway with resolved backends."""
        for name in (PORTAL_ROUTE_NAME, OAUTH2_ROUTE_NAME):
            route = _wait_for_route_accepted(name)
            assert route is not None, f"HTTPRoute {name} did not become Accepted"

    def test_auth_proxy_deployment_available(self):
        """The auth proxy Deployment the routes point at is running.

        The proxy namespace is discovered from the oauth2 route's backendRefs
        (the per-tenant infra namespace, only known at runtime) instead of
        being hardcoded.
        """
        route = get_json_or_none("httproute", OAUTH2_ROUTE_NAME, MAAS_API_DEPLOYMENT_NAMESPACE)
        assert route is not None, f"HTTPRoute {OAUTH2_ROUTE_NAME} not found"

        namespaces = []
        for rule in route.get("spec", {}).get("rules", []):
            for backend in rule.get("backendRefs", []):
                if backend.get("kind") in (None, "Service"):
                    namespace = backend.get("namespace") or MAAS_API_DEPLOYMENT_NAMESPACE
                    if namespace not in namespaces:
                        namespaces.append(namespace)
        assert namespaces, f"No service backendRefs on {OAUTH2_ROUTE_NAME}: {route['spec']}"

        proxies: list[tuple[str, dict]] = []
        for namespace in namespaces:
            for deployment in _list_deployments_by_label(namespace):
                proxies.append((namespace, deployment))
        assert proxies, (
            f"No Deployment labelled {AUTH_PROXY_DEPLOYMENT_LABEL!r} found in {namespaces}"
        )
        assert len(proxies) == 1, (
            "Expected exactly one auth proxy Deployment, got "
            f"{[(ns, d['metadata']['name']) for ns, d in proxies]}"
        )

        namespace, deployment = proxies[0]
        wait_for_deployment_available(
            deployment["metadata"]["name"],
            namespace=namespace,
            timeout=300,
        )
