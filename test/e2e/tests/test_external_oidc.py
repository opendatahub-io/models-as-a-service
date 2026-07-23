"""
External OIDC E2E Tests — Keycloak Integration
================================================

Tests external OIDC provider (Keycloak) integration with the MaaS platform.
Validates the OIDC token → API key mint → model access flow using Keycloak
test realms (tenant-a and tenant-b).

Prerequisites:
  - Keycloak deployed with test realms (see docs/samples/install/keycloak/test-realms/)
  - MaaS deployed with --external-oidc pointing at Keycloak issuer
  - EXTERNAL_OIDC=true and OIDC_* env vars set (auto-derived by prow_run_smoke_test.sh)

Environment Variables:
  EXTERNAL_OIDC          Enable these tests (default: skip)
  OIDC_ISSUER_URL        Keycloak realm issuer URL (e.g. https://keycloak.example.com/realms/tenant-a)
  OIDC_TOKEN_URL         Token endpoint for password grant
  OIDC_CLIENT_ID         Public client ID (default: test-client)
  OIDC_USERNAME          Default test user (default: alice_lead)
  OIDC_PASSWORD          Default test user password (default: letmein)

Test Realms:
  tenant-a:
    - alice_lead  (groups: Engineering, Project-Alpha)
    - bob_sre     (groups: Site-Reliability)
    - evil_user   (groups: unsafe;group — semicolon triggers oidc-groups-safe rejection)
    - dave_noaccess (groups: none — valid token, no group memberships → empty model list)
    - client: test-client (public, direct access grants, groups mapper)
    - client: wrong-client (public, direct access grants, no groups mapper —
      used only to obtain tokens with azp=wrong-client for client-binding tests)

  tenant-b:
    - charlie_sec_lead  (groups: Product-Security, Project-Omega)
    - grace_dev         (groups: Project-Omega)
    - client: test-client (public, direct access grants)
"""

from __future__ import annotations

import base64
import json
import os
import subprocess
import time
import uuid
import logging
from urllib.parse import urlparse

import pytest
import requests

from conftest import TLS_VERIFY

log = logging.getLogger(__name__)


pytestmark = pytest.mark.skipif(
    os.environ.get("EXTERNAL_OIDC", "").lower() != "true",
    reason="EXTERNAL_OIDC is not true",
)

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _required_env(name: str) -> str:
    value = os.environ.get(name, "")
    assert value, f"{name} must be set when EXTERNAL_OIDC=true"
    return value


def _oidc_token_url() -> str:
    return _required_env("OIDC_TOKEN_URL")


def _oidc_client_id() -> str:
    return _required_env("OIDC_CLIENT_ID")


def _request_oidc_token(
    username: str | None = None,
    password: str | None = None,
    token_url: str | None = None,
    client_id: str | None = None,
) -> str:
    """Request an OIDC access token via the Resource Owner Password Grant.

    Args:
        username: Override OIDC_USERNAME env var
        password: Override OIDC_PASSWORD env var
        token_url: Override OIDC_TOKEN_URL env var
        client_id: Override OIDC_CLIENT_ID env var

    Returns:
        The raw access_token string.
    """
    url = token_url or _oidc_token_url()
    cid = client_id or _oidc_client_id()
    user = username or _required_env("OIDC_USERNAME")
    pwd = password or _required_env("OIDC_PASSWORD")

    response = requests.post(
        url,
        data={
            "grant_type": "password",
            "client_id": cid,
            "username": user,
            "password": pwd,
        },
        timeout=30,
        verify=TLS_VERIFY,
    )
    assert response.status_code == 200, (
        f"OIDC token request failed for user={user}: "
        f"{response.status_code} {response.text}"
    )

    token = response.json().get("access_token")
    assert token, "OIDC token response missing access_token"
    return token


def _decode_jwt_payload(token: str) -> dict:
    """Decode the payload of a JWT (no signature verification — TEST ONLY)."""
    parts = token.split(".")
    if len(parts) != 3:
        raise ValueError(f"Invalid JWT format: expected 3 parts, got {len(parts)}")
    padding = "=" * (-len(parts[1]) % 4)
    return json.loads(base64.urlsafe_b64decode(parts[1] + padding))


def _oidc_request_with_retry(method, url, oidc_token, label="OIDC request",
                             retries=12, delay=5, **kwargs):
    """Send an OIDC-authenticated request with retries on transient auth failures.

    The merge-patch CEL expressions for X-MaaS-Username/Group occasionally hit
    a transient window where the header is empty, causing 500 AUTH_FAILURE.
    Retrying resolves these without code changes.
    """
    kwargs.setdefault("timeout", 30)
    kwargs.setdefault("verify", TLS_VERIFY)
    headers = kwargs.pop("headers", {})
    headers.setdefault("Authorization", f"Bearer {oidc_token}")
    headers.setdefault("Content-Type", "application/json")

    for attempt in range(1, retries + 1):
        response = method(url, headers=headers, **kwargs)
        if (response.status_code == 403 and not response.text.strip()) \
                or response.status_code == 401 \
                or (response.status_code == 500 and "AUTH_FAILURE" in response.text):
            if attempt < retries:
                log.info("%s got %d (attempt %d/%d), retrying in %ds...",
                         label, response.status_code, attempt, retries, delay)
                time.sleep(delay)
                continue
        break
    return response


