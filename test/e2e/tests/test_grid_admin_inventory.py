"""System-level tests for Grid organization-admin inventory responses."""

from __future__ import annotations

from collections.abc import Iterator

import json
import os

import pytest
import requests

from grid_inventory_fixtures import (
    ORGANIZATION_A,
    ORGANIZATION_B,
    GridInventoryResources,
    apply_grid_inventory_resources,
    cleanup_grid_inventory_resources,
    new_grid_inventory_resources,
)
from grid_oidc_helpers import (
    find_grid_subscription,
    get_grid_subscriptions,
    request_grid_token,
    wait_for_grid_subscription,
)
from test_helper import MODEL_NAMESPACE, _request_with_gateway_retry, kubectl_curl


pytestmark = [
    pytest.mark.nightly,
    pytest.mark.grid_oidc,
    pytest.mark.skipif(
        os.environ.get("EXTERNAL_OIDC", "").lower() != "true",
        reason="EXTERNAL_OIDC is not true",
    ),
    pytest.mark.xdist_group("external"),
]


@pytest.fixture(scope="module")
def grid_inventory_resources(
    maas_api_base_url: str,
) -> Iterator[GridInventoryResources]:
    resources = new_grid_inventory_resources()
    try:
        apply_grid_inventory_resources(resources)
        wait_for_grid_subscription(
            maas_api_base_url,
            "grid_admin",
            resources.owner_admin_subscription,
        )
        wait_for_grid_subscription(
            maas_api_base_url,
            "grid_admin",
            resources.admin_only_subscription,
        )
        wait_for_grid_subscription(
            maas_api_base_url,
            "grid_admin_b",
            resources.other_organization_subscription,
        )
        yield resources
    finally:
        cleanup_grid_inventory_resources(resources)


