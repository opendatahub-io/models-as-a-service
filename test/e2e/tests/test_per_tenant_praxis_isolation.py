"""
E2E tests for AIGC/praxis-owned per-tenant payload-processing isolation.

Validates dedicated praxis-extproc Deployments, Services, EnvoyFilters, and routing
isolation for each AITenant on the praxis dataplane. Legacy Go IPP env configuration
(GATEWAY_NAME / TENANT_NAMESPACE) is not asserted. Positive routing proof uses hybrid
BBR success plus body-model rejection because praxis-extproc is quiet at INFO.

Requires maas-controller per-tenant reconciliation and default-tenant praxis-extproc.
"""

from __future__ import annotations

import json
import logging
import os
import time

import pytest
import requests

from multitenancy_helpers import (
    DEFAULT_AITENANT_NAME,
    DEFAULT_GATEWAY_NAME,
    GATEWAY_NAMESPACE,
    LABEL_TENANT_INSTANCE,
    TLS_VERIFY,
    _oc_run,
    bootstrap_aitenant_tenant,
    cleanup_discovery_case,
    envoyfilter_grpc_cluster_names,
    envoyfilter_target_gateway,
    extproc_deployment_uses_praxis,
    get_json_or_none,
    ipp_tenant_id,
    make_tenant_model_accessible,
    new_named_tenant_case,
    per_tenant_ipp_names,
    provision_tenant_model,
    redact_sensitive,
    require_aitenant_crd,
    require_tenant_namespace_discovery,
    wait_for_aitenant_cleanup_resources_deleted,
    wait_for_deployment_available,
    wait_for_json,
    wait_for_not_found,
    wait_for_per_tenant_ipp_ready,
)
from test_helper import (
    MODEL_NAME,
    MODEL_PATH,
    _check_ipp_pods_deployed,
    _gateway_url,
    _get_cluster_token,
    _maas_api_url,
    _wait_for_gateway_auth_enforced,
)

log = logging.getLogger(__name__)

pytestmark = pytest.mark.xdist_group("tenant_ipp")

GATEWAY_PROPAGATION_RETRIES = 6
GATEWAY_PROPAGATION_DELAY = 5

PRAXIS_EXT_PROC_CLUSTER_NAMES = (
    "payload-processing-extproc",
    "payload-pre-processing-extproc",
)


def _skip_unless_default_praxis():
    if not _check_ipp_pods_deployed():
        pytest.skip("Default payload-processing stack is not ready")
    if not extproc_deployment_uses_praxis("payload-processing"):
        pytest.skip("default tenant payload-processing is not praxis-extproc")


def _request_with_gateway_retry(method, url, retries=GATEWAY_PROPAGATION_RETRIES, **kwargs):
    from test_helper import _is_transient_gateway_response

    for attempt in range(1, retries + 1):
        response = method(
            url,
            timeout=kwargs.pop("timeout", 45),
            verify=kwargs.pop("verify", TLS_VERIFY),
            **kwargs,
        )
        retryable = _is_transient_gateway_response(response) or (
            response.status_code == 403 and "Access denied" in response.text
        )
        if retryable and attempt < retries:
            log.info(
                "Gateway returned %d (attempt %d/%d), retrying in %ds...",
                response.status_code,
                attempt,
                retries,
                GATEWAY_PROPAGATION_DELAY,
            )
            time.sleep(GATEWAY_PROPAGATION_DELAY)
            continue
        if response.status_code not in (200, 201):
            log.info(
                "Gateway returned non-retryable %d (attempt %d/%d, body: %.300s)",
                response.status_code,
                attempt,
                retries,
                response.text[:300],
            )
        return response
    return response


@pytest.fixture(scope="module")
def praxis_tenant_cases():
    _skip_unless_default_praxis()
    require_tenant_namespace_discovery()
    require_aitenant_crd()
    case_a = new_named_tenant_case("e2e-praxis-a")
    case_b = new_named_tenant_case("e2e-praxis-b")
    try:
        for case in (case_a, case_b):
            bootstrap_aitenant_tenant(case)
            wait_for_per_tenant_ipp_ready(case)
            names = per_tenant_ipp_names(case["tenant_label_name"])
            if not extproc_deployment_uses_praxis(names["processing_deployment"]):
                pytest.skip(
                    f"{names['processing_deployment']} is not praxis-extproc "
                    "(per-tenant praxis dataplane not reconciled)"
                )
        yield case_a, case_b
    finally:
        cleanup_discovery_case(case_a)
        cleanup_discovery_case(case_b)


