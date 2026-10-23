"""Cross-MaaS-tenant resources for Grid governance E2E tests."""

from __future__ import annotations

import uuid
from collections.abc import Iterator
from dataclasses import dataclass

import pytest
import requests

from grid_inventory_fixtures import ORGANIZATION_A
from grid_oidc_helpers import request_tenant_b_token
from multitenancy_helpers import provision_tenant_model
from test_helper import (
    _apply_cr,
    _delete_crs_and_wait,
    _delete_governance_and_wait,
    _request_with_gateway_retry,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
)


@dataclass(frozen=True)
class GridCrossTenantContext:
    tenant_a: dict[str, str]
    tenant_b: dict[str, str]
    subscription_name: str
    auth_policy_name: str
    api_key_id: str


@pytest.fixture(scope="module")
def grid_cross_tenant_context(
    shared_test_tenants,
) -> Iterator[GridCrossTenantContext]:
    tenant_a, tenant_b = shared_test_tenants
    suffix = uuid.uuid4().hex[:8]
    subscription_name = f"grid-xt-{suffix}-sub"
    auth_policy_name = f"grid-xt-{suffix}-access"
    model_name = f"grid-xt-{suffix}-model"
    namespace = tenant_b["namespace"]

    try:
        provision_tenant_model(model_name, namespace, tenant_b["gateway_name"])
        _apply_cr(
            {
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {
                    "name": auth_policy_name,
                    "namespace": namespace,
                },
                "spec": {
                    "modelRefs": [{"name": model_name, "namespace": namespace}],
                    "subjects": {
                        "groups": [{"name": "Product-Security"}],
                        "users": ["charlie_sec_lead"],
                    },
                },
            }
        )
        _apply_cr(
            {
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {
                    "name": subscription_name,
                    "namespace": namespace,
                },
                "spec": {
                    "owner": {
                        "groups": [{"name": "Product-Security"}],
                        "users": ["charlie_sec_lead"],
                    },
                    "organization": {"id": ORGANIZATION_A},
                    "modelRefs": [
                        {
                            "name": model_name,
                            "namespace": namespace,
                            "tokenRateLimits": [{"limit": 100, "window": "1m"}],
                        }
                    ],
                    "priority": 40,
                },
            }
        )
        _wait_for_maas_auth_policy_phase(
            auth_policy_name,
            namespace=namespace,
            timeout=180,
            require_enforced=False,
        )
        _wait_for_maas_subscription_phase(
            subscription_name,
            namespace=namespace,
            timeout=180,
        )

        tenant_b_token = request_tenant_b_token("charlie_sec_lead")
        creation = _request_with_gateway_retry(
            requests.post,
            f"{tenant_b['base_url']}/v1/api-keys",
            headers={"Authorization": f"Bearer {tenant_b_token}"},
            json={
                "name": f"grid-xt-{suffix}-key",
                "subscription": subscription_name,
            },
            timeout=30,
        )
        assert creation.status_code == 201, (
            f"Tenant B API-key creation failed: {creation.status_code} {creation.text}"
        )
        api_key_id = creation.json()["id"]

        yield GridCrossTenantContext(
            tenant_a=tenant_a,
            tenant_b=tenant_b,
            subscription_name=subscription_name,
            auth_policy_name=auth_policy_name,
            api_key_id=api_key_id,
        )

        cleanup_token = request_tenant_b_token("charlie_sec_lead")
        cleanup = _request_with_gateway_retry(
            requests.delete,
            f"{tenant_b['base_url']}/v1/api-keys/{api_key_id}",
            headers={"Authorization": f"Bearer {cleanup_token}"},
            timeout=30,
        )
        assert cleanup.status_code in (200, 404), (
            f"Tenant B API-key cleanup failed: {cleanup.status_code} {cleanup.text}"
        )
    finally:
        _delete_governance_and_wait(
            subscriptions=[(subscription_name, namespace)],
            auth_policies=[(auth_policy_name, namespace)],
            timeout=120,
        )
        _delete_crs_and_wait(
            [
                ("maasmodelref", model_name, namespace),
                ("llminferenceservice", model_name, namespace),
            ],
            timeout=120,
        )
