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

import json
import logging
import subprocess

import pytest
import requests

from multitenancy_helpers import (
    GATEWAY_NAMESPACE,
    TLS_VERIFY,
    _oc_run,
    apply_maas_auth_policy,
    apply_maas_subscription,
    bootstrap_aitenant_tenant,
    cleanup_discovery_case,
    delete_maas_auth_policy,
    delete_maas_subscription,
    get_json_or_none,
    new_named_tenant_case,
    redact_sensitive,
    require_aitenant_crd,
    wait_for_httproute_accepted,
    wait_for_status_phase,
)

from test_helper import (
    _check_ipp_pods_deployed,
    _create_llmis,
    _create_maas_model_ref,
    _delete_cr,
    _get_cluster_token,
    _wait_reconcile,
    chat,
)

log = logging.getLogger(__name__)


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

        # Create models in each tenant namespace
        for case in (case_a, case_b):
            model_name = f"test-model-{case['suffix']}"
            # Track model name early for cleanup
            case["model_name"] = model_name

            # Create LLMIS pointing to tenant gateway
            _create_llmis(
                model_name,
                case["tenant_ns"],
                case["gateway_name"],
                GATEWAY_NAMESPACE,
            )

            # Wait for HTTPRoute to be accepted on the tenant gateway
            wait_for_httproute_accepted(
                f"{model_name}-kserve-route",
                case["tenant_ns"],
                case["gateway_name"],
                timeout=180,
            )

            # Create MaaSModelRef
            _create_maas_model_ref(model_name, case["tenant_ns"], model_name)

            # Create subscription and auth policy (both needed for Ready status)
            apply_maas_subscription(
                f"{model_name}-sub",
                case["tenant_ns"],
                model_ref=model_name,
                model_namespace=case["tenant_ns"],
                token_limit=10000,
            )
            apply_maas_auth_policy(
                f"{model_name}-auth",
                case["tenant_ns"],
                model_ref=model_name,
                model_namespace=case["tenant_ns"],
            )

            # Wait for MaaSModelRef to report Ready
            wait_for_status_phase(
                "maasmodelref",
                model_name,
                case["tenant_ns"],
                expected_phase="Ready",
                timeout=180,
            )

            # Store model path (with /v1 for OpenAI API compatibility)
            case["model_path"] = f"/{case['tenant_ns']}/{model_name}/v1"

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

        api_key_response = requests.post(
            f"{gateway_url}/maas-api/v1/api-keys",
            headers={
                "Authorization": f"Bearer {oc_token}",
                "Content-Type": "application/json",
            },
            json={
                "name": f"e2e-tenant-inference-{case_a['suffix']}",
                "subscription": f"{case_a['model_name']}-sub",
            },
            timeout=45,
            verify=TLS_VERIFY,
        )

        assert api_key_response.status_code in (200, 201), (
            f"Failed to create API key: {api_key_response.status_code} "
            f"{redact_sensitive(api_key_response.text)}"
        )

        api_key = api_key_response.json().get("key")
        assert api_key, f"API key missing in response: {redact_sensitive(api_key_response.json())}"

        # Allow API key to propagate before sending inference request
        _wait_reconcile()

        # Send inference request through tenant gateway
        model_url = f"{gateway_url}{case_a['model_path']}"
        headers = {"Authorization": f"Bearer {api_key}"}

        response = chat(
            "Say hello in one word",
            model_url,
            headers,
            model_name="facebook/opt-125m",
        )

        assert response.status_code == 200, (
            f"Inference failed: {response.status_code} "
            f"{redact_sensitive(response.text[:500])}"
        )

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

        # Create API key for tenant A through tenant A's gateway
        oc_token = _get_cluster_token()

        api_key_response = requests.post(
            f"{gateway_a_url}/maas-api/v1/api-keys",
            headers={
                "Authorization": f"Bearer {oc_token}",
                "Content-Type": "application/json",
            },
            json={
                "name": f"e2e-cross-tenant-{case_a['suffix']}",
                "subscription": f"{case_a['model_name']}-sub",
            },
            timeout=45,
            verify=TLS_VERIFY,
        )

        # If API key creation succeeds, use it to try accessing B's model
        if api_key_response.status_code in (200, 201):
            api_key_body = api_key_response.json()
            api_key = api_key_body.get("key")
            assert api_key, (
                "API key creation returned success without key material: "
                f"{redact_sensitive(api_key_body)}"
            )

            # Try to access tenant B's model through tenant B's gateway using tenant A's API key
            model_b_url = f"{gateway_b_url}{case_b['model_path']}"
            headers = {"Authorization": f"Bearer {api_key}"}

            response = chat(
                "Say hello",
                model_b_url,
                headers,
                model_name="facebook/opt-125m",
            )

            # Should be rejected (401 Unauthorized or 403 Forbidden)
            assert response.status_code in (401, 403), (
                f"Expected 401/403 for cross-tenant access, got {response.status_code}. "
                f"Tenant A should not access tenant B's model via B's gateway. "
                f"Response: {redact_sensitive(response.text[:500])}"
            )
        else:
            # If we can't even create an API key, expect proper auth/permission failure
            # 404 would indicate infrastructure failure, not isolation
            assert api_key_response.status_code in (401, 403), (
                f"Unexpected API key creation failure: {api_key_response.status_code} "
                f"{redact_sensitive(api_key_response.text)}"
            )


