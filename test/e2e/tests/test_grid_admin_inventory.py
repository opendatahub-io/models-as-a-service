"""System-level tests for Grid organization-admin inventory responses."""

from __future__ import annotations

from collections.abc import Iterator

import os

import pytest

from grid_inventory_fixtures import (
    ORGANIZATION_A,
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
def grid_inventory_resources(maas_api_base_url: str) -> Iterator[GridInventoryResources]:
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
            grid_inventory_resources.owner_model_ref,
            grid_inventory_resources.unmatched_model_ref,
        ]
        assert model_refs[0]["subjects"] == {
            "groups": [{"name": grid_inventory_resources.owner_group}],
            "users": [grid_inventory_resources.owner_user],
        }
        assert "subjects" not in model_refs[1]

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
