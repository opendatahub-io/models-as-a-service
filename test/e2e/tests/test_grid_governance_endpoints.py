"""System-level tests for Grid authorization across governance endpoints."""

from __future__ import annotations

import os
import uuid

import pytest
import requests

from grid_governance_fixtures import (
    GridGovernanceContext,
)
from grid_governance_fixtures import governance_context as grid_governance_context  # noqa: F401 - pytest fixture
from grid_oidc_helpers import request_grid_token
from test_helper import _request_with_gateway_retry


pytestmark = [
    pytest.mark.nightly,
    pytest.mark.skipif(
        os.environ.get("EXTERNAL_OIDC", "").lower() != "true",
        reason="EXTERNAL_OIDC is not true",
    ),
    pytest.mark.xdist_group("external"),
]


def _search_grid_api_keys(
    maas_api_base_url: str,
    token: str,
    *,
    subscription: str | None = None,
    username: str | None = None,
):
    filters = {}
    if subscription is not None:
        filters["subscription"] = subscription
    if username is not None:
        filters["username"] = username
    return _request_with_gateway_retry(
        requests.post,
        f"{maas_api_base_url}/v1/api-keys/search",
        headers={"Authorization": f"Bearer {token}"},
        json={
            "filters": filters,
            "pagination": {"limit": 100, "offset": 0},
        },
        timeout=30,
    )


def _bulk_revoke_grid_api_keys(
    maas_api_base_url: str,
    token: str,
    *,
    subscription: str | None = None,
    username: str | None = None,
    dry_run: bool = False,
):
    payload = {"dryRun": dry_run}
    if subscription is not None:
        payload["subscription"] = subscription
    if username is not None:
        payload["username"] = username
    return _request_with_gateway_retry(
        requests.post,
        f"{maas_api_base_url}/v1/api-keys/bulk-revoke",
        headers={"Authorization": f"Bearer {token}"},
        json=payload,
        timeout=30,
    )


