"""System-level tests for Grid governance endpoint boundaries."""

from __future__ import annotations

import json
import os

import pytest
import requests

from grid_governance_fixtures import (
    GridGovernanceContext,
    governance_context,  # noqa: F401 - pytest fixture
)
from grid_inventory_fixtures import ORGANIZATION_A
from grid_oidc_helpers import request_grid_token
from test_helper import (
    MODEL_NAMESPACE,
    _decode_jwt_payload,
    _request_with_gateway_retry,
    _ns,
    kubectl_curl,
)


pytestmark = [
    pytest.mark.nightly,
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
            "https://redhat.com/maas/orgs": claims[
                "https://redhat.com/maas/orgs"
            ],
        },
    }


class TestGridGovernanceInference:
    def test_grid_models_preserve_inference_discovery(
        self, maas_api_base_url: str, governance_context: GridGovernanceContext
    ):
        token = request_grid_token("alice_lead")
        resources = governance_context.resources

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
            model
            for model in models
            if model.get("owned_by") == expected_owned_by
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
        self, maas_api_base_url: str, governance_context: GridGovernanceContext
    ):
        token = request_grid_token("alice_lead")
        resources = governance_context.resources

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
    def test_grid_oidc_token_cannot_discover_tenants(
        self, maas_api_internal_url: str
    ):
        token = request_grid_token("grid_admin")

        status_code, body = kubectl_curl(
            f"{maas_api_internal_url}/v1/tenants",
            headers={"Authorization": f"Bearer {token}"},
        )
        assert status_code == 401, (
            f"Grid OIDC token must not authorize Kubernetes tenant discovery, "
            f"got {status_code} {body}"
        )

    def test_grid_tenant_discovery_remains_unroutable(
        self, maas_api_base_url: str
    ):
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
        governance_context: GridGovernanceContext,
    ):
        resources = governance_context.resources
        api_key = governance_context.keys[2].secret

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
        governance_context: GridGovernanceContext,
    ):
        resources = governance_context.resources

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


class TestGridInternalAPIKeyValidation:
    def test_grid_api_key_validation_preserves_stored_identity(
        self,
        maas_api_internal_url: str,
        governance_context: GridGovernanceContext,
    ):
        key = governance_context.keys[2]

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

