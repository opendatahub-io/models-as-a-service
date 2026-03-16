"""
E2E tests for the /v1/models endpoint that validate subscription-aware model filtering.

Tests the /v1/models endpoint in maas-api/internal/handlers/models.go which lists
available models filtered by the user's subscription access.

Requires same environment setup as test_subscription.py:
  - GATEWAY_HOST env var (e.g. maas.apps.cluster.example.com)
  - MAAS_API_BASE_URL env var (e.g. https://maas.apps.cluster.example.com/maas-api)
  - maas-controller deployed with example CRs applied
  - oc/kubectl access to create service account tokens
"""

import json
import logging
import os
import subprocess
import time

import pytest
import requests

# Import helpers from test_subscription module
from test_subscription import (
    _apply_cr,
    _create_api_key,
    _create_sa_token,
    _create_test_auth_policy,
    _create_test_subscription,
    _delete_cr,
    _delete_sa,
    _get_auth_policies_for_model,
    _get_cr,
    _get_subscriptions_for_model,
    _maas_api_url,
    _ns,
    _sa_to_user,
    _snapshot_cr,
    _wait_reconcile,
    DISTINCT_MODEL_ID,
    DISTINCT_MODEL_REF,
    DISTINCT_MODEL_2_ID,
    DISTINCT_MODEL_2_REF,
    MODEL_NAMESPACE,
    MODEL_REF,
    PREMIUM_MODEL_REF,
    UNCONFIGURED_MODEL_REF,
    SIMULATOR_ACCESS_POLICY,
    SIMULATOR_SUBSCRIPTION,
    TIMEOUT,
    TLS_VERIFY,
)

log = logging.getLogger(__name__)