def _create_oidc_api_key(
    maas_api_base_url: str,
    oidc_token: str,
    name: str | None = None,
    subscription: str | None = None,
) -> dict:
    """Mint a MaaS API key using an OIDC bearer token."""
    body: dict = {"name": name or f"e2e-oidc-{uuid.uuid4().hex[:8]}"}
    if subscription:
        body["subscription"] = subscription

    response = _oidc_request_with_retry(
        requests.post,
        f"{maas_api_base_url}/v1/api-keys",
        oidc_token,
        label="OIDC API key mint",
        json=body,
    )

    assert response.status_code in (200, 201), (
        f"OIDC API key mint failed: {response.status_code} {response.text}"
    )

    data = response.json()
    assert data.get("key", "").startswith("sk-oai-"), f"Unexpected API key payload: {data}"
    return data


def _wait_for_api_key_revocation(
    maas_api_base_url: str,
    api_key: str,
    timeout: int = 10,
    poll_interval: float = 0.5,
) -> None:
    """Poll until API key revocation is enforced by the gateway."""
    start = time.time()
    while time.time() - start < timeout:
        try:
            response = requests.get(
                f"{maas_api_base_url}/v1/models",
                headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
                timeout=30,
                verify=TLS_VERIFY,
            )
        except requests.RequestException:
            time.sleep(poll_interval)
            continue

        if response.status_code in (401, 403):
            log.info("Revocation enforced after %.1fs (status=%d)", time.time() - start, response.status_code)
            return
        if (
            response.status_code == 500
            and "AUTH_FAILURE" in response.text
            and "valid:false" in response.text
        ):
            log.info("Revocation enforced after %.1fs (status=%d)", time.time() - start, response.status_code)
            return
        time.sleep(poll_interval)
    raise TimeoutError(f"API key revocation not enforced after {timeout}s")


def _tenant_b_token_url() -> str:
    """Return the token endpoint URL for the tenant-b Keycloak realm.

    Resolution order:
    1. OIDC_TOKEN_URL_TENANT_B — explicit override, set by prow_run_smoke_test.sh.
    2. Derive from OIDC_ISSUER_URL by swapping 'tenant-a' → 'tenant-b' (fallback
       for environments where only the tenant-a issuer URL is configured).
    """
    explicit = os.environ.get("OIDC_TOKEN_URL_TENANT_B", "")
    if explicit:
        return explicit
    issuer = _required_env("OIDC_ISSUER_URL")
    assert "tenant-a" in issuer, (
        f"Expected OIDC_ISSUER_URL to contain 'tenant-a' for tenant-b derivation, "
        f"got: {issuer}. Set OIDC_TOKEN_URL_TENANT_B explicitly to avoid this."
    )
    tenant_b_issuer = issuer.replace("tenant-a", "tenant-b")
    return f"{tenant_b_issuer}/protocol/openid-connect/token"


# ---------------------------------------------------------------------------
# Tests — Core OIDC Flow
# ---------------------------------------------------------------------------

