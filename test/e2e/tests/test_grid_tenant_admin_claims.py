"""System-level tests for Grid tenant-admin organization claims."""

from __future__ import annotations

import base64
import json
import os
import uuid

import pytest
import requests

from conftest import TLS_VERIFY


pytestmark = [
    pytest.mark.nightly,
    pytest.mark.skipif(
        os.environ.get("EXTERNAL_OIDC", "").lower() != "true",
        reason="EXTERNAL_OIDC is not true",
    ),
    pytest.mark.xdist_group("external"),
]


def _required_env(name: str) -> str:
    value = os.environ.get(name, "")
    assert value, f"{name} must be set when EXTERNAL_OIDC=true"
    return value


def _request_grid_token(username: str) -> str:
    response = requests.post(
        _required_env("OIDC_TOKEN_URL"),
        data={
            "grant_type": "password",
            "client_id": _required_env("OIDC_CLIENT_ID"),
            "username": username,
            "password": "letmein",
        },
        timeout=30,
        verify=TLS_VERIFY,
    )
    assert response.status_code == 200, (
        f"Keycloak token request failed for {username}: "
        f"{response.status_code} {response.text}"
    )
    token = response.json().get("access_token")
    assert token, "Keycloak token response missing access_token"
    return token


def _decode_jwt_payload(token: str) -> dict:
    parts = token.split(".")
    assert len(parts) == 3, f"Invalid JWT format: expected 3 parts, got {len(parts)}"
    padding = "=" * (-len(parts[1]) % 4)
    return json.loads(base64.urlsafe_b64decode(parts[1] + padding))


class TestGridTenantAdminClaims:
    def test_keycloak_emits_organization_claims(self):
        token = _request_grid_token("grid_admin")
        claims = _decode_jwt_payload(token)

        organizations = claims.get("https://redhat.com/maas/orgs")
        assert isinstance(organizations, list), (
            f"Expected organization claim to be a list, got {type(organizations)}"
        )
        assert len(organizations) == 2, (
            f"Expected two organization memberships, got {organizations}"
        )

        assert organizations[0]["organization_id"] == (
            "42c3e46f-2477-44d7-a85b-d3b43f6b31fa"
        )
        assert organizations[0]["roles"] == ["admins"]
        assert organizations[1]["organization_id"] == (
            "53d4f57e-3588-55e8-b96c-4ec54a7c42fa"
        )
        assert organizations[1]["roles"] == ["users"]

    def test_keycloak_groups_remain_organization_qualified(self):
        token = _request_grid_token("grid_admin")
        claims = _decode_jwt_payload(token)

        groups = claims.get("groups")
        assert isinstance(groups, list), f"Expected groups to be a list, got {type(groups)}"
        assert set(groups) == {
            "42c3e46f-2477-44d7-a85b-d3b43f6b31fa/ai-grid-admins",
            "53d4f57e-3588-55e8-b96c-4ec54a7c42fa/ai-grid-users",
        }

        organizations = claims.get("https://redhat.com/maas/orgs")
        assert organizations, "Expected organization memberships to remain present"
        organization_ids = {
            organization["organization_id"] for organization in organizations
        }
        assert all("/" in group for group in groups)
        assert all(group.split("/", 1)[0] in organization_ids for group in groups)

    def test_subscription_listing_accepts_grid_admin_token(self, maas_api_base_url: str):
        token = _request_grid_token("grid_admin")

        response = requests.get(
            f"{maas_api_base_url}/v1/subscriptions",
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert response.status_code == 200, (
            f"GET /v1/subscriptions failed with Grid admin token: "
            f"{response.status_code} {response.text}"
        )
        assert response.headers.get("Cache-Control") == "no-store"

        subscriptions = response.json()
        assert isinstance(subscriptions, list), (
            f"Expected subscription listing to be a list, got {type(subscriptions)}"
        )
        for subscription in subscriptions:
            assert isinstance(subscription["subscription_id_header"], str)
            assert isinstance(subscription["priority"], int)
            assert isinstance(subscription["model_refs"], list)

    def test_me_returns_verified_identity(self, maas_api_base_url: str):
        token = _request_grid_token("grid_admin")
        expected_subject = _decode_jwt_payload(token)["sub"]

        response = requests.get(
            f"{maas_api_base_url}/v1/me",
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert response.status_code == 200, (
            f"GET /v1/me failed with Grid admin token: "
            f"{response.status_code} {response.text}"
        )
        assert response.headers.get("Cache-Control") == "no-store"

        identity = response.json()
        assert set(identity) <= {
            "sub",
            "email",
            "name",
            "https://redhat.com/maas/orgs",
        }
        assert identity["sub"] == expected_subject

        organizations = identity["https://redhat.com/maas/orgs"]
        assert organizations == [
            {
                "organization_id": "42c3e46f-2477-44d7-a85b-d3b43f6b31fa",
                "roles": ["admins"],
            },
            {
                "organization_id": "53d4f57e-3588-55e8-b96c-4ec54a7c42fa",
                "roles": ["users"],
            },
        ]

    def test_me_returns_empty_organizations_for_no_membership(self, maas_api_base_url: str):
        token = _request_grid_token("grid_no_org")
        expected_subject = _decode_jwt_payload(token)["sub"]

        response = requests.get(
            f"{maas_api_base_url}/v1/me",
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert response.status_code == 200, (
            f"GET /v1/me failed for grid_no_org: "
            f"{response.status_code} {response.text}"
        )

        identity = response.json()
        assert identity["sub"] == expected_subject
        assert identity["https://redhat.com/maas/orgs"] == []

    def test_me_rejects_inference_api_key(self, maas_api_base_url: str):
        oidc_token = _request_grid_token("alice_lead")
        key_name = f"e2e-me-api-key-{uuid.uuid4().hex[:8]}"

        create_response = requests.post(
            f"{maas_api_base_url}/v1/api-keys",
            headers={"Authorization": f"Bearer {oidc_token}"},
            json={"name": key_name},
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert create_response.status_code in (200, 201), (
            f"API key creation failed for /v1/me negative test: "
            f"{create_response.status_code} {create_response.text}"
        )
        key_data = create_response.json()
        api_key = key_data["key"]
        key_id = key_data["id"]

        try:
            response = requests.get(
                f"{maas_api_base_url}/v1/me",
                headers={"Authorization": f"Bearer {api_key}"},
                timeout=30,
                verify=TLS_VERIFY,
            )
            assert response.status_code == 401, (
                f"Expected /v1/me to reject an inference API key, got "
                f"{response.status_code} {response.text}"
            )
        finally:
            requests.delete(
                f"{maas_api_base_url}/v1/api-keys/{key_id}",
                headers={"Authorization": f"Bearer {oidc_token}"},
                timeout=30,
                verify=TLS_VERIFY,
            )

    def test_me_requires_verified_sso_identity(self, maas_api_base_url: str):
        response = requests.get(
            f"{maas_api_base_url}/v1/me",
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert response.status_code == 401, (
            f"Expected unauthenticated /v1/me to return 401, got "
            f"{response.status_code} {response.text}"
        )