class TestGridGovernanceAPIKeys:
    def test_grid_sso_key_creation_remains_self_service(
        self, maas_api_base_url: str, governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")
        key_name = f"grid-gov-self-{uuid.uuid4().hex[:8]}"
        created_key_id = None
        try:
            response = _request_with_gateway_retry(
                requests.post,
                f"{maas_api_base_url}/v1/api-keys",
                headers={"Authorization": f"Bearer {token}"},
                json={
                    "name": key_name,
                    "subscription": (
                        governance_context.resources.owner_admin_subscription
                    ),
                },
                timeout=30,
            )
            assert response.status_code == 201, (
                f"Grid SSO API-key creation failed: "
                f"{response.status_code} {response.text}"
            )
            payload = response.json()
            assert payload["name"] == key_name
            assert payload["subscription"] == (
                governance_context.resources.owner_admin_subscription
            )
            assert payload["key"].startswith("sk-oai-")
            created_key_id = payload["id"]
        finally:
            if created_key_id is not None:
                cleanup_response = _request_with_gateway_retry(
                    requests.delete,
                    f"{maas_api_base_url}/v1/api-keys/{created_key_id}",
                    headers={"Authorization": f"Bearer {token}"},
                    timeout=30,
                )
                assert cleanup_response.status_code in (200, 404), (
                    f"Grid SSO API-key cleanup failed: "
                    f"{cleanup_response.status_code} {cleanup_response.text}"
                )

    def test_grid_api_key_config_has_no_admin_fields(
        self, maas_api_base_url: str, governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")

        response = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/api-keys/config",
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
        )
        assert response.status_code == 200, (
            f"GET /v1/api-keys/config failed for Grid admin: "
            f"{response.status_code} {response.text}"
        )
        payload = response.json()
        assert set(payload) == {
            "max_expiration_days",
            "ephemeral_max_expiration",
        }
        assert isinstance(payload["max_expiration_days"], int)
        assert payload["max_expiration_days"] > 0
        assert isinstance(payload["ephemeral_max_expiration"], str)

    def test_grid_api_key_cannot_mint_api_keys(
        self, maas_api_base_url: str, governance_context: GridGovernanceContext
    ):
        inference_key = governance_context.keys[0].secret

        response = _request_with_gateway_retry(
            requests.post,
            f"{maas_api_base_url}/v1/api-keys",
            headers={"Authorization": f"Bearer {inference_key}"},
            json={"name": f"grid-gov-child-{uuid.uuid4().hex[:8]}"},
            timeout=30,
        )
        assert response.status_code == 403, (
            f"Expected inference API key to be rejected from API-key minting, got "
            f"{response.status_code} {response.text}"
        )
        assert response.json()["error"] == "API keys cannot create API keys"

    def test_grid_admin_searches_all_users_in_authorized_subscription(
        self, maas_api_base_url: str, governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")
        resources = governance_context.resources

        response = _search_grid_api_keys(
            maas_api_base_url,
            token,
            subscription=resources.governance_subscription,
        )
        assert response.status_code == 200, (
            f"Grid admin API-key search failed: {response.status_code} {response.text}"
        )
        items = response.json()["data"]
        returned_ids = {item["id"] for item in items}
        expected_ids = {
            governance_context.keys[0].key_id,
            governance_context.keys[1].key_id,
        }
        assert returned_ids == expected_ids, (
            f"Grid admin search must return exactly the authorized subscription's "
            f"keys {expected_ids}, got {returned_ids}"
        )
        assert all("key" not in item for item in items)
        assert all(
            item["subscription"] == resources.governance_subscription
            for item in items
        )

    def test_grid_admin_search_without_subscription_stays_self_service(
        self, maas_api_base_url: str, governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")

        response = _search_grid_api_keys(maas_api_base_url, token)
        assert response.status_code == 200, (
            f"Grid admin own-key search failed: "
            f"{response.status_code} {response.text}"
        )
        returned_ids = {item["id"] for item in response.json()["data"]}
        fixture_ids = {key.key_id for key in governance_context.keys}
        assert fixture_ids.isdisjoint(returned_ids), (
            f"Missing subscription must not broaden Grid admin search to "
            f"administered users; unexpectedly returned {fixture_ids & returned_ids}"
        )

    def test_grid_admin_cannot_search_other_organization_keys(
        self, maas_api_base_url: str, governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")

        response = _search_grid_api_keys(
            maas_api_base_url,
            token,
            subscription=(
                governance_context.resources
                .other_organization_governance_subscription
            ),
        )
        assert response.status_code == 403, (
            f"Grid admin must not search another organization's subscription, got "
            f"{response.status_code} {response.text}"
        )

    def test_grid_admin_inspects_authorized_key_metadata(
        self, maas_api_base_url: str, governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")
        key = governance_context.keys[0]

        response = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/api-keys/{key.key_id}",
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
        )
        assert response.status_code == 200, (
            f"Grid admin API-key inspection failed: "
            f"{response.status_code} {response.text}"
        )
        payload = response.json()
        assert payload["id"] == key.key_id
        assert payload["username"] == key.username
        assert payload["subscription"] == key.subscription
        assert payload["status"] == "active"
        assert "Engineering" in payload["groups"]
        assert "key" not in payload

    def test_grid_admin_cannot_inspect_other_organization_key(
        self, maas_api_base_url: str, governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")
        key = governance_context.keys[2]

        response = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/api-keys/{key.key_id}",
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
        )
        assert response.status_code == 404, (
            f"Grid admin must receive enumeration-safe 404 for another "
            f"organization's key, got {response.status_code} {response.text}"
        )

    def test_grid_admin_revokes_authorized_key(
        self, maas_api_base_url: str, governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")
        key = governance_context.keys[1]

        response = _request_with_gateway_retry(
            requests.delete,
            f"{maas_api_base_url}/v1/api-keys/{key.key_id}",
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
        )
        assert response.status_code == 200, (
            f"Grid admin API-key revocation failed: "
            f"{response.status_code} {response.text}"
        )
        assert response.json()["status"] == "revoked"

    def test_grid_admin_cannot_revoke_other_organization_key(
        self, maas_api_base_url: str, governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")
        key = governance_context.keys[2]

        response = _request_with_gateway_retry(
            requests.delete,
            f"{maas_api_base_url}/v1/api-keys/{key.key_id}",
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
        )
        assert response.status_code == 404, (
            f"Grid admin must receive enumeration-safe 404 when revoking another "
            f"organization's key, got {response.status_code} {response.text}"
        )

    def test_grid_admin_bulk_revoke_dry_run_counts_authorized_scope(
        self, maas_api_base_url: str, governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")

        response = _bulk_revoke_grid_api_keys(
            maas_api_base_url,
            token,
            subscription=governance_context.resources.governance_subscription,
            dry_run=True,
        )
        assert response.status_code == 200, (
            f"Grid admin bulk-revoke dry run failed: "
            f"{response.status_code} {response.text}"
        )
        payload = response.json()
        assert payload["revokedCount"] == 1
        assert payload["dryRun"] is True

        owner_token = request_grid_token("alice_lead")
        inspection = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/api-keys/{governance_context.keys[0].key_id}",
            headers={"Authorization": f"Bearer {owner_token}"},
            timeout=30,
        )
        assert inspection.status_code == 200
        assert inspection.json()["status"] == "active"

    def test_grid_admin_cannot_bulk_revoke_other_organization(
        self, maas_api_base_url: str, governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")

        response = _bulk_revoke_grid_api_keys(
            maas_api_base_url,
            token,
            subscription=(
                governance_context.resources
                .other_organization_governance_subscription
            ),
        )
        assert response.status_code == 403, (
            f"Grid admin must not bulk revoke another organization's subscription, "
            f"got {response.status_code} {response.text}"
        )

        owner_token = request_grid_token("alice_lead")
        inspection = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/api-keys/{governance_context.keys[2].key_id}",
            headers={"Authorization": f"Bearer {owner_token}"},
            timeout=30,
        )
        assert inspection.status_code == 200
        assert inspection.json()["status"] == "active"

    def test_grid_admin_bulk_revokes_authorized_user_subscription_scope(
        self, maas_api_base_url: str, governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")

        response = _bulk_revoke_grid_api_keys(
            maas_api_base_url,
            token,
            subscription=governance_context.resources.governance_subscription,
            username="alice_lead",
        )
        assert response.status_code == 200, (
            f"Grid admin bulk revoke failed: {response.status_code} {response.text}"
        )
        assert response.json()["revokedCount"] == 1

        owner_token = request_grid_token("alice_lead")
        inspection = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/api-keys/{governance_context.keys[0].key_id}",
            headers={"Authorization": f"Bearer {owner_token}"},
            timeout=30,
        )
        assert inspection.status_code == 200
        assert inspection.json()["status"] == "revoked"

    def test_grid_admin_username_only_bulk_revoke_is_denied(
        self, maas_api_base_url: str, governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")

        response = _bulk_revoke_grid_api_keys(
            maas_api_base_url,
            token,
            username="alice_lead",
        )
        assert response.status_code == 403, (
            f"Grid admin username-only bulk revoke must be denied, got "
            f"{response.status_code} {response.text}"
        )

        owner_token = request_grid_token("alice_lead")
        inspection = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/api-keys/{governance_context.keys[2].key_id}",
            headers={"Authorization": f"Bearer {owner_token}"},
            timeout=30,
        )
        assert inspection.status_code == 200
        assert inspection.json()["status"] == "active"


class TestGridGovernanceIdentity:
    def test_me_returns_verified_grid_identity(self, maas_api_base_url: str):
        token = request_grid_token("grid_admin")

        response = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/me",
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
        )
        assert response.status_code == 200, (
            f"GET /v1/me failed for Grid admin: "
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
        assert identity["sub"]
        assert identity["https://redhat.com/maas/orgs"] == [
            {
                "organization_id": "42c3e46f-2477-44d7-a85b-d3b43f6b31fa",
                "roles": ["admins"],
            },
            {
                "organization_id": "53d4f57e-3588-55e8-b96c-4ec54a7c42fa",
                "roles": ["users"],
            },
        ]

    def test_me_returns_empty_organizations_without_membership(
        self, maas_api_base_url: str
    ):
        token = request_grid_token("grid_no_org")

        response = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/me",
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
        )
        assert response.status_code == 200, (
            f"GET /v1/me failed for grid_no_org: "
            f"{response.status_code} {response.text}"
        )

        identity = response.json()
        assert set(identity) <= {
            "sub",
            "email",
            "name",
            "https://redhat.com/maas/orgs",
        }
        assert identity["sub"]
        assert identity["https://redhat.com/maas/orgs"] == []

    def test_me_requires_verified_sso_identity(self, maas_api_base_url: str):
        response = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/me",
            timeout=30,
        )
        assert response.status_code == 401, (
            f"Expected unauthenticated /v1/me to return 401, got "
            f"{response.status_code} {response.text}"
        )

    def test_me_rejects_inference_api_key(
        self, maas_api_base_url: str, governance_context: GridGovernanceContext
    ):
        inference_key = governance_context.keys[2].secret

        response = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/me",
            headers={"Authorization": f"Bearer {inference_key}"},
            timeout=30,
        )
        assert response.status_code == 401, (
            f"Expected /v1/me to reject an inference API key, got "
            f"{response.status_code} {response.text}"
        )
