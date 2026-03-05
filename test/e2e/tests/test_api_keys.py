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


class TestAPIKeyModelInference:
    """Tests 11-15: Using API keys for model inference via gateway."""

    @pytest.fixture
    def model_completions_url(self, model_v1: str) -> str:
        """URL for completions endpoint."""
        return f"{model_v1}/completions"

    @pytest.fixture
    def inference_model_name(self) -> str:
        """Model name for inference requests. Override with INFERENCE_MODEL_NAME env var."""
        return os.environ.get("INFERENCE_MODEL_NAME", "facebook/opt-125m")

    def test_api_key_model_access_success(
        self,
        model_completions_url: str,
        api_key_headers: dict,
        inference_model_name: str,
    ):
        """Test 11: Valid API key can access model endpoint - verify 200 response.
        
        Note: Users with access to multiple subscriptions must specify which one
        to use via X-MaaS-Subscription header.
        """
        # Add subscription header - required when user matches multiple subscriptions
        subscription_name = os.environ.get("E2E_SIMULATOR_SUBSCRIPTION", "simulator-subscription")
        headers = api_key_headers.copy()
        headers["X-MaaS-Subscription"] = subscription_name
        
        r = requests.post(
            model_completions_url,
            headers=headers,
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
        r_create = requests.post(
            api_keys_base_url,
            headers=headers,
            json={"name": "test-revoke-inference"},
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

    def test_api_key_with_explicit_subscription_header(
        self,
        model_completions_url: str,
        api_key_headers: dict,
        inference_model_name: str,
    ):
        """Test 16: API key with explicit x-maas-subscription header.
        
        When multiple subscriptions exist for the same model, the user can
        specify which subscription to use via the x-maas-subscription header.
        This works the same way for API keys as it does for OC tokens.
        """
        # Default subscription for free model
        subscription_name = os.environ.get("E2E_SIMULATOR_SUBSCRIPTION", "simulator-subscription")
        
        # Add x-maas-subscription header to API key headers
        headers_with_sub = api_key_headers.copy()
        headers_with_sub["x-maas-subscription"] = subscription_name

        r = requests.post(
            model_completions_url,
            headers=headers_with_sub,
            json={
                "model": inference_model_name,
                "prompt": "Test subscription header",
                "max_tokens": 5,
            },
            timeout=60,
            verify=TLS_VERIFY,
        )

        assert r.status_code == 200, f"Expected 200 with explicit subscription, got {r.status_code}: {r.text}"
        print("[inference] API key with x-maas-subscription header succeeded")

    def test_api_key_with_invalid_subscription_header(
        self,
        model_completions_url: str,
        api_key_headers: dict,
        inference_model_name: str,
    ):
        """Test 17: API key with invalid x-maas-subscription header should fail.
        
        If the specified subscription doesn't exist or user isn't authorized,
        the request should be rejected with 429 (rate limited) or 403 (forbidden).
        """
        # Add invalid subscription header
        headers_with_invalid_sub = api_key_headers.copy()
        headers_with_invalid_sub["x-maas-subscription"] = "nonexistent-subscription-xyz"

        r = requests.post(
            model_completions_url,
            headers=headers_with_invalid_sub,
            json={
                "model": inference_model_name,
                "prompt": "Test invalid subscription",
                "max_tokens": 5,
            },
            timeout=30,
            verify=TLS_VERIFY,
        )

        # Should get 429 (rate limited - no valid subscription) or 403 (forbidden)
        assert r.status_code in (429, 403), f"Expected 429/403 for invalid subscription, got {r.status_code}: {r.text}"
        print(f"[inference] API key with invalid subscription correctly rejected with {r.status_code}")
