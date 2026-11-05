"""System-level tests for Grid tenant-admin organization claims."""

from __future__ import annotations

import base64
import json
import os
import uuid

import pytest
import requests
import test_helper
from grid_inventory_fixtures import ORGANIZATION_A, ORGANIZATION_B
from grid_oidc_helpers import request_grid_token

pytestmark = [
    pytest.mark.nightly,
    pytest.mark.grid_oidc,
    pytest.mark.skipif(
        os.environ.get("EXTERNAL_OIDC", "").lower() != "true",
        reason="EXTERNAL_OIDC is not true",
    ),
    pytest.mark.xdist_group("external"),
]


def _decode_jwt_payload(token: str) -> dict:
    parts = token.split(".")
    assert len(parts) == 3, f"Invalid JWT format: expected 3 parts, got {len(parts)}"
    padding = "=" * (-len(parts[1]) % 4)
    return json.loads(base64.urlsafe_b64decode(parts[1] + padding))


class TestGridTenantAdminClaims:
    def test_keycloak_organization_claims_are_flat(self):
        token = request_grid_token("grid_admin")
        claims = _decode_jwt_payload(token)

        assert "maasOrganizations" not in claims
        assert "https://redhat.com/maas/orgs" not in claims
        assert claims["org_id"] == ORGANIZATION_A
        assert claims["org_name"] == "coke"
        assert claims["roles"] == ["admins"]

    def test_keycloak_groups_remain_plain(self):
        token = request_grid_token("grid_admin")
        claims = _decode_jwt_payload(token)

        groups = claims["groups"]
        assert groups == ["ai-grid-admins"]

    def test_second_organization_uses_the_same_plain_group(self):
        claims = _decode_jwt_payload(request_grid_token("grid_admin_b"))

        assert "https://redhat.com/maas/orgs" not in claims
        assert claims["org_id"] == ORGANIZATION_B
        assert claims["org_name"] == "org-b"
        assert claims["roles"] == ["admins"]
        assert claims["groups"] == ["ai-grid-admins"]

    def test_non_admin_has_flat_organization_claims(self):
        claims = _decode_jwt_payload(request_grid_token("grid_non_admin"))

        assert "https://redhat.com/maas/orgs" not in claims
        assert claims["org_id"] == ORGANIZATION_A
        assert claims["org_name"] == "coke"
        assert claims["roles"] == ["users"]
        assert claims["groups"] == ["ai-grid-admins"]

    def test_grid_admin_without_private_model_access_sees_only_public_models(
        self, maas_api_base_url: str
    ):
        token = request_grid_token("grid_admin")

        response = test_helper._request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/models",
            headers={"Authorization": f"Bearer {token}"},
            timeout=45,
        )
        assert response.status_code == 200, (
            f"GET /v1/models failed for Grid admin token: "
            f"{response.status_code} {response.text}"
        )
        models = response.json()["data"]
        assert len(models) == 1
        assert models[0]["owned_by"] == f"llm/{test_helper.MODEL_REF}"

    def test_grid_no_org_user_sees_only_public_subscription(
        self, maas_api_base_url: str
    ):
        token = request_grid_token("grid_no_org")

        response = test_helper._request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/subscriptions",
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
        )
        assert response.status_code == 200, (
            f"GET /v1/subscriptions failed for grid_no_org: "
            f"{response.status_code} {response.text}"
        )
        assert response.headers.get("Cache-Control") == "no-store"
        subscriptions = response.json()
        assert [
            subscription["subscription_id_header"] for subscription in subscriptions
        ] == [test_helper.SIMULATOR_SUBSCRIPTION]

    def test_uuid_prefixed_group_without_org_id_cannot_access_private_subscription(
        self, maas_api_base_url: str
    ):
        token = request_grid_token("grid_reserved_group")
        claims = _decode_jwt_payload(token)
        assert claims["groups"] == [
            "42c3e46f-2477-44d7-a85b-d3b43f6b31fa/reserved-ai-grid-admins"
        ]
        assert {"org_id", "org_name", "roles"}.isdisjoint(claims)

        response = test_helper._request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/subscriptions",
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
            auth_denial_is_transient=False,
        )
        assert response.status_code == 403, (
            f"Expected GET /v1/subscriptions to reject grid_reserved_group "
            f"with 403, got "
            f"{response.status_code} {response.text}"
        )

    def test_tampered_grid_token_is_rejected(self, maas_api_base_url: str):
        token = request_grid_token("grid_admin") + "tampered"

        response = test_helper._request_with_gateway_retry(
            requests.post,
            f"{maas_api_base_url}/v1/api-keys",
            headers={"Authorization": f"Bearer {token}"},
            json={"name": f"e2e-grid-tampered-{uuid.uuid4().hex[:8]}"},
            timeout=30,
        )
        assert response.status_code == 401, (
            f"Expected tampered Grid token to be rejected with 401, got "
            f"{response.status_code} {response.text}"
        )

    def test_wrong_client_grid_token_is_rejected(self, maas_api_base_url: str):
        token = request_grid_token("grid_admin", client_id="wrong-client")
        claims = _decode_jwt_payload(token)
        assert claims["azp"] == "wrong-client"

        response = test_helper._request_with_gateway_retry(
            requests.post,
            f"{maas_api_base_url}/v1/api-keys",
            headers={"Authorization": f"Bearer {token}"},
            json={"name": f"e2e-grid-wrong-client-{uuid.uuid4().hex[:8]}"},
            timeout=30,
        )
        assert response.status_code == 403, (
            f"Expected wrong-client Grid token to be rejected with 403, got "
            f"{response.status_code} {response.text}"
        )
