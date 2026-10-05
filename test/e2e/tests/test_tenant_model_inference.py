"""
E2E tests for multi-tenant model inference routing.

These tests validate that:
  - Models created in tenant namespaces route through tenant gateways
  - Inference requests work end-to-end through tenant gateways
  - Tenant isolation is enforced (tenant A cannot access tenant B's models via B's gateway)
  - Gateway-level AuthPolicy controls access

Prerequisites:
  - AITenant CRD available
  - Tenant namespace discovery enabled on controller
  - KServe controller running
  - Gateway infrastructure (openshift-ingress)
"""

import contextlib
import json
import logging

import pytest
import requests

import test_helper
from multitenancy_helpers import (
    GATEWAY_NAMESPACE,
    MODEL_BACKEND_READY_TIMEOUT,
    TLS_VERIFY,
    _oc_run,
    bootstrap_aitenant_tenant,
    cleanup_discovery_case,
    create_api_key_at,
    delete_maas_auth_policy,
    delete_maas_subscription,
    get_json_or_none,
    is_gateway_auth_denial,
    make_tenant_model_accessible,
    new_named_tenant_case,
    per_tenant_gateway_policy_names,
    provision_tenant_model,
    redact_sensitive,
    require_aitenant_crd,
    response_summary,
    wait_for_route_auth_enforced,
)

from test_helper import (
    _check_ipp_pods_deployed,
    _create_llmis,
    _delete_cr,
    _get_cluster_token,
    _poll_status,
    _request_with_gateway_retry,
    chat,
)

log = logging.getLogger(__name__)

pytestmark = pytest.mark.xdist_group("tenant_inference")


# Multi-tenant model inference tests are enabled by default (Phase 1 implementation)
# These tests validate that models route through tenant gateways correctly


def _get_tenant_gateway_url(gateway_name: str) -> str:
    """Get the external URL for a tenant gateway via its OpenShift Route."""
    result = _oc_run(
        ["get", "route", f"{gateway_name}-route", "-n", GATEWAY_NAMESPACE, "-o", "json"]
    )
    if result.returncode != 0:
        raise RuntimeError(
            f"Failed to get route for gateway {gateway_name}: {result.stderr.strip()}"
        )
    route = json.loads(result.stdout)
    host = route["spec"]["host"]
    return f"https://{host}"


@contextlib.contextmanager
def _tenant_gateway_auth_policy(gateway_name: str):
    """Point test_helper's gateway AuthPolicy re-checks at a tenant gateway.

    _poll_status and _request_with_gateway_retry re-check GATEWAY_AUTH_POLICY_NAME
    (the default gateway's policy) on transient replies, which says nothing about
    a tenant gateway.
    """
    original = test_helper.GATEWAY_AUTH_POLICY_NAME
    test_helper.GATEWAY_AUTH_POLICY_NAME = per_tenant_gateway_policy_names(
        gateway_name, gateway_name
    )["gateway_authpolicy"]
    try:
        yield
    finally:
        test_helper.GATEWAY_AUTH_POLICY_NAME = original


def _wait_for_chat_ok(case: dict, gateway_url: str, api_key: str) -> requests.Response:
    """Poll a known-good chat completion until the new key is served with 200."""
    with _tenant_gateway_auth_policy(case["gateway_name"]):
        return _poll_status(
            api_key,
            200,
            path=case["model_path"],
            model_name="facebook/opt-125m",
            timeout=120,
            inference_fn=lambda key, path, extra_headers, model_name: chat(
                "Say hello in one word",
                f"{gateway_url}{path}",
                {"Authorization": f"Bearer {key}"},
                model_name=model_name,
            ),
        )


@pytest.fixture(scope="module")
def tenant_inference_cases():
    """Set up two tenants with models for inference testing."""
    require_aitenant_crd()
    case_a = new_named_tenant_case("e2e-inf-a")
    case_b = new_named_tenant_case("e2e-inf-b")

    try:
        # Bootstrap tenants
        for case in (case_a, case_b):
            bootstrap_aitenant_tenant(case)

        # Create both LLMInferenceServices before waiting on either. The llmisvc
        # controller reconciles them one at a time and runs minutes behind under
        # parallel load, so B should queue while A's wait runs, not after it.
        for case in (case_a, case_b):
            # Track model name early for cleanup
            case["model_name"] = f"test-model-{case['suffix']}"
            _create_llmis(
                case["model_name"],
                case["tenant_ns"],
                case["gateway_name"],
                GATEWAY_NAMESPACE,
            )

        for case in (case_a, case_b):
            model_name = case["model_name"]
            # Re-applies the LLMIS created above unchanged, then waits for it.
            provision_tenant_model(
                model_name,
                case["tenant_ns"],
                case["gateway_name"],
                ready_timeout=MODEL_BACKEND_READY_TIMEOUT,
            )
            make_tenant_model_accessible(
                model_name,
                case["tenant_ns"],
                f"{model_name}-auth",
                f"{model_name}-sub",
                token_limit=10000,
                gateway_name=case["gateway_name"],
            )
            # Store model path (with /v1 for OpenAI API compatibility)
            case["model_path"] = f"/{case['tenant_ns']}/{model_name}/v1"

        for case in (case_a, case_b):
            wait_for_route_auth_enforced(
                f"{_get_tenant_gateway_url(case['gateway_name'])}{case['model_path']}"
            )

        yield case_a, case_b

    finally:
        # Cleanup
        for case in (case_a, case_b):
            if "model_name" in case:
                delete_maas_auth_policy(f"{case['model_name']}-auth", case["tenant_ns"])
                delete_maas_subscription(f"{case['model_name']}-sub", case["tenant_ns"])
                _delete_cr("maasmodelref", case["model_name"], case["tenant_ns"])
                _delete_cr("llminferenceservice", case["model_name"], case["tenant_ns"])

        cleanup_discovery_case(case_a)
        cleanup_discovery_case(case_b)