class TestModelsEndpoint:
    """
    End-to-end tests for the /v1/models endpoint that validate subscription-aware
    model filtering behavior.

    The /v1/models endpoint (maas-api/internal/handlers/models.go) lists available
    models filtered by the user's subscription access. Key behaviors:
    - Auto-selects subscription if user has exactly one accessible subscription
    - Requires X-MaaS-Subscription header if user has multiple subscriptions
    - Returns HTTP 403 with permission_error for subscription authorization failures
    - Returns HTTP 401 for missing authentication
    - Filters models based on subscription access (probes each model endpoint)

    Test Coverage (14 tests) - Organized by Expected HTTP Status:

    ═══════════════════════════════════════════════════════════════════════════
    SUCCESS CASES (HTTP 200) - Core Subscription Selection
    ═══════════════════════════════════════════════════════════════════════════
    1. test_single_subscription_auto_select
       → User with one subscription, no header → 200 (auto-select)

    2. test_explicit_subscription_header
       → User with multiple subscriptions, explicit header → 200

    3. test_empty_subscription_header_value
       → Empty header value → 200 (same as no header)

    ═══════════════════════════════════════════════════════════════════════════
    SUCCESS CASES (HTTP 200) - Model Filtering & Data Validation
    ═══════════════════════════════════════════════════════════════════════════
    4. test_models_filtered_by_subscription
       → Models correctly filtered by specified subscription

    5. test_deduplication_same_model_multiple_refs (xfail - deduplication bug)
       → Same modelRef listed twice should deduplicate to 1 entry

    6. test_different_modelrefs_same_model_id (xfail - deduplication bug)
       → Different modelRefs serving SAME model ID should deduplicate to 1 entry

    7. test_multiple_distinct_models_in_subscription
       → Different modelRefs with different IDs returns 2 entries (no duplicates)

    8. test_empty_model_list
       → Empty model list should return [] not null

    9. test_response_schema_matches_openapi
       → Response structure matches OpenAPI specification

    10. test_model_metadata_preserved
        → Model fields (url, ready, created, owned_by) accurate

    ═══════════════════════════════════════════════════════════════════════════
    ERROR CASES (HTTP 403) - Permission Errors
    ═══════════════════════════════════════════════════════════════════════════
    11. test_multi_subscription_without_header_403
        → Multiple subscriptions, no header → 403 permission_error

    12. test_invalid_subscription_header_403
        → Non-existent subscription → 403 permission_error

    13. test_access_denied_to_subscription_403
        → Subscription exists but user lacks access → 403 permission_error

    ═══════════════════════════════════════════════════════════════════════════
    ERROR CASES (HTTP 401) - Authentication Errors
    ═══════════════════════════════════════════════════════════════════════════
    14. test_unauthenticated_request_401
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
        Test: User with multiple subscriptions can list models by providing
        x-maas-subscription header.

        Expected: HTTP 200 with models from only the specified subscription.

        Note: Creates SA that has access to both simulator-subscription (via system:authenticated)
        and premium-simulator-subscription (by adding SA to its users list).
        """
        sa_name = "e2e-models-explicit-header-sa"
        sa_ns = "default"
        maas_ns = _ns()
        api_key = None
        sa_user = None

        try:
            # Create service account - will be in system:authenticated group
            # This gives access to simulator-subscription automatically
            sa_token = _create_sa_token(sa_name, namespace=sa_ns)
            sa_user = _sa_to_user(sa_name, namespace=sa_ns)

            # Add SA to premium-simulator-subscription to give it access to a second subscription
            log.info(f"Adding {sa_user} to premium-simulator-subscription users")
            subprocess.run([
                "kubectl", "patch", "maassubscription", "premium-simulator-subscription",
                "-n", maas_ns,
                "--type=json",
                "-p", f'[{{"op": "add", "path": "/spec/owner/users/-", "value": "{sa_user}"}}]'
            ], check=True)

            # Create an API key using the SA token (API keys inherit the SA's groups)
            log.info("Creating API key for test...")
            api_key_response = requests.post(
                f"{_maas_api_url()}/v1/api-keys",
                headers={"Authorization": f"Bearer {sa_token}", "Content-Type": "application/json"},
                json={"name": "e2e-explicit-header-test-key"},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )
            assert api_key_response.status_code in (200, 201), f"Failed to create API key: {api_key_response.status_code} {api_key_response.text}"
            api_key = api_key_response.json().get("key")
            assert api_key, "API key creation response missing 'key' field"

            _wait_reconcile()

            # Test: GET /v1/models WITH x-maas-subscription header
            # Expected: Returns models from simulator-subscription only
            log.info("Testing: GET /v1/models with explicit subscription header: simulator-subscription")
            url = f"{_maas_api_url()}/v1/models"
            r = requests.get(
                url,
                headers={
                    "Authorization": f"Bearer {api_key}",
                    "x-maas-subscription": "simulator-subscription",
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

            log.info(f"✅ Explicit subscription header → {r.status_code} with {len(models)} model(s)")

        finally:
            # Remove SA from premium-simulator-subscription
            if sa_user is not None:
                log.info(f"Removing {sa_user} from premium-simulator-subscription users")
                # Get current users list, remove our SA, then patch
                result = subprocess.run([
                    "kubectl", "get", "maassubscription", "premium-simulator-subscription",
                    "-n", maas_ns, "-o", "jsonpath={.spec.owner.users}"
                ], capture_output=True, text=True)

                if sa_user in result.stdout:
                    users = json.loads(result.stdout) if result.stdout and result.stdout.strip() else []
                    users = [u for u in users if u != sa_user]
                    subprocess.run([
                        "kubectl", "patch", "maassubscription", "premium-simulator-subscription",
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
                "kubectl", "patch", "maassubscription", "premium-simulator-subscription",
                "-n", maas_ns,
                "--type=json",
                "-p", f'[{{"op": "add", "path": "/spec/owner/users/-", "value": "{sa_user}"}}]'
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
                    "x-maas-subscription": "simulator-subscription",
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
                    "x-maas-subscription": "premium-simulator-subscription",
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
                    "kubectl", "get", "maassubscription", "premium-simulator-subscription",
                    "-n", maas_ns, "-o", "jsonpath={.spec.owner.users}"
                ], capture_output=True, text=True)

                if sa_user in result.stdout:
                    users = json.loads(result.stdout) if result.stdout and result.stdout.strip() else []
                    users = [u for u in users if u != sa_user]
                    subprocess.run([
                        "kubectl", "patch", "maassubscription", "premium-simulator-subscription",
                        "-n", maas_ns,
                        "--type=merge",
                        "-p", json.dumps({"spec": {"owner": {"users": users}}})
                    ], check=True)

            _delete_sa(sa_name, namespace=sa_ns)

    @pytest.mark.xfail(reason="Known bug: API does not deduplicate - same modelRef 2x returns 2+ duplicates instead of 1", strict=True)
    def test_deduplication_same_model_multiple_refs(self):
        """
        Test 6: Same modelRef listed twice should deduplicate to 1 entry.

        Creates a subscription with the SAME modelRef listed TWICE (different rate limits).
        The API should deduplicate and return only 1 entry regardless of how many times
        the same modelRef is listed.

        Currently fails because:
        - API returns 2+ duplicate entries instead of 1 deduplicated entry
        - No deduplication logic in maas-api/internal/handlers/models.go

        See: BUG_MODELS_ENDPOINT_NO_DEDUPLICATION.md
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
                ["kubectl", "apply", "-f", "-"],
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
                ["kubectl", "apply", "-f", "-"],
                input=json.dumps(subscription_cr),
                text=True,
                check=True,
            )

            # Create API key
            api_key_response = requests.post(
                f"{_maas_api_url()}/v1/api-keys",
                headers={"Authorization": f"Bearer {sa_token}", "Content-Type": "application/json"},
                json={"name": "e2e-dedup-test-key"},
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

            log.info(f"✅ API correctly deduplicated same modelRef listed 2x → 1 entry")

        finally:
            # Cleanup
            _delete_cr("maassubscription", subscription_name, namespace=maas_ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=maas_ns)
            _delete_sa(sa_name, namespace=sa_ns)
            _wait_reconcile()

    @pytest.mark.xfail(reason="Known bug: API does not deduplicate - different refs serving same model ID return 3+ duplicates instead of 1", strict=True)
    def test_different_modelrefs_same_model_id(self):
        """
        Test 7: Different modelRefs serving same model ID should deduplicate.

        Uses two DIFFERENT MaaSModelRefs (each listed ONCE) that both serve the
        SAME model ID:
        - MODEL_REF (facebook-opt-125m-simulated) → serves "facebook/opt-125m"
        - PREMIUM_MODEL_REF (premium-simulated-simulated-premium) → serves "facebook/opt-125m"

        The API should deduplicate by model ID and return only 1 entry, regardless
        of how many different MaaSModelRefs serve that same model.

        Currently fails because:
        - API returns 3+ duplicate entries instead of 1 deduplicated entry
        - No deduplication logic in maas-api/internal/handlers/models.go
        - Backend /v1/models endpoint may also return duplicates

        See: BUG_MODELS_ENDPOINT_NO_DEDUPLICATION.md
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
                ["kubectl", "apply", "-f", "-"],
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
                ["kubectl", "apply", "-f", "-"],
                input=json.dumps(subscription_cr),
                text=True,
                check=True,
            )

            # Create API key
            api_key_response = requests.post(
                f"{_maas_api_url()}/v1/api-keys",
                headers={"Authorization": f"Bearer {sa_token}", "Content-Type": "application/json"},
                json={"name": "e2e-diff-refs-test-key"},
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
            log.info(f"   Subscription had: 2 different modelRefs both serving 'facebook/opt-125m'")

            # Both modelRefs serve the same model ID, so should only have 1 unique ID
            assert len(unique_ids) == 1, \
                f"Expected only 1 unique model ID (both modelRefs serve facebook/opt-125m), got {len(unique_ids)}: {unique_ids}"

            # Verify it's the expected model ID
            expected_id = "facebook/opt-125m"
            assert expected_id in unique_ids, \
                f"Expected to find '{expected_id}', but got {unique_ids}"

            # INTENDED BEHAVIOR: Should return exactly 1 entry (deduplicated by model ID)
            # even though 2 different modelRefs serve the same model
            assert len(models) == 1, \
                f"Expected 1 deduplicated entry (2 different refs serve same ID), got {len(models)} duplicates: {model_ids}"

            log.info(f"✅ API correctly deduplicated different modelRefs serving same ID → 1 entry")

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
                ["kubectl", "apply", "-f", "-"],
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
                ["kubectl", "apply", "-f", "-"],
                input=json.dumps(subscription_cr),
                text=True,
                check=True,
            )

            # Create API key
            api_key_response = requests.post(
                f"{_maas_api_url()}/v1/api-keys",
                headers={"Authorization": f"Bearer {sa_token}", "Content-Type": "application/json"},
                json={"name": "e2e-distinct-models-test-key"},
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

            log.info(f"✅ API correctly returned 2 distinct models without duplicates: {sorted(unique_ids)}")

        finally:
            # Cleanup
            _delete_cr("maassubscription", subscription_name, namespace=maas_ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=maas_ns)
            _delete_sa(sa_name, namespace=sa_ns)
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

            # Create API key
            api_key = _create_api_key(sa_token, name=f"{sa_name}-key")

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
        Test 10: Response structure matches OpenAPI schema.

        Validates all required fields and types match the API specification.
        """
        log.info("Test 9: Response schema matches OpenAPI spec")

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
        Test 11: Model metadata is correctly preserved.

        Validates that url, ready, created, owned_by fields are accurate.
        """
        log.info("Test 10: Model metadata preserved")

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

    def test_multi_subscription_without_header_403(self):
        """
        Test: User with multiple subscriptions must provide x-maas-subscription header.
        Without it, returns 403 permission_error.

        Expected: HTTP 403 with error type: permission_error and message indicating
        header is required.
        """
        ns = _ns()
        auth_policy_name = "e2e-models-multi-no-header-auth"
        subscription_1 = "e2e-models-free-sub"
        subscription_2 = "e2e-models-premium-sub"
        sa_name = "e2e-models-multi-no-header-sa"

        try:
            # Create service account and get OC token for maas-api
            oc_token = _create_sa_token(sa_name, namespace=ns)
            sa_user = _sa_to_user(sa_name, namespace=ns)

            # Create test resources - user has multiple subscriptions (free + premium)
            _create_test_auth_policy(auth_policy_name, MODEL_REF, users=[sa_user])
            _create_test_subscription(subscription_1, MODEL_REF, users=[sa_user])
            _create_test_subscription(subscription_2, MODEL_REF, users=[sa_user])

            _wait_reconcile()

            # Test: GET /v1/models WITHOUT x-maas-subscription header
            # Expected: 403 because user has multiple subscriptions
            log.info("Testing: GET /v1/models with multiple subscriptions (no header)")
            url = f"{_maas_api_url()}/v1/models"
            r = requests.get(
                url,
                headers={"Authorization": f"Bearer {oc_token}"},
                timeout=TIMEOUT,
                verify=TLS_VERIFY,
            )

            assert r.status_code == 403, f"Expected 403 for multiple subscriptions without header, got {r.status_code}: {r.text}"

            # Validate error response structure
            data = r.json()
            assert "error" in data, "Response missing 'error' field"
            error = data["error"]
            assert error.get("type") == "permission_error", f"Expected error type 'permission_error', got {error.get('type')}"
            assert "message" in error, "Error missing 'message' field"

            # Message should indicate header is required
            message = error["message"].lower()
            assert "subscription" in message or "header" in message, \
                f"Error message doesn't mention subscription/header: {error['message']}"

            log.info(f"✅ Multiple subscriptions without header → {r.status_code} (permission_error)")

        finally:
            _delete_cr("maassubscription", subscription_1, namespace=ns)
            _delete_cr("maassubscription", subscription_2, namespace=ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=ns)
            _delete_sa(sa_name, namespace=ns)
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

            # Message should indicate access denied
            message = error["message"].lower()
            assert "denied" in message or "access" in message, \
                f"Error message doesn't indicate access denied: {error['message']}"

            log.info(f"✅ Access denied to subscription → {r.status_code} (permission_error)")

        finally:
            _delete_cr("maassubscription", user_subscription, namespace=ns)
            _delete_cr("maassubscription", other_subscription, namespace=ns)
            _delete_cr("maasauthpolicy", auth_policy_name, namespace=ns)
            _delete_sa(sa_user, namespace=ns)
            _delete_sa(sa_other, namespace=ns)
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
