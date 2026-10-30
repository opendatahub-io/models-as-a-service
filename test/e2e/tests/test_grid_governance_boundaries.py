# ruff: noqa: F811

"""System-level tests for Grid governance endpoint boundaries."""

from __future__ import annotations

import json
import os

import pytest
import requests

from grid_governance_fixtures import (
    GridGovernanceContext,
)
from grid_governance_fixtures import grid_governance_context  # noqa: F401 - pytest fixture
from grid_cross_tenant_fixtures import GridCrossTenantContext
from grid_cross_tenant_fixtures import (
    grid_cross_tenant_context,  # noqa: F401 - pytest fixture
)  # noqa: F401 - pytest fixture
from grid_inventory_fixtures import ORGANIZATION_A
from grid_oidc_helpers import request_grid_token, request_tenant_b_token
from test_helper import (
    MODEL_NAMESPACE,
    _decode_jwt_payload,
    _request_with_gateway_retry,
    _ns,
    kubectl_curl,
)


pytestmark = [
    pytest.mark.nightly,
    pytest.mark.grid_oidc,
    pytest.mark.skipif(
        os.environ.get("EXTERNAL_OIDC", "").lower() != "true",
        reason="EXTERNAL_OIDC is not true",
    ),
    pytest.mark.xdist_group("external"),
]


def _post_internal_json(
    url: str, payload: dict, token: str | None = None
) -> tuple[int, dict]:
    headers = {}
    if token is not None:
        headers["Authorization"] = f"Bearer {token}"
    status_code, body = kubectl_curl(
        url,
        method="POST",
        headers=headers,
        json_body=payload,
    )
    assert status_code == 200, f"POST {url} failed: {status_code} {body}"
    return status_code, json.loads(body)


def _grid_admin_identity_envelope() -> dict:
    token = request_grid_token("grid_admin")
    claims = _decode_jwt_payload(token)
    assert claims is not None

    return {
        "source": "oidc",
        "claims": {
            "iss": claims["iss"],
            "sub": claims["sub"],
            "aud": claims["aud"],
            "azp": claims["azp"],
            "groups": claims["groups"],
            "https://redhat.com/maas/orgs": claims["https://redhat.com/maas/orgs"],
        },
    }


class TestGridGovernanceInference:
    def test_grid_models_preserve_inference_discovery(
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
    ):
        token = request_grid_token("alice_lead")
        resources = grid_governance_context.resources

        response = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/models",
            headers={
                "Authorization": f"Bearer {token}",
                "X-MaaS-Subscription": resources.governance_subscription,
            },
            timeout=45,
        )
        assert response.status_code == 200, (
            f"GET /v1/models failed for Grid user: "
            f"{response.status_code} {response.text}"
        )

        models = response.json()["data"]
        expected_owned_by = f"{MODEL_NAMESPACE}/{resources.owner_model_ref}"
        matching_models = [
            model for model in models if model.get("owned_by") == expected_owned_by
        ]
        assert len(matching_models) == 1, (
            f"Expected exactly the governance model {expected_owned_by}, got "
            f"{[model.get('owned_by') for model in models]}"
        )

        model = matching_models[0]
        assert "roles" not in model
        assert "owner" not in model
        subscriptions = model["subscriptions"]
        assert resources.governance_subscription in {
            subscription["name"] for subscription in subscriptions
        }
        assert all(
            {"roles", "owner", "subjects"}.isdisjoint(subscription)
            for subscription in subscriptions
        )

    def test_grid_model_subscriptions_preserve_inference_discovery(
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
    ):
        token = request_grid_token("alice_lead")
        resources = grid_governance_context.resources

        response = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/model/{resources.owner_model_ref}/subscriptions",
            headers={"Authorization": f"Bearer {token}"},
            timeout=45,
        )
        assert response.status_code == 200, (
            f"GET /v1/model/{{model-id}}/subscriptions failed for Grid user: "
            f"{response.status_code} {response.text}"
        )

        subscriptions = response.json()
        assert isinstance(subscriptions, list)
        matching_subscriptions = [
            subscription
            for subscription in subscriptions
            if subscription.get("subscription_id_header")
            == resources.governance_subscription
        ]
        assert len(matching_subscriptions) == 1, (
            f"Expected governance subscription {resources.governance_subscription}, "
            f"got {[item.get('subscription_id_header') for item in subscriptions]}"
        )

        subscription = matching_subscriptions[0]
        assert {"roles", "owner"}.isdisjoint(subscription)
        model_refs = subscription["model_refs"]
        assert [model_ref["name"] for model_ref in model_refs] == [
            resources.owner_model_ref
        ]
        assert all("subjects" not in model_ref for model_ref in model_refs)


