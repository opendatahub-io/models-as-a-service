"""
E2E tests for the subscription listing endpoints:
  - GET /v1/subscriptions
  - GET /v1/model/:model-id/subscriptions

These endpoints return SubscriptionInfo objects with fields:
  subscription_id_header, subscription_description, display_name, priority,
  model_refs, organization_id, cost_center, labels

Requires same environment setup as test_subscription.py.
"""

import json
import logging
import os

import pytest
import requests

from test_subscription import (
    _create_api_key,
    _create_sa_token,
    _create_test_auth_policy,
    _create_test_subscription,
    _delete_cr,
    _delete_sa,
    _maas_api_url,
    _ns,
    _sa_to_user,
    _wait_reconcile,
    MODEL_NAMESPACE,
    MODEL_REF,
    DISTINCT_MODEL_REF,
    DISTINCT_MODEL_2_REF,
    SIMULATOR_SUBSCRIPTION,
    TIMEOUT,
    TLS_VERIFY,
)

log = logging.getLogger(__name__)


def _validate_subscription_info_schema(sub):
    """Validate a SubscriptionInfo object has the expected structure."""
    assert "subscription_id_header" in sub, f"Missing subscription_id_header: {sub}"
    assert isinstance(sub["subscription_id_header"], str), "subscription_id_header must be string"

    assert "subscription_description" in sub, f"Missing subscription_description: {sub}"
    assert isinstance(sub["subscription_description"], str), "subscription_description must be string"

    assert "priority" in sub, f"Missing priority: {sub}"
    assert isinstance(sub["priority"], int), "priority must be integer"

    assert "model_refs" in sub, f"Missing model_refs: {sub}"
    assert isinstance(sub["model_refs"], list), "model_refs must be a list"

    # Validate model_refs structure
    for ref in sub["model_refs"]:
        assert "name" in ref, f"model_ref missing name: {ref}"
        assert isinstance(ref["name"], str), "model_ref name must be string"

    # Optional fields
    if "display_name" in sub:
        assert isinstance(sub["display_name"], str), "display_name must be string"
    if "organization_id" in sub:
        assert isinstance(sub["organization_id"], str), "organization_id must be string"
    if "cost_center" in sub:
        assert isinstance(sub["cost_center"], str), "cost_center must be string"
    if "labels" in sub:
        assert isinstance(sub["labels"], dict), "labels must be a dict"


