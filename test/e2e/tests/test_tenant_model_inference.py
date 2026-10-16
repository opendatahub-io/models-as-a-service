"""
E2E tests for multi-tenant model inference routing.

These tests validate that:
  - Models created in tenant namespaces route through tenant gateways
  - Inference requests work end-to-end through tenant gateways
  - Gateway auth accepts each tenant's key and rejects it at the other tenant's gateway
  - AuthPolicy EnvoyFilters select only the workload for their Gateway

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
    wait_for_json,
)

from test_helper import (
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


def _create_tenant_api_key(gateway_url, case, *, name_prefix="e2e-body-routing"):
    """Create an API key for a tenant and return it."""
    oc_token = _get_cluster_token()
    r = create_api_key_at(
        f"{gateway_url}/maas-api",
        oc_token,
        f"{name_prefix}-{case['suffix']}",
        subscription=f"{case['model_name']}-sub",
    )
    assert r.status_code in (200, 201), (
        f"Failed to create tenant API key: {r.status_code} {redact_sensitive(r.text)}"
    )
    api_key = r.json().get("key")
    assert api_key, f"API key missing in response: {redact_sensitive(r.json())}"
    return api_key


def _gateway_auth_envoyfilter(gateway_name: str) -> dict:
    """Wait for the EnvoyFilter Kuadrant generates from a Gateway AuthPolicy."""
    return wait_for_json(
        "envoyfilter",
        f"kuadrant-auth-{gateway_name}",
        GATEWAY_NAMESPACE,
        timeout=180,
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

    @pytest.mark.parametrize("tenant_index", [0, 1], ids=["tenant_a", "tenant_b"])
    def test_inference_succeeds_through_tenant_gateway(self, tenant_inference_cases, tenant_index):
        """Each tenant's valid API key succeeds through its own gateway."""
        case = tenant_inference_cases[tenant_index]
        gateway_url = _get_tenant_gateway_url(case["gateway_name"])
        api_key = _create_tenant_api_key(gateway_url, case, name_prefix="e2e-tenant-inference")

        response = _wait_for_chat_ok(case, gateway_url, api_key)

        # Verify response structure
        data = response.json()
        assert "choices" in data, f"Response missing 'choices': {redact_sensitive(data)}"
        assert len(data["choices"]) > 0, f"Empty choices in response: {redact_sensitive(data)}"

    def test_auth_envoyfilters_select_their_gateway_workloads(self, tenant_inference_cases):
        """Each governed tenant Gateway has an auth filter scoped to its own workload."""
        selector_key = "gateway.networking.k8s.io/gateway-name"
        selected_gateways = set()

        for case in tenant_inference_cases:
            auth_policy_name = per_tenant_gateway_policy_names(
                case["tenant_label_name"], case["gateway_name"]
            )["gateway_authpolicy"]
            auth_policy = get_json_or_none("authpolicy", auth_policy_name, GATEWAY_NAMESPACE)
            assert auth_policy is not None, (
                f"Gateway AuthPolicy {GATEWAY_NAMESPACE}/{auth_policy_name} is missing"
            )
            target = (auth_policy.get("spec") or {}).get("targetRef") or {}
            assert target.get("name") == case["gateway_name"], (
                f"{auth_policy_name} targets {target!r}, expected gateway {case['gateway_name']}"
            )

            resource = _gateway_auth_envoyfilter(case["gateway_name"])
            selector = ((resource.get("spec") or {}).get("workloadSelector") or {}).get("labels") or {}
            selected_gateway = selector.get(selector_key)
            assert selected_gateway == case["gateway_name"], (
                f"AuthPolicy EnvoyFilter {resource['metadata']['name']} workload selector targets "
                f"{selected_gateway!r}, expected {case['gateway_name']!r}: {selector!r}"
            )
            owner_gateways = {
                owner.get("name")
                for owner in (resource.get("metadata") or {}).get("ownerReferences") or []
                if owner.get("kind") == "Gateway"
            }
            assert case["gateway_name"] in owner_gateways, (
                f"AuthPolicy EnvoyFilter {resource['metadata']['name']} is owned by "
                f"{owner_gateways!r}, expected Gateway {case['gateway_name']!r}"
            )
            selected_gateways.add(selected_gateway)

        assert len(selected_gateways) == len(tenant_inference_cases), (
            f"Expected a distinct auth workload selector per tenant gateway, got {selected_gateways!r}"
        )

    def test_tenant_isolation_cross_gateway_blocked(self, tenant_inference_cases):
        """Tenant isolation: tenant A cannot access tenant B's model via B's gateway."""
        case_a, case_b = tenant_inference_cases

        # Get gateway URLs
        gateway_a_url = _get_tenant_gateway_url(case_a["gateway_name"])
        gateway_b_url = _get_tenant_gateway_url(case_b["gateway_name"])

        api_key_a = _create_tenant_api_key(gateway_a_url, case_a, name_prefix="e2e-cross-tenant-a")
        api_key_b = _create_tenant_api_key(gateway_b_url, case_b, name_prefix="e2e-cross-tenant-b")

        # Both positive controls must pass before a cross-tenant denial can be
        # attributed to the target gateway's tenant-specific auth configuration.
        _wait_for_chat_ok(case_a, gateway_a_url, api_key_a)
        _wait_for_chat_ok(case_b, gateway_b_url, api_key_b)

        for source_case, target_case, target_gateway_url, foreign_key in (
            (case_a, case_b, gateway_b_url, api_key_a),
            (case_b, case_a, gateway_a_url, api_key_b),
        ):
            model_url = f"{target_gateway_url}{target_case['model_path']}"
            with _tenant_gateway_auth_policy(target_case["gateway_name"]):
                response = _request_with_gateway_retry(
                    requests.post,
                    f"{model_url}/chat/completions",
                    headers={"Authorization": f"Bearer {foreign_key}"},
                    json={
                        "model": "facebook/opt-125m",
                        "messages": [{"role": "user", "content": "Say hello"}],
                    },
                )

            assert is_gateway_auth_denial(response), (
                f"Expected {target_case['tenant_ns']} auth to reject "
                f"{source_case['tenant_ns']}'s key with 401/403, got {response.status_code}. "
                f"Response: {response_summary(response, max_body=500)}"
            )


