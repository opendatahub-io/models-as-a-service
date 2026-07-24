"""
E2E tests for per-tenant IPP (payload-processing) isolation.

Validates that each AITenant receives dedicated IPP Deployments, Services,
EnvoyFilters, and env configuration in the gateway namespace, that inference
traffic reaches the matching IPP stack, and that tenant-scoped resources are
removed when the AITenant is deleted.

Requires maas-controller with per-tenant IPP reconciliation enabled.
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
    deployment_log_snapshot,
    envoyfilter_grpc_cluster_names,
    envoyfilter_target_gateway,
    get_ipp_deployment_env,
    get_json_or_none,
    ipp_logs_show_recent_activity,
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
    _wait_reconcile,
)

log = logging.getLogger(__name__)

GATEWAY_PROPAGATION_RETRIES = 6
GATEWAY_PROPAGATION_DELAY = 5


def _request_with_gateway_retry(method, url, retries=GATEWAY_PROPAGATION_RETRIES, **kwargs):
    """Retry transient gateway/auth propagation errors (empty 403, Authorino AUTH_FAILURE)."""
    for attempt in range(1, retries + 1):
        response = method(
            url,
            timeout=kwargs.pop("timeout", 45),
            verify=kwargs.pop("verify", TLS_VERIFY),
            **kwargs,
        )
        retryable = (response.status_code == 403 and not response.text.strip()) or (
            response.status_code == 500 and "AUTH_FAILURE" in response.text
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
        return response
    return response

requires_default_ipp = pytest.mark.skipif(
    not _check_ipp_pods_deployed(),
    reason="Default payload-processing IPP stack is not ready",
)


@pytest.fixture(scope="module")
def ipp_tenant_cases():
    require_tenant_namespace_discovery()
    require_aitenant_crd()
    case_a = new_named_tenant_case("e2e-ipp-a")
    case_b = new_named_tenant_case("e2e-ipp-b")
    try:
        for case in (case_a, case_b):
            bootstrap_aitenant_tenant(case)
            wait_for_per_tenant_ipp_ready(case)
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
        json={"name": "e2e-ipp-default", "subscription": subscription},
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
            "name": f"e2e-ipp-{case['suffix']}",
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
    """Send hybrid BBR: model-specific URL path plus served model name in the body."""
    return requests.post(
        f"{gateway_url.rstrip('/')}{model_path}/v1/chat/completions",
        headers={
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
        },
        json={
            "model": model_name,
            "messages": [{"role": "user", "content": "ipp routing test"}],
            "max_tokens": 3,
        },
        timeout=45,
        verify=TLS_VERIFY,
    )


class TestPerTenantIPPInfrastructure:
    """Verify per-tenant IPP resources reconcile in the gateway namespace."""

    def test_per_tenant_ipp_deployments_exist(self, ipp_tenant_cases):
        for case in ipp_tenant_cases:
            names = per_tenant_ipp_names(case["tenant_label_name"])
            processing = wait_for_deployment_available(
                names["processing_deployment"], GATEWAY_NAMESPACE, timeout=240
            )
            pre_processing = wait_for_deployment_available(
                names["pre_processing_deployment"], GATEWAY_NAMESPACE, timeout=240
            )
            assert processing["metadata"]["name"] == names["processing_deployment"]
            assert pre_processing["metadata"]["name"] == names["pre_processing_deployment"]

            service = wait_for_json("service", names["processing_service"], GATEWAY_NAMESPACE, timeout=180)
            assert service["metadata"]["name"] == names["processing_service"]

    def test_per_tenant_ipp_env_vars(self, ipp_tenant_cases):
        for case in ipp_tenant_cases:
            names = per_tenant_ipp_names(case["tenant_label_name"])
            env = get_ipp_deployment_env(names["processing_deployment"], GATEWAY_NAMESPACE)
            assert env.get("GATEWAY_NAME") == case["gateway_name"], (
                f"{names['processing_deployment']} GATEWAY_NAME mismatch: {env!r}"
            )
            assert env.get("GATEWAY_NAMESPACE") == GATEWAY_NAMESPACE, (
                f"{names['processing_deployment']} GATEWAY_NAMESPACE mismatch: {env!r}"
            )
            assert env.get("TENANT_NAMESPACE") == case["tenant_ns"], (
                f"{names['processing_deployment']} TENANT_NAMESPACE mismatch: {env!r}"
            )

    def test_per_tenant_envoyfilter_target_ref_isolated(self, ipp_tenant_cases):
        for case in ipp_tenant_cases:
            names = per_tenant_ipp_names(case["tenant_label_name"])
            target = envoyfilter_target_gateway(names["envoyfilter"], GATEWAY_NAMESPACE)
            assert target == case["gateway_name"], (
                f"{names['envoyfilter']} must target gateway {case['gateway_name']}, got {target!r}"
            )

        default_target = envoyfilter_target_gateway("payload-processing", GATEWAY_NAMESPACE)
        assert default_target == DEFAULT_GATEWAY_NAME, (
            f"default payload-processing EnvoyFilter must target {DEFAULT_GATEWAY_NAME}, "
            f"got {default_target!r}"
        )

    def test_per_tenant_envoyfilter_grpc_clusters(self, ipp_tenant_cases):
        for case in ipp_tenant_cases:
            names = per_tenant_ipp_names(case["tenant_label_name"])
            envoyfilter = wait_for_json("envoyfilter", names["envoyfilter"], GATEWAY_NAMESPACE, timeout=180)
            clusters = envoyfilter_grpc_cluster_names(envoyfilter)
            want_processing = (
                f"outbound|9004||{names['processing_service']}.{GATEWAY_NAMESPACE}.svc.cluster.local"
            )
            want_pre_processing = (
                f"outbound|9004||{names['pre_processing_service']}.{GATEWAY_NAMESPACE}.svc.cluster.local"
            )
            assert want_processing in clusters, (
                f"{names['envoyfilter']} missing processing cluster {want_processing!r}; got {clusters!r}"
            )
            assert want_pre_processing in clusters, (
                f"{names['envoyfilter']} missing pre-processing cluster {want_pre_processing!r}; "
                f"got {clusters!r}"
            )

    def test_default_tenant_keeps_legacy_ipp_names(self):
        default_names = per_tenant_ipp_names(DEFAULT_AITENANT_NAME)
        assert default_names["processing_deployment"] == "payload-processing"
        assert default_names["pre_processing_deployment"] == "payload-pre-processing"
        assert get_json_or_none("deployment", "payload-processing", GATEWAY_NAMESPACE) is not None
        assert get_json_or_none("deployment", "payload-pre-processing", GATEWAY_NAMESPACE) is not None

    def test_multiple_tenant_ipp_stacks_coexist(self, ipp_tenant_cases):
        deployment_names = {
            per_tenant_ipp_names(case["tenant_label_name"])["processing_deployment"]
            for case in ipp_tenant_cases
        }
        deployment_names.add("payload-processing")
        assert len(deployment_names) == len(ipp_tenant_cases) + 1
        for name in deployment_names:
            deployment = get_json_or_none("deployment", name, GATEWAY_NAMESPACE)
            assert deployment is not None, f"missing IPP deployment {name}"

    def test_per_tenant_networkpolicy_when_applied(self, ipp_tenant_cases):
        """Per-tenant NetworkPolicy is optional on managed OpenShift ingress namespaces."""
        for case in ipp_tenant_cases:
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


@requires_default_ipp
class TestPerTenantIPPRouting:
    """Verify inference traffic reaches the tenant-scoped IPP stack."""

    @pytest.fixture(scope="class")
    def routing_case(self, ipp_tenant_cases):
        case_a, _ = ipp_tenant_cases
        model_name = f"ipp-route-{case_a['suffix']}"
        case_a["model_name"] = model_name
        case_a["model_path"] = f"/{case_a['tenant_ns']}/{model_name}"
        provision_tenant_model(model_name, case_a["tenant_ns"], case_a["gateway_name"])
        make_tenant_model_accessible(
            model_name,
            case_a["tenant_ns"],
            f"{model_name}-auth",
            f"{model_name}-sub",
        )
        _wait_reconcile()
        return case_a

    def test_default_gateway_hits_default_ipp_only(self, ipp_tenant_cases):
        _, case_b = ipp_tenant_cases
        default_names = per_tenant_ipp_names(DEFAULT_AITENANT_NAME)
        tenant_names = per_tenant_ipp_names(case_b["tenant_label_name"])

        time.sleep(2)
        api_key = _create_default_api_key()
        response = _post_hybrid_chat(_gateway_url(), MODEL_PATH, api_key)
        assert response.status_code == 200, (
            f"Default gateway hybrid BBR failed: {response.status_code} "
            f"{redact_sensitive(response.text[:500])}"
        )

        time.sleep(2)
        default_logs = deployment_log_snapshot(
            default_names["processing_deployment"], since="1m"
        )
        tenant_logs = deployment_log_snapshot(
            tenant_names["processing_deployment"], since="1m"
        )
        assert ipp_logs_show_recent_activity(default_logs), (
            "Expected ext_proc activity in default payload-processing logs"
        )
        assert not ipp_logs_show_recent_activity(tenant_logs), (
            "Tenant IPP logs should stay quiet for default-gateway traffic"
        )

    def test_tenant_gateway_hits_tenant_ipp_only(self, routing_case, ipp_tenant_cases):
        _, case_b = ipp_tenant_cases
        default_names = per_tenant_ipp_names(DEFAULT_AITENANT_NAME)
        tenant_names = per_tenant_ipp_names(routing_case["tenant_label_name"])
        other_names = per_tenant_ipp_names(case_b["tenant_label_name"])

        gateway_url = _get_tenant_gateway_url(routing_case["gateway_name"])
        api_key = _create_tenant_api_key(
            gateway_url,
            routing_case,
            f"{routing_case['model_name']}-sub",
        )

        time.sleep(2)
        response = _post_hybrid_chat(
            gateway_url,
            routing_case["model_path"],
            api_key,
        )
        assert response.status_code == 200, (
            f"Tenant gateway hybrid BBR failed: {response.status_code} "
            f"{redact_sensitive(response.text[:500])}"
        )

        time.sleep(2)
        tenant_logs = deployment_log_snapshot(
            tenant_names["processing_deployment"], since="1m"
        )
        default_logs = deployment_log_snapshot(
            default_names["processing_deployment"], since="1m"
        )
        other_logs = deployment_log_snapshot(
            other_names["processing_deployment"], since="1m"
        )
        assert ipp_logs_show_recent_activity(tenant_logs), (
            f"Expected ext_proc activity in {tenant_names['processing_deployment']} logs"
        )
        assert not ipp_logs_show_recent_activity(other_logs), (
            "Unrelated tenant IPP logs should stay quiet for this gateway request"
        )
        log.info(
            "Tenant routing log check complete (default IPP activity=%s)",
            ipp_logs_show_recent_activity(default_logs),
        )


class TestPerTenantIPPCleanup:
    """Verify tenant-scoped IPP resources are removed when the AITenant is deleted."""

    def test_ipp_resources_removed_on_aitenant_delete(self):
        case = new_named_tenant_case("e2e-ipp-cleanup")
        names = per_tenant_ipp_names(case["tenant_label_name"])
        try:
            bootstrap_aitenant_tenant(case)
            wait_for_per_tenant_ipp_ready(case)
            assert get_json_or_none("deployment", names["processing_deployment"], GATEWAY_NAMESPACE)

            cleanup_discovery_case(case, delete_gateway=True)
            wait_for_aitenant_cleanup_resources_deleted(case, timeout=240)

            assert get_json_or_none("deployment", names["processing_deployment"], GATEWAY_NAMESPACE) is None
            assert get_json_or_none("envoyfilter", names["envoyfilter"], GATEWAY_NAMESPACE) is None
            wait_for_not_found("deployment", names["pre_processing_deployment"], GATEWAY_NAMESPACE, timeout=60)
        finally:
            cleanup_discovery_case(case, delete_gateway=True)

        assert get_json_or_none("deployment", "payload-processing", GATEWAY_NAMESPACE) is not None
        assert ipp_tenant_id(case["tenant_label_name"]) != ""