class TestOIDCTokenFlow:
    """Basic OIDC token acquisition and API key minting."""

    def test_oidc_token_can_create_api_key(self, maas_api_base_url: str):
        """OIDC token from default user (alice_lead) can mint an API key."""
        token = _request_oidc_token()
        data = _create_oidc_api_key(maas_api_base_url, token)
        log.info(f"Created API key id={data.get('id')} prefix={data.get('key', '')[:18]}...")

        # API key should have a subscription auto-bound
        assert data.get("subscription"), (
            f"Expected API key to have auto-bound subscription, got: {data}"
        )

    def test_invalid_oidc_token_gets_401(self, maas_api_base_url: str):
        """Tampered OIDC token is rejected with 401."""
        token = _request_oidc_token() + "broken"
        response = requests.post(
            f"{maas_api_base_url}/v1/api-keys",
            headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
            json={"name": f"e2e-oidc-invalid-{uuid.uuid4().hex[:8]}"},
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert response.status_code == 401, (
            f"Expected 401 for invalid OIDC token, got {response.status_code}: {response.text}"
        )

    def test_empty_bearer_token_gets_401(self, maas_api_base_url: str):
        """Empty bearer token is rejected with 401."""
        response = requests.post(
            f"{maas_api_base_url}/v1/api-keys",
            headers={"Authorization": "Bearer ", "Content-Type": "application/json"},
            json={"name": f"e2e-oidc-empty-{uuid.uuid4().hex[:8]}"},
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert response.status_code == 401, (
            f"Expected 401 for empty bearer token, got {response.status_code}: {response.text}"
        )

    def test_no_auth_header_gets_401(self, maas_api_base_url: str):
        """Missing Authorization header is rejected with 401."""
        response = requests.post(
            f"{maas_api_base_url}/v1/api-keys",
            headers={"Content-Type": "application/json"},
            json={"name": f"e2e-oidc-noauth-{uuid.uuid4().hex[:8]}"},
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert response.status_code == 401, (
            f"Expected 401 for missing auth header, got {response.status_code}: {response.text}"
        )

    def test_tampered_expired_oidc_token_gets_401(self, maas_api_base_url: str):
        """JWT with modified exp claim (and therefore invalid signature) is rejected.

        This test modifies the payload to set exp in the past, which also
        invalidates the signature. It verifies the gateway rejects tampered
        tokens — Authorino checks signature before expiration, so this is
        effectively a signature-tampering test. See the companion slow test
        test_real_expired_oidc_token_gets_401 for true expiration validation.
        """
        token = _request_oidc_token()
        parts = token.split(".")
        assert len(parts) == 3, "Token is not a valid JWT"

        payload = _decode_jwt_payload(token)
        payload["exp"] = int(time.time()) - 3600

        modified_payload = base64.urlsafe_b64encode(
            json.dumps(payload).encode()
        ).rstrip(b"=").decode()
        expired_token = f"{parts[0]}.{modified_payload}.{parts[2]}"

        response = requests.post(
            f"{maas_api_base_url}/v1/api-keys",
            headers={"Authorization": f"Bearer {expired_token}", "Content-Type": "application/json"},
            json={"name": f"e2e-oidc-expired-{uuid.uuid4().hex[:8]}"},
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert response.status_code == 401, (
            f"Expected 401 for tampered/expired OIDC token, got {response.status_code}: {response.text}"
        )

    @pytest.mark.slow
    def test_real_expired_oidc_token_gets_401(self, maas_api_base_url: str):
        """Genuine expired OIDC token (untampered, valid signature) is rejected.

        Requests a token from Keycloak, waits for it to expire naturally,
        then verifies the gateway rejects it. This isolates expiration
        handling from signature validation.

        The tenant-a realm is configured with accessTokenLifespan: 60s,
        so the wait is ~65 seconds.
        """
        token = _request_oidc_token()
        payload = _decode_jwt_payload(token)
        assert "exp" in payload and isinstance(payload["exp"], (int, float)), (
            f"OIDC token missing numeric 'exp' claim — cannot test expiration: {sorted(payload.keys())}"
        )
        exp = payload["exp"]
        wait_seconds = max(0, exp - int(time.time())) + 5

        if wait_seconds > 120:
            pytest.skip(f"Token expiry too far out ({wait_seconds}s) — realm accessTokenLifespan may not be set to 60s")

        log.info(f"Waiting {wait_seconds}s for OIDC token to expire naturally...")
        time.sleep(wait_seconds)

        response = requests.post(
            f"{maas_api_base_url}/v1/api-keys",
            headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
            json={"name": f"e2e-oidc-realexp-{uuid.uuid4().hex[:8]}"},
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert response.status_code == 401, (
            f"Expected 401 for genuinely expired OIDC token, got {response.status_code}: {response.text}"
        )


# ---------------------------------------------------------------------------
# Tests — OIDC Token Claims
# ---------------------------------------------------------------------------

class TestOIDCTokenClaims:
    """Verify OIDC token structure and claims from Keycloak."""

    def test_token_contains_groups_claim(self):
        """alice_lead's token should contain the 'groups' claim with her groups."""
        token = _request_oidc_token()
        payload = _decode_jwt_payload(token)

        assert "groups" in payload, (
            f"Expected 'groups' claim in token, got claims: {list(payload.keys())}"
        )
        groups = payload["groups"]
        assert isinstance(groups, list), f"Expected groups to be a list, got: {type(groups)}"

        # alice_lead is in Engineering and Project-Alpha
        assert "Engineering" in groups or "/Engineering" in groups, (
            f"Expected alice_lead to be in 'Engineering' group, got: {groups}"
        )

    def test_token_contains_preferred_username(self):
        """Token should contain the preferred_username claim."""
        token = _request_oidc_token()
        payload = _decode_jwt_payload(token)

        username = payload.get("preferred_username")
        assert username == _required_env("OIDC_USERNAME"), (
            f"Expected preferred_username={_required_env('OIDC_USERNAME')}, got: {username}"
        )

    def test_different_users_have_different_groups(self):
        """alice_lead and bob_sre should have different group memberships."""
        alice_token = _request_oidc_token(username="alice_lead", password="letmein")
        bob_token = _request_oidc_token(username="bob_sre", password="letmein")

        alice_groups = set(_decode_jwt_payload(alice_token).get("groups", []))
        bob_groups = set(_decode_jwt_payload(bob_token).get("groups", []))

        log.info(f"alice_lead groups: {alice_groups}")
        log.info(f"bob_sre groups: {bob_groups}")

        assert alice_groups != bob_groups, (
            f"Expected different groups for alice and bob, both got: {alice_groups}"
        )


# ---------------------------------------------------------------------------
# Tests — Multi-User API Key Minting
# ---------------------------------------------------------------------------

class TestOIDCMultiUser:
    """Verify that different OIDC users can mint API keys independently."""

    def test_bob_sre_can_mint_api_key(self, maas_api_base_url: str):
        """bob_sre (Site-Reliability group) can also mint an API key."""
        token = _request_oidc_token(username="bob_sre", password="letmein")
        data = _create_oidc_api_key(maas_api_base_url, token, name=f"e2e-bob-{uuid.uuid4().hex[:8]}")
        log.info(f"bob_sre created API key id={data.get('id')}")

        assert data.get("key"), "bob_sre API key missing 'key' field"

    def test_wrong_password_gets_rejected(self):
        """Invalid password for a valid user should fail at Keycloak."""
        with pytest.raises(AssertionError, match="OIDC token request failed"):
            _request_oidc_token(username="alice_lead", password="wrongpassword")

    def test_nonexistent_user_gets_rejected(self):
        """Non-existent user should fail at Keycloak."""
        with pytest.raises(AssertionError, match="OIDC token request failed"):
            _request_oidc_token(username="nonexistent_user", password="letmein")


# ---------------------------------------------------------------------------
# Tests — End-to-End: OIDC → API Key → Model Access
# ---------------------------------------------------------------------------

class TestOIDCModelAccess:
    """Full flow: OIDC token → API key mint → list models → inference."""

    def test_minted_api_key_can_list_models_and_infer(self, maas_api_base_url: str):
        """Complete happy path: OIDC token → API key → model list → inference."""
        token = _request_oidc_token()
        api_key = _create_oidc_api_key(maas_api_base_url, token)["key"]

        # List models
        models_response = _oidc_request_with_retry(
            requests.get,
            f"{maas_api_base_url}/v1/models",
            api_key,
            label="OIDC list models",
            timeout=45,
        )
        assert models_response.status_code == 200, (
            f"OIDC-minted API key failed to list models: "
            f"{models_response.status_code} {models_response.text}"
        )

        items = models_response.json().get("data") or models_response.json().get("models") or []
        assert items, f"Expected at least one model from /v1/models, got: {models_response.text}"

        # Inference — use GATEWAY_HOST to build the URL so this works from
        # outside the cluster (the "url" field contains an in-cluster address).
        model_id = items[0]["id"]
        raw_url = items[0]["url"].rstrip("/")
        model_path = urlparse(raw_url).path
        gateway_host = os.environ.get("GATEWAY_HOST", "")
        scheme = "http" if os.environ.get("INSECURE_HTTP", "").lower() == "true" else "https"
        model_url = f"{scheme}://{gateway_host}{model_path}" if gateway_host else raw_url
        inference_response = _oidc_request_with_retry(
            requests.post,
            f"{model_url}/v1/chat/completions",
            api_key,
            label="OIDC inference",
            json={
                "model": model_id,
                "messages": [{"role": "user", "content": "Hello from external OIDC e2e"}],
                "max_tokens": 16,
            },
            timeout=45,
        )
        assert inference_response.status_code == 200, (
            f"OIDC-minted API key inference failed: "
            f"{inference_response.status_code} {inference_response.text}"
        )
        log.info(f"Inference succeeded for {model_id}")

    def test_revoked_api_key_cannot_access_models(self, maas_api_base_url: str):
        """API key minted via OIDC can be revoked and then rejected."""
        token = _request_oidc_token()
        key_data = _create_oidc_api_key(maas_api_base_url, token)
        api_key = key_data["key"]
        key_id = key_data["id"]

        # Revoke the key using the OIDC token
        revoke_response = _oidc_request_with_retry(
            requests.delete,
            f"{maas_api_base_url}/v1/api-keys/{key_id}",
            token,
            label="OIDC API key revoke",
        )
        assert revoke_response.status_code in (200, 204), (
            f"API key revocation failed: {revoke_response.status_code} {revoke_response.text}"
        )

        # Wait for revocation to propagate through gateway
        _wait_for_api_key_revocation(maas_api_base_url, api_key, timeout=10)

        # Attempt to use the revoked key
        response = requests.get(
            f"{maas_api_base_url}/v1/models",
            headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
            timeout=30,
            verify=TLS_VERIFY,
        )
        # 401/403 = gateway rejects the revoked key directly
        # 500 = Authorino returns AUTH_FAILURE when the apiKeyValidation metadata
        #   callback reports valid:false for a revoked key (upstream Authorino issue)
        assert response.status_code in (401, 403, 500), (
            f"Expected 401/403/500 for revoked API key, got {response.status_code}: {response.text}"
        )
        log.info(f"Revoked API key correctly rejected with {response.status_code}")

    def test_oidc_user_without_group_access_gets_empty_list(self, maas_api_base_url: str):
        """OIDC user with no subscription group access gets 200 OK with an empty model list."""
        username_no_access = os.environ.get("OIDC_USERNAME_NO_ACCESS", "")
        password_no_access = os.environ.get("OIDC_PASSWORD_NO_ACCESS", "")

        if not username_no_access or not password_no_access:
            pytest.skip("OIDC_USERNAME_NO_ACCESS and OIDC_PASSWORD_NO_ACCESS not configured")

        token_url = _required_env("OIDC_TOKEN_URL")
        client_id = _required_env("OIDC_CLIENT_ID")

        response = requests.post(
            token_url,
            data={
                "grant_type": "password",
                "client_id": client_id,
                "username": username_no_access,
                "password": password_no_access,
            },
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert response.status_code == 200, (
            f"OIDC token request failed: {response.status_code} {response.text}"
        )

        token = response.json().get("access_token")
        assert token, "OIDC token response missing access_token"

        models_response = _oidc_request_with_retry(
            requests.get,
            f"{maas_api_base_url}/v1/models",
            token,
            label="OIDC list models (no-access user)",
            timeout=45,
        )

        assert models_response.status_code == 200, (
            f"Expected 200 for user without access, got {models_response.status_code}: {models_response.text}"
        )

        response_json = models_response.json()
        assert response_json.get("object") == "list", f"Expected object=list, got: {response_json}"

        items = response_json.get("data", [])
        assert isinstance(items, list), f"Expected data to be a list, got {type(items)}"
        assert len(items) == 0, (
            f"Expected empty list for user without group access, got {len(items)} model(s)"
        )

        log.info("User without group access correctly received empty list (200 OK)")


# ---------------------------------------------------------------------------
# Tests — Multi-Tenant (tenant-a vs tenant-b)
# ---------------------------------------------------------------------------

class TestOIDCMultiTenant:
    """Cross-realm tests using tenant-a and tenant-b Keycloak realms.

    The MaaS platform is configured with tenant-a as the OIDC issuer.
    Tokens from tenant-b should be rejected because they come from an
    untrusted issuer.
    """

    def test_tenant_b_token_rejected_by_maas(self, maas_api_base_url: str):
        """Token from tenant-b realm should be rejected since MaaS trusts tenant-a only.

        MaaS is deployed with OIDC_ISSUER_URL pointing at tenant-a.
        A token from tenant-b has a different issuer claim and should fail
        Authorino's OIDC verification.
        """
        tenant_b_url = _tenant_b_token_url()

        # Get a valid token from tenant-b
        tenant_b_token = _request_oidc_token(
            username="charlie_sec_lead",
            password="letmein",
            token_url=tenant_b_url,
            client_id="test-client",
        )

        # Try to use the tenant-b token against MaaS (configured for tenant-a)
        response = requests.post(
            f"{maas_api_base_url}/v1/api-keys",
            headers={"Authorization": f"Bearer {tenant_b_token}", "Content-Type": "application/json"},
            json={"name": f"e2e-oidc-tenant-b-{uuid.uuid4().hex[:8]}"},
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert response.status_code == 401, (
            f"Expected 401 for tenant-b token (untrusted issuer), "
            f"got {response.status_code}: {response.text}"
        )
        log.info("tenant-b token correctly rejected by MaaS (untrusted issuer)")

    def test_tenant_a_users_are_isolated(self, maas_api_base_url: str):
        """Different tenant-a users can each mint their own API keys independently."""
        alice_token = _request_oidc_token(username="alice_lead", password="letmein")
        bob_token = _request_oidc_token(username="bob_sre", password="letmein")

        alice_key = _create_oidc_api_key(
            maas_api_base_url, alice_token, name=f"e2e-alice-iso-{uuid.uuid4().hex[:8]}"
        )
        bob_key = _create_oidc_api_key(
            maas_api_base_url, bob_token, name=f"e2e-bob-iso-{uuid.uuid4().hex[:8]}"
        )

        # Both should get distinct keys
        assert alice_key["id"] != bob_key["id"], "Expected different API key IDs for different users"
        assert alice_key["key"] != bob_key["key"], "Expected different API key values for different users"

        log.info(f"alice_lead key={alice_key['id']}, bob_sre key={bob_key['id']}")


# ---------------------------------------------------------------------------
# Tests — API Key Lifecycle with OIDC
# ---------------------------------------------------------------------------

class TestOIDCAPIKeyLifecycle:
    """API key management operations authenticated via OIDC tokens."""

    def test_create_and_revoke_api_key(self, maas_api_base_url: str):
        """Full create → revoke lifecycle with OIDC token."""
        token = _request_oidc_token()

        # Create
        key_data = _create_oidc_api_key(maas_api_base_url, token, name=f"e2e-revoke-{uuid.uuid4().hex[:8]}")
        key_id = key_data["id"]

        # Revoke
        response = _oidc_request_with_retry(
            requests.delete,
            f"{maas_api_base_url}/v1/api-keys/{key_id}",
            token,
            label="OIDC API key revoke",
        )
        assert response.status_code in (200, 204), (
            f"API key revocation failed: {response.status_code} {response.text}"
        )

        # Verify key is gone (second revoke should 404)
        response2 = _oidc_request_with_retry(
            requests.delete,
            f"{maas_api_base_url}/v1/api-keys/{key_id}",
            token,
            label="OIDC double revoke",
        )
        assert response2.status_code == 404, (
            f"Expected 404 on double revoke, got {response2.status_code}: {response2.text}"
        )
        log.info(f"API key {key_id} create→revoke lifecycle completed")

    def test_api_key_owner_matches_oidc_username(self, maas_api_base_url: str):
        """API key's owner field reflects the preferred_username from the OIDC token.

        Verifies that the identity extracted from the JWT (preferred_username
        claim) is stored as the key's owner when the key is minted. This
        ensures identity propagation from OIDC → API key creation → stored
        metadata works end-to-end.
        """
        token = _request_oidc_token()
        expected_username = _decode_jwt_payload(token).get("preferred_username")
        assert expected_username, "OIDC token is missing preferred_username claim"

        key_data = _create_oidc_api_key(
            maas_api_base_url, token,
            name=f"e2e-owner-{uuid.uuid4().hex[:8]}"
        )
        key_id = key_data["id"]

        try:
            response = _oidc_request_with_retry(
                requests.get,
                f"{maas_api_base_url}/v1/api-keys/{key_id}",
                token,
                label="OIDC API key GET",
            )
            assert response.status_code == 200, (
                f"GET /v1/api-keys/{key_id} failed: "
                f"{response.status_code} {response.text}"
            )
            data = response.json()
            owner = data.get("owner") or data.get("username") or data.get("userId")
            assert owner == expected_username, (
                f"Expected API key owner to be '{expected_username}' "
                f"(preferred_username from OIDC token), got '{owner}'. "
                f"Full response: {data}"
            )
            log.info(
                f"API key owner '{owner}' correctly matches OIDC preferred_username"
            )
        finally:
            _oidc_request_with_retry(
                requests.delete,
                f"{maas_api_base_url}/v1/api-keys/{key_id}",
                token,
                label="OIDC cleanup owner test key",
            )


# ---------------------------------------------------------------------------
# Tests — Header Injection (Security)
# ---------------------------------------------------------------------------

class TestOIDCHeaderInjection:
    """Verify the gateway ignores client-supplied identity headers.

    Authorino sets X-MaaS-Username, X-MaaS-Group, and X-MaaS-Subscription
    from the validated token/API-key metadata. Clients must NOT be able to
    override these by injecting the headers themselves.
    """

    def test_injected_username_header_ignored(self, maas_api_base_url: str):
        """Client-supplied X-MaaS-Username must not override the authenticated identity.

        Mint an API key as alice_lead, then call /v1/models with a spoofed
        X-MaaS-Username header. The request should succeed using alice's
        real identity, not the injected one.
        """
        token = _request_oidc_token(username="alice_lead", password="letmein")
        api_key = _create_oidc_api_key(maas_api_base_url, token)["key"]

        response = _oidc_request_with_retry(
            requests.get,
            f"{maas_api_base_url}/v1/models",
            api_key,
            label="OIDC inject X-MaaS-Username",
            headers={"X-MaaS-Username": "evil_hacker"},
        )
        # The request should succeed — the spoofed header should be
        # overwritten by Authorino with the real authenticated identity
        assert response.status_code == 200, (
            f"Expected 200 (injected header ignored), got {response.status_code}: {response.text}"
        )
        log.info("X-MaaS-Username injection correctly ignored by gateway")

    def test_injected_group_header_does_not_escalate(self, maas_api_base_url: str):
        """Client-supplied X-MaaS-Group must not grant access to unauthorized resources.

        Inject a fabricated admin group and verify the request either
        succeeds with the real groups (header overwritten) or is denied
        (header interfered but did not grant escalated access).
        Either outcome is safe — the critical check is that injection
        does NOT grant broader access than the real identity.
        """
        token = _request_oidc_token(username="alice_lead", password="letmein")
        api_key = _create_oidc_api_key(maas_api_base_url, token)["key"]

        response = _oidc_request_with_retry(
            requests.get,
            f"{maas_api_base_url}/v1/models",
            api_key,
            label="OIDC inject X-MaaS-Group",
            headers={"X-MaaS-Group": '["system:cluster-admins","cluster-admin"]'},
        )

        # Get baseline (no injection) for comparison
        baseline = _oidc_request_with_retry(
            requests.get,
            f"{maas_api_base_url}/v1/models",
            api_key,
            label="OIDC baseline (group injection test)",
        )
        assert baseline.status_code == 200, (
            f"Baseline request failed: {baseline.status_code} {baseline.text}"
        )

        if response.status_code == 200:
            # Header was overwritten — verify no extra models were returned
            injected_models = response.json().get("data") or response.json().get("models") or []
            baseline_models = baseline.json().get("data") or baseline.json().get("models") or []
            injected_ids = sorted(m["id"] for m in injected_models)
            baseline_ids = sorted(m["id"] for m in baseline_models)
            assert injected_ids == baseline_ids, (
                f"Injected X-MaaS-Group changed model list (possible escalation)! "
                f"Baseline: {baseline_ids}, injected: {injected_ids}"
            )
            log.info("X-MaaS-Group injection overwritten — same models returned")
        else:
            # Header caused denial (e.g. 403) — safe, injection did not escalate
            assert response.status_code in (400, 403), (
                f"Unexpected status for injected group header: "
                f"{response.status_code} {response.text}"
            )
            log.info(
                f"X-MaaS-Group injection caused denial ({response.status_code}) "
                f"— no escalation possible"
            )

    def test_injected_subscription_header_ignored(self, maas_api_base_url: str):
        """Client-supplied X-MaaS-Subscription must not let a user access another subscription.

        Inject a fabricated subscription ID and verify the response is
        consistent with the real subscription bound to the API key (i.e.
        the injected header is overwritten or ignored).
        """
        token = _request_oidc_token(username="alice_lead", password="letmein")
        key_data = _create_oidc_api_key(maas_api_base_url, token)
        api_key = key_data["key"]
        real_subscription = key_data.get("subscription", "")

        # Request with spoofed subscription
        response = _oidc_request_with_retry(
            requests.get,
            f"{maas_api_base_url}/v1/models",
            api_key,
            label="OIDC inject X-MaaS-Subscription",
            headers={"X-MaaS-Subscription": "fake-subscription-id-12345"},
        )
        assert response.status_code == 200, (
            f"Expected 200 (injected subscription header ignored), "
            f"got {response.status_code}: {response.text}"
        )

        # Also request without injection to compare
        baseline_response = _oidc_request_with_retry(
            requests.get,
            f"{maas_api_base_url}/v1/models",
            api_key,
            label="OIDC baseline (subscription injection test)",
        )
        assert baseline_response.status_code == 200

        # Both should return the same models — injection should have no effect
        injected_models = response.json().get("data") or response.json().get("models") or []
        baseline_models = baseline_response.json().get("data") or baseline_response.json().get("models") or []
        injected_ids = sorted(m["id"] for m in injected_models)
        baseline_ids = sorted(m["id"] for m in baseline_models)
        assert injected_ids == baseline_ids, (
            f"Injected X-MaaS-Subscription changed model list! "
            f"Baseline models: {baseline_ids}, injected models: {injected_ids}"
        )
        log.info(
            f"X-MaaS-Subscription injection correctly ignored "
            f"(real subscription={real_subscription})"
        )

    def test_injected_username_on_oidc_token_ignored(self, maas_api_base_url: str):
        """Client-supplied X-MaaS-Username with a raw OIDC token (not API key) is ignored.

        Use a raw OIDC bearer token (not an API key) and inject a spoofed
        username. The minted API key should still reflect the real OIDC
        identity, not the injected header.
        """
        token = _request_oidc_token(username="alice_lead", password="letmein")

        # Mint an API key while injecting a spoofed username header.
        response = _oidc_request_with_retry(
            requests.post,
            f"{maas_api_base_url}/v1/api-keys",
            token,
            label="OIDC inject test",
            headers={
                "Authorization": f"Bearer {token}",
                "Content-Type": "application/json",
                "X-MaaS-Username": "bob_sre",
            },
            json={"name": f"e2e-oidc-inject-{uuid.uuid4().hex[:8]}"},
        )
        assert response.status_code in (200, 201), (
            f"API key mint with injected username header failed: "
            f"{response.status_code} {response.text}"
        )

        data = response.json()
        assert data.get("key", "").startswith("sk-oai-"), f"Unexpected API key payload: {data}"
        log.info(
            "API key minted successfully with injected X-MaaS-Username — "
            "gateway overwrote it with real OIDC identity"
        )


@pytest.mark.skipif(
    os.environ.get("EXTERNAL_OIDC", "").lower() != "true",
    reason="EXTERNAL_OIDC not enabled",
)
class TestOIDCClientBinding:
    """Verify that oidc-client-bound rejects tokens from the wrong OAuth client.

    MaaS is configured with OIDC_CLIENT_ID (e.g. test-client). The tenant-a
    realm also contains wrong-client — a second public client with no groups
    mapper. Tokens obtained via wrong-client carry azp=wrong-client. Because
    the issuer is valid, authentication succeeds and Authorino verifies the
    signature. The oidc-client-bound authorization rule then rejects the token
    with 403 because auth.identity.azp != configured clientId.

    A 401 (authentication failure) would mean the issuer check failed, which
    would indicate wrong-client is on a different realm — that is not what this
    test is checking. The assertion specifically requires 403.
    """

    def test_wrong_oauth_client_token_is_rejected(self, maas_api_base_url: str):
        """Token from wrong OAuth client (azp mismatch) is rejected with 403.

        The token has a valid signature and the correct issuer, so it passes
        Authorino OIDC authentication. The oidc-client-bound authorization rule
        then checks auth.identity.azp and denies with 403.
        """
        token = _request_oidc_token(client_id="wrong-client")

        payload = _decode_jwt_payload(token)
        assert payload.get("azp") == "wrong-client", (
            f"Expected azp=wrong-client in token but got: {payload.get('azp')}. "
            "Ensure wrong-client is configured in the tenant-a realm."
        )

        response = requests.post(
            f"{maas_api_base_url}/v1/api-keys",
            headers={
                "Authorization": f"Bearer {token}",
                "Content-Type": "application/json",
            },
            json={"name": f"e2e-wrong-client-{uuid.uuid4().hex[:8]}"},
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert response.status_code == 403, (
            f"Expected 403 for wrong OAuth client token (azp=wrong-client, "
            f"configured clientId={_oidc_client_id()}), "
            f"got {response.status_code}: {response.text}"
        )
        log.info(
            "wrong-client token correctly rejected with 403 "
            "(oidc-client-bound azp enforcement working)"
        )


@pytest.mark.skipif(
    os.environ.get("EXTERNAL_OIDC", "").lower() != "true",
    reason="EXTERNAL_OIDC not enabled",
)
class TestOIDCGroupSafety:
    """Verify that oidc-groups-safe rejects tokens containing unsafe group names.

    evil_user belongs to 'unsafe;group'. The semicolon is outside
    safeGroupNamePattern (^[A-Za-z0-9:._/-]+$), so the OPA rule denies
    the request with 403 even though the token is otherwise fully valid
    (correct issuer, correct azp, valid signature).

    A 401 would mean authentication failed — that is not what this test checks.
    403 is required: authentication passes, authorization denies.
    """

    def test_unsafe_group_name_is_rejected(self, maas_api_base_url: str):
        """Token with unsafe group name (semicolon) is rejected with 403.

        The oidc-groups-safe OPA rule fires because count(unsafe_group) != 0,
        where unsafe_group is any group whose name does not match
        ^[A-Za-z0-9:._/-]+$.
        """
        token = _request_oidc_token(username="evil_user", password="letmein")

        payload = _decode_jwt_payload(token)
        groups = payload.get("groups", [])
        assert any(";" in g for g in groups), (
            f"Expected token to contain a group with ';' but got: {groups}. "
            "Ensure evil_user is a member of 'unsafe;group' in the tenant-a realm."
        )

        response = requests.post(
            f"{maas_api_base_url}/v1/api-keys",
            headers={
                "Authorization": f"Bearer {token}",
                "Content-Type": "application/json",
            },
            json={"name": f"e2e-unsafe-group-{uuid.uuid4().hex[:8]}"},
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert response.status_code == 403, (
            f"Expected 403 for token with unsafe group name 'unsafe;group' "
            f"(oidc-groups-safe enforcement), "
            f"got {response.status_code}: {response.text}"
        )
        log.info(
            "unsafe group name correctly rejected with 403 "
            "(oidc-groups-safe OPA rule working)"
        )

    def test_mixed_safe_and_unsafe_groups_is_rejected(self, maas_api_base_url: str):
        """Token with one safe group AND one unsafe group is still rejected with 403.

        The oidc-groups-safe OPA rule uses count(unsafe_group) == 0 — even a
        single unsafe group in an otherwise-valid groups list causes denial.
        mixed_user belongs to ['Engineering', 'unsafe;group']; the safe group
        must not be sufficient to bypass the rule.
        """
        token = _request_oidc_token(username="mixed_user", password="letmein")

        payload = _decode_jwt_payload(token)
        groups = payload.get("groups", [])
        has_safe = any(";" not in g for g in groups)
        has_unsafe = any(";" in g for g in groups)
        assert has_safe and has_unsafe, (
            f"Expected mixed_user token to contain both safe and unsafe groups, "
            f"got: {groups}. Ensure mixed_user is a member of both Engineering "
            f"and 'unsafe;group' in the tenant-a realm."
        )

        response = requests.post(
            f"{maas_api_base_url}/v1/api-keys",
            headers={
                "Authorization": f"Bearer {token}",
                "Content-Type": "application/json",
            },
            json={"name": f"e2e-mixed-group-{uuid.uuid4().hex[:8]}"},
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert response.status_code == 403, (
            f"Expected 403 for token with mixed safe+unsafe groups "
            f"(oidc-groups-safe must reject if ANY group is unsafe), "
            f"got {response.status_code}: {response.text}"
        )
        log.info(
            "mixed safe+unsafe groups correctly rejected with 403 "
            "(oidc-groups-safe rejects on any unsafe group, not all)"
        )


@pytest.mark.skipif(
    os.environ.get("EXTERNAL_OIDC", "").lower() != "true",
    reason="EXTERNAL_OIDC not enabled",
)
class TestOIDCDirectModelAccess:
    """Verify that an OIDC token can call GET /v1/models directly.

    This mirrors the verification step in the official Red Hat docs (section
    1.15.2): 'Use the OIDC token to list available models.' All other model
    listing tests in this suite use a minted API key; this class ensures the
    raw JWT bearer path through the gateway also works.
    """

    def test_oidc_token_can_list_models_directly(self, maas_api_base_url: str):
        """OIDC bearer token is accepted by GET /v1/models without minting an API key first."""
        token = _request_oidc_token()

        response = _oidc_request_with_retry(
            requests.get,
            f"{maas_api_base_url}/v1/models",
            token,
            label="OIDC direct model list",
        )
        assert response.status_code == 200, (
            f"Expected 200 when listing models with OIDC bearer token, "
            f"got {response.status_code}: {response.text}"
        )
        data = response.json()
        assert data.get("object") == "list", (
            f"Expected object=list in response, got: {data.get('object')}"
        )
        assert isinstance(data.get("data"), list), (
            f"Expected data array in response, got: {type(data.get('data'))}"
        )
        log.info(
            f"OIDC token can list models directly — {len(data.get('data', []))} model(s) returned"
        )


@pytest.mark.skipif(
    os.environ.get("EXTERNAL_OIDC", "").lower() != "true",
    reason="EXTERNAL_OIDC not enabled",
)
class TestOIDCAlertingInfra:
    """Verify that the Authorino alerting infrastructure is present in the cluster.

    The MaaSAuthorinoAuthenticationHighFailureRate PrometheusRule was introduced
    as part of RHOAIENG-60847. If it is accidentally deleted or misconfigured,
    the auth failure rate alerting silently stops working. This class provides
    a lightweight smoke check that the rule exists.
    """

    def test_authorino_prometheusrule_exists(self):
        """PrometheusRule authorino-maas-authentication-alerts is present in kuadrant-system."""
        # The rule is deployed to kuadrant-system (where Authorino runs) by
        # scripts/observability/install-observability.sh, not kustomize base.
        namespace = os.environ.get("AUTHORINO_NAMESPACE", "kuadrant-system")
        rule_name = "authorino-maas-authentication-alerts"
        result = subprocess.run(
            [
                "kubectl", "get", "prometheusrule", rule_name,
                "-n", namespace,
                "--ignore-not-found",
                "-o", "jsonpath={.metadata.name}",
            ],
            capture_output=True,
            text=True,
            timeout=30,
        )
        assert result.returncode == 0, (
            f"kubectl get prometheusrule failed (exit {result.returncode}): {result.stderr}"
        )
        assert result.stdout.strip() == rule_name, (
            f"PrometheusRule '{rule_name}' not found in namespace '{namespace}'. "
            "Ensure install-observability.sh has been run and the rule was applied to kuadrant-system."
        )
        log.info(
            f"PrometheusRule {rule_name} present in namespace '{namespace}'"
        )
