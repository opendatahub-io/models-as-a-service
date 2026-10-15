"""System-level tests for Grid governance endpoint boundaries."""

from __future__ import annotations

import os

import pytest
import requests

from grid_governance_fixtures import (
    GridGovernanceContext,
    governance_context,  # noqa: F401 - pytest fixture
)
from grid_oidc_helpers import request_grid_token
from test_helper import MODEL_NAMESPACE, _request_with_gateway_retry


pytestmark = [
    pytest.mark.nightly,
    pytest.mark.skipif(
        os.environ.get("EXTERNAL_OIDC", "").lower() != "true",
        reason="EXTERNAL_OIDC is not true",
    ),
    pytest.mark.xdist_group("external"),
]


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