def _get_tenant_gateway_url(gateway_name: str) -> str:
    result = _oc_run(
        ["get", "route", f"{gateway_name}-route", "-n", GATEWAY_NAMESPACE, "-o", "json"]
    )
    if result.returncode != 0:
        raise RuntimeError(
            f"Failed to get route for gateway {gateway_name}: {result.stderr.strip()}"
        )
    route = json.loads(result.stdout)
    return f"https://{route['spec']['host']}"


def _create_default_api_key() -> str:
    oc_token = _get_cluster_token()
    subscription = os.environ.get("E2E_SIMULATOR_SUBSCRIPTION", "simulator-subscription")
    response = _request_with_gateway_retry(
        requests.post,
        f"{_maas_api_url()}/v1/api-keys",
        headers={
            "Authorization": f"Bearer {oc_token}",
            "Content-Type": "application/json",
        },
        json={"name": "e2e-praxis-default", "subscription": subscription},
    )
    assert response.status_code in (200, 201), (
        f"Failed to create default-tenant API key: {response.status_code} "
        f"{redact_sensitive(response.text)}"
    )
    api_key = response.json().get("key")
    assert api_key, f"API key missing in response: {redact_sensitive(response.json())}"
    return api_key


def _create_tenant_api_key(gateway_url: str, case: dict[str, str], subscription_name: str) -> str:
    oc_token = _get_cluster_token()
    response = _request_with_gateway_retry(
        requests.post,
        f"{gateway_url.rstrip('/')}/maas-api/v1/api-keys",
        headers={
            "Authorization": f"Bearer {oc_token}",
            "Content-Type": "application/json",
        },
        json={
            "name": f"e2e-praxis-{case['suffix']}",
            "subscription": subscription_name,
        },
    )
    assert response.status_code in (200, 201), (
        f"Failed to create tenant API key: {response.status_code} "
        f"{redact_sensitive(response.text)}"
    )
    api_key = response.json().get("key")
    assert api_key, f"API key missing in response: {redact_sensitive(response.json())}"
    return api_key


def _post_hybrid_chat(
    gateway_url: str,
    model_path: str,
    api_key: str,
    *,
    model_name: str = MODEL_NAME,
) -> requests.Response:
    return _request_with_gateway_retry(
        requests.post,
        f"{gateway_url.rstrip('/')}{model_path}/v1/chat/completions",
        headers={
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
        },
        json={
            "model": model_name,
            "messages": [{"role": "user", "content": "praxis routing test"}],
            "max_tokens": 3,
        },
    )


def _assert_praxis_envoyfilter_grpc_clusters(envoyfilter: dict, names: dict[str, str]) -> None:
    clusters = envoyfilter_grpc_cluster_names(envoyfilter)
    cluster_blob = " ".join(clusters)
    fixed_ok = all(name in clusters for name in PRAXIS_EXT_PROC_CLUSTER_NAMES)
    service_ok = (
        names["processing_service"] in cluster_blob
        and names["pre_processing_service"] in cluster_blob
    )
    assert fixed_ok or service_ok, (
        f"expected praxis extproc cluster names {PRAXIS_EXT_PROC_CLUSTER_NAMES!r} "
        f"or tenant services {names['processing_service']!r} / "
        f"{names['pre_processing_service']!r}; got {clusters!r}"
    )