class TestGridAdminInventory:
    def test_grid_admin_receives_owner_and_admin_inventory(
        self, maas_api_base_url: str, grid_inventory_resources: GridInventoryResources
    ):
        token = request_grid_token("grid_admin")
        subscriptions = get_grid_subscriptions(maas_api_base_url, token)
        subscription = find_grid_subscription(
            subscriptions, grid_inventory_resources.owner_admin_subscription
        )
        assert subscription is not None

        assert subscription["roles"] == ["owner", "admin"]
        assert subscription["organization_id"] == ORGANIZATION_A
        assert subscription["owner"] == {
            "groups": [{"name": grid_inventory_resources.owner_group}],
            "users": [grid_inventory_resources.owner_user],
        }

        model_refs = subscription["model_refs"]
        assert [model_ref["name"] for model_ref in model_refs] == [
            grid_inventory_resources.owner_model_ref
        ]
        assert model_refs[0]["subjects"] == {
            "groups": [{"name": grid_inventory_resources.owner_group}],
            "users": [grid_inventory_resources.owner_user],
        }

        model_refs = _grid_model_refs(
            maas_api_base_url,
            token,
            grid_inventory_resources.owner_admin_subscription,
        )
        assert (
            f"{MODEL_NAMESPACE}/{grid_inventory_resources.owner_model_ref}"
            in model_refs
        )
        assert (
            f"{MODEL_NAMESPACE}/{grid_inventory_resources.unmatched_model_ref}"
            not in model_refs
        )

    def test_grid_admin_receives_admin_only_inventory(
        self, maas_api_base_url: str, grid_inventory_resources: GridInventoryResources
    ):
        token = request_grid_token("grid_admin")
        subscriptions = get_grid_subscriptions(maas_api_base_url, token)
        subscription = find_grid_subscription(
            subscriptions, grid_inventory_resources.admin_only_subscription
        )
        assert subscription is not None

        assert subscription["roles"] == ["admin"]
        assert subscription["organization_id"] == ORGANIZATION_A
        assert subscription["owner"] == {
            "groups": [
                {"name": grid_inventory_resources.admin_only_owner_group}
            ]
        }

        model_refs = subscription["model_refs"]
        assert len(model_refs) == 1
        assert model_refs[0]["name"] == grid_inventory_resources.owner_model_ref
        assert model_refs[0]["subjects"] == {
            "groups": [
                {"name": grid_inventory_resources.admin_only_owner_group}
            ]
        }

    def test_grid_admin_does_not_receive_other_organization_inventory(
        self, maas_api_base_url: str, grid_inventory_resources: GridInventoryResources
    ):
        token = request_grid_token("grid_admin")
        subscriptions = get_grid_subscriptions(maas_api_base_url, token)

        assert find_grid_subscription(
            subscriptions,
            grid_inventory_resources.other_organization_subscription,
        ) is None
        assert find_grid_subscription(
            subscriptions,
            grid_inventory_resources.owner_admin_subscription,
        ) is not None
        assert find_grid_subscription(
            subscriptions,
            grid_inventory_resources.admin_only_subscription,
        ) is not None

    def test_grid_admin_b_receives_only_its_organization_inventory(
        self, maas_api_base_url: str, grid_inventory_resources: GridInventoryResources
    ):
        token = request_grid_token("grid_admin_b")
        subscriptions = get_grid_subscriptions(maas_api_base_url, token)
        subscription = find_grid_subscription(
            subscriptions,
            grid_inventory_resources.other_organization_subscription,
        )

        assert subscription is not None
        assert subscription["organization_id"] == ORGANIZATION_B
        assert subscription["owner"]["groups"] == [
            {"name": grid_inventory_resources.other_organization_owner_group}
        ]
        assert find_grid_subscription(
            subscriptions,
            grid_inventory_resources.owner_admin_subscription,
        ) is None
        assert find_grid_subscription(
            subscriptions,
            grid_inventory_resources.admin_only_subscription,
        ) is None
        assert [model_ref["name"] for model_ref in subscription["model_refs"]] == [
            grid_inventory_resources.unmatched_model_ref
        ]

        model_refs = _grid_model_refs(
            maas_api_base_url,
            token,
            grid_inventory_resources.other_organization_subscription,
        )
        assert (
            f"{MODEL_NAMESPACE}/{grid_inventory_resources.unmatched_model_ref}"
            in model_refs
        )
        assert (
            f"{MODEL_NAMESPACE}/{grid_inventory_resources.owner_model_ref}"
            not in model_refs
        )

    def test_flat_identity_api_key_uses_qualified_group_snapshot(
        self,
        maas_api_base_url: str,
        maas_api_internal_url: str,
        grid_inventory_resources: GridInventoryResources,
    ):
        token = request_grid_token("grid_admin")
        create_response = _request_with_gateway_retry(
            requests.post,
            f"{maas_api_base_url}/v1/api-keys",
            headers={"Authorization": f"Bearer {token}"},
            json={
                "name": f"grid-flat-groups-{grid_inventory_resources.suffix}",
                "subscription": grid_inventory_resources.owner_admin_subscription,
            },
            timeout=30,
        )
        assert create_response.status_code == 201, (
            f"Flat-identity API-key creation failed: "
            f"{create_response.status_code} {create_response.text}"
        )
        created_key = create_response.json()
        api_key = created_key["key"]

        try:
            status_code, body = kubectl_curl(
                f"{maas_api_internal_url}/internal/v1/api-keys/validate",
                method="POST",
                json_body={"key": api_key},
            )
            assert status_code == 200, (
                f"Internal API-key validation failed: {status_code} {body}"
            )
            validated_key = json.loads(body)
            assert validated_key["groups"] == [
                "system:authenticated",
                grid_inventory_resources.owner_group,
            ]
            assert json.loads(validated_key["groupsJSON"]) == validated_key["groups"]

            model_refs = _grid_model_refs(maas_api_base_url, api_key)
            assert (
                f"{MODEL_NAMESPACE}/{grid_inventory_resources.owner_model_ref}"
                in model_refs
            )
            assert (
                f"{MODEL_NAMESPACE}/{grid_inventory_resources.unmatched_model_ref}"
                not in model_refs
            )
        finally:
            cleanup_response = _request_with_gateway_retry(
                requests.delete,
                f"{maas_api_base_url}/v1/api-keys/{created_key['id']}",
                headers={"Authorization": f"Bearer {token}"},
                timeout=30,
            )
            assert cleanup_response.status_code in (200, 404), (
                f"Flat-identity API-key cleanup failed: "
                f"{cleanup_response.status_code} {cleanup_response.text}"
            )


def _grid_model_refs(
    maas_api_base_url: str, token: str, subscription: str | None = None
) -> set[str]:
    headers = {"Authorization": f"Bearer {token}"}
    if subscription is not None:
        headers["X-MaaS-Subscription"] = subscription
    response = _request_with_gateway_retry(
        requests.get,
        f"{maas_api_base_url}/v1/models",
        headers=headers,
        timeout=45,
    )
    assert response.status_code == 200, (
        f"GET /v1/models failed for Grid identity: "
        f"{response.status_code} {response.text}"
    )
    return {
        model["owned_by"]
        for model in response.json()["data"]
        if model.get("owned_by")
    }
