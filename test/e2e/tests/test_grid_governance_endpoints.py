# ruff: noqa: F811

"""System-level tests for Grid authorization across governance endpoints."""

from __future__ import annotations

import os
import uuid

import pytest
import requests
from grid_governance_fixtures import (
    GridGovernanceContext,
    create_governance_api_key,
    grid_governance_context,  # noqa: F401 - pytest fixture
    revoke_governance_api_key,
)
from grid_inventory_fixtures import ORGANIZATION_A, ORGANIZATION_B
from grid_oidc_helpers import request_grid_token
from test_helper import _ns, _request_with_gateway_retry

pytestmark = [
    pytest.mark.nightly,
    pytest.mark.grid_oidc,
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
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
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
                        grid_governance_context.resources.owner_admin_subscription
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
                grid_governance_context.resources.owner_admin_subscription
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
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
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
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
    ):
        inference_key = grid_governance_context.keys[0].secret

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

    def test_grid_admin_searches_all_users_in_authorized_subscription(
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")
        resources = grid_governance_context.resources

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
            grid_governance_context.keys[0].key_id,
            grid_governance_context.keys[1].key_id,
        }
        assert returned_ids == expected_ids, (
            f"Grid admin search must return exactly the authorized subscription's "
            f"keys {expected_ids}, got {returned_ids}"
        )
        assert all("key" not in item for item in items)
        assert all(
            item["subscription"] == resources.governance_subscription for item in items
        )

    def test_grid_admin_search_canonicalizes_authorized_username_scope(
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")
        resources = grid_governance_context.resources
        subscription_alias = f"{_ns()}/{resources.governance_subscription}"

        response = _search_grid_api_keys(
            maas_api_base_url,
            token,
            subscription=subscription_alias,
            username="bob_sre",
        )
        assert response.status_code == 200, (
            f"Grid admin canonical API-key search failed: "
            f"{response.status_code} {response.text}"
        )
        items = response.json()["data"]
        returned_ids = {item["id"] for item in items}
        expected_ids = {grid_governance_context.keys[1].key_id}
        assert returned_ids == expected_ids, (
            f"Canonical Grid admin search must return exactly Bob's key "
            f"{expected_ids}, got {returned_ids}"
        )
        assert all("key" not in item for item in items)
        assert all(
            item["subscription"] == resources.governance_subscription for item in items
        )

    def test_grid_admin_search_without_subscription_returns_administered_keys(
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")

        response = _search_grid_api_keys(maas_api_base_url, token)
        assert response.status_code == 200, (
            f"Grid admin own-key search failed: {response.status_code} {response.text}"
        )
        payload = response.json()
        assert isinstance(payload["data"], list), (
            f"Grid admin own-key search must return a data array, got "
            f"{payload['data']!r}"
        )
        returned_ids = {item["id"] for item in payload["data"]}
        expected_ids = {
            grid_governance_context.keys[0].key_id,
            grid_governance_context.keys[1].key_id,
        }
        assert expected_ids.issubset(returned_ids), (
            f"Missing subscription must return all keys from Grid admin's "
            f"authorized organization; expected {expected_ids}, got {returned_ids}"
        )
        assert grid_governance_context.keys[2].key_id not in returned_ids, (
            f"Missing subscription must not broaden Grid admin search to another "
            f"organization; unexpectedly returned "
            f"{grid_governance_context.keys[2].key_id}"
        )

    def test_grid_admin_cannot_search_other_organization_keys(
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")
        resources = grid_governance_context.resources

        response = _search_grid_api_keys(
            maas_api_base_url,
            token,
            subscription=resources.other_organization_governance_subscription,
        )
        assert response.status_code == 200, (
            f"Grid admin must receive an empty result for another organization's "
            f"subscription, got "
            f"{response.status_code} {response.text}"
        )
        payload = response.json()
        assert payload["object"] == "list"
        assert payload["data"] in ([], None)
        assert payload["has_more"] is False

    def test_grid_admin_inspects_authorized_key_metadata(
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")
        key = grid_governance_context.keys[0]

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
        assert "key" not in payload

    def test_grid_admin_cannot_inspect_other_organization_key(
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")
        key = grid_governance_context.keys[2]

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
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")
        key = grid_governance_context.keys[1]

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
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")
        key = grid_governance_context.keys[2]

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
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")

        initial_statuses = {}
        for key in grid_governance_context.keys[:2]:
            inspection = _request_with_gateway_retry(
                requests.get,
                f"{maas_api_base_url}/v1/api-keys/{key.key_id}",
                headers={"Authorization": f"Bearer {token}"},
                timeout=30,
            )
            assert inspection.status_code == 200
            initial_statuses[key.key_id] = inspection.json()["status"]

        response = _bulk_revoke_grid_api_keys(
            maas_api_base_url,
            token,
            subscription=grid_governance_context.resources.governance_subscription,
            dry_run=True,
        )
        assert response.status_code == 200, (
            f"Grid admin bulk-revoke dry run failed: "
            f"{response.status_code} {response.text}"
        )
        payload = response.json()
        expected_active_count = sum(
            status == "active" for status in initial_statuses.values()
        )
        assert payload["revokedCount"] == expected_active_count
        assert payload["dryRun"] is True

        owner_token = request_grid_token("alice_lead")
        inspection = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/api-keys/{grid_governance_context.keys[0].key_id}",
            headers={"Authorization": f"Bearer {owner_token}"},
            timeout=30,
        )
        assert inspection.status_code == 200
        assert (
            inspection.json()["status"]
            == initial_statuses[grid_governance_context.keys[0].key_id]
        )

    def test_grid_admin_cannot_bulk_revoke_other_organization(
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")
        resources = grid_governance_context.resources

        response = _bulk_revoke_grid_api_keys(
            maas_api_base_url,
            token,
            subscription=resources.other_organization_governance_subscription,
        )
        assert response.status_code == 403, (
            f"Grid admin must not bulk revoke another organization's subscription, "
            f"got {response.status_code} {response.text}"
        )

        owner_token = request_grid_token("alice_lead")
        inspection = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/api-keys/{grid_governance_context.keys[2].key_id}",
            headers={"Authorization": f"Bearer {owner_token}"},
            timeout=30,
        )
        assert inspection.status_code == 200
        assert inspection.json()["status"] == "active"

    def test_grid_admin_bulk_revokes_authorized_user_subscription_scope(
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")

        response = _bulk_revoke_grid_api_keys(
            maas_api_base_url,
            token,
            subscription=grid_governance_context.resources.governance_subscription,
            username="alice_lead",
        )
        assert response.status_code == 200, (
            f"Grid admin bulk revoke failed: {response.status_code} {response.text}"
        )
        assert response.json()["revokedCount"] == 1

        owner_token = request_grid_token("alice_lead")
        inspection = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/api-keys/{grid_governance_context.keys[0].key_id}",
            headers={"Authorization": f"Bearer {owner_token}"},
            timeout=30,
        )
        assert inspection.status_code == 200
        assert inspection.json()["status"] == "revoked"

    def test_grid_admin_username_only_bulk_revoke_stays_in_organization(
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
    ):
        token = request_grid_token("grid_admin")
        key = create_governance_api_key(
            maas_api_base_url,
            "alice_lead",
            grid_governance_context.resources.governance_subscription,
            suffix=uuid.uuid4().hex[:8],
        )

        try:
            owner_token = request_grid_token("alice_lead")
            initial_statuses = []
            for key_id in (grid_governance_context.keys[0].key_id, key.key_id):
                inspection = _request_with_gateway_retry(
                    requests.get,
                    f"{maas_api_base_url}/v1/api-keys/{key_id}",
                    headers={"Authorization": f"Bearer {owner_token}"},
                    timeout=30,
                )
                assert inspection.status_code == 200
                initial_statuses.append(inspection.json()["status"])

            response = _bulk_revoke_grid_api_keys(
                maas_api_base_url,
                token,
                username="alice_lead",
            )
            assert response.status_code == 200, (
                f"Grid admin username-only bulk revoke failed: "
                f"{response.status_code} {response.text}"
            )
            expected_active_count = sum(
                status == "active" for status in initial_statuses
            )
            assert response.json()["revokedCount"] == expected_active_count

            inspection = _request_with_gateway_retry(
                requests.get,
                f"{maas_api_base_url}/v1/api-keys/{key.key_id}",
                headers={"Authorization": f"Bearer {owner_token}"},
                timeout=30,
            )
            assert inspection.status_code == 200
            assert inspection.json()["status"] == "revoked"

            inspection = _request_with_gateway_retry(
                requests.get,
                f"{maas_api_base_url}/v1/api-keys/{grid_governance_context.keys[2].key_id}",
                headers={"Authorization": f"Bearer {owner_token}"},
                timeout=30,
            )
            assert inspection.status_code == 200
            assert inspection.json()["status"] == "active"
        finally:
            revoke_governance_api_key(maas_api_base_url, key)

    def test_non_admin_username_only_bulk_revoke_is_denied(
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
    ):
        token = request_grid_token("alice_lead")

        response = _bulk_revoke_grid_api_keys(
            maas_api_base_url,
            token,
            username="bob_sre",
        )
        assert response.status_code == 403, (
            f"Non-admin username-only bulk revoke must be denied, got "
            f"{response.status_code} {response.text}"
        )

        owner_token = request_grid_token("alice_lead")
        inspection = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/api-keys/{grid_governance_context.keys[2].key_id}",
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
            f"GET /v1/me failed for Grid admin: {response.status_code} {response.text}"
        )
        assert response.headers.get("Cache-Control") == "no-store"

        identity = response.json()
        assert set(identity) <= {
            "iss",
            "sub",
            "email",
            "name",
            "given_name",
            "family_name",
            "preferred_username",
            "groups",
            "org_id",
            "org_name",
            "roles",
        }
        assert identity["iss"]
        assert identity["sub"]
        assert identity["preferred_username"] == "grid_admin"
        assert identity["groups"] == ["ai-grid-admins"]
        assert identity["org_id"] == ORGANIZATION_A
        assert identity["org_name"] == "coke"
        assert identity["roles"] == ["admins"]
        assert "https://redhat.com/maas/orgs" not in identity

    def test_me_returns_flat_identity_for_second_organization(
        self, maas_api_base_url: str
    ):
        token = request_grid_token("grid_admin_b")

        response = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/me",
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
        )
        assert response.status_code == 200, (
            f"GET /v1/me failed for Grid organization B admin: {response.status_code}"
        )
        identity = response.json()
        assert identity["preferred_username"] == "grid_admin_b"
        assert identity["groups"] == ["ai-grid-admins"]
        assert identity["org_id"] == ORGANIZATION_B
        assert identity["org_name"] == "org-b"
        assert identity["roles"] == ["admins"]
        assert "https://redhat.com/maas/orgs" not in identity

    def test_me_omits_organization_claims_without_organization_identity(
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
            f"GET /v1/me failed for grid_no_org: {response.status_code} {response.text}"
        )

        identity = response.json()
        assert set(identity) <= {
            "iss",
            "sub",
            "email",
            "name",
            "given_name",
            "family_name",
            "preferred_username",
            "groups",
            "org_id",
            "org_name",
            "roles",
        }
        assert identity["sub"]
        assert {"org_id", "org_name", "roles"}.isdisjoint(identity)

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
        self, maas_api_base_url: str, grid_governance_context: GridGovernanceContext
    ):
        inference_key = grid_governance_context.keys[2].secret

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