class TestPerTenantPraxisInfrastructure:
    """Verify per-tenant praxis-extproc resources reconcile in the gateway namespace."""

    def test_per_tenant_praxis_deployments_exist(self, praxis_tenant_cases):
        for case in praxis_tenant_cases:
            names = per_tenant_ipp_names(case["tenant_label_name"])
            processing = wait_for_deployment_available(
                names["processing_deployment"], GATEWAY_NAMESPACE, timeout=240
            )
            pre_processing = wait_for_deployment_available(
                names["pre_processing_deployment"], GATEWAY_NAMESPACE, timeout=240
            )
            assert processing["metadata"]["name"] == names["processing_deployment"]
            assert pre_processing["metadata"]["name"] == names["pre_processing_deployment"]
            assert extproc_deployment_uses_praxis(names["processing_deployment"]), (
                f"{names['processing_deployment']} must run praxis-extproc"
            )
            assert extproc_deployment_uses_praxis(names["pre_processing_deployment"]), (
                f"{names['pre_processing_deployment']} must run praxis-extproc"
            )

            service = wait_for_json("service", names["processing_service"], GATEWAY_NAMESPACE, timeout=180)
            assert service["metadata"]["name"] == names["processing_service"]

    def test_per_tenant_envoyfilter_workload_selector_isolated(self, praxis_tenant_cases):
        for case in praxis_tenant_cases:
            names = per_tenant_ipp_names(case["tenant_label_name"])
            target = envoyfilter_target_gateway(names["envoyfilter"], GATEWAY_NAMESPACE)
            assert target == case["gateway_name"], (
                f"{names['envoyfilter']} workloadSelector must select gateway "
                f"{case['gateway_name']}, got {target!r}"
            )

        default_target = envoyfilter_target_gateway("payload-processing", GATEWAY_NAMESPACE)
        assert default_target == DEFAULT_GATEWAY_NAME, (
            f"default payload-processing EnvoyFilter workloadSelector must select "
            f"{DEFAULT_GATEWAY_NAME}, got {default_target!r}"
        )

    def test_per_tenant_envoyfilter_grpc_clusters(self, praxis_tenant_cases):
        for case in praxis_tenant_cases:
            names = per_tenant_ipp_names(case["tenant_label_name"])
            envoyfilter = wait_for_json("envoyfilter", names["envoyfilter"], GATEWAY_NAMESPACE, timeout=180)
            _assert_praxis_envoyfilter_grpc_clusters(envoyfilter, names)

    def test_default_tenant_keeps_unsuffixed_payload_processing_names(self):
        _skip_unless_default_praxis()
        default_names = per_tenant_ipp_names(DEFAULT_AITENANT_NAME)
        assert default_names["processing_deployment"] == "payload-processing"
        assert default_names["pre_processing_deployment"] == "payload-pre-processing"
        assert get_json_or_none("deployment", "payload-processing", GATEWAY_NAMESPACE) is not None
        assert get_json_or_none("deployment", "payload-pre-processing", GATEWAY_NAMESPACE) is not None
        assert extproc_deployment_uses_praxis("payload-processing")

    def test_multiple_tenant_praxis_stacks_coexist(self, praxis_tenant_cases):
        deployment_names = {
            per_tenant_ipp_names(case["tenant_label_name"])["processing_deployment"]
            for case in praxis_tenant_cases
        }
        deployment_names.add("payload-processing")
        assert len(deployment_names) == len(praxis_tenant_cases) + 1
        for name in deployment_names:
            deployment = get_json_or_none("deployment", name, GATEWAY_NAMESPACE)
            assert deployment is not None, f"missing praxis deployment {name}"

    def test_per_tenant_networkpolicy_when_applied(self, praxis_tenant_cases):
        for case in praxis_tenant_cases:
            names = per_tenant_ipp_names(case["tenant_label_name"])
            np = get_json_or_none("networkpolicy", names["networkpolicy"], GATEWAY_NAMESPACE)
            if np is None:
                pytest.skip(
                    f"Per-tenant NetworkPolicy {names['networkpolicy']} not present "
                    "(may be blocked by managed-ingress webhook on OpenShift)"
                )
            match_exprs = np["spec"]["podSelector"].get("matchExpressions") or []
            tenant_values = next(
                (expr.get("values") or [] for expr in match_exprs if expr.get("key") == LABEL_TENANT_INSTANCE),
                [],
            )
            assert names["processing_deployment"] in tenant_values


