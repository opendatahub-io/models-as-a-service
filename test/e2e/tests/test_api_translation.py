"""E2E for RHOAIENG-102128 (direction a): Anthropic Messages client -> OpenAI backend
API translation through the gateway.

With MaasTenantConfig.spec.apiTranslation.enabled on the default tenant, an
Anthropic /v1/messages request against an OpenAI-backed model is translated to
OpenAI /v1/chat/completions upstream and returned as well-formed Anthropic
(role=assistant text blocks for JSON; message_start..message_stop for SSE). With
translation off, OpenAI /v1/chat/completions passes through and /v1/messages is no
longer translated; auth is enforced throughout.

Requests use the shared plane: the gateway-root /v1/messages|/v1/chat/completions
with the canonical model id in the body (BBR). Skip-gated on the deployed
Praxis/ExtProc image rendering the translator chain.
"""

from __future__ import annotations

import json
import logging
import time

import pytest
import requests
from multitenancy_helpers import (
    GATEWAY_NAMESPACE,
    TLS_VERIFY,
    _oc_run,
    redact_sensitive,
)
from test_helper import (
    MODEL_CANONICAL_ID,
    SIMULATOR_SUBSCRIPTION,
    _gateway_url,
    _get_cluster_token,
    _maas_api_url,
    _wait_for_gateway_auth_enforced,
)

log = logging.getLogger(__name__)

pytestmark = pytest.mark.xdist_group("tenant_ipp")

DEFAULT_TENANT_NS = "models-as-a-service"
TENANT_CONFIG = "maastenantconfig"
DEFAULT_TENANT = "default-tenant"
PLUGINS_CONFIGMAP = "payload-processing-plugins"
EXTPROC_DEPLOYMENT = "payload-processing"
TRANSLATOR_KEYS = ("anthropic_messages_request", "anthropic_messages_to_chat_completions")
RENDER_TIMEOUT = 120
ROLLOUT_TIMEOUT = 180
POLL = 5
GATEWAY_RETRIES = 6
GATEWAY_RETRY_DELAY = 5


def _set_api_translation(enabled: bool) -> None:
    body = json.dumps({"spec": {"apiTranslation": {"enabled": enabled}}})
    result = _oc_run(
        ["patch", TENANT_CONFIG, DEFAULT_TENANT, "-n", DEFAULT_TENANT_NS, "--type=merge", "-p", body],
        timeout=60,
    )
    if result.returncode != 0:
        raise RuntimeError(f"Failed to set apiTranslation={enabled}: {(result.stderr or result.stdout).strip()}")


def _api_translation_enabled() -> bool:
    result = _oc_run(
        ["get", TENANT_CONFIG, DEFAULT_TENANT, "-n", DEFAULT_TENANT_NS,
         "-o", "jsonpath={.spec.apiTranslation.enabled}"],
        timeout=60,
    )
    return (result.stdout or "").strip() == "true"


def _plugins_have_translator() -> bool:
    result = _oc_run(["get", "configmap", PLUGINS_CONFIGMAP, "-n", GATEWAY_NAMESPACE, "-o", "jsonpath={.data}"], timeout=60)
    data = result.stdout or ""
    return all(key in data for key in TRANSLATOR_KEYS)


def _wait_translator_rendered(timeout: int = RENDER_TIMEOUT) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        if _plugins_have_translator():
            return True
        time.sleep(POLL)
    return False


def _rollout_extproc() -> None:
    # The ExtProc applies the (dis)enabled translator chain on rollout; the
    # ConfigMap change alone does not take effect for in-flight traffic.
    _oc_run(
        ["rollout", "status", f"deployment/{EXTPROC_DEPLOYMENT}", "-n", GATEWAY_NAMESPACE,
         f"--timeout={ROLLOUT_TIMEOUT}s"],
        timeout=ROLLOUT_TIMEOUT + 30,
    )


def _apply_api_translation(enabled: bool, *, gate: bool = False) -> bool:
    _set_api_translation(enabled)
    if enabled and gate and not _wait_translator_rendered():
        return False
    _rollout_extproc()
    return True


def _post(url: str, api_key: str, body: dict, *, stream: bool = False) -> requests.Response:
    for attempt in range(1, GATEWAY_RETRIES + 1):
        response = requests.post(
            url,
            headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
            json=body,
            timeout=60,
            verify=TLS_VERIFY,
            stream=stream,
        )
        if response.status_code in (404, 503) and not stream and attempt < GATEWAY_RETRIES:
            time.sleep(GATEWAY_RETRY_DELAY)
            continue
        return response
    return response