class TestListSubscriptions:
    """E2E tests for GET /v1/subscriptions."""

    def test_returns_accessible_subscriptions(self):
        """Authenticated user gets their accessible subscriptions."""
        sa_name = "e2e-list-subs-sa"
        sa_ns = "default"

        try:
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)
            api_key = _create_api_key(sa_token, name=f"{sa_name}-key")

            _wait_reconcile()

            url = f"{_maas_api_url()}/v1/subscriptions"
            r = requests.get(
                url,
                headers={"Authorization": f"Bearer {api_key}"},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r.status_code == 200, f"Expected 200, got {r.status_code}: {r.text}"

            data = r.json()
            assert isinstance(data, list), f"Expected array response, got {type(data).__name__}"

            # User should have at least one subscription (simulator-subscription via system:authenticated)
            assert len(data) >= 1, f"Expected at least 1 subscription, got {len(data)}"

            # Validate schema of each subscription
            for sub in data:
                _validate_subscription_info_schema(sub)

            # Verify simulator-subscription is present
            sub_ids = [s["subscription_id_header"] for s in data]
            assert SIMULATOR_SUBSCRIPTION in sub_ids, \
                f"Expected '{SIMULATOR_SUBSCRIPTION}' in accessible subscriptions, got {sub_ids}"

            log.info(f"GET /v1/subscriptions -> {r.status_code} with {len(data)} subscription(s): {sub_ids}")

        finally:
            _delete_sa(sa_name, namespace=sa_ns)

    def test_unauthenticated_returns_401(self):
        """Request without auth returns 401."""
        url = f"{_maas_api_url()}/v1/subscriptions"
        r = requests.get(url, timeout=TIMEOUT, verify=TLS_VERIFY)
        assert r.status_code == 401, f"Expected 401, got {r.status_code}: {r.text}"
        log.info(f"GET /v1/subscriptions (no auth) -> {r.status_code}")

    def test_subscription_includes_model_refs(self):
        """Subscriptions include model_refs with name and rate limit info."""
        sa_name = "e2e-list-subs-refs-sa"
        sa_ns = "default"
        maas_ns = _ns()
        subscription_name = "e2e-list-subs-refs-sub"

        try:
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)
            sa_user = _sa_to_user(sa_name, namespace=sa_ns)

            _create_test_subscription(
                subscription_name, MODEL_REF,
                users=[sa_user],
                token_limit=500, window="1h",
            )

            api_key = _create_api_key(sa_token, name=f"{sa_name}-key")
            _wait_reconcile()

            url = f"{_maas_api_url()}/v1/subscriptions"
            r = requests.get(
                url,
                headers={"Authorization": f"Bearer {api_key}"},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r.status_code == 200
            data = r.json()

            # Find our test subscription
            test_sub = next(
                (s for s in data if s["subscription_id_header"] == subscription_name),
                None,
            )
            assert test_sub is not None, \
                f"Test subscription '{subscription_name}' not found in {[s['subscription_id_header'] for s in data]}"

            # Validate model_refs
            assert len(test_sub["model_refs"]) >= 1, "Expected at least 1 model_ref"
            ref = test_sub["model_refs"][0]
            assert ref["name"] == MODEL_REF, f"Expected model_ref name '{MODEL_REF}', got '{ref['name']}'"

            # Validate token_rate_limits if present
            if "token_rate_limits" in ref and ref["token_rate_limits"]:
                trl = ref["token_rate_limits"][0]
                assert "limit" in trl, "token_rate_limit missing 'limit'"
                assert "window" in trl, "token_rate_limit missing 'window'"

            log.info(f"Subscription '{subscription_name}' has model_refs: {test_sub['model_refs']}")

        finally:
            _delete_cr("maassubscription", subscription_name, namespace=maas_ns)
            _delete_sa(sa_name, namespace=sa_ns)


class TestListSubscriptionsForModel:
    """E2E tests for GET /v1/model/:model-id/subscriptions."""

    def test_returns_subscriptions_for_model(self):
        """Returns only subscriptions that include the requested model."""
        sa_name = "e2e-subs-model-sa"
        sa_ns = "default"
        maas_ns = _ns()
        sub_with_model = "e2e-subs-model-match"
        sub_without_model = "e2e-subs-model-nomatch"

        try:
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)
            sa_user = _sa_to_user(sa_name, namespace=sa_ns)

            # Create two subscriptions: one with DISTINCT_MODEL_REF, one with DISTINCT_MODEL_2_REF
            _create_test_subscription(sub_with_model, DISTINCT_MODEL_REF, users=[sa_user])
            _create_test_subscription(sub_without_model, DISTINCT_MODEL_2_REF, users=[sa_user])

            api_key = _create_api_key(sa_token, name=f"{sa_name}-key")
            _wait_reconcile()

            # Query for subscriptions that include DISTINCT_MODEL_REF
            url = f"{_maas_api_url()}/v1/model/{DISTINCT_MODEL_REF}/subscriptions"
            r = requests.get(
                url,
                headers={"Authorization": f"Bearer {api_key}"},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r.status_code == 200, f"Expected 200, got {r.status_code}: {r.text}"

            data = r.json()
            assert isinstance(data, list), f"Expected array response, got {type(data).__name__}"

            sub_ids = [s["subscription_id_header"] for s in data]

            # The matching subscription should be present
            assert sub_with_model in sub_ids, \
                f"Expected '{sub_with_model}' in results, got {sub_ids}"

            # The non-matching subscription should NOT be present
            assert sub_without_model not in sub_ids, \
                f"'{sub_without_model}' should not be in results for model {DISTINCT_MODEL_REF}, got {sub_ids}"

            # Validate schema
            for sub in data:
                _validate_subscription_info_schema(sub)

            log.info(f"GET /v1/model/{DISTINCT_MODEL_REF}/subscriptions -> {len(data)} subscription(s): {sub_ids}")

        finally:
            _delete_cr("maassubscription", sub_with_model, namespace=maas_ns)
            _delete_cr("maassubscription", sub_without_model, namespace=maas_ns)
            _delete_sa(sa_name, namespace=sa_ns)

    def test_unknown_model_returns_empty(self):
        """Querying subscriptions for a model not in any subscription returns empty list."""
        sa_name = "e2e-subs-unknown-model-sa"
        sa_ns = "default"

        try:
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)
            api_key = _create_api_key(sa_token, name=f"{sa_name}-key")

            _wait_reconcile()

            url = f"{_maas_api_url()}/v1/model/nonexistent-model-xyz/subscriptions"
            r = requests.get(
                url,
                headers={"Authorization": f"Bearer {api_key}"},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r.status_code == 200, f"Expected 200, got {r.status_code}: {r.text}"

            data = r.json()
            assert isinstance(data, list), f"Expected array response, got {type(data).__name__}"
            assert len(data) == 0, f"Expected empty list for unknown model, got {len(data)}: {data}"

            log.info(f"GET /v1/model/nonexistent-model-xyz/subscriptions -> {r.status_code} with [] (empty)")

        finally:
            _delete_sa(sa_name, namespace=sa_ns)

    def test_unauthenticated_returns_401(self):
        """Request without auth returns 401."""
        url = f"{_maas_api_url()}/v1/model/{MODEL_REF}/subscriptions"
        r = requests.get(url, timeout=TIMEOUT, verify=TLS_VERIFY)
        assert r.status_code == 401, f"Expected 401, got {r.status_code}: {r.text}"
        log.info(f"GET /v1/model/{MODEL_REF}/subscriptions (no auth) -> {r.status_code}")
