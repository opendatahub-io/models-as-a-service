"""Isolated Kubernetes resources for Grid admin-inventory E2E tests."""

from __future__ import annotations

import uuid
from dataclasses import dataclass

from test_helper import (
    MODEL_NAMESPACE,
    MODEL_REF,
    _apply_cr,
    _create_maas_model_ref,
    _delete_crs_and_wait,
    _delete_governance_and_wait,
    _ns,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
    _wait_for_model_ready,
)


ORGANIZATION_A = "42c3e46f-2477-44d7-a85b-d3b43f6b31fa"
ORGANIZATION_B = "53d4f57e-3588-55e8-b96c-4ec54a7c42fa"


@dataclass(frozen=True)
class GridInventoryResources:
    owner_admin_subscription: str
    admin_only_subscription: str
    other_organization_subscription: str
    owner_model_ref: str
    unmatched_model_ref: str
    owner_auth_policy: str
    unmatched_auth_policy: str
    owner_group: str
    owner_user: str
    admin_only_owner_group: str
    other_organization_owner_group: str
    non_owner_group: str
    unqualified_user: str


def new_grid_inventory_resources() -> GridInventoryResources:
    suffix = uuid.uuid4().hex[:8]
    return GridInventoryResources(
        owner_admin_subscription=f"grid-inv-{suffix}-owner",
        admin_only_subscription=f"grid-inv-{suffix}-admin",
        other_organization_subscription=f"grid-inv-{suffix}-org-b",
        owner_model_ref=f"grid-inv-{suffix}-model",
        unmatched_model_ref=f"grid-inv-{suffix}-unmatched",
        owner_auth_policy=f"grid-inv-{suffix}-owner-access",
        unmatched_auth_policy=f"grid-inv-{suffix}-unmatched-access",
        owner_group=f"{ORGANIZATION_A}/ai-grid-admins",
        owner_user=f"{ORGANIZATION_A}/inventory-owner",
        admin_only_owner_group=f"{ORGANIZATION_A}/admin-only-owners",
        other_organization_owner_group=f"{ORGANIZATION_B}/unrelated-owners",
        non_owner_group=f"{ORGANIZATION_A}/model-only-users",
        unqualified_user="unqualified-inventory-user",
    )


def _apply_auth_policy(
    name: str,
    model_ref: str,
    groups: list[str],
    users: list[str],
) -> None:
    _apply_cr(
        {
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSAuthPolicy",
            "metadata": {"name": name, "namespace": _subscription_namespace()},
            "spec": {
                "modelRefs": [
                    {"name": model_ref, "namespace": MODEL_NAMESPACE}
                ],
                "subjects": {
                    "groups": [{"name": group} for group in groups],
                    "users": users,
                },
            },
        }
    )


def _subscription_namespace() -> str:
    return _ns()


def _apply_subscription(
    name: str,
    model_refs: list[str],
    owner_groups: list[str],
    owner_users: list[str],
    organization_id: str,
) -> None:
    _apply_cr(
        {
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSSubscription",
            "metadata": {"name": name, "namespace": _subscription_namespace()},
            "spec": {
                "owner": {
                    "groups": [{"name": group} for group in owner_groups],
                    "users": owner_users,
                },
                "organization": {"id": organization_id},
                "modelRefs": [
                    {
                        "name": model_ref,
                        "namespace": MODEL_NAMESPACE,
                        "tokenRateLimits": [{"limit": 100, "window": "1m"}],
                    }
                    for model_ref in model_refs
                ],
                "priority": 30,
            },
        }
    )


def apply_grid_inventory_resources(resources: GridInventoryResources) -> None:
    _wait_for_model_ready(MODEL_REF, namespace=MODEL_NAMESPACE, timeout=180)
    _create_maas_model_ref(
        resources.owner_model_ref,
        MODEL_NAMESPACE,
        MODEL_REF,
    )
    _create_maas_model_ref(
        resources.unmatched_model_ref,
        MODEL_NAMESPACE,
        MODEL_REF,
    )
    _wait_for_model_ready(
        resources.owner_model_ref, namespace=MODEL_NAMESPACE, timeout=180
    )
    _wait_for_model_ready(
        resources.unmatched_model_ref, namespace=MODEL_NAMESPACE, timeout=180
    )

    _apply_auth_policy(
        resources.owner_auth_policy,
        resources.owner_model_ref,
        [
            resources.owner_group,
            resources.admin_only_owner_group,
            resources.other_organization_owner_group,
            resources.non_owner_group,
        ],
        [resources.owner_user, resources.unqualified_user],
    )
    _apply_auth_policy(
        resources.unmatched_auth_policy,
        resources.unmatched_model_ref,
        [resources.non_owner_group],
        [resources.unqualified_user],
    )
    _apply_subscription(
        resources.owner_admin_subscription,
        [resources.owner_model_ref, resources.unmatched_model_ref],
        [resources.owner_group],
        [resources.owner_user],
        ORGANIZATION_A,
    )
    _apply_subscription(
        resources.admin_only_subscription,
        [resources.owner_model_ref],
        [resources.admin_only_owner_group],
        [],
        ORGANIZATION_A,
    )
    _apply_subscription(
        resources.other_organization_subscription,
        [resources.owner_model_ref],
        [resources.other_organization_owner_group],
        [],
        ORGANIZATION_B,
    )

    namespace = _subscription_namespace()
    for policy_name in (
        resources.owner_auth_policy,
        resources.unmatched_auth_policy,
    ):
        _wait_for_maas_auth_policy_phase(
            policy_name,
            namespace=namespace,
            timeout=180,
            require_enforced=False,
        )
    for subscription_name in (
        resources.owner_admin_subscription,
        resources.admin_only_subscription,
        resources.other_organization_subscription,
    ):
        _wait_for_maas_subscription_phase(
            subscription_name,
            namespace=namespace,
            timeout=180,
        )


def cleanup_grid_inventory_resources(resources: GridInventoryResources) -> None:
    namespace = _subscription_namespace()
    _delete_governance_and_wait(
        subscriptions=[
            (resources.owner_admin_subscription, namespace),
            (resources.admin_only_subscription, namespace),
            (resources.other_organization_subscription, namespace),
        ],
        auth_policies=[
            (resources.owner_auth_policy, namespace),
            (resources.unmatched_auth_policy, namespace),
        ],
        timeout=120,
    )
    _delete_crs_and_wait(
        [
            ("maasmodelref", resources.owner_model_ref, MODEL_NAMESPACE),
            ("maasmodelref", resources.unmatched_model_ref, MODEL_NAMESPACE),
        ],
        timeout=120,
    )