def _anthropic_body(stream: bool = False) -> dict:
    body = {
        "model": MODEL_CANONICAL_ID,
        "max_tokens": 32,
        "messages": [{"role": "user", "content": [{"type": "text", "text": "api translation ping"}]}],
    }
    if stream:
        body["stream"] = True
    return body


def _openai_body() -> dict:
    return {"model": MODEL_CANONICAL_ID, "messages": [{"role": "user", "content": "ping"}], "max_tokens": 8}


def _sse_event_names(response: requests.Response) -> list[str]:
    names: list[str] = []
    for raw in response.iter_lines(decode_unicode=True):
        if raw and raw.startswith("event:"):
            names.append(raw[len("event:"):].strip())
    return names


def _create_default_api_key() -> str:
    response = requests.post(
        f"{_maas_api_url()}/v1/api-keys",
        headers={"Authorization": f"Bearer {_get_cluster_token()}", "Content-Type": "application/json"},
        json={"name": "e2e-apitranslation", "subscription": SIMULATOR_SUBSCRIPTION},
        timeout=45,
        verify=TLS_VERIFY,
    )
    assert response.status_code in (200, 201), (
        f"Failed to create API key: {response.status_code} {redact_sensitive(response.text)}"
    )
    key = response.json().get("key")
    assert key, f"API key missing: {redact_sensitive(response.json())}"
    return key


def _is_well_formed_anthropic(body: dict) -> bool:
    content = body.get("content")
    return (
        body.get("role") == "assistant"
        and isinstance(content, list)
        and bool(content)
        and content[0].get("type") == "text"
    )


@pytest.fixture(scope="module")
def api_translation_env():
    original = _api_translation_enabled()
    _wait_for_gateway_auth_enforced()
    api_key = _create_default_api_key()
    try:
        yield {"api_key": api_key}
    finally:
        _apply_api_translation(original)


class TestApiTranslation:
    """Anthropic->OpenAI translation works when enabled and is gated when off."""

    @pytest.mark.serial
    def test_anthropic_to_openai_translation_and_gating(self, api_translation_env):
        api_key = api_translation_env["api_key"]
        gw = _gateway_url().rstrip("/")
        messages_url = f"{gw}/v1/messages"
        chat_url = f"{gw}/v1/chat/completions"

        log.info("Auth: a bogus key must be rejected at the gateway")
        bogus = _post(messages_url, "sk-bogus-not-real", _anthropic_body())
        assert bogus.status_code in (401, 403), f"expected 401/403 for bogus key, got {bogus.status_code}"

        log.info("Enabling apiTranslation on %s/%s", DEFAULT_TENANT_NS, DEFAULT_TENANT)
        if not _apply_api_translation(True, gate=True):
            pytest.skip("deployed Praxis/ExtProc image did not render the apiTranslation chain")

        log.info("Anthropic /v1/messages (JSON) must return well-formed Anthropic")
        r = _post(messages_url, api_key, _anthropic_body())
        assert r.status_code == 200, f"translation JSON failed: {r.status_code} {redact_sensitive(r.text[:300])}"
        assert _is_well_formed_anthropic(r.json()), f"not well-formed Anthropic: {redact_sensitive(r.text[:300])}"

        log.info("Anthropic /v1/messages (SSE) must stream message_start..message_stop")
        stream = _post(messages_url, api_key, _anthropic_body(stream=True), stream=True)
        assert stream.status_code == 200, f"translation SSE failed: {stream.status_code}"
        names = _sse_event_names(stream)
        assert "message_start" in names and "message_stop" in names, f"expected Anthropic SSE events, got {names}"

        log.info("Disabling apiTranslation; translation must be gated off, passthrough intact")
        _apply_api_translation(False)
        passthrough = _post(chat_url, api_key, _openai_body())
        assert passthrough.status_code == 200, (
            f"OpenAI passthrough failed after disabling: {passthrough.status_code} "
            f"{redact_sensitive(passthrough.text[:300])}"
        )
        gated = _post(messages_url, api_key, _anthropic_body())
        assert not (gated.status_code == 200 and _is_well_formed_anthropic(gated.json() if gated.ok else {})), (
            "Anthropic /v1/messages was still translated after disabling apiTranslation"
        )