class TestTenantModelInference:
    """Multi-tenant model inference routing tests."""

    def test_model_routes_through_tenant_gateway(self, tenant_inference_cases):
        """Verify models created in tenant namespaces route through tenant gateways."""
        case_a, _ = tenant_inference_cases

        # Check MaaSModelRef status shows tenant gateway
        model_ref = get_json_or_none("maasmodelref", case_a["model_name"], case_a["tenant_ns"])
        assert model_ref is not None, f"MaaSModelRef {case_a['tenant_ns']}/{case_a['model_name']} not found"

        status = model_ref.get("status", {})
        assert status.get("httpRouteGatewayName") == case_a["gateway_name"], (
            f"Expected gateway {case_a['gateway_name']}, "
            f"got {status.get('httpRouteGatewayName')}"
        )
        assert status.get("httpRouteGatewayNamespace") == GATEWAY_NAMESPACE

    def test_inference_succeeds_through_tenant_gateway(self, tenant_inference_cases):
        """Happy path: inference request succeeds through tenant A's gateway."""
        case_a, _ = tenant_inference_cases

        # Get tenant gateway URL
        gateway_url = _get_tenant_gateway_url(case_a["gateway_name"])

        # Create API key via tenant's maas-api
        oc_token = _get_cluster_token()

        api_key_response = create_api_key_at(
            f"{gateway_url}/maas-api",
            oc_token,
            f"e2e-tenant-inference-{case_a['suffix']}",
            subscription=f"{case_a['model_name']}-sub",
        )

        assert api_key_response.status_code in (200, 201), (
            f"Failed to create API key: {api_key_response.status_code} "
            f"{redact_sensitive(api_key_response.text)}"
        )

        api_key = api_key_response.json().get("key")
        assert api_key, f"API key missing in response: {redact_sensitive(api_key_response.json())}"

        # Send inference request through tenant gateway
        response = _wait_for_chat_ok(case_a, gateway_url, api_key)

        # Verify response structure
        data = response.json()
        assert "choices" in data, f"Response missing 'choices': {redact_sensitive(data)}"
        assert len(data["choices"]) > 0, f"Empty choices in response: {redact_sensitive(data)}"

    def test_tenant_isolation_cross_gateway_blocked(self, tenant_inference_cases):
        """Tenant isolation: tenant A cannot access tenant B's model via B's gateway."""
        case_a, case_b = tenant_inference_cases

        # Get gateway URLs
        gateway_a_url = _get_tenant_gateway_url(case_a["gateway_name"])
        gateway_b_url = _get_tenant_gateway_url(case_b["gateway_name"])
        model_b_url = f"{gateway_b_url}{case_b['model_path']}"

        # Create API key for tenant A through tenant A's gateway
        oc_token = _get_cluster_token()

        api_key_response = create_api_key_at(
            f"{gateway_a_url}/maas-api",
            oc_token,
            f"e2e-cross-tenant-{case_a['suffix']}",
            subscription=f"{case_a['model_name']}-sub",
        )
        assert api_key_response.status_code in (200, 201), (
            f"Failed to create API key: {api_key_response.status_code} "
            f"{redact_sensitive(api_key_response.text)}"
        )
        api_key_body = api_key_response.json()
        api_key = api_key_body.get("key")
        assert api_key, (
            "API key creation returned success without key material: "
            f"{redact_sensitive(api_key_body)}"
        )

        # Positive control: the key is served on A's own gateway, so a rejection
        # on B below is about the tenant boundary, not a key that does not work yet.
        _wait_for_chat_ok(case_a, gateway_a_url, api_key)

        # A 200 below must mean a key-validation leak, not a route that B's
        # gateway has not programmed auth for yet.
        wait_for_route_auth_enforced(
            model_b_url,
            what=f"B route not enforcing auth yet: {model_b_url} kept accepting an unknown API key",
        )

        # Try to access tenant B's model through tenant B's gateway using tenant A's API key.
        # Empty 401/403 are retried; the last reply is checked as B's verdict.
        with _tenant_gateway_auth_policy(case_b["gateway_name"]):
            response = _request_with_gateway_retry(
                requests.post,
                f"{model_b_url}/chat/completions",
                headers={"Authorization": f"Bearer {api_key}"},
                json={"model": "facebook/opt-125m", "messages": [{"role": "user", "content": "Say hello"}]},
            )

        # Should be rejected (401 Unauthorized or 403 Forbidden) by B's auth
        assert is_gateway_auth_denial(response), (
            f"Expected B's auth to reject tenant A's key with 401/403, got {response.status_code}. "
            f"Tenant A should not access tenant B's model via B's gateway. "
            f"Response: {response_summary(response, max_body=500)}"
        )