_TENANT_INFERENCE_CHAT_BODY = {
    "model": "facebook/opt-125m",
    "messages": [{"role": "user", "content": "Say hello"}],
}

_FORGED_IDENTITY_HEADERS_ON_INFERENCE = {
    "X-MaaS-Username": "cluster-admin",
    "X-MaaS-Group": "system:cluster-admins,system:masters",
    "X-MaaS-KeyName": "forged-prod-key",
    "X-MaaS-Identity": '{"source":"oidc","claims":{}}',
    "X-MaaS-Key-Id": "fake-key-id-00000",
}


def _tenant_chat_completions_url(gateway_url: str, model_path: str) -> str:
    return f"{gateway_url}{model_path}/chat/completions"


class TestTenantGatewayNegativeSecurity:
    """Per-tenant gateway negative auth (RHOAIENG-100758).

    Complements test_negative_security.py (default gateway) and
    test_auth_envoyfilters_select_their_gateway_workloads (RHOAIENG-99372).
    """

    @pytest.mark.parametrize("tenant_index", [0, 1], ids=["tenant_a", "tenant_b"])
    def test_missing_api_key_denied_on_tenant_gateway(self, tenant_inference_cases, tenant_index):
        """Governed inference on each tenant gateway requires Authorization."""
        case = tenant_inference_cases[tenant_index]
        gateway_url = _get_tenant_gateway_url(case["gateway_name"])
        url = _tenant_chat_completions_url(gateway_url, case["model_path"])
        response = requests.post(
            url,
            headers={"Content-Type": "application/json"},
            json=_TENANT_INFERENCE_CHAT_BODY,
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert is_gateway_auth_denial(response), (
            f"Expected 401/403 without API key on {case['gateway_name']!r}, "
            f"got {response.status_code}. Response: {response_summary(response, max_body=500)}"
        )

    @pytest.mark.parametrize("tenant_index", [0, 1], ids=["tenant_a", "tenant_b"])
    def test_invalid_api_key_denied_on_tenant_gateway(self, tenant_inference_cases, tenant_index):
        """Invalid sk-oai-* keys are rejected on each tenant gateway URL."""
        case = tenant_inference_cases[tenant_index]
        gateway_url = _get_tenant_gateway_url(case["gateway_name"])
        url = _tenant_chat_completions_url(gateway_url, case["model_path"])
        response = requests.post(
            url,
            headers={
                "Content-Type": "application/json",
                "Authorization": "Bearer sk-oai-e2e-invalid-not-a-real-key",
            },
            json=_TENANT_INFERENCE_CHAT_BODY,
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert is_gateway_auth_denial(response), (
            f"Expected 401/403 for invalid API key on {case['gateway_name']!r}, "
            f"got {response.status_code}. Response: {response_summary(response, max_body=500)}"
        )

    @pytest.mark.parametrize("tenant_index", [0, 1], ids=["tenant_a", "tenant_b"])
    def test_forged_identity_headers_denied_on_tenant_gateway(self, tenant_inference_cases, tenant_index):
        """Client-supplied X-MaaS identity headers are denied on each tenant gateway (RHOAIENG-99369)."""
        case = tenant_inference_cases[tenant_index]
        gateway_url = _get_tenant_gateway_url(case["gateway_name"])
        api_key = _create_tenant_api_key(gateway_url, case, name_prefix="e2e-tenant-neg-sec")
        _wait_for_chat_ok(case, gateway_url, api_key)

        url = _tenant_chat_completions_url(gateway_url, case["model_path"])
        with _tenant_gateway_auth_policy(case["gateway_name"]):
            response = requests.post(
                url,
                headers={
                    "Content-Type": "application/json",
                    "Authorization": f"Bearer {api_key}",
                    **_FORGED_IDENTITY_HEADERS_ON_INFERENCE,
                },
                json=_TENANT_INFERENCE_CHAT_BODY,
                timeout=30,
                verify=TLS_VERIFY,
            )
        assert is_gateway_auth_denial(response), (
            f"Expected 401/403 for forged identity headers on {case['gateway_name']!r}, "
            f"got {response.status_code}. Response: {response_summary(response, max_body=500)}"
        )
        assert "sk-oai-" not in response.text, (
            "Denied inference response must not contain API key material"
        )


# ─── Body-based routing ────────────────────────────────────────────────────


class TestTenantBodyRouting:
    """Verify body-based routing in a multi-tenant context.

    Payload-processing pre-processing extracts the ``model`` field from the JSON
    body and sets the ``X-Gateway-Model-Name`` header. These tests prove the
    body model field drives routing: a correct model succeeds while a wrong
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
