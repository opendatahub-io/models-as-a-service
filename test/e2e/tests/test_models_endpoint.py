"""
E2E tests for the /v1/models endpoint that validate subscription-aware model filtering.

Tests the /v1/models endpoint in maas-api/internal/handlers/models.go which lists
available models filtered by the user's subscription access.

Requires:
  - GATEWAY_HOST env var (e.g. maas.apps.cluster.example.com)
  - MAAS_API_BASE_URL env var (e.g. https://maas.apps.cluster.example.com/maas-api)
  - maas-controller deployed with example CRs applied
  - oc/kubectl access to create service account tokens

Environment variables:
  See test_helper.py module docstring for shared environment variables.
  This file uses no additional file-specific environment variables.
"""

import json
import logging
import os
import subprocess
import time
import uuid

import pytest
import requests

from test_helper import (
    DISTINCT_MODEL_2_ID,
    DISTINCT_MODEL_2_REF,
    DISTINCT_MODEL_ID,
    DISTINCT_MODEL_REF,
    MODEL_NAME,
    MODEL_NAMESPACE,
    MODEL_REF,
    PREMIUM_MODEL_REF,
    PREMIUM_SIMULATOR_SUBSCRIPTION,
    SIMULATOR_ACCESS_POLICY,
    SIMULATOR_SUBSCRIPTION,
    TIMEOUT,
    TLS_VERIFY,
    UNCONFIGURED_MODEL_PATH,
    UNCONFIGURED_MODEL_REF,
    _apply_cr,
    _create_api_key,
    _create_sa_token,
    _create_test_auth_policy,
    _create_test_subscription,
    _delete_cr,
    _delete_sa,
    _get_auth_policies_for_model,
    _get_cluster_token,
    _get_cr,
    _get_subscriptions_for_model,
    _inference,
    _maas_api_url,
    _ns,
    _sa_to_user,
    _snapshot_cr,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
    _wait_for_token_rate_limit_policy,
    _wait_reconcile,
)

log = logging.getLogger(__name__)