# ─── Body-based routing ────────────────────────────────────────────────────


requires_ipp = pytest.mark.skipif(
    not _check_ipp_pods_deployed(),
    reason="Payload-processing (IPP) pods not deployed; body routing tests require IPP",
)


def _create_tenant_api_key(gateway_url, case):
    """Create an API key for a tenant and return (api_key, gateway_url)."""
    oc_token = _get_cluster_token()
    r = create_api_key_at(
        f"{gateway_url}/maas-api",
        oc_token,
        f"e2e-body-routing-{case['suffix']}",
        subscription=f"{case['model_name']}-sub",
    )
    assert r.status_code in (200, 201), (
        f"Failed to create API key: {r.status_code} {redact_sensitive(r.text)}"
    )
    api_key = r.json().get("key")
    assert api_key, f"API key missing in response: {redact_sensitive(r.json())}"
    return api_key


@requires_ipp
class TestTenantBodyRouting:
    """Verify body-based routing in a multi-tenant context.

    IPP pre-processing extracts the ``model`` field from the JSON body and
    sets the ``X-Gateway-Model-Name`` header. These tests prove the body
    model field drives routing: a correct model succeeds while a wrong
    model is rejected by the model-provider-resolver plugin.
    """

    def _post_chat(self, gateway_url, model_path, api_key, body):
        url = f"{gateway_url}{model_path}/chat/completions"
        headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {api_key}",
        }
        return requests.post(url, headers=headers, json=body, timeout=30, verify=TLS_VERIFY)

    @pytest.mark.parametrize(
        "body, should_succeed",
        [
            (
                {"model": "facebook/opt-125m", "messages": [{"role": "user", "content": "hello"}]},
                True,
            ),
            (
                {"model": "nonexistent-model", "messages": [{"role": "user", "content": "hello"}]},
                False,
            ),
            (
                {"messages": [{"role": "user", "content": "hello"}]},
                False,
            ),
        ],
        ids=["correct-model", "wrong-model", "missing-model"],
    )
    def test_model_in_body_routing(self, tenant_inference_cases, body, should_succeed):
        """Body routing preserves the success and rejection cases for model identity."""
        case_a, _ = tenant_inference_cases
        gateway_url = _get_tenant_gateway_url(case_a["gateway_name"])
        api_key = _create_tenant_api_key(gateway_url, case_a)
        # Rejections below only count once the key is known to be served.
        _wait_for_chat_ok(case_a, gateway_url, api_key)

        r = self._post_chat(gateway_url, case_a["model_path"], api_key, body)
        if should_succeed:
            assert r.status_code == 200, (
                f"Expected 200 with correct model in body, got {r.status_code}. "
                f"Response: {redact_sensitive(r.text[:500])}"
            )
            data = r.json()
            assert "choices" in data, f"Response missing 'choices': {redact_sensitive(data)}"
        else:
            assert r.status_code != 200, (
                f"Expected rejection for model body {body!r}, got 200. "
                "Body routing may not be active — request succeeded via path routing alone."
            )
        log.info("Body routing (%s): HTTP %d", "success" if should_succeed else "rejection", r.status_code)

    def test_each_tenant_routes_to_own_model(self, tenant_inference_cases):
        """Both tenants route correctly with their own model in body."""
        case_a, case_b = tenant_inference_cases

        for case in (case_a, case_b):
            gateway_url = _get_tenant_gateway_url(case["gateway_name"])
            api_key = _create_tenant_api_key(gateway_url, case)
            _wait_for_chat_ok(case, gateway_url, api_key)

            r = self._post_chat(gateway_url, case["model_path"], api_key, {
                "model": "facebook/opt-125m",
                "messages": [{"role": "user", "content": "hello"}],
            })
            assert r.status_code == 200, (
                f"Tenant {case['gateway_name']}: expected 200, got {r.status_code}. "
                f"Response: {redact_sensitive(r.text[:500])}"
            )
            data = r.json()
            assert "choices" in data
            log.info(
                "Body routing tenant %s: HTTP %d",
                case["gateway_name"], r.status_code,
            )
