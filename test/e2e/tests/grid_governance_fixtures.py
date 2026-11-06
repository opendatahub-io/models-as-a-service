"""API-key fixtures for Grid governance E2E tests."""

from __future__ import annotations

import uuid
from collections.abc import Iterator
from dataclasses import dataclass

import pytest
import requests

from grid_inventory_fixtures import (
    ORGANIZATION_A,
    GridInventoryResources,
    apply_grid_inventory_resources,
    cleanup_grid_inventory_resources,
    new_grid_inventory_resources,
)
from grid_oidc_helpers import request_grid_token
from test_helper import (
    MODEL_NAMESPACE,
    _apply_cr,
    _delete_governance_and_wait,
    _ns,
    _request_with_gateway_retry,
    _wait_for_maas_subscription_phase,
)


@dataclass(frozen=True)
class GridGovernanceKey:
    key_id: str
    username: str
    subscription: str
    secret: str


@dataclass(frozen=True)
class GridGovernanceContext:
    resources: GridInventoryResources
    keys: tuple[GridGovernanceKey, ...]


@dataclass(frozen=True)
class GridRetainedSubscriptionContext:
    subscription_name: str
    key: GridGovernanceKey


def create_governance_api_key(
    maas_api_base_url: str,
    username: str,
    subscription: str,
    *,
    suffix: str,
) -> GridGovernanceKey:
    token = request_grid_token(username)
    response = _request_with_gateway_retry(
        requests.post,
        f"{maas_api_base_url}/v1/api-keys",
        headers={"Authorization": f"Bearer {token}"},
        json={
            "name": f"grid-gov-{suffix}-{username}-{subscription[-4:]}",
            "subscription": subscription,
        },
        timeout=30,
    )
    assert response.status_code == 201, (
        f"Grid API-key creation failed for {username}/{subscription}: "
        f"{response.status_code} {response.text}"
    )
    payload = response.json()
    assert payload["key"].startswith("sk-oai-")
    assert payload["subscription"] == subscription
    return GridGovernanceKey(
        key_id=payload["id"],
        username=username,
        subscription=subscription,
        secret=payload["key"],
    )


def create_grid_governance_context(
    maas_api_base_url: str,
    resources: GridInventoryResources,
) -> GridGovernanceContext:
    suffix = uuid.uuid4().hex[:8]
    keys: list[GridGovernanceKey] = []
    try:
        keys.append(
            create_governance_api_key(
                maas_api_base_url,
                "alice_lead",
                resources.governance_subscription,
                suffix=suffix,
            )
        )
        keys.append(
            create_governance_api_key(
                maas_api_base_url,
                "bob_sre",
                resources.governance_subscription,
                suffix=suffix,
            )
        )
        keys.append(
            create_governance_api_key(
                maas_api_base_url,
                "alice_lead",
                resources.other_organization_governance_subscription,
                suffix=suffix,
            )
        )
    except BaseException:
        for key in keys:
            revoke_governance_api_key(maas_api_base_url, key)
        raise
    return GridGovernanceContext(resources=resources, keys=tuple(keys))


def revoke_governance_api_key(maas_api_base_url: str, key: GridGovernanceKey) -> None:
    token = request_grid_token(key.username)
    response = _request_with_gateway_retry(
        requests.delete,
        f"{maas_api_base_url}/v1/api-keys/{key.key_id}",
        headers={"Authorization": f"Bearer {token}"},
        timeout=30,
    )
    assert response.status_code in (200, 404), (
        f"Grid API-key cleanup failed for {key.username}/{key.key_id}: "
        f"{response.status_code} {response.text}"
    )


def cleanup_grid_governance_context(
    maas_api_base_url: str, context: GridGovernanceContext
) -> None:
    for key in context.keys:
        revoke_governance_api_key(maas_api_base_url, key)


@pytest.fixture
def grid_retained_subscription_context(
    maas_api_base_url: str,
    grid_governance_context: GridGovernanceContext,
) -> Iterator[GridRetainedSubscriptionContext]:
    resources = grid_governance_context.resources
    suffix = uuid.uuid4().hex[:8]
    subscription_name = f"grid-retain-{suffix}-sub"
    namespace = _ns()
    key: GridGovernanceKey | None = None

    try:
        _apply_cr(
            {
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {
                    "name": subscription_name,
                    "namespace": namespace,
                    "annotations": {
                        "maas.opendatahub.io/retain-api-keys-on-delete": "true"
                    },
                },
                "spec": {
                    "owner": {
                        "groups": [{"name": resources.governance_owner_group}],
                    },
                    "organization": {"id": ORGANIZATION_A},
                    "modelRefs": [
                        {
                            "name": resources.owner_model_ref,
                            "namespace": MODEL_NAMESPACE,
                            "tokenRateLimits": [{"limit": 100, "window": "1m"}],
                        }
                    ],
                    "priority": 35,
                },
            }
        )
        _wait_for_maas_subscription_phase(subscription_name, namespace=namespace)
        key = create_governance_api_key(
            maas_api_base_url,
            "alice_lead",
            subscription_name,
            suffix=suffix,
        )
        yield GridRetainedSubscriptionContext(
            subscription_name=subscription_name,
            key=key,
        )
    finally:
        if key is not None:
            revoke_governance_api_key(maas_api_base_url, key)
        _delete_governance_and_wait(subscriptions=[(subscription_name, namespace)])


@pytest.fixture(scope="module")
def grid_governance_context(
    maas_api_base_url: str,
) -> Iterator[GridGovernanceContext]:
    resources = new_grid_inventory_resources()
    context: GridGovernanceContext | None = None
    try:
        apply_grid_inventory_resources(resources)
        context = create_grid_governance_context(maas_api_base_url, resources)
        yield context
    finally:
        if context is not None:
            cleanup_grid_governance_context(maas_api_base_url, context)
        cleanup_grid_inventory_resources(resources)
