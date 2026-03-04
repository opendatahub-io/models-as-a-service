import logging
import pytest
import requests
import time

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
            verify=False,
        )
        assert r.status_code == 201, f"Expected 201, got {r.status_code}: {r.text}"
        data = r.json()

        # Verify response structure
        assert "id" in data and "key" in data and "name" in data
        key = data["key"]
        assert key.startswith("sk-oai-"), f"Expected sk-oai- prefix, got: {key[:20]}"
        assert len(key) > len("sk-oai-"), "Key body should not be empty"
        # Note: status field not included in create response (only in list/get)

        print(f"[create] Created key id={data['id']}, key prefix={key[:15]}...")

        # Verify plaintext key is NOT returned on subsequent GET
        r_get = requests.get(f"{api_keys_base_url}/{data['id']}", headers=headers, timeout=30, verify=False)
        assert r_get.status_code == 200
        assert "key" not in r_get.json(), "Plaintext key should not be in GET (show-once pattern)"

    def test_list_api_keys(self, api_keys_base_url: str, headers: dict):
        """Test 2: List own keys - verify basic functionality."""
        # Create two keys
        r1 = requests.post(api_keys_base_url, headers=headers, json={"name": "test-key-list-1"}, timeout=30, verify=False)
        assert r1.status_code == 201
        key1_id = r1.json()["id"]

        r2 = requests.post(api_keys_base_url, headers=headers, json={"name": "test-key-list-2"}, timeout=30, verify=False)
        assert r2.status_code == 201
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
            verify=False
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
            verify=False
        )
        assert r_limit.status_code == 200
        limited_items = (r_limit.json().get("items") or r_limit.json().get("data") or [])
        assert len(limited_items) <= 1
        print(f"[list] Pagination works: limit=1 returned {len(limited_items)} items")

    def test_revoke_api_key(self, api_keys_base_url: str, headers: dict):
        """Test 3: Revoke key - verify status change to 'revoked'."""
        # Create a key
        r_create = requests.post(api_keys_base_url, headers=headers, json={"name": "test-key-revoke"}, timeout=30, verify=False)
        assert r_create.status_code == 201
        key_id = r_create.json()["id"]

        # Revoke it using DELETE
        r = requests.delete(f"{api_keys_base_url}/{key_id}", headers=headers, timeout=30, verify=False)
        assert r.status_code == 200
        assert r.json().get("status") == "revoked"

        # Verify GET shows revoked status
        r_get = requests.get(f"{api_keys_base_url}/{key_id}", headers=headers, timeout=30, verify=False)
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
        r_create = requests.post(api_keys_base_url, headers=headers, json={"name": "regular-user-key"}, timeout=30, verify=False)
        assert r_create.status_code == 201
        user_key_id = r_create.json()["id"]

        # Get username
        r_get = requests.get(f"{api_keys_base_url}/{user_key_id}", headers=headers, timeout=30, verify=False)
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
            verify=False
        )
        assert r_admin.status_code == 200
        items = r_admin.json().get("items") or r_admin.json().get("data") or []
        key_ids = [item["id"] for item in items]
        assert user_key_id in key_ids
        print(f"[admin] Admin listed {len(items)} keys for '{username}'")

        # Admin revokes user's key using DELETE
        r_revoke = requests.delete(f"{api_keys_base_url}/{user_key_id}", headers=admin_headers, timeout=30, verify=False)
        assert r_revoke.status_code == 200
        assert r_revoke.json().get("status") == "revoked"
        print(f"[admin] Admin successfully revoked user's key {user_key_id}")

    def test_non_admin_cannot_access_other_users_keys(self, api_keys_base_url: str, headers: dict, admin_headers: dict):
        """Test 5: Non-admin cannot access other user's keys - verify 403."""
        if not admin_headers:
            pytest.skip("ADMIN_OC_TOKEN not set")

        # Admin creates a key
        r_admin = requests.post(api_keys_base_url, headers=admin_headers, json={"name": "admin-only-key"}, timeout=30, verify=False)
        assert r_admin.status_code == 201
        admin_key_id = r_admin.json()["id"]

        # Regular user tries to GET admin's key
        r_get = requests.get(f"{api_keys_base_url}/{admin_key_id}", headers=headers, timeout=30, verify=False)
        assert r_get.status_code == 403, f"Expected 403, got {r_get.status_code}"

        # Regular user tries to revoke admin's key using DELETE
        r_revoke = requests.delete(f"{api_keys_base_url}/{admin_key_id}", headers=headers, timeout=30, verify=False)
        assert r_revoke.status_code == 403, f"Expected 403, got {r_revoke.status_code}"
        print("[authz] Non-admin correctly got 403 for admin's key")


