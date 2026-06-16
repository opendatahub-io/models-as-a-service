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

Run with ENABLE_TENANT_MODEL_INFERENCE_E2E=true.
"""

import json
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
    env_bool,
    get_json_or_none,
    new_named_tenant_case,
    redact_sensitive,
    require_aitenant_crd,
    wait_for_json,
    wait_for_status_phase,
)

from test_helper import (
    _create_llmis,
    _create_maas_model_ref,
    _delete_cr,
    _get_cluster_token,
    chat,
)


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
    # Respect INSECURE_HTTP flag for test mode consistency
    scheme = "http" if env_bool("INSECURE_HTTP") else "https"
    return f"{scheme}://{host}"


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

            # Create LLMIS pointing to tenant gateway
            _create_llmis(
                model_name,
                case["tenant_ns"],
                case["gateway_name"],
                GATEWAY_NAMESPACE,
            )

            # Wait for HTTPRoute to be created by KServe
            wait_for_json(
                "httproute",
                f"{model_name}-kserve-route",
                case["tenant_ns"],
                timeout=120,
            )

            # Create MaaSModelRef
            _create_maas_model_ref(model_name, case["tenant_ns"], model_name)

            # Create subscription and auth policy (both needed for Ready status)
            apply_maas_subscription(
                f"{model_name}-sub",
                case["tenant_ns"],
                model_ref=model_name,
                model_namespace=case["tenant_ns"],
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

            # Store model info in case
            case["model_name"] = model_name
            case["model_path"] = f"/{case['tenant_ns']}/{model_name}"

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
            api_key = api_key_response.json().get("key")

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
