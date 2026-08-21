"""Worker-scoped AITenant bootstrap for parallel E2E (Phase 3 pilot).

Each pytest-xdist worker gets its own discovered tenant namespace, gateway route,
and baseline simulator/premium auth+subscription CRs so Bucket C tests do not
collide on models-as-a-service.
"""

from __future__ import annotations

import os
from contextlib import contextmanager
from typing import Iterator, Optional

from test_helper import (
    MODEL_NAMESPACE,
    MODEL_REF,
    PREMIUM_MODEL_REF,
    PREMIUM_SIMULATOR_SUBSCRIPTION,
    SIMULATOR_ACCESS_POLICY,
    SIMULATOR_SUBSCRIPTION,
    TLS_VERIFY,
    _apply_cr,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
)
from multitenancy_helpers import (
    INFRA_NAMESPACE,
    bootstrap_aitenant_tenant,
    cleanup_discovery_case,
    new_named_tenant_case,
    per_tenant_gateway_policy_names,
    require_aitenant_crd,
    wait_for_deployment_available,
    wait_for_gateway_authpolicy_ready,
    wait_for_route_admitted,
)


_WORKER_TENANT_ENV_KEYS = (
    "MAAS_SUBSCRIPTION_NAMESPACE",
    "GATEWAY_HOST",
    "MAAS_API_BASE_URL",
    "E2E_GATEWAY_AUTH_POLICY_NAME",
)


def worker_tenant_enabled() -> bool:
    """Opt-out via E2E_USE_WORKER_TENANT=false (default: enabled when AITenant CRD exists)."""
    return os.environ.get("E2E_USE_WORKER_TENANT", "true").lower() not in ("0", "false", "no")


def xdist_worker_suffix() -> str:
    worker = os.environ.get("PYTEST_XDIST_WORKER", "master")
    if worker == "master":
        return "main"
    return worker.replace("gw", "w")


def build_worker_tenant_case(worker_suffix: str) -> dict[str, str]:
    return new_named_tenant_case(f"e2e-worker-{worker_suffix}")


def _route_host(case: dict[str, str]) -> str:
    route = wait_for_route_admitted(f"{case['gateway_name']}-route")
    host = route.get("spec", {}).get("host")
    if not host:
        raise RuntimeError(f"Route {case['gateway_name']}-route missing spec.host")
    return host


def _apply_baseline_stack(namespace: str) -> None:
    """Mirror prow/CI baseline auth+subscriptions inside the worker tenant namespace."""
    _apply_cr(
        {
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSSubscription",
            "metadata": {"name": SIMULATOR_SUBSCRIPTION, "namespace": namespace},
            "spec": {
                "owner": {"groups": [{"name": "system:authenticated"}], "users": []},
                "modelRefs": [
                    {
                        "name": MODEL_REF,
                        "namespace": MODEL_NAMESPACE,
                        "tokenRateLimits": [{"limit": 100, "window": "1m"}],
                    }
                ],
                "priority": 10,
            },
        }
    )
    _apply_cr(
        {
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSAuthPolicy",
            "metadata": {"name": SIMULATOR_ACCESS_POLICY, "namespace": namespace},
            "spec": {
                "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE}],
                "subjects": {"groups": [{"name": "system:authenticated"}], "users": []},
            },
        }
    )
    _apply_cr(
        {
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSSubscription",
            "metadata": {"name": PREMIUM_SIMULATOR_SUBSCRIPTION, "namespace": namespace},
            "spec": {
                "owner": {"groups": [{"name": "premium-user"}], "users": []},
                "modelRefs": [
                    {
                        "name": PREMIUM_MODEL_REF,
                        "namespace": MODEL_NAMESPACE,
                        "tokenRateLimits": [{"limit": 1000, "window": "1m"}],
                    }
                ],
                "priority": 20,
            },
        }
    )
    _apply_cr(
        {
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSAuthPolicy",
            "metadata": {"name": "premium-simulator-access", "namespace": namespace},
            "spec": {
                "modelRefs": [{"name": PREMIUM_MODEL_REF, "namespace": MODEL_NAMESPACE}],
                "subjects": {"groups": [{"name": "premium-user"}], "users": []},
            },
        }
    )

    _wait_for_maas_auth_policy_phase(
        SIMULATOR_ACCESS_POLICY,
        namespace=namespace,
        timeout=int(os.environ.get("E2E_AUTHPOLICY_PHASE_TIMEOUT", "120")),
        require_auth_policies=False,
        require_enforced=False,
    )
    _wait_for_maas_subscription_phase(
        SIMULATOR_SUBSCRIPTION,
        namespace=namespace,
        timeout=180,
    )
    _wait_for_maas_auth_policy_phase(
        "premium-simulator-access",
        namespace=namespace,
        timeout=int(os.environ.get("E2E_AUTHPOLICY_PHASE_TIMEOUT", "120")),
        require_auth_policies=False,
        require_enforced=False,
    )
    _wait_for_maas_subscription_phase(
        PREMIUM_SIMULATOR_SUBSCRIPTION,
        namespace=namespace,
        timeout=180,
    )


def bootstrap_worker_tenant(case: dict[str, str]) -> dict[str, str]:
    """Create AITenant + baseline CRs; return enriched case dict for tests."""
    require_aitenant_crd()
    bootstrap_aitenant_tenant(case)

    host = _route_host(case)
    scheme = "http" if os.environ.get("INSECURE_HTTP", "").lower() == "true" else "https"
    case["gateway_host"] = host
    case["base_url"] = f"{scheme}://{host}/maas-api"
    case["namespace"] = case["tenant_ns"]
    case["gateway_authpolicy_name"] = per_tenant_gateway_policy_names(
        case["tenant_label_name"],
        case["gateway_name"],
    )["gateway_authpolicy"]

    deployment_name = f"maas-api-{case['tenant_label_name']}"
    wait_for_deployment_available(deployment_name, namespace=INFRA_NAMESPACE, timeout=180)

    _apply_baseline_stack(case["tenant_ns"])

    wait_for_gateway_authpolicy_ready(
        case["gateway_name"],
        timeout=int(os.environ.get("E2E_GATEWAY_ENFORCED_TIMEOUT", "240")),
    )
    return case


@contextmanager
def activate_worker_tenant(case: Optional[dict[str, str]]) -> Iterator[None]:
    """Point test_helper URL/namespace env vars at a worker tenant for the test scope."""
    if not case:
        yield
        return

    saved = {key: os.environ.get(key) for key in _WORKER_TENANT_ENV_KEYS}
    os.environ["MAAS_SUBSCRIPTION_NAMESPACE"] = case["tenant_ns"]
    os.environ["GATEWAY_HOST"] = case["gateway_host"]
    os.environ["MAAS_API_BASE_URL"] = case["base_url"]
    os.environ["E2E_GATEWAY_AUTH_POLICY_NAME"] = case["gateway_authpolicy_name"]
    try:
        yield
    finally:
        for key, value in saved.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value


def teardown_worker_tenant(case: dict[str, str]) -> None:
    cleanup_discovery_case(case)
