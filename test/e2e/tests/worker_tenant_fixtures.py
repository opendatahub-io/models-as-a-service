"""Worker-scoped AITenant bootstrap for parallel E2E (Phase 3 pilot).

Each pytest-xdist worker gets its own discovered tenant namespace, gateway route,
and baseline simulator/premium auth+subscription CRs so Bucket C tests do not
collide on models-as-a-service.
"""

from __future__ import annotations

import os
from contextlib import contextmanager
from dataclasses import dataclass, replace
from typing import Iterator, Optional

from test_helper import (
    PREMIUM_SIMULATOR_SUBSCRIPTION,
    SIMULATOR_ACCESS_POLICY,
    SIMULATOR_SUBSCRIPTION,
    _apply_cr,
    _create_llmis,
    _create_maas_model_ref,
    _wait_for_model_ready,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
)
from multitenancy_helpers import (
    INFRA_NAMESPACE,
    _oc_run,
    apply_gateway_access_label,
    bootstrap_aitenant_tenant,
    cleanup_discovery_case,
    new_named_tenant_case,
    per_tenant_gateway_policy_names,
    require_aitenant_crd,
    wait_for_deployment_available,
    wait_for_gateway_authpolicy_ready,
    wait_for_llmisvc_backend_ready,
    wait_for_route_admitted,
)


@dataclass(frozen=True)
class WorkerTenantContext:
    worker_id: str
    suffix: str
    tenant_name: str
    tenant_namespace: str
    model_namespace: str
    gateway_name: str
    route_host: str = ""
    api_base_url: str = ""
    api_deployment_name: str = ""
    gateway_authpolicy_name: str = ""
    model_ref: str = ""
    premium_model_ref: str = ""
    distinct_model_ref: str = ""
    distinct_model_2_ref: str = ""
    unconfigured_model_ref: str = ""
    embedding_model_ref: str = ""
    policy_name: str = SIMULATOR_ACCESS_POLICY
    subscription_name: str = SIMULATOR_SUBSCRIPTION
    premium_subscription_name: str = PREMIUM_SIMULATOR_SUBSCRIPTION

    def tenant_case(self) -> dict[str, str]:
        return {
            "suffix": self.suffix,
            "tenant_ns": self.tenant_namespace,
            "tenant_label_name": self.tenant_name,
            "gateway_name": self.gateway_name,
            "policy_name": self.policy_name,
            "subscription_name": self.subscription_name,
        }


_WORKER_TENANT_ENV_KEYS = (
    "MAAS_SUBSCRIPTION_NAMESPACE",
    "GATEWAY_HOST",
    "MAAS_API_BASE_URL",
    "E2E_GATEWAY_AUTH_POLICY_NAME",
    "E2E_MODEL_NAMESPACE",
)


def worker_tenant_enabled() -> bool:
    """Opt-out via E2E_USE_WORKER_TENANT=false (default: enabled when AITenant CRD exists)."""
    return os.environ.get("E2E_USE_WORKER_TENANT", "true").lower() not in ("0", "false", "no")


def xdist_worker_suffix() -> str:
    worker = os.environ.get("PYTEST_XDIST_WORKER", "master")
    if worker == "master":
        return "main"
    return worker.replace("gw", "w")


def serial_only_selection(request) -> bool:
    """Return whether all selected tests in this module are marked serial.

    Module-scoped fixtures cannot inspect a single test item's marker. Inspect
    the collected selection instead, and reject mixed serial/parallel runs so
    a serial test can never silently execute against worker-owned state.
    """
    selected = [
        item for item in request.session.items
        if str(item.path) == str(request.node.path)
    ]
    if not selected:
        return False
    serial = [item.get_closest_marker("serial") is not None for item in selected]
    if any(serial) and not all(serial):
        raise RuntimeError(
            f"mixed serial and parallel selection for {request.node.nodeid}; "
            "run the serial and parallel E2E passes separately"
        )
    return all(serial)


