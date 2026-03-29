import os
import time
import uuid
import logging

import pytest
import requests

from conftest import TLS_VERIFY

log = logging.getLogger(__name__)

pytestmark = pytest.mark.skipif(
    os.environ.get("EXTERNAL_OIDC", "").lower() != "true",
    reason="external OIDC tests are disabled",
)


def _required_env(name: str) -> str:
    value = os.environ.get(name, "")
    assert value, f"{name} must be set when EXTERNAL_OIDC=true"
    return value


def _request_oidc_token() -> str:
    token_url = _required_env("OIDC_TOKEN_URL")
    client_id = _required_env("OIDC_CLIENT_ID")
    username = _required_env("OIDC_USERNAME")
    password = _required_env("OIDC_PASSWORD")

    response = requests.post(
        token_url,
        data={
            "grant_type": "password",
            "client_id": client_id,
            "username": username,
            "password": password,
        },
        timeout=30,
        verify=TLS_VERIFY,
    )
    assert response.status_code == 200, f"OIDC token request failed: {response.status_code} {response.text}"

    token = response.json().get("access_token")
    assert token, "OIDC token response missing access_token"
    return token


def _create_oidc_api_key(maas_api_base_url: str, oidc_token: str) -> dict:
    response = requests.post(
        f"{maas_api_base_url}/v1/api-keys",
        headers={"Authorization": f"Bearer {oidc_token}", "Content-Type": "application/json"},
        json={"name": f"e2e-oidc-{uuid.uuid4().hex[:8]}"},
        timeout=30,
        verify=TLS_VERIFY,
    )
    assert response.status_code in (200, 201), f"OIDC API key mint failed: {response.status_code} {response.text}"

    data = response.json()
    assert data.get("key", "").startswith("sk-oai-"), f"Unexpected API key payload: {data}"
    return data


class TestExternalOIDC:
    def test_oidc_token_can_create_api_key(self, maas_api_base_url: str):
        token = _request_oidc_token()
        data = _create_oidc_api_key(maas_api_base_url, token)
        print(f"[oidc] created api key id={data.get('id')} prefix={data.get('key', '')[:18]}...")

    def test_invalid_oidc_token_gets_401(self, maas_api_base_url: str):
        token = _request_oidc_token() + "broken"
        response = requests.post(
            f"{maas_api_base_url}/v1/api-keys",
            headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
            json={"name": f"e2e-oidc-invalid-{uuid.uuid4().hex[:8]}"},
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert response.status_code == 401, f"Expected 401 for invalid OIDC token, got {response.status_code}: {response.text}"

    def test_minted_api_key_can_list_models_and_infer(self, maas_api_base_url: str):
        token = _request_oidc_token()
        api_key = _create_oidc_api_key(maas_api_base_url, token)["key"]
        headers = {"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"}

        models_response = requests.get(
            f"{maas_api_base_url}/v1/models",
            headers=headers,
            timeout=45,
            verify=TLS_VERIFY,
        )
        assert models_response.status_code == 200, f"OIDC-minted API key failed to list models: {models_response.status_code} {models_response.text}"

        items = models_response.json().get("data") or models_response.json().get("models") or []
        assert items, f"Expected at least one model from /v1/models, got: {models_response.text}"

        model_id = items[0]["id"]
        model_url = items[0]["url"].rstrip("/")
        inference_response = requests.post(
            f"{model_url}/v1/chat/completions",
            headers=headers,
            json={
                "model": model_id,
                "messages": [{"role": "user", "content": "Hello from external OIDC e2e"}],
                "max_tokens": 16,
            },
            timeout=45,
            verify=TLS_VERIFY,
        )
        assert inference_response.status_code == 200, (
            f"OIDC-minted API key inference failed: {inference_response.status_code} {inference_response.text}"
        )

        print(f"[oidc] inference succeeded for {model_id} at {time.time()}")