class TestGridTenantDiscoveryBoundary:
    def test_grid_oidc_token_cannot_discover_tenants(self, maas_api_internal_url: str):
        token = request_grid_token("grid_admin")

        status_code, body = kubectl_curl(
            f"{maas_api_internal_url}/v1/tenants",
            headers={"Authorization": f"Bearer {token}"},
        )
        assert status_code == 401, (
            f"Grid OIDC token must not authorize Kubernetes tenant discovery, "
            f"got {status_code} {body}"
        )

    def test_grid_tenant_discovery_remains_unroutable(self, maas_api_base_url: str):
        token = request_grid_token("grid_admin")
        gateway_base_url = maas_api_base_url.removesuffix("/maas-api")

        response = _request_with_gateway_retry(
            requests.get,
            f"{gateway_base_url}/v1/tenants",
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
        )
        assert response.status_code == 404, (
            f"Root /v1/tenants route must remain unroutable, got "
            f"{response.status_code} {response.text}"
        )


class TestGridInternalSubscriptionCallback:
    def test_legacy_grid_subscription_selection_is_unchanged(
        self,
        maas_api_internal_url: str,
        grid_governance_context: GridGovernanceContext,
    ):
        resources = grid_governance_context.resources
        api_key = grid_governance_context.keys[2].secret

        _, response = _post_internal_json(
            f"{maas_api_internal_url}/internal/v1/subscriptions/select",
            {
                "username": "alice_lead",
                "groups": ["Engineering"],
                "requestedSubscription": resources.governance_subscription,
            },
            api_key,
        )

        assert response["name"] == resources.governance_subscription
        assert response["accessAllowed"] is True
        assert "admin" not in response
        assert "purpose" not in response

    def test_grid_subscription_admin_callback_returns_distinct_decision(
        self,
        maas_api_internal_url: str,
        grid_governance_context: GridGovernanceContext,
    ):
        resources = grid_governance_context.resources

        _, response = _post_internal_json(
            f"{maas_api_internal_url}/internal/v1/subscriptions/select",
            {
                "purpose": "subscription-admin",
                "requestedSubscription": resources.governance_subscription,
                "identity": _grid_admin_identity_envelope(),
            },
        )

        assert response["purpose"] == "subscription-admin"
        assert response["accessAllowed"] is False
        assert "name" not in response
        admin = response["admin"]
        assert admin["allowed"] is True
        scope = admin["scope"]
        assert scope["tenant"]
        assert scope["namespace"] == _ns()
        assert scope["subscription"] == resources.governance_subscription
        assert scope["organizationId"] == ORGANIZATION_A


