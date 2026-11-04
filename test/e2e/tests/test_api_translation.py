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

pytestmark = [pytest.mark.xdist_group("tenant_ipp"), pytest.mark.api_translation]

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


def _set_api_translation(enabled: bool | None) -> None:
    # enabled=None removes the field via a JSON merge-patch null, restoring an
    # originally-omitted spec.apiTranslation.enabled instead of writing an explicit false.
    body = json.dumps({"spec": {"apiTranslation": {"enabled": enabled}}})
    result = _oc_run(
        ["patch", TENANT_CONFIG, DEFAULT_TENANT, "-n", DEFAULT_TENANT_NS, "--type=merge", "-p", body],
        timeout=60,
    )
    if result.returncode != 0:
        raise RuntimeError(f"Failed to set apiTranslation={enabled}: {(result.stderr or result.stdout).strip()}")


def _api_translation_state() -> bool | None:
    """Tri-state read of spec.apiTranslation.enabled so teardown can restore faithfully:
    None when the field is omitted, else the bool. Raises if the read fails rather than
    masking it as False (which would disable translation on a tenant that had it on)."""
    result = _oc_run(
        ["get", TENANT_CONFIG, DEFAULT_TENANT, "-n", DEFAULT_TENANT_NS,
         "-o", "jsonpath={.spec.apiTranslation.enabled}"],
        timeout=60,
    )
    if result.returncode != 0:
        raise RuntimeError(f"Failed to read apiTranslation state: {(result.stderr or result.stdout).strip()}")
    value = (result.stdout or "").strip()
    if value == "":
        return None
    return value == "true"


def _plugins_have_translator() -> bool:
    result = _oc_run(["get", "configmap", PLUGINS_CONFIGMAP, "-n", GATEWAY_NAMESPACE, "-o", "jsonpath={.data}"], timeout=60)
    data = result.stdout or ""
    return all(key in data for key in TRANSLATOR_KEYS)


def _wait_translator_state(present: bool, timeout: int = RENDER_TIMEOUT) -> bool:
    """Wait until the translator chain is present (enable) or absent (disable) in the
    ConfigMap, i.e. until the controller has reconciled the apiTranslation toggle. This
    is the barrier that makes the subsequent rollout meaningful: without it, oc rollout
    status can race ahead of the controller and report the pre-toggle generation as
    already complete."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        if _plugins_have_translator() == present:
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


def _apply_api_translation(enabled: bool | None, *, gate: bool = False) -> bool:
    _set_api_translation(enabled)
    # Wait for the controller to reconcile the toggle (keys present on enable, absent on
    # disable) before rolling the ExtProc, so the rollout waits for the post-toggle
    # generation rather than racing the controller. A restored-to-omitted state (None)
    # is treated as disabled for the translator-chain barrier.
    reconciled = _wait_translator_state(present=bool(enabled))
    if enabled and gate and not reconciled:
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
        # Retry transient route-propagation/ExtProc-reload responses for streaming
        # requests too: a just-enabled SSE route can 404 briefly, and a single-shot
        # streaming check would otherwise fail on a slower/colder cluster.
        if response.status_code in (404, 503) and attempt < GATEWAY_RETRIES:
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


def _is_translated(response: requests.Response) -> bool:
    """True when /v1/messages returned a well-formed Anthropic translation (200)."""
    return response.status_code == 200 and _is_well_formed_anthropic(response.json() if response.ok else {})


def _wait_until_gated(url: str, api_key: str, timeout: int = RENDER_TIMEOUT) -> requests.Response:
    """Poll /v1/messages until translation is no longer served. Disabling is
    eventually-consistent at the data plane (beyond the ConfigMap edit and ExtProc
    rollout), so a single check can still catch a stale translated 200. Returns the
    first non-translated response, or the last one seen on timeout."""
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        last = requests.post(
            url,
            headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
            json=_anthropic_body(),
            timeout=60,
            verify=TLS_VERIFY,
        )
        if not _is_translated(last):
            return last
        time.sleep(POLL)
    return last


@pytest.fixture(scope="module")
def api_translation_env():
    """Shared setup: a real API key, gateway auth enforced, and restoration of the
    tenant's original apiTranslation state after the module's tests finish."""
    original = _api_translation_state()
    _wait_for_gateway_auth_enforced()
    api_key = _create_default_api_key()
    try:
        yield {"api_key": api_key}
    finally:
        _apply_api_translation(original)


@pytest.fixture
def translation_on(api_translation_env):
    """Enable apiTranslation and wait for the ExtProc to render the translator
    chain. Skips the test if the deployed image cannot render it."""
    log.info("Enabling apiTranslation on %s/%s", DEFAULT_TENANT_NS, DEFAULT_TENANT)
    if not _apply_api_translation(True, gate=True):
        pytest.skip("deployed Praxis/ExtProc image did not render the apiTranslation chain")
    return api_translation_env["api_key"]


@pytest.fixture
def translation_off(api_translation_env):
    """Disable apiTranslation and roll the ExtProc so the change takes effect."""
    log.info("Disabling apiTranslation on %s/%s", DEFAULT_TENANT_NS, DEFAULT_TENANT)
    _apply_api_translation(False)
    return api_translation_env["api_key"]


class TestApiTranslation:
    """Anthropic->OpenAI translation works when enabled and is gated when off."""

    @pytest.mark.serial
    def test_auth_rejects_invalid_key(self, api_translation_env):
        """An invalid key must be rejected at the gateway regardless of translation state."""
        messages_url = f"{_gateway_url().rstrip('/')}/v1/messages"
        invalid = _post(messages_url, "sk-invalid-not-real", _anthropic_body())
        assert invalid.status_code == 401, f"expected 401 Unauthorized for invalid key, got {invalid.status_code}"

    @pytest.mark.serial
    def test_translation_when_enabled(self, translation_on):
        """With apiTranslation on, Anthropic /v1/messages returns well-formed Anthropic
        for both JSON and SSE responses."""
        api_key = translation_on
        messages_url = f"{_gateway_url().rstrip('/')}/v1/messages"

        log.info("Anthropic /v1/messages (JSON) must return well-formed Anthropic")
        r = _post(messages_url, api_key, _anthropic_body())
        assert r.status_code == 200, f"translation JSON failed: {r.status_code} {redact_sensitive(r.text[:300])}"
        assert _is_well_formed_anthropic(r.json()), f"not well-formed Anthropic: {redact_sensitive(r.text[:300])}"

        log.info("Anthropic /v1/messages (SSE) must stream message_start..message_stop")
        stream = _post(messages_url, api_key, _anthropic_body(stream=True), stream=True)
        assert stream.status_code == 200, f"translation SSE failed: {stream.status_code}"
        names = _sse_event_names(stream)
        assert "message_start" in names and "message_stop" in names, f"expected Anthropic SSE events, got {names}"

    @pytest.mark.serial
    def test_gated_when_disabled(self, translation_off):
        """With apiTranslation off, OpenAI passthrough still works and Anthropic
        /v1/messages is no longer translated."""
        api_key = translation_off
        gw = _gateway_url().rstrip("/")

        passthrough = _post(f"{gw}/v1/chat/completions", api_key, _openai_body())
        assert passthrough.status_code == 200, (
            f"OpenAI passthrough failed after disabling: {passthrough.status_code} "
            f"{redact_sensitive(passthrough.text[:300])}"
        )
        gated = _wait_until_gated(f"{gw}/v1/messages", api_key)
        assert not _is_translated(gated), (
            f"Anthropic /v1/messages was still translated {RENDER_TIMEOUT}s after disabling "
            f"apiTranslation (status {gated.status_code})"
        )