class TestAPIKeyValidation:
    """Tests 6-7: Internal validation endpoint."""

    def test_validate_active_key(self, api_keys_base_url: str, api_keys_validation_url: str, headers: dict):
        """Test 6: Validate active key - verify returns valid:true with username."""
        # Create a key
        r = requests.post(api_keys_base_url, headers=headers, json={"name": "test-validate-active"}, timeout=30, verify=False)
        assert r.status_code == 201
        key = r.json()["key"]

        # Validate it
        r_val = requests.post(
            api_keys_validation_url,
            headers={"Content-Type": "application/json"},
            json={"key": key},
            timeout=30,
            verify=False,
        )
        assert r_val.status_code == 200
        val_data = r_val.json()
        assert val_data.get("valid") is True
        assert "username" in val_data or "user" in val_data
        print(f"[validate] Active key valid, user={val_data.get('username') or val_data.get('user')}")

    def test_validate_revoked_key(self, api_keys_base_url: str, api_keys_validation_url: str, headers: dict):
        """Test 7: Validate revoked key - verify returns valid:false."""
        # Create and revoke a key
        r = requests.post(api_keys_base_url, headers=headers, json={"name": "test-validate-revoked"}, timeout=30, verify=False)
        assert r.status_code == 201
        data = r.json()
        key, key_id = data["key"], data["id"]

        requests.delete(f"{api_keys_base_url}/{key_id}", headers=headers, timeout=30, verify=False)
        time.sleep(1)  # Allow revocation to propagate

        # Validate revoked key
        r_val = requests.post(
            api_keys_validation_url,
            headers={"Content-Type": "application/json"},
            json={"key": key},
            timeout=30,
            verify=False,
        )

        # Should return 200 with valid:false or 401/403
        if r_val.status_code == 200:
            assert r_val.json().get("valid") is False
            print("[validate] Revoked key returned valid=false")
        else:
            assert r_val.status_code in (401, 403)
            print(f"[validate] Revoked key returned {r_val.status_code}")


class TestAPIKeyBulkOperations:
    """Tests for bulk operations like bulk-revoke."""

    def test_bulk_revoke_own_keys(self, api_keys_base_url: str, headers: dict):
        """Test 8: Bulk revoke - user can bulk revoke their own keys."""
        # Create multiple keys
        key_ids = []
        for i in range(3):
            r = requests.post(api_keys_base_url, headers=headers, json={"name": f"bulk-test-{i}"}, timeout=30, verify=False)
            assert r.status_code == 201
            key_ids.append(r.json()["id"])

        # Get username from one of the keys
        r_get = requests.get(f"{api_keys_base_url}/{key_ids[0]}", headers=headers, timeout=30, verify=False)
        username = r_get.json().get("username") or r_get.json().get("owner")
        assert username

        # Bulk revoke all keys for this user
        r_bulk = requests.post(
            f"{api_keys_base_url}/bulk-revoke",
            headers=headers,
            json={"username": username},
            timeout=30,
            verify=False
        )
        assert r_bulk.status_code == 200
        data = r_bulk.json()
        assert data.get("revokedCount") >= 3, f"Expected at least 3 revoked, got {data.get('revokedCount')}"
        print(f"[bulk-revoke] Successfully revoked {data.get('revokedCount')} keys for user {username}")

        # Verify keys are revoked
        for key_id in key_ids:
            r_check = requests.get(f"{api_keys_base_url}/{key_id}", headers=headers, timeout=30, verify=False)
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
            verify=False
        )
        assert r_bulk.status_code == 403, f"Expected 403, got {r_bulk.status_code}: {r_bulk.text}"
        print("[bulk-revoke] Non-admin correctly got 403 when trying to bulk revoke other user's keys")

    def test_bulk_revoke_admin_can_revoke_any_user(self, api_keys_base_url: str, headers: dict, admin_headers: dict):
        """Test 10: Bulk revoke - admin can bulk revoke any user's keys."""
        if not admin_headers:
            pytest.skip("ADMIN_OC_TOKEN not set")

        # Create a key as regular user
        r = requests.post(api_keys_base_url, headers=headers, json={"name": "admin-bulk-revoke-test"}, timeout=30, verify=False)
        assert r.status_code == 201
        key_id = r.json()["id"]

        # Get username
        r_get = requests.get(f"{api_keys_base_url}/{key_id}", headers=headers, timeout=30, verify=False)
        username = r_get.json().get("username") or r_get.json().get("owner")
        assert username

        # Admin bulk revokes user's keys
        r_bulk = requests.post(
            f"{api_keys_base_url}/bulk-revoke",
            headers=admin_headers,
            json={"username": username},
            timeout=30,
            verify=False
        )
        assert r_bulk.status_code == 200
        data = r_bulk.json()
        assert data.get("revokedCount") >= 1
        print(f"[bulk-revoke] Admin successfully revoked {data.get('revokedCount')} keys for user {username}")