class TestGridCrossTenantQualifiedSubscription:
    def test_grid_admin_cannot_use_cross_tenant_qualified_target(
        self,
        maas_api_base_url: str,
        maas_api_internal_url: str,
        grid_cross_tenant_context: GridCrossTenantContext,
    ):
        context = grid_cross_tenant_context
        qualified_subscription = (
            f"{context.tenant_b['namespace']}/{context.subscription_name}"
        )
        token = request_grid_token("grid_admin")

        creation_attempt = _request_with_gateway_retry(
            requests.post,
            f"{maas_api_base_url}/v1/api-keys",
            headers={"Authorization": f"Bearer {token}"},
            json={
                "name": f"{context.subscription_name}-cross-tenant",
                "subscription": qualified_subscription,
            },
            timeout=30,
        )
        if creation_attempt.status_code == 201:
            accidental_key_id = creation_attempt.json()["id"]
            accidental_cleanup = _request_with_gateway_retry(
                requests.delete,
                f"{maas_api_base_url}/v1/api-keys/{accidental_key_id}",
                headers={"Authorization": f"Bearer {token}"},
                timeout=30,
            )
            assert accidental_cleanup.status_code in (200, 404), (
                f"Failed to clean up an unexpectedly minted cross-tenant key: "
                f"{accidental_cleanup.status_code} {accidental_cleanup.text}"
            )
        assert creation_attempt.status_code == 400, (
            f"Tenant A Grid admin must not mint a key for Tenant B's "
            f"qualified subscription, got "
            f"{creation_attempt.status_code} {creation_attempt.text}"
        )
        assert creation_attempt.json()["code"] == "invalid_subscription"

        search = _request_with_gateway_retry(
            requests.post,
            f"{maas_api_base_url}/v1/api-keys/search",
            headers={"Authorization": f"Bearer {token}"},
            json={
                "filters": {
                    "subscription": qualified_subscription,
                    "username": "charlie_sec_lead",
                },
                "pagination": {"limit": 100, "offset": 0},
            },
            timeout=30,
        )
        assert search.status_code == 403, (
            f"Tenant A Grid admin must not search Tenant B's qualified "
            f"subscription, got {search.status_code} {search.text}"
        )

        bulk_revoke = _request_with_gateway_retry(
            requests.post,
            f"{maas_api_base_url}/v1/api-keys/bulk-revoke",
            headers={"Authorization": f"Bearer {token}"},
            json={"subscription": qualified_subscription},
            timeout=30,
        )
        assert bulk_revoke.status_code == 403, (
            f"Tenant A Grid admin must not bulk revoke Tenant B's qualified "
            f"subscription, got {bulk_revoke.status_code} {bulk_revoke.text}"
        )

        key_inspection = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/api-keys/{context.api_key_id}",
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
        )
        assert key_inspection.status_code == 404, (
            f"Tenant A Grid admin must not inspect Tenant B's API key, got "
            f"{key_inspection.status_code} {key_inspection.text}"
        )

        key_revocation = _request_with_gateway_retry(
            requests.delete,
            f"{maas_api_base_url}/v1/api-keys/{context.api_key_id}",
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
        )
        assert key_revocation.status_code == 404, (
            f"Tenant A Grid admin must not revoke Tenant B's API key, got "
            f"{key_revocation.status_code} {key_revocation.text}"
        )

        _, selection = _post_internal_json(
            f"{maas_api_internal_url}/internal/v1/subscriptions/select",
            {
                "purpose": "subscription-admin",
                "requestedSubscription": qualified_subscription,
                "identity": _grid_admin_identity_envelope(),
            },
        )
        admin = selection["admin"]
        assert admin["allowed"] is False
        assert admin["reason"] == "not_found"
        assert "scope" not in admin

        tenant_b_token = request_tenant_b_token("charlie_sec_lead")
        tenant_b_inspection = _request_with_gateway_retry(
            requests.get,
            f"{context.tenant_b['base_url']}/v1/api-keys/{context.api_key_id}",
            headers={"Authorization": f"Bearer {tenant_b_token}"},
            timeout=30,
        )
        assert tenant_b_inspection.status_code == 200, (
            f"Tenant B owner must still inspect its API key: "
            f"{tenant_b_inspection.status_code} {tenant_b_inspection.text}"
        )
        assert tenant_b_inspection.json()["status"] == "active"


class TestGridInternalAPIKeyValidation:
    def test_grid_api_key_validation_preserves_stored_identity(
        self,
        maas_api_internal_url: str,
        grid_governance_context: GridGovernanceContext,
    ):
        key = grid_governance_context.keys[2]

        _, response = _post_internal_json(
            f"{maas_api_internal_url}/internal/v1/api-keys/validate",
            {"key": key.secret},
        )

        assert response["valid"] is True
        assert response["keyId"] == key.key_id
        assert response["username"] == key.username
        assert response["subscription"] == key.subscription
        assert "Engineering" in response["groups"]
        assert {
            "https://redhat.com/maas/orgs",
            "organizations",
            "roles",
            "admin",
            "owner",
        }.isdisjoint(response)


class TestGridInternalLifecycleBoundary:
    def test_grid_cleanup_remains_unroutable(self, maas_api_base_url: str):
        token = request_grid_token("grid_admin")

        response = _request_with_gateway_retry(
            requests.post,
            f"{maas_api_base_url}/internal/v1/api-keys/cleanup",
            headers={"Authorization": f"Bearer {token}"},
            json={},
            timeout=30,
        )
        assert response.status_code == 404, (
            f"Internal API-key cleanup must remain unroutable, got "
            f"{response.status_code} {response.text}"
        )

    def test_grid_tenant_key_revocation_remains_unroutable(
        self, maas_api_base_url: str
    ):
        token = request_grid_token("grid_admin")

        response = _request_with_gateway_retry(
            requests.delete,
            f"{maas_api_base_url}/internal/v1/tenants/unmatched-grid-tenant/api-keys",
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
        )
        assert response.status_code == 404, (
            f"Internal tenant API-key revocation must remain unroutable, got "
            f"{response.status_code} {response.text}"
        )


class TestGridHealthBoundary:
    def test_grid_health_remains_public_and_unaugmented(self, maas_api_base_url: str):
        response = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/health",
            timeout=30,
        )
        assert response.status_code == 200, (
            f"Unauthenticated health probe failed: "
            f"{response.status_code} {response.text}"
        )
        assert response.json() == {"status": "healthy"}
