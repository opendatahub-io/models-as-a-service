"""
API Keys Feature End-to-End Tests
==================================

Tests the complete API Keys lifecycle and model access:
- CRUD operations (create, list, get, revoke)
- Authorization (admin vs non-admin access)
- Bulk operations (own keys and admin bulk revoke)
- Model inference with API keys (success and failure scenarios)

Prerequisites:
- MaaS platform deployed with API Keys feature enabled
- At least one model deployed with AuthPolicy and Subscription

Environment Variables:
- GATEWAY_HOST: Gateway hostname (e.g., maas.apps.cluster.example.com)
- TOKEN: OpenShift token for regular user API management (or uses `oc whoami -t`)
- ADMIN_OC_TOKEN: Admin token for admin-specific tests (optional, tests skip if not set)
- MODEL_NAME: Override model name for inference tests (optional)
- INFERENCE_MODEL_NAME: Model name for inference request body (optional)
- E2E_SIMULATOR_SUBSCRIPTION: Bound on the session ``api_key`` fixture at mint (default: simulator-subscription)

Admin Tests:
Admin tests (TestAPIKeyAuthorization, admin bulk revoke) require ADMIN_OC_TOKEN.
In Prow CI, this is automatically configured by prow_run_smoke_test.sh.
For local testing:
  1. Create admin SA: oc create sa tester-admin -n default
  2. Grant admin: oc adm policy add-cluster-role-to-user cluster-admin system:serviceaccount:default:tester-admin
  3. Get token: export ADMIN_OC_TOKEN=$(oc create token tester-admin -n default)
"""

import logging
import os
import pytest
import requests
import time

from conftest import TLS_VERIFY

log = logging.getLogger(__name__)


@pytest.fixture
def model_completions_url(model_v1: str) -> str:
    """URL for completions endpoint."""
    return f"{model_v1}/completions"


@pytest.fixture
def inference_model_name() -> str:
    """Model name for inference requests. Override with INFERENCE_MODEL_NAME env var."""
    return os.environ.get("INFERENCE_MODEL_NAME", "facebook/opt-125m")