def build_worker_tenant_case(worker_suffix: str) -> WorkerTenantContext:
    case = new_named_tenant_case(f"e2e-worker-{worker_suffix}")
    return WorkerTenantContext(
        worker_id=worker_suffix,
        suffix=case["suffix"],
        tenant_name=case["tenant_label_name"],
        tenant_namespace=case["tenant_ns"],
        model_namespace=f"e2e-models-{case['tenant_label_name']}",
        gateway_name=case["gateway_name"],
        model_ref=f"simulator-{worker_suffix}-{case['suffix']}",
        premium_model_ref=f"premium-{worker_suffix}-{case['suffix']}",
        distinct_model_ref=f"distinct-{worker_suffix}-{case['suffix']}",
        distinct_model_2_ref=f"distinct-2-{worker_suffix}-{case['suffix']}",
        unconfigured_model_ref=f"unconfigured-{worker_suffix}-{case['suffix']}",
        embedding_model_ref=f"embedding-{worker_suffix}-{case['suffix']}",
    )


def _route_host(case: dict[str, str]) -> str:
    route = wait_for_route_admitted(f"{case['gateway_name']}-route")
    host = route.get("spec", {}).get("host")
    if not host:
        raise RuntimeError(f"Route {case['gateway_name']}-route missing spec.host")
    return host