# ─── Body-based routing ────────────────────────────────────────────────────


requires_ipp = pytest.mark.skipif(
    not _check_ipp_pods_deployed(),
    reason="Payload-processing (IPP) pods not deployed; body routing tests require IPP",
)


def _create_tenant_api_key(gateway_url, case):
    """Create an API key for a tenant and return (api_key, gateway_url)."""
    oc_token = _get_cluster_token()
    r = requests.post(
        f"{gateway_url}/maas-api/v1/api-keys",
        headers={
            "Authorization": f"Bearer {oc_token}",
            "Content-Type": "application/json",
        },
        json={
            "name": f"e2e-body-routing-{case['suffix']}",
            "subscription": f"{case['model_name']}-sub",
        },
        timeout=45,
        verify=TLS_VERIFY,
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

    def test_correct_model_in_body_succeeds(self, tenant_inference_cases):
        """Correct model name in body passes through IPP and reaches backend."""
        case_a, _ = tenant_inference_cases
        gateway_url = _get_tenant_gateway_url(case_a["gateway_name"])
        api_key = _create_tenant_api_key(gateway_url, case_a)
        _wait_reconcile()

        r = self._post_chat(gateway_url, case_a["model_path"], api_key, {
            "model": "facebook/opt-125m",
            "messages": [{"role": "user", "content": "hello"}],
        })
        assert r.status_code == 200, (
            f"Expected 200 with correct model in body, got {r.status_code}. "
            f"Response: {redact_sensitive(r.text[:500])}"
        )
        data = r.json()
        assert "choices" in data, f"Response missing 'choices': {redact_sensitive(data)}"
        log.info("Body routing (correct model): HTTP %d", r.status_code)

    def test_wrong_model_in_body_rejected(self, tenant_inference_cases):
        """Wrong model name in body is rejected by IPP model-provider-resolver."""
        case_a, _ = tenant_inference_cases
        gateway_url = _get_tenant_gateway_url(case_a["gateway_name"])
        api_key = _create_tenant_api_key(gateway_url, case_a)
        _wait_reconcile()

        r = self._post_chat(gateway_url, case_a["model_path"], api_key, {
            "model": "nonexistent-model",
            "messages": [{"role": "user", "content": "hello"}],
        })
        assert r.status_code != 200, (
            f"Expected rejection for wrong model in body, got 200. "
            f"Body routing may not be active — request succeeded via path routing alone."
        )
        log.info("Body routing (wrong model): HTTP %d", r.status_code)

    def test_missing_model_in_body_rejected(self, tenant_inference_cases):
        """Missing model field in body is rejected by IPP."""
        case_a, _ = tenant_inference_cases
        gateway_url = _get_tenant_gateway_url(case_a["gateway_name"])
        api_key = _create_tenant_api_key(gateway_url, case_a)
        _wait_reconcile()

        r = self._post_chat(gateway_url, case_a["model_path"], api_key, {
            "messages": [{"role": "user", "content": "hello"}],
        })
        assert r.status_code != 200, (
            f"Expected rejection for missing model in body, got 200. "
            f"Body routing may not be active — request succeeded without model field."
        )
        log.info("Body routing (missing model): HTTP %d", r.status_code)

    def test_each_tenant_routes_to_own_model(self, tenant_inference_cases):
        """Both tenants route correctly with their own model in body."""
        case_a, case_b = tenant_inference_cases

        for case in (case_a, case_b):
            gateway_url = _get_tenant_gateway_url(case["gateway_name"])
            api_key = _create_tenant_api_key(gateway_url, case)
            _wait_reconcile()

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