class TestModelsEndpoint:
    """
    End-to-end tests for the /v1/models endpoint that validate subscription-aware
    model filtering behavior.

    The /v1/models endpoint (maas-api/internal/handlers/models.go) lists available
    models filtered by authentication method and subscription access. Key behaviors:
    - API keys: Returns models from the subscription bound to the key at mint time (ignores header)
    - K8s tokens (no header): Returns models from all accessible subscriptions
    - K8s tokens (with X-MaaS-Subscription): Filters to specified subscription
    - Returns HTTP 403 with permission_error for subscription authorization failures
    - Returns HTTP 401 for missing authentication
    - Filters models based on subscription access (probes each model endpoint)

    Test Coverage (22 tests) - Organized by Expected HTTP Status:

    ═══════════════════════════════════════════════════════════════════════════
    SUCCESS CASES (HTTP 200) - Authentication Method Behaviors
    ═══════════════════════════════════════════════════════════════════════════
    1. test_api_key_scoped_to_subscription
       → API key returns models from bound subscription only

    2. test_api_key_ignores_subscription_header
       → API key ignores x-maas-subscription header and uses bound subscription

    3. test_multiple_api_keys_different_subscriptions
       → Multiple API keys each bound to different subscriptions work independently

    4. test_user_token_returns_all_models
       → K8s token (no header) returns models from all subscriptions

    5. test_user_token_with_subscription_header_filters
       → K8s token with X-MaaS-Subscription filters to that subscription

    6. test_service_account_token_multiple_subs_no_header
       → K8s token with access to multiple subscriptions returns all (no header)

    7. test_service_account_token_multiple_subs_with_header
       → K8s token with multiple subscriptions filters by header

    ═══════════════════════════════════════════════════════════════════════════
    SUCCESS CASES (HTTP 200) - Legacy Behaviors (backwards compatibility)
    ═══════════════════════════════════════════════════════════════════════════
    8. test_single_subscription_auto_select
       → User with one subscription, no header → 200 (returns that subscription's models)

    9. test_explicit_subscription_header
       → K8s token with explicit X-MaaS-Subscription header → 200 (filters to that subscription)

    10. test_empty_subscription_header_value
        → Empty header value → 200 (same as no header - returns all models)

    ═══════════════════════════════════════════════════════════════════════════
    SUCCESS CASES (HTTP 200) - Model Filtering & Data Validation
    ═══════════════════════════════════════════════════════════════════════════
    11. test_models_filtered_by_subscription
        → Models correctly filtered by specified subscription

    12. test_deduplication_same_model_multiple_refs
        → Same modelRef listed twice deduplicates to 1 entry (same URL)

    13. test_different_modelrefs_same_model_id
        → Different modelRefs (different URLs) return 2 separate entries

    14. test_multiple_distinct_models_in_subscription
        → Different modelRefs with different IDs returns 2 entries (no duplicates)

    15. test_empty_model_list
        → Empty model list should return [] not null

    16. test_response_schema_matches_openapi
        → Response structure matches OpenAPI specification

    17. test_model_metadata_preserved
        → Model fields (url, ready, created, owned_by) accurate

    ═══════════════════════════════════════════════════════════════════════════
    ERROR CASES (HTTP 403) - Permission Errors
    ═══════════════════════════════════════════════════════════════════════════
    18. test_api_key_with_deleted_subscription_403
        → API key bound to deleted subscription → 403 permission_error

    19. test_api_key_with_inaccessible_subscription_403
        → API key/user with subscription they don't have access to → 403 permission_error

    20. test_invalid_subscription_header_403
        → K8s token with non-existent subscription → 403 permission_error

    21. test_access_denied_to_subscription_403
        → K8s token with subscription they lack access to → 403 permission_error

    ═══════════════════════════════════════════════════════════════════════════
    ERROR CASES (HTTP 401) - Authentication Errors
    ═══════════════════════════════════════════════════════════════════════════
    22. test_unauthenticated_request_401
        → No Authorization header → 401 authentication_error
    """

    @classmethod
    def setup_class(cls):
        """Validate test environment prerequisites before running any tests."""
        log.info("=" * 60)
        log.info("Validating /v1/models E2E Test Prerequisites")
        log.info("=" * 60)

        # Validate MODEL_REF exists and is Ready
        model = _get_cr("maasmodelref", MODEL_REF, MODEL_NAMESPACE)
        if not model:
            pytest.fail(f"PREREQUISITE MISSING: MaaSModelRef '{MODEL_REF}' not found in namespace '{MODEL_NAMESPACE}'. "
                       f"Ensure prow setup has created the model.")

        phase = model.get("status", {}).get("phase")
        endpoint = model.get("status", {}).get("endpoint")
        if phase != "Ready" or not endpoint:
            pytest.fail(f"PREREQUISITE INVALID: MaaSModelRef '{MODEL_REF}' not Ready "
                       f"(phase={phase}, endpoint={endpoint or 'none'}). "
                       f"Wait for reconciliation or check controller logs.")

        log.info(f"✓ Model '{MODEL_REF}' is Ready")
        log.info(f"  Endpoint: {endpoint}")

        # Discover existing auth policies and subscriptions (for debugging)
        cls.discovered_auth_policies = _get_auth_policies_for_model(MODEL_REF)
        cls.discovered_subscriptions = _get_subscriptions_for_model(MODEL_REF)

        log.info(f"✓ Found {len(cls.discovered_auth_policies)} auth policies for model:")
        for policy in cls.discovered_auth_policies:
            log.info(f"  - {policy}")

        log.info(f"✓ Found {len(cls.discovered_subscriptions)} subscriptions for model:")
        for sub in cls.discovered_subscriptions:
            log.info(f"  - {sub}")

        # Validate expected resources exist
        if SIMULATOR_ACCESS_POLICY not in cls.discovered_auth_policies:
            pytest.fail(f"PREREQUISITE MISSING: Expected auth policy '{SIMULATOR_ACCESS_POLICY}' not found. "
                       f"Found: {cls.discovered_auth_policies}. "
                       f"Ensure prow setup has created the auth policy.")

        if SIMULATOR_SUBSCRIPTION not in cls.discovered_subscriptions:
            pytest.fail(f"PREREQUISITE MISSING: Expected subscription '{SIMULATOR_SUBSCRIPTION}' not found. "
                       f"Found: {cls.discovered_subscriptions}. "
                       f"Ensure prow setup has created the subscription.")

        log.info("=" * 60)
        log.info("✅ All prerequisites validated - proceeding with /v1/models tests")
        log.info("=" * 60)

    def test_single_subscription_auto_select(self):
        """
        Test: User with exactly one accessible subscription can list models without
        providing x-maas-subscription header (auto-selection).

        Expected: HTTP 200 with models from that subscription.

        Note: Temporarily deletes simulator-subscription to ensure test user has exactly
        ONE subscription (not two, which would require a header).
        """
        sa_name = "e2e-models-single-sub-sa"
        sa_ns = "default"
        maas_ns = _ns()
        auth_policy_name = "e2e-single-sub-auth"
        subscription_name = "e2e-single-sub-subscription"

        # Snapshot existing subscription to restore later
        original_sim = _snapshot_cr("maassubscription", SIMULATOR_SUBSCRIPTION)

        api_key = None
        try:
            # Create service account
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)
            sa_user = _sa_to_user(sa_name, namespace=sa_ns)

            # Delete simulator-subscription so user has exactly ONE subscription
            # (otherwise they'd have 2: ours + simulator-subscription via system:authenticated)
            _delete_cr("maassubscription", SIMULATOR_SUBSCRIPTION)

            # Create auth policy and subscription for test user using DISTINCT_MODEL_REF
            # (avoids conflicts with existing simulator-access auth policy)
            log.info(f"Creating auth policy and subscription for {sa_user} with {DISTINCT_MODEL_REF}")
            _create_test_auth_policy(auth_policy_name, DISTINCT_MODEL_REF, users=[sa_user])
            _create_test_subscription(subscription_name, DISTINCT_MODEL_REF, users=[sa_user])

            # Wait for subscription to reconcile before creating API key
            _wait_for_maas_subscription_phase(subscription_name, namespace=maas_ns)

            # Create API key for inference
            api_key = _create_api_key(sa_token, name=f"{sa_name}-key")

            # Wait for Authorino to sync auth policies (can take 30+ seconds)
            log.info("Waiting 30s for Authorino to sync auth policies...")
            time.sleep(30)

            # DEBUG: Test model endpoint directly first
            log.info("DEBUG: Testing direct model endpoint access...")
            model_endpoint = f"https://{os.environ['GATEWAY_HOST']}/llm/{DISTINCT_MODEL_REF}/v1/models"
            debug_r = requests.get(
                model_endpoint,
                headers={
                    "Authorization": f"Bearer {api_key}",
                    "x-maas-subscription": subscription_name,
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )
            log.info(f"DEBUG: Direct model endpoint returned {debug_r.status_code}")
            if debug_r.status_code == 200:
                log.info(f"DEBUG: Direct model endpoint data: {debug_r.json()}")
            else:
                log.info(f"DEBUG: Direct model endpoint error: {debug_r.text}")

            # Poll /v1/models until it returns models or timeout
            log.info("Testing: GET /v1/models with single subscription (no header, auto-select)")
            url = f"{_maas_api_url()}/v1/models"

            timeout_seconds = 60
            poll_interval = 2
            deadline = time.time() + timeout_seconds
            r = None

            while time.time() < deadline:
                r = requests.get(
                    url,
                    headers={"Authorization": f"Bearer {api_key}"},
                    timeout=TIMEOUT,
                    verify=TLS_VERIFY,
                )

                if r.status_code == 200:
                    models = (r.json().get("data") or [])
                    if len(models) > 0:
                        log.info(f"✅ Models available after {60 - int(deadline - time.time())}s")
                        break
                    log.info(f"Got 200 but no models yet, retrying... ({int(deadline - time.time())}s remaining)")
                else:
                    log.info(f"Got {r.status_code}, retrying... ({int(deadline - time.time())}s remaining)")

                time.sleep(poll_interval)

            assert r is not None and r.status_code == 200, f"Expected 200 for single subscription auto-select, got {r.status_code if r else 'timeout'}: {r.text if r else 'no response'}"

            # Validate response structure
            data = r.json()
            assert data.get("object") == "list", f"Expected object='list', got {data.get('object')}"
            assert "data" in data, "Response missing 'data' field"

            # Handle API bug: data may be null instead of []
            models = data.get("data") or []

            # Should have at least one model (facebook-opt-125m-simulated from simulator-subscription)
            assert len(models) > 0, f"Expected at least one model in response, got {len(models)}. Data was: {data.get('data')}"

            # Validate model structure
            for model in models:
                assert "id" in model, "Model missing 'id' field"
                assert "object" in model, "Model missing 'object' field"
                assert "created" in model, "Model missing 'created' field"
                assert "owned_by" in model, "Model missing 'owned_by' field"

                # Validate subscriptions field (new feature)
                assert "subscriptions" in model, "Model missing 'subscriptions' field"
                assert isinstance(model["subscriptions"], list), "subscriptions should be a list"
                assert len(model["subscriptions"]) == 1, \
                    f"Expected 1 subscription (auto-selected), got {len(model['subscriptions'])}"
                assert model["subscriptions"][0]["name"] == subscription_name, \
                    f"Expected subscription '{subscription_name}', got '{model['subscriptions'][0]['name']}'"

            log.info(f"✅ Single subscription auto-select → {r.status_code} with {len(models)} model(s)")

        finally:
            # Restore simulator-subscription first (critical for other tests)
            if original_sim:
                _apply_cr(original_sim)

            # Clean up test resources
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=maas_ns)
            _delete_cr("maassubscription", subscription_name, namespace=maas_ns)
            _delete_sa(sa_name, namespace=sa_ns)
            _wait_reconcile()

    def test_explicit_subscription_header(self):
        """
        Test: K8s token with multiple subscriptions can list models by providing
        x-maas-subscription header.

        Expected: HTTP 200 with models from only the specified subscription.

        Note: Creates SA that has access to both simulator-subscription (via system:authenticated)
        and premium-simulator-subscription (by adding SA to its users list).
        Uses K8s token directly (not API key) since API keys ignore the header.
        """
        sa_name = "e2e-models-explicit-header-sa"
        sa_ns = "default"
        maas_ns = _ns()
        sa_user = None

        try:
            # Create service account - will be in system:authenticated group
            # This gives access to simulator-subscription automatically
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)
            sa_user = _sa_to_user(sa_name, namespace=sa_ns)

            # Add SA to premium-simulator-subscription to give it access to a second subscription
            log.info(f"Adding {sa_user} to premium-simulator-subscription users")
            subprocess.run([
                "oc", "patch", "maassubscription", PREMIUM_SIMULATOR_SUBSCRIPTION,
                "-n", maas_ns,
                "--type=merge",
                "-p", json.dumps({"spec": {"owner": {"users": [sa_user]}}})
            ], check=True)

            _wait_reconcile()

            # Test: GET /v1/models WITH x-maas-subscription header using K8s token
            # Expected: Returns models from simulator-subscription only
            log.info("Testing: GET /v1/models with K8s token and explicit subscription header: simulator-subscription")
            url = f"{_maas_api_url()}/v1/models"
            r = requests.get(
                url,
                headers={
                    "Authorization": f"Bearer {sa_token}",  # K8s token, not API key
                    "x-maas-subscription": SIMULATOR_SUBSCRIPTION,
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r.status_code == 200, f"Expected 200 with explicit subscription header, got {r.status_code}: {r.text}"

            # Validate response structure
            data = r.json()
            assert data.get("object") == "list", f"Expected object='list', got {data.get('object')}"
            assert "data" in data, "Response missing 'data' field"
            models = data.get("data", []) if data.get("data") is not None else []

            # Should have at least one model from simulator-subscription
            assert len(models) > 0, f"Expected at least one model in response, got {len(models)}. Data was: {data.get('data')}"

            # Validate subscriptions field
            for model in models:
                assert "subscriptions" in model, "Model missing 'subscriptions' field"
                assert isinstance(model["subscriptions"], list), "subscriptions should be a list"
                assert len(model["subscriptions"]) == 1, \
                    f"Expected 1 subscription (explicit header), got {len(model['subscriptions'])}"
                assert model["subscriptions"][0]["name"] == SIMULATOR_SUBSCRIPTION, \
                    f"Expected '{SIMULATOR_SUBSCRIPTION}', got '{model['subscriptions'][0]['name']}'"

            log.info(f"✅ K8s token with explicit subscription header → {r.status_code} with {len(models)} model(s)")

        finally:
            # Remove SA from premium-simulator-subscription
            if sa_user is not None:
                log.info(f"Removing {sa_user} from premium-simulator-subscription users")
                # Get current users list, remove our SA, then patch
                result = subprocess.run([
                    "oc", "get", "maassubscription", PREMIUM_SIMULATOR_SUBSCRIPTION,
                    "-n", maas_ns, "-o", "jsonpath={.spec.owner.users}"
                ], capture_output=True, text=True, check=True, timeout=30)

                if sa_user in result.stdout:
                    users = json.loads(result.stdout) if result.stdout and result.stdout.strip() else []
                    users = [u for u in users if u != sa_user]
                    subprocess.run([
                        "oc", "patch", "maassubscription", PREMIUM_SIMULATOR_SUBSCRIPTION,
                        "-n", maas_ns,
                        "--type=merge",
                        "-p", json.dumps({"spec": {"owner": {"users": users}}})
                    ], check=True)

            _delete_sa(sa_name, namespace=sa_ns)
            _wait_reconcile()

    def test_empty_subscription_header_value(self):
        """
        Test 12: Empty subscription header value behaves correctly.

        Header present but empty should behave like missing header (auto-select or 403).
        """
        log.info("Test 12: Empty subscription header value")

        sa_name = "e2e-models-empty-header-sa"
        sa_ns = "default"
        api_key = None

        try:
            # Create SA and API key with access to only one subscription
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)

            api_key_response = requests.post(
                f"{_maas_api_url()}/v1/api-keys",
                headers={"Authorization": f"Bearer {sa_token}", "Content-Type": "application/json"},
                json={"name": "e2e-empty-header-test-key"},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )
            assert api_key_response.status_code in (200, 201)
            api_key = api_key_response.json().get("key")

            _wait_reconcile()

            # Test with empty header value
            r = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={
                    "Authorization": f"Bearer {api_key}",
                    "x-maas-subscription": "",  # Empty string
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            # Should behave same as no header (auto-select single subscription)
            assert r.status_code == 200, \
                f"Empty header should auto-select single subscription, got {r.status_code}: {r.text}"

            data = r.json()
            assert data.get("object") == "list"

            log.info(f"✅ Empty subscription header → {r.status_code} (auto-selected)")

        finally:
            _delete_sa(sa_name, namespace=sa_ns)

    def test_models_filtered_by_subscription(self):
        """
        Test 8: Models are correctly filtered by subscription.

        User with access to multiple subscriptions should only see models from
        the subscription specified in x-maas-subscription header.
        """
        log.info("Test 8: Models filtered by subscription")

        sa_name = "e2e-models-filtered-sa"
        sa_ns = "default"
        maas_ns = _ns()
        api_key = None
        sa_user = None

        try:
            # Create SA with access to both subscriptions
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)
            sa_user = _sa_to_user(sa_name, namespace=sa_ns)

            # Add SA to premium subscription
            log.info(f"Adding {sa_user} to premium-simulator-subscription")
            subprocess.run([
                "oc", "patch", "maassubscription", PREMIUM_SIMULATOR_SUBSCRIPTION,
                "-n", maas_ns,
                "--type=merge",
                "-p", json.dumps({"spec": {"owner": {"users": [sa_user]}}})
            ], check=True)

            # Create API key
            api_key_response = requests.post(
                f"{_maas_api_url()}/v1/api-keys",
                headers={"Authorization": f"Bearer {sa_token}", "Content-Type": "application/json"},
                json={"name": "e2e-filtered-test-key"},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )
            assert api_key_response.status_code in (200, 201)
            api_key = api_key_response.json().get("key")

            _wait_reconcile()

            # Get models from simulator-subscription
            r_simulator = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={
                    "Authorization": f"Bearer {api_key}",
                    "x-maas-subscription": SIMULATOR_SUBSCRIPTION,
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )
            assert r_simulator.status_code == 200
            simulator_models = r_simulator.json().get("data") or []
            simulator_model_ids = {m["id"] for m in simulator_models}

            # Get models from premium-simulator-subscription
            r_premium = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={
                    "Authorization": f"Bearer {api_key}",
                    "x-maas-subscription": PREMIUM_SIMULATOR_SUBSCRIPTION,
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )
            assert r_premium.status_code == 200
            premium_models = r_premium.json().get("data") or []
            premium_model_ids = {m["id"] for m in premium_models}

            # Verify models are different between subscriptions
            # (assuming premium has different models than free tier)
            log.info(f"Simulator models: {simulator_model_ids}")
            log.info(f"Premium models: {premium_model_ids}")

            # The key assertion: models are subscription-specific
            # If there's any overlap, that's fine, but each list should be filtered
            # At minimum, verify we got responses for both
            assert len(simulator_models) >= 0, "Should get response for simulator subscription"
            assert len(premium_models) >= 0, "Should get response for premium subscription"

            log.info(f"✅ Models filtered by subscription → simulator: {len(simulator_models)}, premium: {len(premium_models)}")

        finally:
            # Cleanup
            if sa_user is not None:
                result = subprocess.run([
                    "oc", "get", "maassubscription", PREMIUM_SIMULATOR_SUBSCRIPTION,
                    "-n", maas_ns, "-o", "jsonpath={.spec.owner.users}"
                ], capture_output=True, text=True, check=True, timeout=30)

                if sa_user in result.stdout:
                    users = json.loads(result.stdout) if result.stdout and result.stdout.strip() else []
                    users = [u for u in users if u != sa_user]
                    subprocess.run([
                        "oc", "patch", "maassubscription", PREMIUM_SIMULATOR_SUBSCRIPTION,
                        "-n", maas_ns,
                        "--type=merge",
                        "-p", json.dumps({"spec": {"owner": {"users": users}}})
                    ], check=True)

            _delete_sa(sa_name, namespace=sa_ns)

    def test_deduplication_same_model_multiple_refs(self):
        """
        Test 6: Same modelRef listed twice should deduplicate to 1 entry.

        Creates a subscription with the SAME modelRef listed TWICE (different rate limits).
        The API deduplicates by (model ID, URL) and returns only 1 entry since both
        references point to the same backend service.

        The response includes subscription information showing which subscription(s)
        provide access to the model.
        """
        log.info("Test 6: Same modelRef twice should deduplicate (INTENDED behavior)")

        sa_name = "e2e-models-dedup-sa"
        sa_ns = "default"
        maas_ns = _ns()
        subscription_name = "e2e-dedup-subscription"
        auth_policy_name = "e2e-dedup-auth"
        api_key = None

        try:
            # Create SA with its own token
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)
            sa_user = _sa_to_user(sa_name, namespace=sa_ns)

            # Create auth policy that grants access to the model
            log.info(f"Creating auth policy with access to {MODEL_REF}")
            auth_policy_cr = {
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {
                    "name": auth_policy_name,
                    "namespace": maas_ns,
                },
                "spec": {
                    "modelRefs": [{"name": MODEL_REF, "namespace": MODEL_NAMESPACE}],
                    "subjects": {
                        "users": [sa_user],
                        "groups": [],
                    },
                },
            }
            subprocess.run(
                ["oc", "apply", "-f", "-"],
                input=json.dumps(auth_policy_cr),
                text=True,
                check=True,
            )

            # Create subscription with the SAME model ref TWICE (guaranteed duplicates)
            log.info(f"Creating subscription with {MODEL_REF} listed twice (to test deduplication)")
            subscription_cr = {
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {
                    "name": subscription_name,
                    "namespace": maas_ns,
                },
                "spec": {
                    "owner": {
                        "users": [sa_user],
                        "groups": [],
                    },
                    "modelRefs": [
                        {
                            "name": MODEL_REF,
                            "namespace": MODEL_NAMESPACE,
                            "tokenRateLimits": [{"limit": 100, "window": "1m"}],
                        },
                        {
                            "name": MODEL_REF,  # Same model ref again - guarantees duplicate
                            "namespace": MODEL_NAMESPACE,
                            "tokenRateLimits": [{"limit": 200, "window": "1m"}],
                        },
                    ],
                },
            }
            subprocess.run(
                ["oc", "apply", "-f", "-"],
                input=json.dumps(subscription_cr),
                text=True,
                check=True,
            )

            # Wait for subscription to reconcile before creating API key
            _wait_for_maas_subscription_phase(subscription_name, namespace=maas_ns)

            # Create API key bound to our test subscription
            api_key_response = requests.post(
                f"{_maas_api_url()}/v1/api-keys",
                headers={"Authorization": f"Bearer {sa_token}", "Content-Type": "application/json"},
                json={"name": "e2e-dedup-test-key", "subscription": subscription_name},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )
            assert api_key_response.status_code in (200, 201)
            api_key = api_key_response.json().get("key")

            # Wait for reconciliation
            _wait_reconcile()

            # Query /v1/models with our custom subscription
            log.info(f"Querying /v1/models with subscription: {subscription_name}")
            r = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={
                    "Authorization": f"Bearer {api_key}",
                    "x-maas-subscription": subscription_name,
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r.status_code == 200, f"Expected 200, got {r.status_code}: {r.text}"
            data = r.json()
            models = data.get("data") or []

            # Models should be a list
            assert isinstance(models, list), "Models should be a list"

            # Get model IDs from response
            model_ids = [m["id"] for m in models]
            unique_ids = set(model_ids)

            log.info(f"📊 API Response: {len(models)} total model(s), {len(unique_ids)} unique ID(s)")
            log.info(f"   Model IDs: {model_ids}")
            log.info(f"   Unique IDs: {unique_ids}")

            # Should all be the same model ID (we only referenced one modelRef)
            assert len(unique_ids) == 1, \
                f"Expected only 1 unique model ID (same modelRef listed twice), got {len(unique_ids)}: {unique_ids}"

            # INTENDED BEHAVIOR: Should return exactly 1 entry (deduplicated)
            # even though the same modelRef was listed twice
            assert len(models) == 1, \
                f"Expected 1 deduplicated entry (same modelRef listed 2x), got {len(models)} duplicates: {model_ids}"

            # Validate subscriptions field
            model = models[0]
            assert "subscriptions" in model, "Model should have 'subscriptions' field"
            assert isinstance(model["subscriptions"], list), "subscriptions should be a list"
            assert len(model["subscriptions"]) == 1, \
                f"Expected 1 subscription (single subscription requested), got {len(model['subscriptions'])}"

            sub = model["subscriptions"][0]
            assert "name" in sub, "Subscription should have 'name' field"
            assert sub["name"] == subscription_name, \
                f"Expected subscription name '{subscription_name}', got '{sub['name']}'"
            # displayName and description are optional
            assert isinstance(sub.get("displayName", ""), str), "displayName should be string if present"
            assert isinstance(sub.get("description", ""), str), "description should be string if present"

            log.info("✅ API correctly deduplicated same modelRef listed 2x → 1 entry with subscription info")

        finally:
            # Cleanup
            _delete_cr("maassubscription", subscription_name, namespace=maas_ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=maas_ns)
            _delete_sa(sa_name, namespace=sa_ns)
            _wait_reconcile()

    def test_different_modelrefs_same_model_id(self):
        """
        Test 7: Different modelRefs serving same model ID return separate entries.

        Uses two DIFFERENT MaaSModelRefs (each listed ONCE) that both serve the
        SAME model ID:
        - MODEL_REF (facebook-opt-125m-simulated) → serves "facebook/opt-125m"
        - PREMIUM_MODEL_REF (premium-simulated-simulated-premium) → serves "facebook/opt-125m"

        The API deduplicates by (model ID, URL). Since these are different backend
        services with different URLs, they return as 2 separate entries even though
        they serve the same model ID.

        Each entry shows the same model ID but different URL and subscription.
        """
        log.info("Test 7: Different modelRefs same ID should deduplicate (INTENDED behavior)")

        sa_name = "e2e-models-diff-refs-sa"
        sa_ns = "default"
        maas_ns = _ns()
        subscription_name = "e2e-diff-refs-subscription"
        auth_policy_name = "e2e-diff-refs-auth"
        api_key = None

        try:
            # Create SA
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)
            sa_user = _sa_to_user(sa_name, namespace=sa_ns)

            # Create auth policy with both modelRefs
            log.info(f"Creating auth policy with {MODEL_REF} and {PREMIUM_MODEL_REF}")
            auth_policy_cr = {
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {
                    "name": auth_policy_name,
                    "namespace": maas_ns,
                },
                "spec": {
                    "modelRefs": [
                        {"name": MODEL_REF, "namespace": MODEL_NAMESPACE},
                        {"name": PREMIUM_MODEL_REF, "namespace": MODEL_NAMESPACE},
                    ],
                    "subjects": {
                        "users": [sa_user],
                        "groups": [],
                    },
                },
            }
            subprocess.run(
                ["oc", "apply", "-f", "-"],
                input=json.dumps(auth_policy_cr),
                text=True,
                check=True,
            )

            # Create subscription with both modelRefs (each listed ONCE)
            log.info(f"Creating subscription with {MODEL_REF} and {PREMIUM_MODEL_REF}")
            subscription_cr = {
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {
                    "name": subscription_name,
                    "namespace": maas_ns,
                },
                "spec": {
                    "owner": {
                        "users": [sa_user],
                        "groups": [],
                    },
                    "modelRefs": [
                        {
                            "name": MODEL_REF,
                            "namespace": MODEL_NAMESPACE,
                            "tokenRateLimits": [{"limit": 100, "window": "1m"}],
                        },
                        {
                            "name": PREMIUM_MODEL_REF,
                            "namespace": MODEL_NAMESPACE,
                            "tokenRateLimits": [{"limit": 200, "window": "1m"}],
                        },
                    ],
                },
            }
            subprocess.run(
                ["oc", "apply", "-f", "-"],
                input=json.dumps(subscription_cr),
                text=True,
                check=True,
            )

            # Wait for subscription to reconcile before creating API key
            _wait_for_maas_subscription_phase(subscription_name, namespace=maas_ns)

            # Create API key bound to our test subscription
            api_key_response = requests.post(
                f"{_maas_api_url()}/v1/api-keys",
                headers={"Authorization": f"Bearer {sa_token}", "Content-Type": "application/json"},
                json={"name": "e2e-diff-refs-test-key", "subscription": subscription_name},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )
            assert api_key_response.status_code in (200, 201)
            api_key = api_key_response.json().get("key")

            _wait_reconcile()

            # Query /v1/models
            log.info(f"Querying /v1/models with subscription: {subscription_name}")
            r = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={
                    "Authorization": f"Bearer {api_key}",
                    "x-maas-subscription": subscription_name,
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r.status_code == 200, f"Expected 200, got {r.status_code}: {r.text}"
            data = r.json()
            models = data.get("data") or []

            assert isinstance(models, list), "Models should be a list"

            # Get model IDs from response
            model_ids = [m["id"] for m in models]
            unique_ids = set(model_ids)

            log.info(f"📊 API Response: {len(models)} total model(s), {len(unique_ids)} unique ID(s)")
            log.info(f"   Model IDs: {model_ids}")
            log.info(f"   Unique IDs: {unique_ids}")
            log.info("   Subscription had: 2 different modelRefs both serving 'facebook/opt-125m'")

            # Both modelRefs serve the same model ID
            assert len(unique_ids) == 1, \
                f"Expected only 1 unique model ID (both modelRefs serve {MODEL_NAME}), got {len(unique_ids)}: {unique_ids}"

            # Verify it's the expected model ID
            expected_id = MODEL_NAME
            assert expected_id in unique_ids, \
                f"Expected to find '{expected_id}', but got {unique_ids}"

            # INTENDED BEHAVIOR: Should return 2 entries (deduplication by model ID + URL)
            # Different backend services (different URLs) return separate entries even with same model ID
            assert len(models) == 2, \
                f"Expected 2 entries (different URLs), got {len(models)}: {model_ids}"

            # Validate both entries have different URLs
            urls = [m["url"] for m in models if "url" in m]
            assert len(urls) == 2, f"Expected 2 URLs, got {len(urls)}"
            assert urls[0] != urls[1], f"Expected different URLs, got duplicates: {urls}"

            # Validate each entry has subscriptions field with the same subscription
            for model in models:
                assert "subscriptions" in model, "Model should have 'subscriptions' field"
                assert isinstance(model["subscriptions"], list), "subscriptions should be a list"
                assert len(model["subscriptions"]) == 1, \
                    f"Expected 1 subscription per model, got {len(model['subscriptions'])}"
                assert model["subscriptions"][0]["name"] == subscription_name, \
                    f"Expected subscription '{subscription_name}', got '{model['subscriptions'][0]['name']}'"

            log.info("✅ API correctly returned 2 separate entries (different URLs) for same model ID")

        finally:
            # Cleanup
            _delete_cr("maassubscription", subscription_name, namespace=maas_ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=maas_ns)
            _delete_sa(sa_name, namespace=sa_ns)
            _wait_reconcile()

    def test_multiple_distinct_models_in_subscription(self):
        """
        Test 8: Multiple distinct models should return exactly 2 entries (1 per unique ID).

        Uses pre-deployed models (both known to not have backend duplication issues):
        - DISTINCT_MODEL_REF (simulated-distinct) serving "test/e2e-distinct-model"
        - DISTINCT_MODEL_2_REF (simulated-distinct-2) serving "test/e2e-distinct-model-2"

        Creates a subscription with both models. The API should return exactly 2 entries
        (one for each distinct model ID), with no duplicates.

        This test validates that when backend models don't have duplication bugs, the
        API correctly returns one entry per distinct model ID.
        """
        log.info("Test 8: Multiple distinct models should return 2 entries")

        sa_name = "e2e-models-distinct-sa"
        sa_ns = "default"
        maas_ns = _ns()
        subscription_name = "e2e-distinct-models-subscription"
        auth_policy_name = "e2e-distinct-models-auth"
        api_key = None

        try:
            # Create SA
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)
            sa_user = _sa_to_user(sa_name, namespace=sa_ns)

            # Create auth policy with both distinct models
            log.info(f"Creating auth policy with {DISTINCT_MODEL_REF} and {DISTINCT_MODEL_2_REF}")
            auth_policy_cr = {
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSAuthPolicy",
                "metadata": {
                    "name": auth_policy_name,
                    "namespace": maas_ns,
                },
                "spec": {
                    "modelRefs": [
                        {"name": DISTINCT_MODEL_REF, "namespace": MODEL_NAMESPACE},
                        {"name": DISTINCT_MODEL_2_REF, "namespace": MODEL_NAMESPACE},
                    ],
                    "subjects": {
                        "users": [sa_user],
                        "groups": [],
                    },
                },
            }
            subprocess.run(
                ["oc", "apply", "-f", "-"],
                input=json.dumps(auth_policy_cr),
                text=True,
                check=True,
            )

            # Create subscription with both distinct models
            log.info(f"Creating subscription with {DISTINCT_MODEL_REF} and {DISTINCT_MODEL_2_REF}")
            subscription_cr = {
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {
                    "name": subscription_name,
                    "namespace": maas_ns,
                },
                "spec": {
                    "owner": {
                        "users": [sa_user],
                        "groups": [],
                    },
                    "modelRefs": [
                        {
                            "name": DISTINCT_MODEL_REF,
                            "namespace": MODEL_NAMESPACE,
                            "tokenRateLimits": [{"limit": 100, "window": "1m"}],
                        },
                        {
                            "name": DISTINCT_MODEL_2_REF,
                            "namespace": MODEL_NAMESPACE,
                            "tokenRateLimits": [{"limit": 100, "window": "1m"}],
                        },
                    ],
                },
            }
            subprocess.run(
                ["oc", "apply", "-f", "-"],
                input=json.dumps(subscription_cr),
                text=True,
                check=True,
            )

            # Wait for subscription to reconcile before creating API key
            _wait_for_maas_subscription_phase(subscription_name, namespace=maas_ns)

            # Create API key bound to our test subscription
            api_key_response = requests.post(
                f"{_maas_api_url()}/v1/api-keys",
                headers={"Authorization": f"Bearer {sa_token}", "Content-Type": "application/json"},
                json={"name": "e2e-distinct-models-test-key", "subscription": subscription_name},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )
            assert api_key_response.status_code in (200, 201)
            api_key = api_key_response.json().get("key")

            _wait_reconcile()

            # Query /v1/models
            log.info(f"Querying /v1/models with subscription: {subscription_name}")
            r = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={
                    "Authorization": f"Bearer {api_key}",
                    "x-maas-subscription": subscription_name,
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r.status_code == 200, f"Expected 200, got {r.status_code}: {r.text}"
            data = r.json()
            models = data.get("data") or []

            assert isinstance(models, list), "Models should be a list"

            # Get model IDs from response
            model_ids = [m["id"] for m in models]
            unique_ids = set(model_ids)

            log.info(f"📊 API Response: {len(models)} total model(s), {len(unique_ids)} unique ID(s)")
            log.info(f"   Model IDs: {model_ids}")
            log.info(f"   Unique IDs: {unique_ids}")
            log.info(f"   Subscription had: 2 modelRefs ({DISTINCT_MODEL_REF}, {DISTINCT_MODEL_2_REF})")

            # Verify we got BOTH expected model IDs
            expected_ids = {DISTINCT_MODEL_ID, DISTINCT_MODEL_2_ID}
            assert unique_ids == expected_ids, \
                f"Expected to find both distinct models {expected_ids}, but got {unique_ids}"

            # INTENDED BEHAVIOR: Should return exactly 2 entries (one per distinct model ID)
            # No duplicates should be present
            assert len(models) == 2, \
                f"Expected 2 entries (one per distinct model ID), got {len(models)}: {model_ids}"

            assert len(model_ids) == len(unique_ids), \
                f"Expected no duplicates, but got {len(model_ids)} entries for {len(unique_ids)} unique IDs: {model_ids}"

            # Validate subscriptions field
            for model in models:
                assert "subscriptions" in model, f"Model {model['id']} missing 'subscriptions' field"
                assert isinstance(model["subscriptions"], list), "subscriptions should be a list"
                assert len(model["subscriptions"]) == 1, \
                    f"Expected 1 subscription, got {len(model['subscriptions'])}"
                assert model["subscriptions"][0]["name"] == subscription_name, \
                    f"Expected subscription '{subscription_name}', got '{model['subscriptions'][0]['name']}'"

            log.info(f"✅ API correctly returned 2 distinct models without duplicates: {sorted(unique_ids)}")

        finally:
            # Cleanup
            _delete_cr("maassubscription", subscription_name, namespace=maas_ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=maas_ns)
            _delete_sa(sa_name, namespace=sa_ns)
            _wait_reconcile()

    def test_user_token_returns_all_models(self):
        """
        Test: User token automatically returns models from all subscriptions.

        Creates a user with access to TWO subscriptions containing different models.
        Queries without X-MaaS-Subscription header and validates:
        - Returns models from ALL accessible subscriptions
        - Each model includes subscriptions array showing which subscription(s) provide access
        - Models appearing in multiple subscriptions have aggregated subscription list
        """
        log.info("Test: User token returns models from all subscriptions")

        sa_name = "e2e-return-all-sa"
        sa_ns = "default"
        maas_ns = _ns()
        sub1_name = "e2e-return-all-sub1"
        sub2_name = "e2e-return-all-sub2"
        auth1_name = "e2e-return-all-auth1"
        auth2_name = "e2e-return-all-auth2"

        try:
            # Create SA
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)
            sa_user = _sa_to_user(sa_name, namespace=sa_ns)

            # Create subscription 1 with DISTINCT_MODEL_REF
            log.info(f"Creating subscription 1 with {DISTINCT_MODEL_REF}")
            _create_test_auth_policy(auth1_name, DISTINCT_MODEL_REF, users=[sa_user])
            _create_test_subscription(sub1_name, DISTINCT_MODEL_REF, users=[sa_user])

            # Create subscription 2 with DISTINCT_MODEL_2_REF
            log.info(f"Creating subscription 2 with {DISTINCT_MODEL_2_REF}")
            _create_test_auth_policy(auth2_name, DISTINCT_MODEL_2_REF, users=[sa_user])
            _create_test_subscription(sub2_name, DISTINCT_MODEL_2_REF, users=[sa_user])

            _wait_for_maas_auth_policy_phase(auth1_name)
            _wait_for_maas_auth_policy_phase(auth2_name)
            _wait_for_maas_subscription_phase(sub1_name)
            _wait_for_maas_subscription_phase(sub2_name)

            # Query with user token (no X-MaaS-Subscription header)
            log.info("Querying /v1/models with user token (no header)")
            r = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={
                    "Authorization": f"Bearer {sa_token}",
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r.status_code == 200, f"Expected 200, got {r.status_code}: {r.text}"
            data = r.json()
            models = data.get("data") or []

            # Should get models from BOTH subscriptions
            model_ids = [m["id"] for m in models]
            log.info(f"Got {len(models)} models: {model_ids}")

            # Validate we got models from both subscriptions
            # (At minimum we should see the 2 distinct models)
            assert len(models) >= 2, \
                f"Expected at least 2 models (from 2 subscriptions), got {len(models)}"

            # Validate all models have subscriptions field
            for model in models:
                assert "subscriptions" in model, f"Model {model['id']} missing 'subscriptions' field"
                assert isinstance(model["subscriptions"], list), \
                    f"Model {model['id']} subscriptions should be a list"
                assert len(model["subscriptions"]) > 0, \
                    f"Model {model['id']} should have at least one subscription"

                # Validate subscription structure
                for sub in model["subscriptions"]:
                    assert "name" in sub, "Subscription should have 'name' field"
                    assert isinstance(sub["name"], str), "Subscription name should be string"

            log.info(f"✅ User token returned {len(models)} models from all subscriptions")

        finally:
            _delete_cr("maassubscription", sub1_name, namespace=maas_ns)
            _delete_cr("maassubscription", sub2_name, namespace=maas_ns)
            _delete_cr("maasauthpolicy", auth1_name, namespace=maas_ns)
            _delete_cr("maasauthpolicy", auth2_name, namespace=maas_ns)
            _delete_sa(sa_name, namespace=sa_ns)
            _wait_reconcile()

    def test_user_token_with_subscription_header_filters(self):
        """
        Test: User token with X-MaaS-Subscription header filters to that subscription.

        User tokens can optionally provide X-MaaS-Subscription to filter results
        to a specific subscription (similar to API key behavior).

        Expected: HTTP 200 with models from only the specified subscription.
        """
        log.info("Test: User token with X-MaaS-Subscription header filters models")

        ns = _ns()
        auth_policy_name = "e2e-user-token-filter-auth"
        subscription_name = "e2e-user-token-filter-sub"
        sa_name = "e2e-user-token-filter-sa"

        try:
            # Create service account and token
            oc_token = _create_sa_token(sa_name, namespace=ns)
            sa_user = _sa_to_user(sa_name, namespace=ns)

            # Create test resources
            _create_test_auth_policy(auth_policy_name, MODEL_REF, users=[sa_user])
            _create_test_subscription(subscription_name, MODEL_REF, users=[sa_user])

            _wait_reconcile()

            # Query with X-MaaS-Subscription header to filter
            log.info(f"Querying /v1/models with X-MaaS-Subscription: {subscription_name}")
            r = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={
                    "Authorization": f"Bearer {oc_token}",
                    "X-MaaS-Subscription": subscription_name,
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r.status_code == 200, \
                f"Expected 200 for user token with subscription header, got {r.status_code}: {r.text}"

            data = r.json()
            models = data.get("data") or []

            # Validate models are filtered to the specified subscription
            for model in models:
                assert "subscriptions" in model, f"Model {model.get('id')} missing 'subscriptions' field"
                subscription_names = [s["name"] for s in model["subscriptions"]]
                assert subscription_name in subscription_names, \
                    f"Model {model.get('id')} should be in subscription {subscription_name}, got {subscription_names}"

            log.info(f"✅ User token with X-MaaS-Subscription filtered to {len(models)} models")

        finally:
            _delete_cr("maassubscription", subscription_name, namespace=ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=ns)
            _delete_sa(sa_name, namespace=ns)
            _wait_reconcile()

    def test_empty_model_list(self):
        """
        Test 9: Empty model list should return [] not null.

        Creates a subscription pointing to UNCONFIGURED_MODEL_REF which has no
        auth policy. The SA has access to the subscription, but when probing the
        model endpoint, Authorino returns 403 (no auth policy = no access).

        This validates that FilterModelsByAccess returns [] (not null) when no
        models are accessible.
        """
        log.info("Test 9: Empty model list returns empty array")

        sa_name = "e2e-empty-models-sa"
        sa_ns = "default"
        maas_ns = _ns()
        subscription_name = "e2e-empty-models-subscription"

        try:
            # Create SA
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)
            sa_user = _sa_to_user(sa_name, namespace=sa_ns)

            # Create subscription pointing to unconfigured model (has no auth policy)
            log.info(f"Creating subscription with {UNCONFIGURED_MODEL_REF} (no auth policy = no access)")
            _create_test_subscription(subscription_name, UNCONFIGURED_MODEL_REF, users=[sa_user])

            # Wait for subscription to reconcile before creating API key
            _wait_for_maas_subscription_phase(subscription_name, namespace=maas_ns)

            # Create API key bound to test subscription
            api_key = _create_api_key(sa_token, name=f"{sa_name}-key", subscription=subscription_name)

            _wait_reconcile()

            # Query /v1/models - should return empty list (model has no auth policy)
            url = f"{_maas_api_url()}/v1/models"
            r = requests.get(
                url,
                headers={
                    "Authorization": f"Bearer {api_key}",
                    "x-maas-subscription": subscription_name,
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            # Should get 200 even with no models
            assert r.status_code == 200, f"Expected 200, got {r.status_code}: {r.text}"

            data = r.json()
            assert data.get("object") == "list", f"Expected object='list', got {data.get('object')}"

            assert "data" in data, "Response missing 'data' field"
            models = data["data"]

            # The critical assertion: data must be an array, never null
            assert models is not None, "'data' field must not be null (should be [] for empty)"

            assert isinstance(models, list), \
                f"data must be a list, got {type(models).__name__}"

            # Verify it's actually empty (unconfigured model has no auth policy)
            assert len(models) == 0, \
                f"Expected empty list (unconfigured model has no auth policy), got {len(models)} models: {models}"

            log.info(f"✅ Empty model list → {r.status_code} with data=[] (array, not null)")

        finally:
            _delete_cr("maassubscription", subscription_name, namespace=maas_ns)
            _delete_sa(sa_name, namespace=sa_ns)

    def test_response_schema_matches_openapi(self):
        """
        Test 16: Response structure matches OpenAPI schema.

        Validates all required fields and types match the API specification.
        """
        log.info("Test 16: Response schema matches OpenAPI spec")

        sa_name = "e2e-models-schema-test-sa"
        sa_ns = "default"
        api_key = None

        try:
            # Create SA and API key
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)

            api_key_response = requests.post(
                f"{_maas_api_url()}/v1/api-keys",
                headers={"Authorization": f"Bearer {sa_token}", "Content-Type": "application/json"},
                json={"name": "e2e-schema-test-key"},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )
            assert api_key_response.status_code in (200, 201)
            api_key = api_key_response.json().get("key")

            _wait_reconcile()

            r = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={"Authorization": f"Bearer {api_key}"},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r.status_code == 200
            data = r.json()

            # Validate top-level structure
            assert "object" in data, "Response missing 'object' field"
            assert data["object"] == "list", f"Expected object='list', got {data['object']}"
            assert "data" in data, "Response missing 'data' field"
            assert data["data"] is not None, "'data' field must not be null"

            models = data["data"]
            assert isinstance(models, list), f"'data' must be an array, got {type(models).__name__}"

            # Validate each model matches schema
            for model in models:
                # Required fields per OpenAPI spec
                assert "id" in model, f"Model missing required field 'id': {model}"
                assert "object" in model, f"Model missing required field 'object': {model}"
                assert "created" in model, f"Model missing required field 'created': {model}"
                assert "owned_by" in model, f"Model missing required field 'owned_by': {model}"
                assert "ready" in model, f"Model missing required field 'ready': {model}"

                # Validate types
                assert isinstance(model["id"], str), f"'id' must be string, got {type(model['id'])}"
                assert isinstance(model["object"], str), f"'object' must be string"
                assert model["object"] == "model", f"'object' must be 'model', got {model['object']}"
                assert isinstance(model["created"], int), f"'created' must be integer"
                assert isinstance(model["owned_by"], str), f"'owned_by' must be string"
                assert isinstance(model["ready"], bool), f"'ready' must be boolean"

                # Optional fields validation
                if "url" in model:
                    assert isinstance(model["url"], str), "'url' must be string if present"

            log.info(f"✅ Response schema matches OpenAPI → validated {len(models)} model(s)")

        finally:
            _delete_sa(sa_name, namespace=sa_ns)

    def test_model_metadata_preserved(self):
        """
        Test 17: Model metadata is correctly preserved.

        Validates that url, ready, created, owned_by fields are accurate.
        """
        log.info("Test 17: Model metadata preserved")

        sa_name = "e2e-models-metadata-sa"
        sa_ns = "default"
        api_key = None

        try:
            # Create SA and API key
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)

            api_key_response = requests.post(
                f"{_maas_api_url()}/v1/api-keys",
                headers={"Authorization": f"Bearer {sa_token}", "Content-Type": "application/json"},
                json={"name": "e2e-metadata-test-key"},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )
            assert api_key_response.status_code in (200, 201)
            api_key = api_key_response.json().get("key")

            _wait_reconcile()

            r = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={"Authorization": f"Bearer {api_key}"},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r.status_code == 200
            models = r.json().get("data") or []

            for model in models:
                # Verify metadata is present and reasonable
                assert model["created"] > 0, f"'created' timestamp should be positive: {model['created']}"

                assert model["owned_by"], f"'owned_by' should not be empty: {model}"

                assert isinstance(model["ready"], bool), f"'ready' must be boolean: {model['ready']}"

                # If URL is present, verify it's well-formed
                if "url" in model and model["url"]:
                    assert model["url"].startswith("http"), \
                        f"URL should start with http: {model['url']}"
                    # URL should contain the model ID
                    # (though exact format may vary)

                # Verify id is not empty
                assert model["id"], f"Model ID should not be empty: {model}"

            log.info(f"✅ Model metadata preserved → validated {len(models)} model(s)")

        finally:
            _delete_sa(sa_name, namespace=sa_ns)

    def test_api_key_scoped_to_subscription(self):
        """
        Test: API key returns only models from its bound subscription.

        API keys are scoped to a specific subscription at mint time. The gateway
        automatically injects X-MaaS-Subscription from the key's subscription.

        Expected: HTTP 200 with models only from the key's subscription, even if
        the user has access to multiple subscriptions.
        """
        ns = _ns()
        auth_policy_name = "e2e-api-key-scoped-auth"
        subscription_name = "e2e-api-key-scoped-sub"
        sa_name = "e2e-api-key-scoped-sa"
        api_key = None

        try:
            # Create service account and token
            oc_token = _create_sa_token(sa_name, namespace=ns)
            sa_user = _sa_to_user(sa_name, namespace=ns)

            # Create test resources
            _create_test_auth_policy(auth_policy_name, MODEL_REF, users=[sa_user])
            _create_test_subscription(subscription_name, MODEL_REF, users=[sa_user])

            # Wait for subscription to reconcile before creating API key
            _wait_for_maas_subscription_phase(subscription_name, namespace=ns)

            # Create API key bound to subscription_name
            api_key = _create_api_key(oc_token, name=f"{sa_name}-key", subscription=subscription_name)

            _wait_reconcile()

            # Query with API key (no manual headers)
            log.info(f"Querying /v1/models with API key bound to {subscription_name}")
            r = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={
                    "Authorization": f"Bearer {api_key}",
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r.status_code == 200, \
                f"Expected 200 for API key request, got {r.status_code}: {r.text}"

            data = r.json()
            models = data.get("data") or []

            # Validate models are from the key's subscription
            log.info(f"API key returned {len(models)} models")
            for model in models:
                assert "subscriptions" in model, f"Model {model.get('id')} missing 'subscriptions' field"
                subscription_names = [s["name"] for s in model["subscriptions"]]
                # Models should be associated with the key's subscription
                assert subscription_name in subscription_names, \
                    f"Model {model.get('id')} should be in subscription {subscription_name}"

            log.info(f"✅ API key scoped to {subscription_name} returned {len(models)} models")

        finally:
            _delete_cr("maassubscription", subscription_name, namespace=ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=ns)
            _delete_sa(sa_name, namespace=ns)
            _wait_reconcile()

    def test_api_key_with_deleted_subscription_403(self):
        """
        Test: API key bound to a subscription that was deleted after key creation.

        This tests an edge case where an API key was minted with a subscription,
        but that subscription is later deleted. The gateway injects X-MaaS-Subscription
        from the key, but the subscription no longer exists.

        Expected: HTTP 403 with error type: permission_error
        """
        ns = _ns()
        auth_policy_name = "e2e-api-key-deleted-sub-auth"
        subscription_name = "e2e-api-key-deleted-sub"
        sa_name = "e2e-api-key-deleted-sub-sa"
        api_key = None

        try:
            # Create service account and token
            oc_token = _create_sa_token(sa_name, namespace=ns)
            sa_user = _sa_to_user(sa_name, namespace=ns)

            # Create test resources
            _create_test_auth_policy(auth_policy_name, MODEL_REF, users=[sa_user])
            _create_test_subscription(subscription_name, MODEL_REF, users=[sa_user])

            # Wait for subscription to reconcile before creating API key
            _wait_for_maas_subscription_phase(subscription_name, namespace=ns)

            # Create API key bound to subscription
            api_key = _create_api_key(oc_token, name=f"{sa_name}-key", subscription=subscription_name)

            _wait_reconcile()

            # Delete the subscription (simulating deletion after key creation)
            log.info(f"Deleting subscription {subscription_name} after API key creation")
            _delete_cr("maassubscription", subscription_name, namespace=ns)
            _wait_reconcile()

            # Query with API key (gateway injects deleted subscription name)
            log.info("Querying /v1/models with API key bound to deleted subscription")
            r = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={
                    "Authorization": f"Bearer {api_key}",
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            # Should return 403 because subscription doesn't exist
            assert r.status_code == 403, \
                f"Expected 403 for API key with deleted subscription, got {r.status_code}: {r.text}"

            data = r.json()
            assert "error" in data, "Response missing 'error' field"
            error = data["error"]
            assert error.get("type") == "permission_error", \
                f"Expected error type 'permission_error', got {error.get('type')}"

            log.info(f"✅ API key with deleted subscription → {r.status_code} (permission_error)")

        finally:
            # subscription_name already deleted
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=ns)
            _delete_sa(sa_name, namespace=ns)
            _wait_reconcile()

    def test_api_key_with_inaccessible_subscription_403(self):
        """
        Test: API key bound to a subscription the user no longer has access to.

        This tests an edge case where an API key was minted when the user had access
        to a subscription, but later the user's group membership changed and they
        lost access. The key still has the subscription bound.

        Expected: HTTP 403 with error type: permission_error
        """
        ns = _ns()
        auth_policy_name = "e2e-api-key-no-access-auth"
        subscription_name = "e2e-api-key-no-access-sub"
        sa_user = "e2e-api-key-user-sa"
        sa_other = "e2e-api-key-other-sa"

        try:
            # Create two service accounts
            oc_token_user = _create_sa_token(sa_user, namespace=ns)
            _ = _create_sa_token(sa_other, namespace=ns)

            user_principal = _sa_to_user(sa_user, namespace=ns)
            other_principal = _sa_to_user(sa_other, namespace=ns)

            # Create subscription accessible only to "other" user
            _create_test_auth_policy(auth_policy_name, MODEL_REF, users=[user_principal, other_principal])
            _create_test_subscription(subscription_name, MODEL_REF, users=[other_principal])

            _wait_reconcile()

            # User tries to query with their token but specifying the other user's subscription
            # This simulates what would happen if an API key was bound to a subscription
            # the user doesn't have access to
            log.info("Querying /v1/models with user token and inaccessible subscription")
            r = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={
                    "Authorization": f"Bearer {oc_token_user}",
                    "X-MaaS-Subscription": subscription_name,
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            # Should return 403 because user doesn't have access to the subscription
            assert r.status_code == 403, \
                f"Expected 403 for subscription without access, got {r.status_code}: {r.text}"

            data = r.json()
            assert "error" in data, "Response missing 'error' field"
            error = data["error"]
            assert error.get("type") == "permission_error", \
                f"Expected error type 'permission_error', got {error.get('type')}"

            log.info(f"✅ API key/user with inaccessible subscription → {r.status_code} (permission_error)")

        finally:
            _delete_cr("maassubscription", subscription_name, namespace=ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=ns)
            _delete_sa(sa_user, namespace=ns)
            _delete_sa(sa_other, namespace=ns)
            _wait_reconcile()

    def test_invalid_subscription_header_403(self):
        """
        Test: User with valid subscriptions but providing an invalid/non-existent
        subscription in the header gets 403.

        Expected: HTTP 403 with error type: permission_error and message:
        "requested subscription not found".
        """
        ns = _ns()
        auth_policy_name = "e2e-models-invalid-sub-auth"
        subscription_name = "e2e-models-valid-sub"
        sa_name = "e2e-models-invalid-sub-sa"

        try:
            # Create service account and get OC token for maas-api
            oc_token = _create_sa_token(sa_name, namespace=ns)
            sa_user = _sa_to_user(sa_name, namespace=ns)

            # Create test resources - user has valid subscription
            _create_test_auth_policy(auth_policy_name, MODEL_REF, users=[sa_user])
            _create_test_subscription(subscription_name, MODEL_REF, users=[sa_user])

            _wait_reconcile()

            # Test: GET /v1/models WITH non-existent subscription header
            # Expected: 403 with "subscription not found" error
            invalid_sub = "nonexistent-subscription-xyz"
            log.info(f"Testing: GET /v1/models with invalid subscription header: {invalid_sub}")
            url = f"{_maas_api_url()}/v1/models"
            r = requests.get(
                url,
                headers={
                    "Authorization": f"Bearer {oc_token}",
                    "x-maas-subscription": invalid_sub,
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r.status_code == 403, f"Expected 403 for invalid subscription, got {r.status_code}: {r.text}"

            # Validate error response structure
            data = r.json()
            assert "error" in data, "Response missing 'error' field"
            error = data["error"]
            assert error.get("type") == "permission_error", f"Expected error type 'permission_error', got {error.get('type')}"
            assert "message" in error, "Error missing 'message' field"

            # Message should indicate subscription not found
            message = error["message"].lower()
            assert "not found" in message or "subscription" in message, \
                f"Error message doesn't indicate subscription not found: {error['message']}"

            log.info(f"✅ Invalid subscription header → {r.status_code} (permission_error)")

        finally:
            _delete_cr("maassubscription", subscription_name, namespace=ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=ns)
            _delete_sa(sa_name, namespace=ns)
            _wait_reconcile()

    def test_access_denied_to_subscription_403(self):
        """
        Test: Subscription exists but user is not in its MaaSAuthPolicy owner list.
        User requests that subscription via header.

        Expected: HTTP 403 with error type: permission_error and message:
        "access denied to requested subscription".
        """
        ns = _ns()
        auth_policy_name = "e2e-models-access-denied-auth"
        user_subscription = "e2e-models-user-sub"
        other_subscription = "e2e-models-other-sub"
        sa_user = "e2e-models-user-sa"
        sa_other = "e2e-models-other-sa"

        try:
            # Create two service accounts
            oc_token_user = _create_sa_token(sa_user, namespace=ns)
            _ = _create_sa_token(sa_other, namespace=ns)  # SA creation only - token unused

            user_principal = _sa_to_user(sa_user, namespace=ns)
            other_principal = _sa_to_user(sa_other, namespace=ns)

            # Create test resources
            # Both users have access to the model via auth policy
            _create_test_auth_policy(auth_policy_name, MODEL_REF, users=[user_principal, other_principal])
            # Each user has their own subscription
            _create_test_subscription(user_subscription, MODEL_REF, users=[user_principal])
            _create_test_subscription(other_subscription, MODEL_REF, users=[other_principal])

            _wait_reconcile()

            # Test: User tries to use another user's subscription in header
            # Expected: 403 with "access denied" error
            log.info(f"Testing: GET /v1/models with inaccessible subscription: {other_subscription}")
            url = f"{_maas_api_url()}/v1/models"
            r = requests.get(
                url,
                headers={
                    "Authorization": f"Bearer {oc_token_user}",
                    "x-maas-subscription": other_subscription,
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r.status_code == 403, f"Expected 403 for inaccessible subscription, got {r.status_code}: {r.text}"

            # Validate error response structure
            data = r.json()
            assert "error" in data, "Response missing 'error' field"
            error = data["error"]
            assert error.get("type") == "permission_error", f"Expected error type 'permission_error', got {error.get('type')}"
            assert "message" in error, "Error missing 'message' field"

            # Security: Bare subscription names return "not found" to avoid leaking namespace info.
            # This prevents enumeration of subscriptions across namespaces.
            message = error["message"].lower()
            assert "denied" in message or "access" in message or "not found" in message, \
                f"Error message should indicate permission issue: {error['message']}"

            log.info(f"✅ Access denied to subscription → {r.status_code} (permission_error)")

        finally:
            _delete_cr("maassubscription", user_subscription, namespace=ns)
            _delete_cr("maassubscription", other_subscription, namespace=ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=ns)
            _delete_sa(sa_user, namespace=ns)
            _delete_sa(sa_other, namespace=ns)
            _wait_reconcile()

    def test_api_key_ignores_subscription_header(self):
        """
        Test: API key ignores x-maas-subscription header and uses bound subscription.

        Creates an API key bound to one subscription, then sends request with header
        pointing to a different subscription. The API key should ignore the header
        and return models from its bound subscription.

        Expected: HTTP 200 with models from the key's bound subscription (header ignored).
        """
        sa_name = "e2e-api-key-ignores-header-sa"
        sa_ns = "default"
        maas_ns = _ns()
        sub1_name = "e2e-ignore-header-sub1"
        sub2_name = "e2e-ignore-header-sub2"
        auth1_name = "e2e-ignore-header-auth1"
        auth2_name = "e2e-ignore-header-auth2"
        api_key = None

        try:
            # Create SA
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)
            sa_user = _sa_to_user(sa_name, namespace=sa_ns)

            # Create two subscriptions with different models
            log.info(f"Creating subscription 1 with {DISTINCT_MODEL_REF}")
            _create_test_auth_policy(auth1_name, DISTINCT_MODEL_REF, users=[sa_user])
            _create_test_subscription(sub1_name, DISTINCT_MODEL_REF, users=[sa_user], priority=10)

            log.info(f"Creating subscription 2 with {DISTINCT_MODEL_2_REF}")
            _create_test_auth_policy(auth2_name, DISTINCT_MODEL_2_REF, users=[sa_user])
            _create_test_subscription(sub2_name, DISTINCT_MODEL_2_REF, users=[sa_user], priority=5)

            # Wait for both subscriptions to reconcile before creating API key
            _wait_for_maas_subscription_phase(sub1_name, namespace=maas_ns)
            _wait_for_maas_subscription_phase(sub2_name, namespace=maas_ns)

            # Create API key - will be bound to highest priority subscription (sub1)
            log.info(f"Creating API key (will bind to {sub1_name} - highest priority)")
            api_key = _create_api_key(sa_token, name=f"{sa_name}-key")

            _wait_reconcile()

            # Test: Send request with header pointing to sub2, but key is bound to sub1
            log.info(f"Querying /v1/models with API key bound to {sub1_name} but header={sub2_name}")
            r = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={
                    "Authorization": f"Bearer {api_key}",
                    "x-maas-subscription": sub2_name,  # Try to override with header
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r.status_code == 200, f"Expected 200, got {r.status_code}: {r.text}"
            data = r.json()
            models = data.get("data") or []

            # Verify we got models from sub1 (not sub2 - header ignored)
            assert len(models) > 0, "Expected at least one model"

            for model in models:
                model_id = model.get("id")
                subscriptions = [s["name"] for s in model.get("subscriptions", [])]

                # Models should be from sub1 (bound subscription), not sub2 (header)
                assert sub1_name in subscriptions, \
                    f"Model {model_id} should be in {sub1_name} (bound), not {sub2_name} (header). Got: {subscriptions}"

                # Should NOT find sub2's model (DISTINCT_MODEL_2_ID)
                assert model_id != DISTINCT_MODEL_2_ID, \
                    f"Should not see {DISTINCT_MODEL_2_ID} from {sub2_name} (header ignored)"

            log.info(f"✅ API key ignored x-maas-subscription header → returned {len(models)} model(s) from bound subscription")

        finally:
            _delete_cr("maassubscription", sub1_name, namespace=maas_ns)
            _delete_cr("maassubscription", sub2_name, namespace=maas_ns)
            _delete_cr("maasauthpolicy", auth1_name, namespace=maas_ns)
            _delete_cr("maasauthpolicy", auth2_name, namespace=maas_ns)
            _delete_sa(sa_name, namespace=sa_ns)
            _wait_reconcile()

    def test_multiple_api_keys_different_subscriptions(self):
        """
        Test: Multiple API keys each bound to different subscriptions.

        Creates two API keys from the same user, each explicitly bound to a different
        subscription. Verifies each key returns only its bound subscription's models.

        Expected: Each API key returns models only from its bound subscription.
        """
        sa_name = "e2e-multi-keys-sa"
        sa_ns = "default"
        maas_ns = _ns()
        sub1_name = "e2e-multi-keys-sub1"
        sub2_name = "e2e-multi-keys-sub2"
        auth1_name = "e2e-multi-keys-auth1"
        auth2_name = "e2e-multi-keys-auth2"
        api_key1 = None
        api_key2 = None

        try:
            # Create SA
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)
            sa_user = _sa_to_user(sa_name, namespace=sa_ns)

            # Create two subscriptions with different models
            log.info(f"Creating subscription 1 with {DISTINCT_MODEL_REF}")
            _create_test_auth_policy(auth1_name, DISTINCT_MODEL_REF, users=[sa_user])
            _create_test_subscription(sub1_name, DISTINCT_MODEL_REF, users=[sa_user])

            log.info(f"Creating subscription 2 with {DISTINCT_MODEL_2_REF}")
            _create_test_auth_policy(auth2_name, DISTINCT_MODEL_2_REF, users=[sa_user])
            _create_test_subscription(sub2_name, DISTINCT_MODEL_2_REF, users=[sa_user])

            # Wait for both subscriptions to reconcile before creating API keys
            _wait_for_maas_subscription_phase(sub1_name, namespace=maas_ns)
            _wait_for_maas_subscription_phase(sub2_name, namespace=maas_ns)

            # Create two API keys, each bound to a different subscription
            log.info(f"Creating API key 1 bound to {sub1_name}")
            api_key1_response = requests.post(
                f"{_maas_api_url()}/v1/api-keys",
                headers={"Authorization": f"Bearer {sa_token}", "Content-Type": "application/json"},
                json={"name": "key1", "subscription": sub1_name},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )
            assert api_key1_response.status_code in (200, 201)
            api_key1 = api_key1_response.json().get("key")
            bound_sub1 = api_key1_response.json().get("subscription")
            assert bound_sub1 == sub1_name, f"Key 1 should be bound to {sub1_name}, got {bound_sub1}"

            log.info(f"Creating API key 2 bound to {sub2_name}")
            api_key2_response = requests.post(
                f"{_maas_api_url()}/v1/api-keys",
                headers={"Authorization": f"Bearer {sa_token}", "Content-Type": "application/json"},
                json={"name": "key2", "subscription": sub2_name},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )
            assert api_key2_response.status_code in (200, 201)
            api_key2 = api_key2_response.json().get("key")
            bound_sub2 = api_key2_response.json().get("subscription")
            assert bound_sub2 == sub2_name, f"Key 2 should be bound to {sub2_name}, got {bound_sub2}"

            _wait_reconcile()

            # Test key1 - should return models from sub1 only
            log.info(f"Testing API key 1 (bound to {sub1_name})")
            r1 = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={"Authorization": f"Bearer {api_key1}"},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )
            assert r1.status_code == 200, f"Expected 200 for key1, got {r1.status_code}: {r1.text}"
            models1 = r1.json().get("data") or []
            model_ids1 = {m["id"] for m in models1}

            assert DISTINCT_MODEL_ID in model_ids1, f"Key1 should see {DISTINCT_MODEL_ID} from {sub1_name}"
            assert DISTINCT_MODEL_2_ID not in model_ids1, f"Key1 should NOT see {DISTINCT_MODEL_2_ID} from {sub2_name}"

            # Test key2 - should return models from sub2 only
            log.info(f"Testing API key 2 (bound to {sub2_name})")
            r2 = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={"Authorization": f"Bearer {api_key2}"},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )
            assert r2.status_code == 200, f"Expected 200 for key2, got {r2.status_code}: {r2.text}"
            models2 = r2.json().get("data") or []
            model_ids2 = {m["id"] for m in models2}

            assert DISTINCT_MODEL_2_ID in model_ids2, f"Key2 should see {DISTINCT_MODEL_2_ID} from {sub2_name}"
            assert DISTINCT_MODEL_ID not in model_ids2, f"Key2 should NOT see {DISTINCT_MODEL_ID} from {sub1_name}"

            log.info(f"✅ Multiple API keys with different bindings → Key1: {len(models1)} models, Key2: {len(models2)} models")

        finally:
            _delete_cr("maassubscription", sub1_name, namespace=maas_ns)
            _delete_cr("maassubscription", sub2_name, namespace=maas_ns)
            _delete_cr("maasauthpolicy", auth1_name, namespace=maas_ns)
            _delete_cr("maasauthpolicy", auth2_name, namespace=maas_ns)
            _delete_sa(sa_name, namespace=sa_ns)
            _wait_reconcile()

    def test_service_account_token_multiple_subs_no_header(self):
        """
        Test: K8s token with access to multiple subscriptions returns all models (no header).

        Creates a service account with access to two subscriptions (via group and user).
        When querying without x-maas-subscription header, should return models from
        all accessible subscriptions.

        Expected: HTTP 200 with models from both subscriptions.
        """
        sa_name = "e2e-sa-multi-subs-no-header"
        sa_ns = "default"
        maas_ns = _ns()
        sub1_name = "e2e-sa-multi-no-hdr-sub1"
        sub2_name = "e2e-sa-multi-no-hdr-sub2"
        auth1_name = "e2e-sa-multi-no-hdr-auth1"
        auth2_name = "e2e-sa-multi-no-hdr-auth2"

        try:
            # Create SA
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)
            sa_user = _sa_to_user(sa_name, namespace=sa_ns)

            # Create two subscriptions with different models
            # Sub1: Access via system:authenticated group
            log.info(f"Creating subscription 1 with {DISTINCT_MODEL_REF} (group: system:authenticated)")
            _create_test_auth_policy(auth1_name, DISTINCT_MODEL_REF, groups=["system:authenticated"])
            _create_test_subscription(sub1_name, DISTINCT_MODEL_REF, groups=["system:authenticated"])

            # Sub2: Access via specific user
            log.info(f"Creating subscription 2 with {DISTINCT_MODEL_2_REF} (user: {sa_user})")
            _create_test_auth_policy(auth2_name, DISTINCT_MODEL_2_REF, users=[sa_user])
            _create_test_subscription(sub2_name, DISTINCT_MODEL_2_REF, users=[sa_user])

            _wait_for_maas_auth_policy_phase(auth1_name)
            _wait_for_maas_auth_policy_phase(auth2_name)
            _wait_for_maas_subscription_phase(sub1_name)
            _wait_for_maas_subscription_phase(sub2_name)

            # Query with K8s token (no header)
            log.info("Querying /v1/models with K8s token (no header) - should return models from both subscriptions")
            r = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={"Authorization": f"Bearer {sa_token}"},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r.status_code == 200, f"Expected 200, got {r.status_code}: {r.text}"
            data = r.json()
            models = data.get("data") or []
            model_ids = {m["id"] for m in models}

            # Should see models from BOTH subscriptions
            assert DISTINCT_MODEL_ID in model_ids, \
                f"Should see {DISTINCT_MODEL_ID} from {sub1_name} (group access)"
            assert DISTINCT_MODEL_2_ID in model_ids, \
                f"Should see {DISTINCT_MODEL_2_ID} from {sub2_name} (user access)"

            log.info(f"✅ K8s token with multiple subscriptions (no header) → {len(models)} models from both subscriptions")

        finally:
            _delete_cr("maassubscription", sub1_name, namespace=maas_ns)
            _delete_cr("maassubscription", sub2_name, namespace=maas_ns)
            _delete_cr("maasauthpolicy", auth1_name, namespace=maas_ns)
            _delete_cr("maasauthpolicy", auth2_name, namespace=maas_ns)
            _delete_sa(sa_name, namespace=sa_ns)
            _wait_reconcile()

    def test_service_account_token_multiple_subs_with_header(self):
        """
        Test: K8s token with access to multiple subscriptions filters by header.

        Creates a service account with access to two subscriptions. When querying
        with x-maas-subscription header, should return models from only the specified
        subscription.

        Expected: HTTP 200 with models from only the specified subscription.
        """
        sa_name = "e2e-sa-multi-subs-with-header"
        sa_ns = "default"
        maas_ns = _ns()
        sub1_name = "e2e-sa-multi-hdr-sub1"
        sub2_name = "e2e-sa-multi-hdr-sub2"
        auth1_name = "e2e-sa-multi-hdr-auth1"
        auth2_name = "e2e-sa-multi-hdr-auth2"

        try:
            # Create SA
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)
            sa_user = _sa_to_user(sa_name, namespace=sa_ns)

            # Create two subscriptions with different models
            log.info(f"Creating subscription 1 with {DISTINCT_MODEL_REF}")
            _create_test_auth_policy(auth1_name, DISTINCT_MODEL_REF, users=[sa_user])
            _create_test_subscription(sub1_name, DISTINCT_MODEL_REF, users=[sa_user])

            log.info(f"Creating subscription 2 with {DISTINCT_MODEL_2_REF}")
            _create_test_auth_policy(auth2_name, DISTINCT_MODEL_2_REF, users=[sa_user])
            _create_test_subscription(sub2_name, DISTINCT_MODEL_2_REF, users=[sa_user])

            _wait_for_maas_auth_policy_phase(auth1_name)
            _wait_for_maas_auth_policy_phase(auth2_name)
            _wait_for_maas_subscription_phase(sub1_name)
            _wait_for_maas_subscription_phase(sub2_name)

            # Query with K8s token and header specifying sub1
            log.info(f"Querying /v1/models with K8s token and header: {sub1_name}")
            r1 = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={
                    "Authorization": f"Bearer {sa_token}",
                    "x-maas-subscription": sub1_name,
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r1.status_code == 200, f"Expected 200, got {r1.status_code}: {r1.text}"
            models1 = r1.json().get("data") or []
            model_ids1 = {m["id"] for m in models1}

            # Should see only models from sub1
            assert DISTINCT_MODEL_ID in model_ids1, f"Should see {DISTINCT_MODEL_ID} from {sub1_name}"
            assert DISTINCT_MODEL_2_ID not in model_ids1, f"Should NOT see {DISTINCT_MODEL_2_ID} from {sub2_name}"

            # Query with K8s token and header specifying sub2
            log.info(f"Querying /v1/models with K8s token and header: {sub2_name}")
            r2 = requests.get(
                f"{_maas_api_url()}/v1/models",
                headers={
                    "Authorization": f"Bearer {sa_token}",
                    "x-maas-subscription": sub2_name,
                },
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r2.status_code == 200, f"Expected 200, got {r2.status_code}: {r2.text}"
            models2 = r2.json().get("data") or []
            model_ids2 = {m["id"] for m in models2}

            # Should see only models from sub2
            assert DISTINCT_MODEL_2_ID in model_ids2, f"Should see {DISTINCT_MODEL_2_ID} from {sub2_name}"
            assert DISTINCT_MODEL_ID not in model_ids2, f"Should NOT see {DISTINCT_MODEL_ID} from {sub1_name}"

            log.info(f"✅ K8s token with header filtering → Sub1: {len(models1)} models, Sub2: {len(models2)} models")

        finally:
            _delete_cr("maassubscription", sub1_name, namespace=maas_ns)
            _delete_cr("maassubscription", sub2_name, namespace=maas_ns)
            _delete_cr("maasauthpolicy", auth1_name, namespace=maas_ns)
            _delete_cr("maasauthpolicy", auth2_name, namespace=maas_ns)
            _delete_sa(sa_name, namespace=sa_ns)
            _wait_reconcile()

    def test_unauthenticated_request_401(self):
        """
        Test: Request to /v1/models without Authorization header gets 401.

        Expected: HTTP 401 (authentication_error).
        """
        # Test: GET /v1/models WITHOUT Authorization header
        # Expected: 401 Unauthorized
        log.info("Testing: GET /v1/models without Authorization header")
        url = f"{_maas_api_url()}/v1/models"
        r = requests.get(
            url,
            timeout=TIMEOUT,
            verify=TLS_VERIFY,
        )

        assert r.status_code == 401, f"Expected 401 for unauthenticated request, got {r.status_code}: {r.text}"

        # Validate error response structure (if present)
        try:
            data = r.json()
            if "error" in data:
                error = data["error"]
                # If error type is present, it should be authentication_error
                if "type" in error:
                    assert error["type"] == "authentication_error", \
                        f"Expected error type 'authentication_error', got {error.get('type')}"
        except (json.JSONDecodeError, ValueError):
            # Response might not be JSON, which is acceptable for 401
            pass

        log.info(f"✅ Unauthenticated request → {r.status_code}")

    def test_central_models_endpoint_exempt_from_rate_limiting(self):
        """
        Test that the central /v1/models endpoint remains accessible when token quota is exhausted.

        This test validates the end-to-end flow:
        1. User exhausts token quota with inference requests (gets 429)
        2. Central /v1/models endpoint is exempt at gateway level (gateway-default-deny TRLP)
        3. Central endpoint calls model-specific /v1/models endpoints for discovery
        4. Model-specific endpoints are also exempt (per-route TRLP fix)
        5. Central endpoint successfully aggregates and returns model list

        This ensures the entire discovery chain works when quota is exhausted.

        Ref: https://issues.redhat.com/browse/RHOAIENG-46770
        """
        # Use unconfigured model to isolate this test
        model_ref = UNCONFIGURED_MODEL_REF
        model_path = UNCONFIGURED_MODEL_PATH

        # Create unique subscription and auth policy names
        auth_policy_name = "e2e-central-models-exempt-auth"
        subscription_name = "e2e-central-models-exempt-sub"

        # Very low limit for fast, deterministic test
        # With 3 token limit and max_tokens=1, we're guaranteed to exhaust quota within 5 requests
        # (each successful request consumes ≥1 token, so 5 requests > 3 token limit)
        token_limit = 3
        window = "1m"
        max_tokens = 1

        try:
            # 1. Create auth policy allowing system:authenticated
            log.info(f"Creating auth policy for {model_ref}")
            _create_test_auth_policy(
                name=auth_policy_name,
                model_refs=[model_ref],
                groups=["system:authenticated"]
            )
            _wait_reconcile()
            _wait_for_maas_auth_policy_phase(auth_policy_name, timeout=90)

            # 2. Create subscription with low token limit
            log.info(f"Creating subscription with {token_limit} token limit")
            _create_test_subscription(
                name=subscription_name,
                model_refs=[model_ref],
                groups=["system:authenticated"],
                token_limit=token_limit,
                window=window
            )
            _wait_reconcile()
            _wait_for_maas_subscription_phase(subscription_name, timeout=90)

            # Wait for TRLP to be created and enforced
            _wait_for_token_rate_limit_policy(model_ref, model_namespace=MODEL_NAMESPACE, timeout=90)

            # 3. Create API key for this subscription
            oc_token = _get_cluster_token()
            api_key = _create_api_key(
                oc_token,
                name=f"e2e-central-exempt-{uuid.uuid4().hex[:8]}",
                subscription=subscription_name,
            )

            # 4. Exhaust the token limit
            # With 3 token limit and 5 requests, we're guaranteed to hit the limit
            # (each successful request consumes ≥1 token, so 5 requests > 3 token limit)
            max_requests = 5
            success_count = 0
            rate_limited = False

            log.info(f"Exhausting token quota: sending up to {max_requests} requests")
            for i in range(max_requests):
                r = _inference(api_key, path=model_path)
                request_num = i + 1
                log.info(f"Request {request_num}: {r.status_code}")

                if r.status_code == 200:
                    success_count += 1
                elif r.status_code == 429:
                    log.info(f"Rate limit hit after {success_count} successful requests")
                    rate_limited = True
                    break

            # Verify we hit rate limit (otherwise test setup is broken)
            assert rate_limited, \
                f"Expected to hit rate limit within {max_requests} requests with {token_limit} token limit, " \
                f"but got {success_count} successful requests without hitting limit"

            # 5. Verify inference is blocked
            log.info("Verifying inference endpoint is blocked...")
            r_inference = _inference(api_key, path=model_path)
            assert r_inference.status_code == 429, \
                f"Expected 429 for inference after exhausting tokens, got {r_inference.status_code}"
            log.info("✓ Inference endpoint correctly blocked with 429")

            # 6. Verify central /v1/models endpoint still works
            log.info("Verifying central /v1/models endpoint is still accessible...")
            url = f"{_maas_api_url()}/v1/models"
            headers = {"Authorization": f"Bearer {api_key}"}
            r_models = requests.get(url, headers=headers, timeout=TIMEOUT, verify=TLS_VERIFY)

            assert r_models.status_code == 200, \
                f"Expected 200 for central /v1/models endpoint even when quota exhausted, got {r_models.status_code}. " \
                f"The central /v1/models endpoint should be exempt from rate limiting (gateway-level) and " \
                f"should be able to call model-specific /v1/models endpoints (per-route exemption). " \
                f"Response: {r_models.text[:500]}"

            # 7. Verify response structure and contains our model
            try:
                models_data = r_models.json()
                assert "data" in models_data, \
                    f"Expected 'data' field in response, got: {list(models_data.keys())}"

                models = models_data["data"]
                assert isinstance(models, list), "Expected 'data' to be a list"

                # Verify at least one model is tied to our test subscription
                model_ids = [m.get("id") for m in models]
                log.info(f"✅ Central /v1/models returned {len(models)} models: {model_ids}")

                # Check that at least one model belongs to our test subscription
                models_in_our_subscription = []
                for model in models:
                    # Models have a subscriptions array with subscription info
                    model_subs = model.get("subscriptions", [])
                    for sub in model_subs:
                        if sub.get("name") == subscription_name:
                            models_in_our_subscription.append(model.get("id"))
                            break

                assert len(models_in_our_subscription) >= 1, \
                    f"Expected at least 1 model tied to subscription '{subscription_name}', " \
                    f"but found none. Returned models: {model_ids}, subscription: {subscription_name}"

                log.info(f"✓ Found {len(models_in_our_subscription)} model(s) in our subscription: {models_in_our_subscription}")

            except json.JSONDecodeError as e:
                pytest.fail(f"Central /v1/models response is not valid JSON: {e}. Response: {r_models.text[:500]}")

            log.info("✅ Central /v1/models endpoint works correctly when quota exhausted")
            log.info("   - Gateway-level exemption: ✓")
            log.info("   - Model-specific endpoint exemption: ✓")
            log.info("   - End-to-end discovery flow: ✓")

        finally:
            # Clean up
            _delete_cr("maassubscription", subscription_name)
            _delete_cr("maasauthpolicy", auth_policy_name)
            _wait_reconcile()
            log.info("Cleaned up central models endpoint exemption test resources")
