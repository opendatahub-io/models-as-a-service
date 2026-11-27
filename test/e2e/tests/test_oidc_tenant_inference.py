"""Both-tenant OIDC inference E2E tests — RHAI-31081.

Happy path for both test tenants: a user of each Keycloak realm signs in
(password grant), mints an API key through their tenant's own gateway, and
runs inference on a model deployed in their tenant namespace. Each tenant's
OIDCReady condition must report Available before inference runs.

Tenant setup mirrors the existing shared-tenant fixtures: tenant-b comes from
``shared_test_tenants`` (bootstrapped with the realm-b issuer). The shared
tenant-a is deliberately deployed *without* external OIDC (its tests use
cluster tokens), so this module bootstraps its own realm-a tenant with the
default tenant's issuer — the same bootstrap path tenant-b uses, which keeps
the gateway AuthPolicy correctly seeded.

Prerequisites: EXTERNAL_OIDC=true.
"""

from __future__ import annotations

import os
import time
import uuid

import pytest
import requests

from conftest import TLS_VERIFY, _xdist_worker_suffix
from grid_oidc_helpers import request_grid_token, request_tenant_b_token
from multitenancy_helpers import (
    AITENANT_NAMESPACE,
    bootstrap_aitenant_tenant,
    cleanup_discovery_case,
    delete_maas_auth_policy,
    delete_maas_subscription,
    external_oidc_from_env,
    new_named_tenant_case,
    provision_tenant_model,
    make_tenant_model_accessible,
    wait_for_deployment_available,
    wait_for_json,
    wait_for_route_admitted,
)
from test_helper import (
    MAAS_API_DEPLOYMENT_NAMESPACE,
    MODEL_NAME,
    TIMEOUT,
    _delete_cr,
    _request_with_gateway_retry,
    assert_inference_response,
)

pytestmark = [
    pytest.mark.nightly,
    pytest.mark.external,
    pytest.mark.full_suite,
    pytest.mark.skipif(
        os.environ.get("EXTERNAL_OIDC", "").lower() != "true",
        reason="EXTERNAL_OIDC is not true",
    ),
    pytest.mark.xdist_group("oidc_tenant_inference"),
]

# Realm users with valid credentials in the test realms (password: letmein).
TENANT_A_USER = "alice_lead"
TENANT_B_USER = "grace_dev"

OIDC_READY_TIMEOUT = 180


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _mint_api_key_with_realm_token(base_url: str, token: str, name: str) -> str:
    """Mint an API key with a realm OIDC token.

    Uses _request_with_gateway_retry so transient gateway/auth-propagation
    failures (the AUTH_FAILURE window after fresh CEL patches) are retried
    by the shared helper instead of a hand-rolled sleep loop.
    """
    response = _request_with_gateway_retry(
        requests.post,
        f"{base_url}/v1/api-keys",
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
        json={"name": name},
        timeout=TIMEOUT,
        verify=TLS_VERIFY,
    )
    assert response.status_code in (200, 201), (
        f"API key mint failed for {name} at {base_url}: "
        f"{response.status_code} {response.text[:200]}"
    )
    data = response.json()
    assert data.get("key"), f"API key mint returned no key: {data}"
    return data["key"]


def _infer_at(api_base_url: str, api_key: str, model_path: str) -> requests.Response:
    """POST a completion through the tenant's own gateway."""
    gateway_base = api_base_url.rstrip("/")
    if gateway_base.endswith("/maas-api"):
        gateway_base = gateway_base[: -len("/maas-api")]
    return requests.post(
        f"{gateway_base}{model_path}/v1/completions",
        headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
        json={"model": MODEL_NAME, "prompt": "Hello", "max_tokens": 3},
        timeout=TIMEOUT,
        verify=TLS_VERIFY,
    )


def _wait_for_tenant_api_ready(base_url: str, *, timeout: int = 300) -> None:
    """Wait until the tenant's maas-api answers through its own gateway.

    Bootstrap waits only for the HTTPRoute to be Accepted and the Deployment
    to be Available; the gateway dataplane can still be loading the new
    tenant's listeners for a while after that, answering 503 from the router.
    Health is the unauthenticated readiness signal, so poll for it instead of
    burning the first real request's retry budget.
    """
    deadline = time.time() + timeout
    last = "no response"
    while time.time() < deadline:
        try:
            response = requests.get(f"{base_url}/health", timeout=30, verify=TLS_VERIFY)
            if response.status_code == 200:
                return
            last = f"HTTP {response.status_code}"
        except requests.RequestException as exc:
            last = str(exc)
        time.sleep(5)
    raise AssertionError(
        f"maas-api at {base_url} did not become healthy within {timeout}s (last: {last})"
    )