class TestPerTenantPraxisRouting:
    """Verify inference traffic reaches the tenant-scoped praxis-extproc stack."""

    @pytest.fixture(scope="class")
    def routing_case(self, praxis_tenant_cases):
        case_a, _ = praxis_tenant_cases
        model_name = f"praxis-route-{case_a['suffix']}"
        case_a["model_name"] = model_name
        case_a["model_path"] = f"/{case_a['tenant_ns']}/{model_name}"
        provision_tenant_model(model_name, case_a["tenant_ns"], case_a["gateway_name"])
        make_tenant_model_accessible(
            model_name,
            case_a["tenant_ns"],
            f"{model_name}-auth",
            f"{model_name}-sub",
            gateway_name=case_a["gateway_name"],
        )
        return case_a

    def test_default_gateway_hits_default_praxis_only(self, praxis_tenant_cases):
        # Fixture boots sibling tenants so EnvoyFilter isolation coexists with default stack.
        _ = praxis_tenant_cases

        _wait_for_gateway_auth_enforced()
        api_key = _create_default_api_key()
        _TRANSIENT_WARMUP = {403, 500, 502, 503}
        deadline = time.time() + 90
        warmup = None
        while time.time() < deadline:
            warmup = _post_hybrid_chat(_gateway_url(), MODEL_PATH, api_key)
            if warmup.status_code == 200:
                break
            if warmup.status_code not in _TRANSIENT_WARMUP:
                log.warning(
                    "Default gateway warmup got terminal %d, not retrying (body: %.300s)",
                    warmup.status_code,
                    redact_sensitive(warmup.text[:300]),
                )
                break
            log.warning(
                "Default gateway warmup got %d (body: %.300s), retrying...",
                warmup.status_code,
                redact_sensitive(warmup.text[:300]),
            )
            time.sleep(2)
        assert warmup is not None and warmup.status_code == 200, (
            f"Default gateway inference warmup failed: "
            f"{warmup.status_code if warmup is not None else 'no response'} "
            f"{redact_sensitive(warmup.text[:500]) if warmup is not None else ''}"
        )
        response = _post_hybrid_chat(_gateway_url(), MODEL_PATH, api_key)
        assert response.status_code == 200, (
            f"Default gateway hybrid BBR failed: {response.status_code} "
            f"{redact_sensitive(response.text[:500])}"
        )

        wrong_body = _post_hybrid_chat(
            _gateway_url(),
            MODEL_PATH,
            api_key,
            model_name="nonexistent-praxis-route-model",
        )
        assert wrong_body.status_code != 200, (
            "Expected default praxis payload-processing to reject unresolvable body model; "
            f"got {wrong_body.status_code}. Request may have bypassed the processor."
        )
        log.info(
            "Default praxis routing verified via hybrid BBR 200 and body-model rejection (HTTP %d)",
            wrong_body.status_code,
        )

    def test_tenant_gateway_hits_tenant_praxis_only(self, routing_case, praxis_tenant_cases):
        _ = praxis_tenant_cases
        tenant_names = per_tenant_ipp_names(routing_case["tenant_label_name"])

        gateway_url = _get_tenant_gateway_url(routing_case["gateway_name"])
        api_key = _create_tenant_api_key(
            gateway_url,
            routing_case,
            f"{routing_case['model_name']}-sub",
        )

        response = _post_hybrid_chat(
            gateway_url,
            routing_case["model_path"],
            api_key,
        )
        assert response.status_code == 200, (
            f"Tenant gateway hybrid BBR failed: {response.status_code} "
            f"{redact_sensitive(response.text[:500])}"
        )

        wrong_body = _post_hybrid_chat(
            gateway_url,
            routing_case["model_path"],
            api_key,
            model_name="nonexistent-praxis-tenant-model",
        )
        assert wrong_body.status_code != 200, (
            f"Expected tenant praxis stack to reject unresolvable body model; got {wrong_body.status_code}"
        )
        log.info(
            "Tenant praxis routing verified for %s via hybrid BBR and body-model rejection",
            tenant_names["processing_deployment"],
        )


class TestPerTenantPraxisCleanup:
    """Verify tenant-scoped praxis resources are removed when the AITenant is deleted."""

    def test_praxis_resources_removed_on_aitenant_delete(self, praxis_tenant_cases):
        _skip_unless_default_praxis()
        case_a, case_b = praxis_tenant_cases
        names = per_tenant_ipp_names(case_a["tenant_label_name"])
        sibling = per_tenant_ipp_names(case_b["tenant_label_name"])
        assert extproc_deployment_uses_praxis(names["processing_deployment"])
        assert get_json_or_none("deployment", names["processing_deployment"], GATEWAY_NAMESPACE)

        cleanup_discovery_case(case_a, delete_gateway=True)
        wait_for_aitenant_cleanup_resources_deleted(case_a, timeout=240)

        assert get_json_or_none("deployment", names["processing_deployment"], GATEWAY_NAMESPACE) is None
        assert get_json_or_none("envoyfilter", names["envoyfilter"], GATEWAY_NAMESPACE) is None
        wait_for_not_found("deployment", names["pre_processing_deployment"], GATEWAY_NAMESPACE, timeout=60)

        assert get_json_or_none("deployment", sibling["processing_deployment"], GATEWAY_NAMESPACE) is not None
        assert get_json_or_none("deployment", "payload-processing", GATEWAY_NAMESPACE) is not None
        assert ipp_tenant_id(case_a["tenant_label_name"]) != ""