class TestAPIKeyCRUD:
    """Tests 1-3: Create, list, and revoke API keys."""

    def test_create_api_key(self, api_keys_base_url: str, headers: dict):
        """Test 1: Create API key - verify format and show-once pattern."""
        r = requests.post(
            api_keys_base_url,
            headers=headers,
            json={"name": "test-key-create"},
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert r.status_code in (200, 201), f"Expected 200/201, got {r.status_code}: {r.text}"
        data = r.json()

        # Verify response structure
        assert "id" in data and "key" in data and "name" in data
        key = data["key"]
        assert key.startswith("sk-oai-"), f"Expected sk-oai- prefix, got: {key[:20]}"
        assert len(key) > len("sk-oai-"), "Key body should not be empty"
        # Note: status field not included in create response (only in list/get)

        print(f"[create] Created key id={data['id']}, key prefix={key[:15]}...")

        # Verify plaintext key is NOT returned on subsequent GET
        r_get = requests.get(f"{api_keys_base_url}/{data['id']}", headers=headers, timeout=30, verify=TLS_VERIFY)
        assert r_get.status_code == 200
        assert "key" not in r_get.json(), "Plaintext key should not be in GET (show-once pattern)"

    def test_list_api_keys(self, api_keys_base_url: str, headers: dict):
        """Test 2: List own keys - verify basic functionality."""
        # Create two keys
        r1 = requests.post(api_keys_base_url, headers=headers, json={"name": "test-key-list-1"}, timeout=30, verify=TLS_VERIFY)
        assert r1.status_code in (200, 201)
        key1_id = r1.json()["id"]

        r2 = requests.post(api_keys_base_url, headers=headers, json={"name": "test-key-list-2"}, timeout=30, verify=TLS_VERIFY)
        assert r2.status_code in (200, 201)
        key2_id = r2.json()["id"]

        # List keys using search endpoint
        r = requests.post(
            f"{api_keys_base_url}/search",
            headers=headers,
            json={
                "filters": {"status": ["active"]},
                "sort": {"by": "created_at", "order": "desc"},
                "pagination": {"limit": 50, "offset": 0}
            },
            timeout=30,
            verify=TLS_VERIFY
        )
        assert r.status_code == 200
        data = r.json()
        items = data.get("items") or data.get("data") or []
        assert len(items) >= 2

        # Verify our keys are in the list
        key_ids = [item["id"] for item in items]
        assert key1_id in key_ids and key2_id in key_ids

        # Verify no plaintext keys in list
        for item in items:
            assert "key" not in item

        print(f"[list] Found {len(items)} keys")

        # Test pagination
        r_limit = requests.post(
            f"{api_keys_base_url}/search",
            headers=headers,
            json={
                "filters": {"status": ["active"]},
                "sort": {"by": "created_at", "order": "desc"},
                "pagination": {"limit": 1, "offset": 0}
            },
            timeout=30,
            verify=TLS_VERIFY
        )
        assert r_limit.status_code == 200
        limited_items = (r_limit.json().get("items") or r_limit.json().get("data") or [])
        assert len(limited_items) <= 1
        print(f"[list] Pagination works: limit=1 returned {len(limited_items)} items")

    def test_revoke_api_key(self, api_keys_base_url: str, headers: dict):
        """Test 3: Revoke key - verify status change to 'revoked'."""
        # Create a key
        r_create = requests.post(api_keys_base_url, headers=headers, json={"name": "test-key-revoke"}, timeout=30, verify=TLS_VERIFY)
        assert r_create.status_code in (200, 201)
        key_id = r_create.json()["id"]

        # Revoke it using DELETE
        r = requests.delete(f"{api_keys_base_url}/{key_id}", headers=headers, timeout=30, verify=TLS_VERIFY)
        assert r.status_code == 200
        assert r.json().get("status") == "revoked"

        # Verify GET shows revoked status
        r_get = requests.get(f"{api_keys_base_url}/{key_id}", headers=headers, timeout=30, verify=TLS_VERIFY)
        assert r_get.status_code == 200
        assert r_get.json().get("status") == "revoked"
        print(f"[revoke] Key {key_id} successfully revoked")


class TestAPIKeyAuthorization:
    """Tests 4-5: Admin and non-admin access control."""

    def test_admin_manage_other_users_keys(self, api_keys_base_url: str, headers: dict, admin_headers: dict):
        """Test 4: Admin can manage other user's keys - list and revoke."""
        if not admin_headers:
            pytest.skip("ADMIN_OC_TOKEN not set")

        # Create key as regular user
        r_create = requests.post(api_keys_base_url, headers=headers, json={"name": "regular-user-key"}, timeout=30, verify=TLS_VERIFY)
        assert r_create.status_code in (200, 201)
        user_key_id = r_create.json()["id"]

        # Get username
        r_get = requests.get(f"{api_keys_base_url}/{user_key_id}", headers=headers, timeout=30, verify=TLS_VERIFY)
        username = r_get.json().get("username") or r_get.json().get("owner")
        assert username

        print(f"[admin] User '{username}' created key {user_key_id}")

        # Admin lists keys filtered by username using search endpoint
        r_admin = requests.post(
            f"{api_keys_base_url}/search",
            headers=admin_headers,
            json={
                "filters": {"username": username, "status": ["active"]},
                "sort": {"by": "created_at", "order": "desc"},
                "pagination": {"limit": 50, "offset": 0}
            },
            timeout=30,
            verify=TLS_VERIFY
        )
        assert r_admin.status_code == 200
        items = r_admin.json().get("items") or r_admin.json().get("data") or []
        key_ids = [item["id"] for item in items]
        assert user_key_id in key_ids
        print(f"[admin] Admin listed {len(items)} keys for '{username}'")

        # Admin revokes user's key using DELETE
        r_revoke = requests.delete(f"{api_keys_base_url}/{user_key_id}", headers=admin_headers, timeout=30, verify=TLS_VERIFY)
        assert r_revoke.status_code == 200
        assert r_revoke.json().get("status") == "revoked"
        print(f"[admin] Admin successfully revoked user's key {user_key_id}")

    def test_non_admin_cannot_access_other_users_keys(self, api_keys_base_url: str, headers: dict, admin_headers: dict):
        """Test 5: Non-admin cannot access other user's keys - verify denial.
        
        Note: API returns 404 instead of 403 for IDOR protection (prevents key enumeration).
        This is a security best practice - returning 403 would reveal the key exists.
        """
        if not admin_headers:
            pytest.skip("ADMIN_OC_TOKEN not set")

        # Admin creates a key
        r_admin = requests.post(api_keys_base_url, headers=admin_headers, json={"name": "admin-only-key"}, timeout=30, verify=TLS_VERIFY)
        assert r_admin.status_code in (200, 201)
        admin_key_id = r_admin.json()["id"]

        # Regular user tries to GET admin's key - returns 404 for IDOR protection
        r_get = requests.get(f"{api_keys_base_url}/{admin_key_id}", headers=headers, timeout=30, verify=TLS_VERIFY)
        assert r_get.status_code == 404, f"Expected 404 (IDOR protection), got {r_get.status_code}"

        # Regular user tries to revoke admin's key - returns 404 for IDOR protection
        r_revoke = requests.delete(f"{api_keys_base_url}/{admin_key_id}", headers=headers, timeout=30, verify=TLS_VERIFY)
        assert r_revoke.status_code == 404, f"Expected 404 (IDOR protection), got {r_revoke.status_code}"
        print("[authz] Non-admin correctly got 404 (IDOR protection) for admin's key")


class TestAPIKeyBulkOperations:
    """Tests for bulk operations like bulk-revoke."""

    def test_bulk_revoke_own_keys(self, api_keys_base_url: str, headers: dict):
        """Test 8: Bulk revoke - user can bulk revoke their own keys."""
        # Create multiple keys
        key_ids = []
        for i in range(3):
            r = requests.post(api_keys_base_url, headers=headers, json={"name": f"bulk-test-{i}"}, timeout=30, verify=TLS_VERIFY)
            assert r.status_code in (200, 201)
            key_ids.append(r.json()["id"])

        # Get username from one of the keys
        r_get = requests.get(f"{api_keys_base_url}/{key_ids[0]}", headers=headers, timeout=30, verify=TLS_VERIFY)
        username = r_get.json().get("username") or r_get.json().get("owner")
        assert username

        # Bulk revoke all keys for this user
        r_bulk = requests.post(
            f"{api_keys_base_url}/bulk-revoke",
            headers=headers,
            json={"username": username},
            timeout=30,
            verify=TLS_VERIFY
        )
        assert r_bulk.status_code == 200
        data = r_bulk.json()
        assert data.get("revokedCount") >= 3, f"Expected at least 3 revoked, got {data.get('revokedCount')}"
        print(f"[bulk-revoke] Successfully revoked {data.get('revokedCount')} keys for user {username}")

        # Verify keys are revoked
        for key_id in key_ids:
            r_check = requests.get(f"{api_keys_base_url}/{key_id}", headers=headers, timeout=30, verify=TLS_VERIFY)
            if r_check.status_code == 200:
                assert r_check.json().get("status") == "revoked"

    def test_bulk_revoke_other_user_forbidden(self, api_keys_base_url: str, headers: dict):
        """Test 9: Bulk revoke - non-admin cannot bulk revoke other user's keys."""
        # Try to bulk revoke another user's keys (should fail with 403)
        r_bulk = requests.post(
            f"{api_keys_base_url}/bulk-revoke",
            headers=headers,
            json={"username": "someotheruser"},
            timeout=30,
            verify=TLS_VERIFY
        )
        assert r_bulk.status_code == 403, f"Expected 403, got {r_bulk.status_code}: {r_bulk.text}"
        print("[bulk-revoke] Non-admin correctly got 403 when trying to bulk revoke other user's keys")

    def test_bulk_revoke_admin_can_revoke_any_user(self, api_keys_base_url: str, headers: dict, admin_headers: dict):
        """Test 10: Bulk revoke - admin can bulk revoke any user's keys."""
        if not admin_headers:
            pytest.skip("ADMIN_OC_TOKEN not set")

        # Create a key as regular user
        r = requests.post(api_keys_base_url, headers=headers, json={"name": "admin-bulk-revoke-test"}, timeout=30, verify=TLS_VERIFY)
        assert r.status_code in (200, 201)
        key_id = r.json()["id"]

        # Get username
        r_get = requests.get(f"{api_keys_base_url}/{key_id}", headers=headers, timeout=30, verify=TLS_VERIFY)
        username = r_get.json().get("username") or r_get.json().get("owner")
        assert username

        # Admin bulk revokes user's keys
        r_bulk = requests.post(
            f"{api_keys_base_url}/bulk-revoke",
            headers=admin_headers,
            json={"username": username},
            timeout=30,
            verify=TLS_VERIFY
        )
        assert r_bulk.status_code == 200
        data = r_bulk.json()
        assert data.get("revokedCount") >= 1
        print(f"[bulk-revoke] Admin successfully revoked {data.get('revokedCount')} keys for user {username}")


class TestAPIKeyExpiration:
    """Tests for API key expiration policy enforcement.
    
    These tests verify that the maxExpirationDays limit is enforced when creating API keys.
    
    Environment Variables:
    - API_KEY_MAX_EXPIRATION_DAYS: The configured max expiration in days (set on maas-api deployment).
      Must be explicitly set by the e2e test harness to match the maas-api deployment configuration.
      Default is 30 days. Minimum is 1 day.
    """

    @pytest.fixture
    def max_expiration_days(self) -> int:
        """Get the configured max expiration days from environment.
        
        Defaults to 90 days if API_KEY_MAX_EXPIRATION_DAYS is not set,
        matching the maas-api default (constant.DefaultAPIKeyMaxExpirationDays).
        """
        val = os.environ.get("API_KEY_MAX_EXPIRATION_DAYS", "90")
        try:
            return int(val)
        except ValueError:
            pytest.skip(
                f"API_KEY_MAX_EXPIRATION_DAYS={val!r} is not a valid integer; "
                "skipping expiration policy tests"
            )

    def test_create_key_within_expiration_limit(self, api_keys_base_url: str, headers: dict, max_expiration_days: int):
        """Test: Creating API key with expiration within the limit should succeed."""

        # Request expiration at half the limit (e.g., 15 days if limit is 30)
        expires_in_hours = (max_expiration_days // 2) * 24
        if expires_in_hours <= 0:
            expires_in_hours = 24  # At least 1 day

        r = requests.post(
            api_keys_base_url,
            headers=headers,
            json={
                "name": "test-within-limit",
                "description": f"Test key with {expires_in_hours}h expiration",
                "expiresIn": f"{expires_in_hours}h"
            },
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert r.status_code in (200, 201), f"Expected 200/201, got {r.status_code}: {r.text}"
        data = r.json()
        assert "key" in data, "Response should contain key"
        assert "expiresAt" in data, "Response should contain expiresAt"
        print(f"[expiration] Created key within limit: expires_in={expires_in_hours}h, expiresAt={data.get('expiresAt')}")

    def test_create_key_at_expiration_limit(self, api_keys_base_url: str, headers: dict, max_expiration_days: int):
        """Test: Creating API key with expiration exactly at the limit should succeed."""

        # Request expiration exactly at the limit
        expires_in_hours = max_expiration_days * 24

        r = requests.post(
            api_keys_base_url,
            headers=headers,
            json={
                "name": "test-at-limit",
                "description": f"Test key with exactly {max_expiration_days} days expiration",
                "expiresIn": f"{expires_in_hours}h"
            },
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert r.status_code in (200, 201), f"Expected 200/201, got {r.status_code}: {r.text}"
        data = r.json()
        assert "key" in data, "Response should contain key"
        assert "expiresAt" in data, "Response should contain expiresAt"
        print(f"[expiration] Created key at limit: expires_in={expires_in_hours}h ({max_expiration_days} days)")

    def test_create_key_exceeds_expiration_limit(self, api_keys_base_url: str, headers: dict, max_expiration_days: int):
        """Test: Creating API key with expiration exceeding the limit should fail."""

        # Request expiration exceeding the limit (e.g., 2x the limit)
        exceeds_days = max_expiration_days * 2
        expires_in_hours = exceeds_days * 24

        r = requests.post(
            api_keys_base_url,
            headers=headers,
            json={
                "name": "test-exceeds-limit",
                "description": f"Test key with {exceeds_days} days expiration (exceeds {max_expiration_days} day limit)",
                "expiresIn": f"{expires_in_hours}h"
            },
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert r.status_code == 400, f"Expected 400 for exceeding limit, got {r.status_code}: {r.text}"
        
        # Verify error message mentions the limit
        error_text = r.text.lower()
        assert "exceed" in error_text or "maximum" in error_text, \
            f"Error message should mention exceeding maximum: {r.text}"
        print(f"[expiration] Correctly rejected key exceeding limit: {exceeds_days} days > {max_expiration_days} days")

    def test_create_key_without_expiration(self, api_keys_base_url: str, headers: dict, max_expiration_days: int):
        """Test: Creating API key without expiration should succeed (expiration is optional by default)."""
        r = requests.post(
            api_keys_base_url,
            headers=headers,
            json={
                "name": "test-no-expiration",
                "description": "Test key without expiration"
            },
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert r.status_code in (200, 201), f"Expected 200/201, got {r.status_code}: {r.text}"
        data = r.json()
        assert "key" in data, "Response should contain key"
        # expiresAt should be absent or null for non-expiring keys
        expires_at = data.get("expiresAt")
        if expires_at:
            print(f"[expiration] Key created with default expiration: {expires_at}")
        else:
            print("[expiration] Key created without expiration (never expires)")

    def test_create_key_with_short_expiration(self, api_keys_base_url: str, headers: dict):
        """Test: Creating API key with very short expiration (1 hour) should succeed."""
        r = requests.post(
            api_keys_base_url,
            headers=headers,
            json={
                "name": "test-short-expiration",
                "description": "Test key with 1 hour expiration",
                "expiresIn": "1h"
            },
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert r.status_code in (200, 201), f"Expected 200/201, got {r.status_code}: {r.text}"
        data = r.json()
        assert "expiresAt" in data, "Response should contain expiresAt"
        print(f"[expiration] Created key with 1h expiration: expiresAt={data.get('expiresAt')}")


class TestAPIKeyModelInference:
    """Tests 11-15: Using API keys for model inference via gateway."""

    def test_api_key_model_access_success(
        self,
        model_completions_url: str,
        api_key_headers: dict,
        inference_model_name: str,
    ):
        """Test 11: Valid API key can access model endpoint - verify 200 response.

        Subscription is bound on the key at mint (see conftest ``api_key`` fixture).
        """
        r = requests.post(
            model_completions_url,
            headers=api_key_headers,
            json={
                "model": inference_model_name,
                "prompt": "Hello world",
                "max_tokens": 10,
            },
            timeout=60,
            verify=TLS_VERIFY,
        )

        assert r.status_code == 200, f"Expected 200, got {r.status_code}: {r.text}"
        data = r.json()

        # Verify response structure
        assert "choices" in data, "Response should contain 'choices'"
        assert len(data["choices"]) > 0, "Should have at least one choice"
        assert "text" in data["choices"][0] or "message" in data["choices"][0], "Choice should have text or message"

        print(f"[inference] Model access succeeded: {data.get('model')}, tokens={data.get('usage', {}).get('total_tokens')}")

    def test_invalid_api_key_rejected(
        self,
        model_completions_url: str,
        inference_model_name: str,
    ):
        """Test 12: Invalid API key should be rejected with 403."""
        invalid_headers = {
            "Authorization": "Bearer sk-oai-invalid-key-12345",
            "Content-Type": "application/json",
        }

        r = requests.post(
            model_completions_url,
            headers=invalid_headers,
            json={
                "model": inference_model_name,
                "prompt": "Test",
                "max_tokens": 5,
            },
            timeout=30,
            verify=TLS_VERIFY,
        )

        assert r.status_code == 403, f"Expected 403 for invalid key, got {r.status_code}: {r.text}"
        print("[inference] Invalid API key correctly rejected with 403")

    def test_no_auth_header_rejected(
        self,
        model_completions_url: str,
        inference_model_name: str,
    ):
        """Test 13: Missing Authorization header should be rejected with 401."""
        no_auth_headers = {"Content-Type": "application/json"}

        r = requests.post(
            model_completions_url,
            headers=no_auth_headers,
            json={
                "model": inference_model_name,
                "prompt": "Test",
                "max_tokens": 5,
            },
            timeout=30,
            verify=TLS_VERIFY,
        )

        assert r.status_code == 401, f"Expected 401 for missing auth, got {r.status_code}: {r.text}"
        print("[inference] Missing auth header correctly rejected with 401")

    def test_revoked_api_key_rejected(
        self,
        api_keys_base_url: str,
        model_completions_url: str,
        headers: dict,
        inference_model_name: str,
    ):
        """Test 14: Revoked API key should be rejected with 403."""
        # Create a new key
        designated = os.environ.get("E2E_SIMULATOR_SUBSCRIPTION", "simulator-subscription")
        r_create = requests.post(
            api_keys_base_url,
            headers=headers,
            json={"name": "test-revoke-inference", "subscription": designated},
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert r_create.status_code in (200, 201), f"Failed to create key: {r_create.text}"
        data = r_create.json()
        key = data["key"]
        key_id = data["id"]

        # Verify key works first
        key_headers = {"Authorization": f"Bearer {key}", "Content-Type": "application/json"}
        r_test = requests.post(
            model_completions_url,
            headers=key_headers,
            json={"model": inference_model_name, "prompt": "Test", "max_tokens": 5},
            timeout=60,
            verify=TLS_VERIFY,
        )
        # Key should work (200) or might fail for other reasons - we just need to test revocation
        initial_status = r_test.status_code
        print(f"[inference] Key before revoke: HTTP {initial_status}")

        # Revoke the key
        r_revoke = requests.delete(
            f"{api_keys_base_url}/{key_id}",
            headers=headers,
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert r_revoke.status_code == 200, f"Failed to revoke: {r_revoke.text}"
        assert r_revoke.json().get("status") == "revoked"

        # Wait for revocation to propagate
        time.sleep(2)

        # Try to use revoked key
        r_revoked = requests.post(
            model_completions_url,
            headers=key_headers,
            json={"model": inference_model_name, "prompt": "Test", "max_tokens": 5},
            timeout=30,
            verify=TLS_VERIFY,
        )

        assert r_revoked.status_code == 403, f"Expected 403 for revoked key, got {r_revoked.status_code}: {r_revoked.text}"
        print("[inference] Revoked key correctly rejected with 403")

    def test_api_key_chat_completions(
        self,
        model_v1: str,
        api_key_headers: dict,
        inference_model_name: str,
    ):
        """Test 15: API key can access chat/completions endpoint (if supported)."""
        chat_url = f"{model_v1}/chat/completions"

        r = requests.post(
            chat_url,
            headers=api_key_headers,
            json={
                "model": inference_model_name,
                "messages": [{"role": "user", "content": "Hello"}],
                "max_tokens": 10,
            },
            timeout=60,
            verify=TLS_VERIFY,
        )

        # Chat completions may not be supported by all models
        if r.status_code == 404:
            pytest.skip("Chat completions endpoint not available for this model")

        if r.status_code == 200:
            data = r.json()
            assert "choices" in data
            print(f"[inference] Chat completions succeeded: {data.get('model')}")
        else:
            # Some models may return different errors
            print(f"[inference] Chat completions returned {r.status_code}: {r.text[:200]}")
            # Don't fail - chat may not be supported
            pytest.skip(f"Chat completions returned {r.status_code}")


class TestAPIKeyRevocationE2E:
    """End-to-end revocation tests: double revoke, nonexistent key, bulk revoke propagation, remint after revoke."""

    def test_double_revoke_returns_404(self, api_keys_base_url: str, headers: dict):
        """Revoking the same key twice should return 404 on the second attempt."""
        # Create a key
        r_create = requests.post(
            api_keys_base_url, headers=headers, json={"name": "test-double-revoke"}, timeout=30, verify=TLS_VERIFY
        )
        assert r_create.status_code in (200, 201), f"Failed to create key: {r_create.text}"
        key_id = r_create.json()["id"]

        # First revoke succeeds
        r1 = requests.delete(f"{api_keys_base_url}/{key_id}", headers=headers, timeout=30, verify=TLS_VERIFY)
        assert r1.status_code == 200
        assert r1.json().get("status") == "revoked"

        # Second revoke returns 404 (key is no longer active)
        r2 = requests.delete(f"{api_keys_base_url}/{key_id}", headers=headers, timeout=30, verify=TLS_VERIFY)
        assert r2.status_code == 404, f"Expected 404 on double revoke, got {r2.status_code}: {r2.text}"
        print(f"[revoke] Double revoke correctly returns 404 for key {key_id}")

    def test_revoke_nonexistent_key_returns_404(self, api_keys_base_url: str, headers: dict):
        """Revoking a key ID that doesn't exist should return 404."""
        r = requests.delete(
            f"{api_keys_base_url}/nonexistent-uuid-12345", headers=headers, timeout=30, verify=TLS_VERIFY
        )
        assert r.status_code == 404, f"Expected 404 for nonexistent key, got {r.status_code}: {r.text}"
        print("[revoke] Nonexistent key correctly returns 404")

    def test_revoke_then_create_new_key_works(
        self,
        api_keys_base_url: str,
        model_completions_url: str,
        headers: dict,
        inference_model_name: str,
    ):
        """After revoking a key, a newly created key should still work for inference."""
        designated = os.environ.get("E2E_SIMULATOR_SUBSCRIPTION", "simulator-subscription")

        # Create key A
        r_a = requests.post(
            api_keys_base_url,
            headers=headers,
            json={"name": "test-revoke-remint-a", "subscription": designated},
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert r_a.status_code in (200, 201), f"Failed to create key A: {r_a.text}"
        key_a = r_a.json()["key"]
        key_a_id = r_a.json()["id"]

        # Revoke key A
        r_revoke = requests.delete(f"{api_keys_base_url}/{key_a_id}", headers=headers, timeout=30, verify=TLS_VERIFY)
        assert r_revoke.status_code == 200

        # Create key B (new key after revocation)
        r_b = requests.post(
            api_keys_base_url,
            headers=headers,
            json={"name": "test-revoke-remint-b", "subscription": designated},
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert r_b.status_code in (200, 201), f"Failed to create key B: {r_b.text}"
        key_b = r_b.json()["key"]

        # Poll until revoked key A is rejected (revocation may take time to propagate)
        max_wait = 30
        poll_interval = 0.5
        deadline = time.monotonic() + max_wait
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                break
            r_a_inf = requests.post(
                model_completions_url,
                headers={"Authorization": f"Bearer {key_a}", "Content-Type": "application/json"},
                json={"model": inference_model_name, "prompt": "Test", "max_tokens": 5},
                timeout=min(remaining, 10),
                verify=TLS_VERIFY,
            )
            if r_a_inf.status_code == 403:
                break
            time.sleep(poll_interval)
        assert r_a_inf.status_code == 403, (
            f"Revoked key A should be rejected within {max_wait}s, got {r_a_inf.status_code}"
        )

        # Key B should work (200)
        r_b_inf = requests.post(
            model_completions_url,
            headers={"Authorization": f"Bearer {key_b}", "Content-Type": "application/json"},
            json={"model": inference_model_name, "prompt": "Test", "max_tokens": 5},
            timeout=60,
            verify=TLS_VERIFY,
        )
        assert r_b_inf.status_code == 200, f"New key B should work, got {r_b_inf.status_code}: {r_b_inf.text}"
        print("[revoke] Revoked key A rejected, new key B works — remint after revoke succeeds")

    def test_individual_revoke_multiple_keys(self, api_keys_base_url: str, headers: dict):
        """Create multiple keys and revoke each individually — verifies per-key DELETE returns 200."""
        designated = os.environ.get("E2E_SIMULATOR_SUBSCRIPTION", "simulator-subscription")

        # Create 3 keys to revoke
        key_ids = []
        for i in range(3):
            r = requests.post(
                api_keys_base_url,
                headers=headers,
                json={"name": f"test-bulk-api-{i}", "subscription": designated},
                timeout=30,
                verify=TLS_VERIFY,
            )
            assert r.status_code in (200, 201), f"Failed to create key {i}: {r.text}"
            key_ids.append(r.json()["id"])

        # Individually revoke them so we don't nuke the session key
        for kid in key_ids:
            r = requests.delete(f"{api_keys_base_url}/{kid}", headers=headers, timeout=30, verify=TLS_VERIFY)
            assert r.status_code == 200, f"Failed to revoke key {kid}: {r.text}"

        print(f"[bulk-revoke] Individually revoked {len(key_ids)} keys")

    def test_revoke_keys_rejected_at_gateway(
        self,
        api_keys_base_url: str,
        model_completions_url: str,
        headers: dict,
        inference_model_name: str,
    ):
        """After individually revoking keys, they should be rejected at the gateway."""
        designated = os.environ.get("E2E_SIMULATOR_SUBSCRIPTION", "simulator-subscription")

        # Create 3 keys, capturing plaintext and IDs
        keys = []
        for i in range(3):
            r = requests.post(
                api_keys_base_url,
                headers=headers,
                json={"name": f"test-revoke-gw-{i}", "subscription": designated},
                timeout=30,
                verify=TLS_VERIFY,
            )
            assert r.status_code in (200, 201), f"Failed to create key {i}: {r.text}"
            keys.append({"id": r.json()["id"], "key": r.json()["key"]})

        # Smoke-test: verify at least one key works before revocation
        r_smoke = requests.post(
            model_completions_url,
            headers={"Authorization": f"Bearer {keys[0]['key']}", "Content-Type": "application/json"},
            json={"model": inference_model_name, "prompt": "Test", "max_tokens": 5},
            timeout=60,
            verify=TLS_VERIFY,
        )
        assert r_smoke.status_code == 200, (
            f"Key should work before revoke, got {r_smoke.status_code}: {r_smoke.text}"
        )

        # Individually revoke all 3 keys (not bulk-revoke, to avoid nuking session key)
        for k in keys:
            r = requests.delete(f"{api_keys_base_url}/{k['id']}", headers=headers, timeout=30, verify=TLS_VERIFY)
            assert r.status_code == 200, f"Failed to revoke key {k['id']}: {r.text}"

        # Poll until all revoked keys are rejected at the gateway
        max_wait = 30
        poll_interval = 0.5
        for i, k in enumerate(keys):
            deadline = time.monotonic() + max_wait
            while True:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    break
                r_inf = requests.post(
                    model_completions_url,
                    headers={"Authorization": f"Bearer {k['key']}", "Content-Type": "application/json"},
                    json={"model": inference_model_name, "prompt": "Test", "max_tokens": 5},
                    timeout=min(remaining, 10),
                    verify=TLS_VERIFY,
                )
                if r_inf.status_code == 403:
                    break
                time.sleep(poll_interval)
            assert r_inf.status_code == 403, (
                f"Key {i} should be rejected within {max_wait}s after revoke, got {r_inf.status_code}: {r_inf.text}"
            )
        print("[revoke] All 3 keys correctly rejected at gateway after individual revocation")


class TestEphemeralKeyCleanup:
    """Tests for ephemeral API key cleanup (CronJob + internal endpoint).

    Validates that:
    - Ephemeral keys can be created with short expiration
    - The cleanup CronJob exists and is correctly configured
    - Triggering cleanup does not delete active (non-expired) ephemeral keys
    - Cleanup returns a well-formed response with deletedCount

    The cleanup endpoint (POST /internal/v1/api-keys/cleanup) is cluster-internal
    and not exposed on the public Route. These tests trigger it via the CronJob
    mechanism (kubectl create job --from=cronjob/maas-api-key-cleanup) or via
    oc exec into the maas-api pod.

    Environment Variables:
    - DEPLOYMENT_NAMESPACE: Namespace where maas-api is deployed (default: opendatahub)
    """

    @pytest.fixture
    def deployment_namespace(self) -> str:
        return os.environ.get("DEPLOYMENT_NAMESPACE", "opendatahub")

    def test_cronjob_exists_and_configured(self, deployment_namespace: str):
        """Verify the maas-api-key-cleanup CronJob exists with expected configuration."""
        import subprocess as sp

        result = sp.run(
            ["oc", "get", "cronjob", "maas-api-key-cleanup",
             "-n", deployment_namespace, "-o", "json"],
            capture_output=True, text=True,
        )
        if result.returncode != 0:
            pytest.skip(
                f"CronJob maas-api-key-cleanup not found in {deployment_namespace}: "
                f"{result.stderr.strip()}"
            )

        import json as _json
        cj = _json.loads(result.stdout)
        spec = cj["spec"]

        # Verify schedule (every 15 minutes)
        assert spec["schedule"] == "*/15 * * * *", \
            f"Expected schedule '*/15 * * * *', got '{spec['schedule']}'"

        # Verify concurrency policy
        assert spec["concurrencyPolicy"] == "Forbid", \
            "CronJob should use Forbid concurrency policy"

        # Verify the curl command targets the internal cleanup endpoint
        containers = spec["jobTemplate"]["spec"]["template"]["spec"]["containers"]
        assert len(containers) >= 1
        container_spec = containers[0]
        # Command is in the 'command' field (shell script via /bin/sh -c)
        cmd_parts = container_spec.get("command", [])
        cmd_str = " ".join(cmd_parts)
        assert "/internal/v1/api-keys/cleanup" in cmd_str, \
            f"CronJob command should target cleanup endpoint, got: {cmd_str}"

        # Verify security context (non-root, read-only fs)
        sec_ctx = container_spec.get("securityContext", {})
        assert sec_ctx.get("runAsNonRoot", False) is True, \
            "Cleanup container should run as non-root"
        assert sec_ctx.get("readOnlyRootFilesystem", False) is True, \
            "Cleanup container should have read-only root filesystem"

        print(f"[cleanup] CronJob validated: schedule={spec['schedule']}, "
              f"concurrency={spec['concurrencyPolicy']}")

    def test_cleanup_networkpolicy_exists(self, deployment_namespace: str):
        """Verify the cleanup NetworkPolicy exists and restricts cleanup pod access."""
        import subprocess as sp

        result = sp.run(
            ["oc", "get", "networkpolicy", "maas-api-cleanup-restrict",
             "-n", deployment_namespace, "-o", "json"],
            capture_output=True, text=True,
        )
        if result.returncode != 0:
            pytest.skip(
                f"NetworkPolicy maas-api-cleanup-restrict not found in "
                f"{deployment_namespace}: {result.stderr.strip()}"
            )

        import json as _json
        np = _json.loads(result.stdout)
        spec = np["spec"]

        # Verify it targets cleanup pods
        selector = spec.get("podSelector", {}).get("matchLabels", {})
        assert selector.get("app") == "maas-api-cleanup", \
            f"NetworkPolicy should target app=maas-api-cleanup, got: {selector}"

        # Verify policy types include both Egress and Ingress
        policy_types = spec.get("policyTypes", [])
        assert "Egress" in policy_types, "NetworkPolicy should control egress"
        assert "Ingress" in policy_types, "NetworkPolicy should control ingress"

        # Verify ingress is blocked (empty list)
        assert spec.get("ingress") == [] or spec.get("ingress") is None, \
            "Cleanup pods should have no inbound traffic"

        print("[cleanup] NetworkPolicy validated: cleanup pods restricted to maas-api egress only")

    def test_create_ephemeral_key(self, api_keys_base_url: str, headers: dict):
        """Create an ephemeral key and verify it appears in search with includeEphemeral."""
        # Create ephemeral key with short expiration (30 minutes)
        r = requests.post(
            api_keys_base_url,
            headers=headers,
            json={
                "name": "e2e-ephemeral-cleanup-test",
                "ephemeral": True,
                "expiresIn": "30m",
            },
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert r.status_code in (200, 201), \
            f"Expected 200/201 creating ephemeral key, got {r.status_code}: {r.text}"
        data = r.json()
        assert data.get("ephemeral") is True, "Key should be marked as ephemeral"
        key_id = data["id"]
        print(f"[cleanup] Created ephemeral key: id={key_id}, expiresAt={data.get('expiresAt')}")

        # Verify ephemeral key appears in search with includeEphemeral filter
        r_search = requests.post(
            f"{api_keys_base_url}/search",
            headers=headers,
            json={
                "filters": {"status": ["active"], "includeEphemeral": True},
                "pagination": {"limit": 50, "offset": 0},
            },
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert r_search.status_code == 200
        items = r_search.json().get("items") or r_search.json().get("data") or []
        found_ids = [item["id"] for item in items]
        assert key_id in found_ids, \
            f"Ephemeral key {key_id} should appear in search with includeEphemeral=true"

        # Verify ephemeral key is excluded from default search (without includeEphemeral)
        r_default = requests.post(
            f"{api_keys_base_url}/search",
            headers=headers,
            json={
                "filters": {"status": ["active"]},
                "pagination": {"limit": 50, "offset": 0},
            },
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert r_default.status_code == 200
        default_items = r_default.json().get("items") or r_default.json().get("data") or []
        default_ids = [item["id"] for item in default_items]
        assert key_id not in default_ids, \
            "Ephemeral key should be excluded from default search (includeEphemeral defaults to false)"

        print(f"[cleanup] Ephemeral key visibility verified: visible with filter, hidden by default")

    def test_trigger_cleanup_preserves_active_keys(
        self, api_keys_base_url: str, headers: dict, deployment_namespace: str,
    ):
        """Trigger cleanup and verify active ephemeral keys are NOT deleted.

        Creates an ephemeral key, triggers cleanup via oc exec into maas-api pod,
        and asserts the active key survives cleanup (only expired keys beyond the
        30-minute grace period are deleted).
        """
        import subprocess as sp

        # Create an ephemeral key with 1 hour expiration (won't expire during test)
        r = requests.post(
            api_keys_base_url,
            headers=headers,
            json={
                "name": "e2e-cleanup-survival-test",
                "ephemeral": True,
                "expiresIn": "1h",
            },
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert r.status_code in (200, 201), \
            f"Expected 200/201, got {r.status_code}: {r.text}"
        key_id = r.json()["id"]
        print(f"[cleanup] Created ephemeral key for survival test: id={key_id}")

        # Trigger cleanup via oc exec into maas-api pod
        # This calls the internal endpoint directly, same as the CronJob does
        get_pod = sp.run(
            ["oc", "get", "pods", "-n", deployment_namespace,
             "-l", "app.kubernetes.io/name=maas-api",
             "-o", "jsonpath={.items[0].metadata.name}"],
            capture_output=True, text=True,
        )
        if get_pod.returncode != 0 or not get_pod.stdout.strip():
            pytest.skip(
                f"Cannot find maas-api pod in {deployment_namespace}: "
                f"{get_pod.stderr.strip()}"
            )

        pod_name = get_pod.stdout.strip()
        print(f"[cleanup] Triggering cleanup via oc exec into {pod_name}")

        cleanup_result = sp.run(
            ["oc", "exec", pod_name, "-n", deployment_namespace, "--",
             "curl", "-sf", "-X", "POST",
             "http://localhost:8080/internal/v1/api-keys/cleanup"],
            capture_output=True, text=True, timeout=30,
        )

        if cleanup_result.returncode != 0:
            # curl may not be available in the maas-api container; try wget
            cleanup_result = sp.run(
                ["oc", "exec", pod_name, "-n", deployment_namespace, "--",
                 "wget", "-q", "-O-", "--post-data=",
                 "http://localhost:8080/internal/v1/api-keys/cleanup"],
                capture_output=True, text=True, timeout=30,
            )

        if cleanup_result.returncode != 0:
            pytest.skip(
                f"Cannot exec into maas-api pod to trigger cleanup "
                f"(neither curl nor wget available): {cleanup_result.stderr.strip()}"
            )

        import json as _json
        cleanup_resp = _json.loads(cleanup_result.stdout)
        deleted_count = cleanup_resp.get("deletedCount", -1)
        assert deleted_count >= 0, \
            f"Cleanup response should have non-negative deletedCount, got: {cleanup_resp}"
        print(f"[cleanup] Cleanup completed: deletedCount={deleted_count}, "
              f"message={cleanup_resp.get('message')}")

        # Verify our active ephemeral key survived cleanup
        r_get = requests.get(
            f"{api_keys_base_url}/{key_id}",
            headers=headers,
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert r_get.status_code == 200, \
            f"Active ephemeral key {key_id} should survive cleanup, got {r_get.status_code}"
        assert r_get.json().get("status") == "active", \
            f"Key should still be active after cleanup, got: {r_get.json().get('status')}"
        print(f"[cleanup] Active ephemeral key {key_id} survived cleanup (correct behavior)")