def _wait_for_oidc_ready_available(tenant_name: str) -> None:
    """Assert the tenant's OIDCReady condition is True/Available."""
    def _predicate(aitenant: dict) -> bool:
        for condition in (aitenant.get("status") or {}).get("conditions") or []:
            if (
                condition.get("type") == "OIDCReady"
                and condition.get("status") == "True"
                and condition.get("reason") == "Available"
            ):
                return True
        return False

    wait_for_json(
        "aitenant",
        tenant_name,
        AITENANT_NAMESPACE,
        predicate=_predicate,
        timeout=OIDC_READY_TIMEOUT,
    )


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

@pytest.fixture(scope="module")
def oidc_inference_tenants(shared_test_tenants):
    """One OIDC-configured tenant per realm, each with an accessible model and
    a realm-token-minted API key.

    tenant-b reuses the shared fixture (already bootstrapped with the realm-b
    issuer); tenant-a is bootstrapped here with the default tenant's (realm-a)
    issuer because the shared tenant-a intentionally has no external OIDC.
    """
    tenant_b = dict(shared_test_tenants[1])

    case_a = new_named_tenant_case(f"e2e-oidc-a-{_xdist_worker_suffix()}")
    realm_a_oidc = external_oidc_from_env()
    assert realm_a_oidc is not None, "EXTERNAL_OIDC=true must provide OIDC_ISSUER_URL"

    tenant_a: dict[str, str] = {}
    try:
        bootstrap_aitenant_tenant(case_a, external_oidc=realm_a_oidc)
        route = wait_for_route_admitted(f"{case_a['gateway_name']}-route")
        tenant_a = {
            "name": case_a["tenant_label_name"],
            "namespace": case_a["tenant_ns"],
            "gateway_name": case_a["gateway_name"],
            "base_url": f"https://{route['spec']['host']}/maas-api",
        }
        wait_for_deployment_available(
            f"maas-api-{tenant_a['name']}",
            namespace=MAAS_API_DEPLOYMENT_NAMESPACE,
            timeout=180,
        )

        suffix = uuid.uuid4().hex[:6]
        for tenant, realm_token in (
            (tenant_a, request_grid_token(TENANT_A_USER)),
            (tenant_b, request_tenant_b_token(TENANT_B_USER)),
        ):
            model_name = f"oidc-inf-model-{suffix}-{tenant['name']}"
            policy_name = f"oidc-inf-policy-{suffix}-{tenant['name']}"
            subscription_name = f"oidc-inf-sub-{suffix}-{tenant['name']}"
            provision_tenant_model(model_name, tenant["namespace"], tenant["gateway_name"])
            make_tenant_model_accessible(
                model_name,
                tenant["namespace"],
                policy_name,
                subscription_name,
                gateway_name=tenant["gateway_name"],
            )
            _wait_for_tenant_api_ready(tenant["base_url"])
            tenant["api_key"] = _mint_api_key_with_realm_token(
                tenant["base_url"], realm_token, f"e2e-oidc-inf-{suffix}-{tenant['name']}"
            )
            tenant["model_path"] = f"/{tenant['namespace']}/{model_name}"
            tenant["_model_name"] = model_name
            tenant["_policy_name"] = policy_name
            tenant["_subscription_name"] = subscription_name

        yield {"tenant_a": tenant_a, "tenant_b": tenant_b}
    finally:
        for tenant in (tenant_a, tenant_b):
            if not tenant:
                continue
            if tenant.get("_subscription_name"):
                delete_maas_subscription(tenant["_subscription_name"], tenant["namespace"])
            if tenant.get("_policy_name"):
                delete_maas_auth_policy(tenant["_policy_name"], tenant["namespace"])
            if tenant.get("_model_name"):
                _delete_cr("maasmodelref", tenant["_model_name"], tenant["namespace"])
                _delete_cr("llminferenceservice", tenant["_model_name"], tenant["namespace"])
        cleanup_discovery_case(case_a)


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

class TestOIDCTenantInference:
    def test_tenant_a_realm_oidc_inference(self, oidc_inference_tenants):
        """A realm-a user's token mints a key and infers through tenant-a's
        own gateway, with the tenant's OIDCReady reporting Available."""
        tenant = oidc_inference_tenants["tenant_a"]
        _wait_for_oidc_ready_available(tenant["name"])

        response = _infer_at(tenant["base_url"], tenant["api_key"], tenant["model_path"])
        assert response.status_code == 200, (
            f"Tenant-a OIDC inference failed: {response.status_code} {response.text[:200]}"
        )
        assert_inference_response(response)

    def test_tenant_b_realm_oidc_inference(self, oidc_inference_tenants):
        """A realm-b user's token mints a key and infers through tenant-b's
        own gateway, with the tenant's OIDCReady reporting Available."""
        tenant = oidc_inference_tenants["tenant_b"]
        _wait_for_oidc_ready_available(tenant["name"])

        response = _infer_at(tenant["base_url"], tenant["api_key"], tenant["model_path"])
        assert response.status_code == 200, (
            f"Tenant-b OIDC inference failed: {response.status_code} {response.text[:200]}"
        )
        assert_inference_response(response)