def _apply_baseline_stack(context: WorkerTenantContext) -> None:
    """Mirror prow/CI baseline auth+subscriptions inside the worker tenant namespace."""
    _apply_cr(
        {
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSSubscription",
            "metadata": {"name": context.subscription_name, "namespace": context.tenant_namespace},
            "spec": {
                "owner": {"groups": [{"name": "system:authenticated"}], "users": []},
                "modelRefs": [
                    {
                        "name": context.model_ref,
                        "namespace": context.model_namespace,
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
            "metadata": {"name": context.policy_name, "namespace": context.tenant_namespace},
            "spec": {
                "modelRefs": [{"name": context.model_ref, "namespace": context.model_namespace}],
                "subjects": {"groups": [{"name": "system:authenticated"}], "users": []},
            },
        }
    )
    _apply_cr(
        {
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSSubscription",
            "metadata": {"name": context.premium_subscription_name, "namespace": context.tenant_namespace},
            "spec": {
                "owner": {"groups": [{"name": "premium-user"}], "users": []},
                "modelRefs": [
                    {
                        "name": context.premium_model_ref,
                        "namespace": context.model_namespace,
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
            "metadata": {"name": "premium-simulator-access", "namespace": context.tenant_namespace},
            "spec": {
                "modelRefs": [{"name": context.premium_model_ref, "namespace": context.model_namespace}],
                "subjects": {"groups": [{"name": "premium-user"}], "users": []},
            },
        }
    )

    _wait_for_maas_auth_policy_phase(
        context.policy_name,
        namespace=context.tenant_namespace,
        timeout=int(os.environ.get("E2E_AUTHPOLICY_PHASE_TIMEOUT", "120")),
        require_auth_policies=False,
        require_enforced=False,
    )
    _wait_for_maas_subscription_phase(
        context.subscription_name,
        namespace=context.tenant_namespace,
        timeout=180,
    )
    _wait_for_maas_auth_policy_phase(
        "premium-simulator-access",
        namespace=context.tenant_namespace,
        timeout=int(os.environ.get("E2E_AUTHPOLICY_PHASE_TIMEOUT", "120")),
        require_auth_policies=False,
        require_enforced=False,
    )
    _wait_for_maas_subscription_phase(
        context.premium_subscription_name,
        namespace=context.tenant_namespace,
        timeout=180,
    )


def bootstrap_worker_tenant(context: WorkerTenantContext) -> WorkerTenantContext:
    """Create AITenant + baseline CRs; return enriched case dict for tests."""
    require_aitenant_crd()
    case = context.tenant_case()
    bootstrap_aitenant_tenant(case)

    host = _route_host(case)
    scheme = "http" if os.environ.get("INSECURE_HTTP", "").lower() == "true" else "https"
    gateway_authpolicy_name = per_tenant_gateway_policy_names(
        case["tenant_label_name"],
        case["gateway_name"],
    )["gateway_authpolicy"]

    deployment_name = f"maas-api-{case['tenant_label_name']}"
    wait_for_deployment_available(deployment_name, namespace=INFRA_NAMESPACE, timeout=180)

    apply_gateway_access_label(context.model_namespace, context.gateway_name)
    for model_ref, model_alias in (
        (context.model_ref, f"e2e/{context.model_ref}"),
        (context.premium_model_ref, f"e2e/{context.premium_model_ref}"),
    ):
        _create_llmis(model_ref, context.model_namespace, context.gateway_name, model_name=model_alias)
        wait_for_llmisvc_backend_ready(model_ref, context.model_namespace, context.gateway_name)
        _create_maas_model_ref(
            model_ref,
            context.model_namespace,
            model_ref,
            tenant_ref=context.tenant_name,
        )

    context = replace(
        context,
        route_host=host,
        api_base_url=f"{scheme}://{host}/maas-api",
        api_deployment_name=deployment_name,
        gateway_authpolicy_name=gateway_authpolicy_name,
    )
    _apply_baseline_stack(context)

    wait_for_gateway_authpolicy_ready(
        case["gateway_name"],
        timeout=int(os.environ.get("E2E_GATEWAY_ENFORCED_TIMEOUT", "240")),
    )
    for model_ref in (context.model_ref, context.premium_model_ref):
        _wait_for_model_ready(
            model_ref,
            namespace=context.model_namespace,
            timeout=int(os.environ.get("E2E_MODELREF_READY_TIMEOUT", "180")),
        )
    return context


def ensure_worker_models(
    context: WorkerTenantContext,
    model_refs: tuple[str, ...],
) -> None:
    """Provision optional worker models immediately before a module needs them."""
    aliases = {
        context.distinct_model_ref: f"e2e/{context.distinct_model_ref}",
        context.distinct_model_2_ref: f"e2e/{context.distinct_model_2_ref}",
        context.unconfigured_model_ref: f"e2e/{context.unconfigured_model_ref}",
        context.embedding_model_ref: f"e2e/{context.embedding_model_ref}",
    }
    timeout = int(os.environ.get("E2E_MODEL_BACKEND_READY_TIMEOUT", "180"))
    for model_ref in model_refs:
        if model_ref not in aliases:
            raise ValueError(f"unsupported optional worker model {model_ref!r}")
        _create_llmis(
            model_ref,
            context.model_namespace,
            context.gateway_name,
            model_name=aliases[model_ref],
        )
        wait_for_llmisvc_backend_ready(
            model_ref,
            context.model_namespace,
            context.gateway_name,
            timeout=timeout,
        )
        _create_maas_model_ref(
            model_ref,
            context.model_namespace,
            model_ref,
            tenant_ref=context.tenant_name,
        )


@contextmanager
def activate_worker_tenant(case: Optional[WorkerTenantContext]) -> Iterator[None]:
    """Point test_helper URL/namespace env vars at a worker tenant for the test scope."""
    if not case:
        yield
        return

    saved = {key: os.environ.get(key) for key in _WORKER_TENANT_ENV_KEYS}
    os.environ["MAAS_SUBSCRIPTION_NAMESPACE"] = case.tenant_namespace
    os.environ["GATEWAY_HOST"] = case.route_host
    os.environ["MAAS_API_BASE_URL"] = case.api_base_url
    os.environ["E2E_GATEWAY_AUTH_POLICY_NAME"] = case.gateway_authpolicy_name
    os.environ["E2E_MODEL_NAMESPACE"] = case.model_namespace
    try:
        yield
    finally:
        for key, value in saved.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value


def teardown_worker_tenant(case: WorkerTenantContext) -> None:
    # Namespace finalization can outlive the pytest worker when KServe-owned
    # resources are still terminating. Submit deletion without waiting so a
    # successful test run is not converted into a teardown timeout.
    try:
        _oc_run(
            ["delete", "namespace", case.model_namespace, "--ignore-not-found", "--wait=false"],
            timeout=30,
        )
    finally:
        cleanup_discovery_case(case.tenant_case())
