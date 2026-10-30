"""E2E for RHOAIENG-97464: a tenant switched live from legacy IPP to Praxis keeps
serving inference and keeps enforcing auth, with no manual pod deletes.

Gated behind TENANT_EXTERNAL_MODEL_LEGACY_IPP_E2E (needs the legacy Go IPP image
pin); otherwise the tenant comes up on praxis directly and there is no switch.
"""

from __future__ import annotations

import json
import logging
import time

import pytest
import requests
from multitenancy_helpers import (
    GATEWAY_NAMESPACE,
    PAYLOAD_PROCESSING_TYPE_IPP,
    PAYLOAD_PROCESSING_TYPE_PRAXIS,
    TLS_VERIFY,
    _oc_run,
    bootstrap_aitenant_tenant,
    cleanup_discovery_case,
    ensure_payload_processing_type_on_tenant_config,
    extproc_deployment_uses_praxis,
    make_tenant_model_accessible,
    new_named_tenant_case,
    per_tenant_ipp_names,
    provision_tenant_model,
    redact_sensitive,
    require_aitenant_crd,
    require_tenant_namespace_discovery,
    wait_for_per_tenant_ipp_ready,
)
from tenant_external_model_helpers import require_legacy_ipp_coexistence_support
from test_helper import MODEL_NAME, _get_cluster_token

log = logging.getLogger(__name__)

pytestmark = pytest.mark.xdist_group("tenant_ipp")

SWITCH_RECONCILE_TIMEOUT = 300
POLL_INTERVAL = 5
GATEWAY_RETRIES = 6
GATEWAY_RETRY_DELAY = 5


def _request_with_gateway_retry(method, url, retries=GATEWAY_RETRIES, **kwargs):
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
            time.sleep(GATEWAY_RETRY_DELAY)
            continue
        return response
    return response


def _tenant_gateway_url(gateway_name: str) -> str:
    result = _oc_run(["get", "route", f"{gateway_name}-route", "-n", GATEWAY_NAMESPACE, "-o", "json"])
    if result.returncode != 0:
        raise RuntimeError(f"Failed to get route for gateway {gateway_name}: {result.stderr.strip()}")
    return f"https://{json.loads(result.stdout)['spec']['host']}"


def _create_tenant_api_key(gateway_url: str, case: dict[str, str], subscription_name: str) -> str:
    response = _request_with_gateway_retry(
        requests.post,
        f"{gateway_url.rstrip('/')}/maas-api/v1/api-keys",
        headers={"Authorization": f"Bearer {_get_cluster_token()}", "Content-Type": "application/json"},
        json={"name": f"e2e-switch-{case['suffix']}", "subscription": subscription_name},
    )
    assert response.status_code in (200, 201), (
        f"Failed to create tenant API key: {response.status_code} {redact_sensitive(response.text)}"
    )
    api_key = response.json().get("key")
    assert api_key, f"API key missing in response: {redact_sensitive(response.json())}"
    return api_key


def _post_hybrid_chat(gateway_url: str, model_path: str, api_key: str, *, model_name: str = MODEL_NAME):
    return _request_with_gateway_retry(
        requests.post,
        f"{gateway_url.rstrip('/')}{model_path}/v1/chat/completions",
        headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
        json={"model": model_name, "messages": [{"role": "user", "content": "switch continuity"}], "max_tokens": 3},
    )


def _wait_until_praxis(deployment_name, *, timeout=SWITCH_RECONCILE_TIMEOUT):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        last = extproc_deployment_uses_praxis(deployment_name)
        if last:
            return
        time.sleep(POLL_INTERVAL)
    raise AssertionError(f"{deployment_name} did not become praxis-extproc within {timeout}s (last={last!r})")


@pytest.fixture(scope="module")
def switch_case():
    require_legacy_ipp_coexistence_support()
    require_tenant_namespace_discovery()
    require_aitenant_crd()
    case = new_named_tenant_case("e2e-ipp-cont")
    try:
        log.info("Bootstrapping tenant %s on legacy Go IPP", case["tenant_label_name"])
        bootstrap_aitenant_tenant(case, payload_processing_type=PAYLOAD_PROCESSING_TYPE_IPP)
        wait_for_per_tenant_ipp_ready(case)
        names = per_tenant_ipp_names(case["tenant_label_name"])
        if extproc_deployment_uses_praxis(names["processing_deployment"]):
            pytest.skip(
                f"{names['processing_deployment']} came up on praxis-extproc; "
                "legacy Go IPP image pin not active, nothing to switch"
            )

        model_name = f"ipp-cont-{case['suffix']}"
        provision_tenant_model(model_name, case["tenant_ns"], case["gateway_name"])
        make_tenant_model_accessible(
            model_name,
            case["tenant_ns"],
            f"{model_name}-auth",
            f"{model_name}-sub",
            gateway_name=case["gateway_name"],
        )
        case["model_name"] = model_name
        case["model_path"] = f"/{case['tenant_ns']}/{model_name}"
        case["gateway_url"] = _tenant_gateway_url(case["gateway_name"])
        case["api_key"] = _create_tenant_api_key(case["gateway_url"], case, f"{model_name}-sub")
        yield case
    finally:
        cleanup_discovery_case(case)


class TestIppSwitchTrafficContinuity:
    """A live IPP→Praxis switch keeps inference serving and auth enforced."""

    def _assert_serves(self, case, phase: str):
        response = _post_hybrid_chat(case["gateway_url"], case["model_path"], case["api_key"])
        assert response.status_code == 200, (
            f"[{phase}] inference failed: {response.status_code} {redact_sensitive(response.text[:300])}"
        )

    def _assert_auth_enforced(self, case, phase: str):
        response = _post_hybrid_chat(case["gateway_url"], case["model_path"], "sk-bogus-not-a-real-key")
        assert response.status_code in (401, 403), (
            f"[{phase}] expected 401/403 for a bogus key, got {response.status_code}"
        )

    def test_switch_preserves_inference_and_auth(self, switch_case):
        names = per_tenant_ipp_names(switch_case["tenant_label_name"])
        assert not extproc_deployment_uses_praxis(names["processing_deployment"])

        log.info("Baseline on legacy Go IPP: inference + auth")
        self._assert_serves(switch_case, "ipp")
        self._assert_auth_enforced(switch_case, "ipp")

        ensure_payload_processing_type_on_tenant_config(
            switch_case["tenant_ns"], PAYLOAD_PROCESSING_TYPE_PRAXIS
        )
        log.info("Switched %s to praxis; waiting for reconcile", switch_case["tenant_label_name"])
        _wait_until_praxis(names["processing_deployment"])
        _wait_until_praxis(names["pre_processing_deployment"])

        log.info("After switch on praxis-extproc: inference continuity + auth")
        self._assert_serves(switch_case, "praxis")
        self._assert_auth_enforced(switch_case, "praxis")
