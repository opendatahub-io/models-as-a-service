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

import pytest
import requests
from multitenancy_helpers import (
    GATEWAY_NAMESPACE,
    TLS_VERIFY,
    _oc_run,
    extproc_deployment_uses_praxis,
    redact_sensitive,
    wait_until,
)
from test_helper import (
    MODEL_CANONICAL_ID,
    SIMULATOR_SUBSCRIPTION,
    _create_api_key_with_id,
    _gateway_url,
    _get_cluster_token,
    _ns,
    _request_with_gateway_retry,
    _revoke_api_key,
    _wait_for_gateway_auth_enforced,
)

log = logging.getLogger(__name__)

pytestmark = [pytest.mark.xdist_group("tenant_ipp"), pytest.mark.api_translation, pytest.mark.full_suite]


DEFAULT_TENANT_NS = _ns()
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
    if result.returncode != 0:
        # A failed read is not evidence the translator is absent; raise so callers do
        # not mistake an unreadable ConfigMap for a reconciled (empty) one.
        raise RuntimeError(f"Failed to read {PLUGINS_CONFIGMAP}: {(result.stderr or result.stdout).strip()}")
    data = result.stdout or ""
    return all(key in data for key in TRANSLATOR_KEYS)


def _wait_translator_state(present: bool, timeout: int = RENDER_TIMEOUT) -> bool:
    """Wait until the translator chain is present (enable) or absent (disable) in the
    ConfigMap, i.e. until the controller has reconciled the apiTranslation toggle. This
    is the barrier that makes the subsequent rollout meaningful: without it, oc rollout
    status can race ahead of the controller and report the pre-toggle generation as
    already complete."""

    def _reconciled() -> bool:
        try:
            return _plugins_have_translator() == present
        except RuntimeError:
            return False  # transient read failure: keep polling rather than trust an unread state

    try:
        return bool(
            wait_until(
                _reconciled,
                timeout,
                f"translator chain {'present' if present else 'absent'} in {PLUGINS_CONFIGMAP}",
                interval=POLL,
            )
        )
    except AssertionError:
        return False


def _rollout_extproc() -> None:
    restart = _oc_run(
        ["rollout", "restart", f"deployment/{EXTPROC_DEPLOYMENT}", "-n", GATEWAY_NAMESPACE],
        timeout=60,
    )
    if restart.returncode != 0:
        raise RuntimeError(
            f"Failed to restart {EXTPROC_DEPLOYMENT}: {(restart.stderr or restart.stdout).strip()}"
        )
    _oc_run(
        ["rollout", "status", f"deployment/{EXTPROC_DEPLOYMENT}", "-n", GATEWAY_NAMESPACE,
         f"--timeout={ROLLOUT_TIMEOUT}s"],
        timeout=ROLLOUT_TIMEOUT + 30,
    )


def _apply_api_translation(enabled: bool | None, *, gate: bool = False) -> bool:
    _set_api_translation(enabled)
    # Wait for the controller to reconcile the toggle before rolling the ext_proc,
    # so the rollout targets the post-toggle generation instead of racing it.
    reconciled = _wait_translator_state(present=bool(enabled))
    if enabled and gate and not reconciled:
        return False
    _rollout_extproc()
    return True


def _post(
    url: str,
    api_key: str,
    body: dict,
    *,
    stream: bool = False,
    auth_denial_is_transient: bool = True,
) -> requests.Response:
    headers = {"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"}
    return _request_with_gateway_retry(
        requests.post,
        url,
        headers=headers,
        json=body,
        stream=stream,
        retries=GATEWAY_RETRIES,
        delay=GATEWAY_RETRY_DELAY,
        auth_denial_is_transient=auth_denial_is_transient,
        extra_transient_statuses=(404,),
    )


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


def _create_default_api_key() -> tuple[str, str | None]:
    """Create the shared API key and return (key, id) via the shared
    ``_create_api_key_with_id`` helper (retries transient gateway/AuthPolicy
    propagation errors); the id lets the fixture revoke the key on teardown."""
    return _create_api_key_with_id(_get_cluster_token(), "e2e-apitranslation", SIMULATOR_SUBSCRIPTION)


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
    """Poll /v1/messages until translation is no longer served, via the shared
    wait_until helper. Disabling is eventually-consistent at the data plane (beyond the
    ConfigMap edit and ExtProc rollout), so a single check can catch a stale translated
    200. A transient gateway error (502/503/504) is not proof of gating, so keep polling
    on those too.

    wait_until accepts the first *truthy* return, but a requests.Response is falsy for a
    non-2xx status (``Response.__bool__`` is ``.ok``) — so the gated 404 can't be the
    sentinel. Return a bool and capture the response out of band."""
    captured: dict[str, requests.Response] = {}

    def _gated() -> bool:
        response = requests.post(
            url,
            headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
            json=_anthropic_body(),
            timeout=60,
            verify=TLS_VERIFY,
        )
        captured["response"] = response
        return response.status_code not in (502, 503, 504) and not _is_translated(response)

    wait_until(_gated, timeout, f"{url} stops serving translation", interval=POLL)
    return captured["response"]


@pytest.fixture(scope="module")
def api_translation_env():
    """Shared setup: a real API key, gateway auth enforced, and restoration of the
    tenant's original apiTranslation state after the module's tests finish."""
    original = _api_translation_state()
    _wait_for_gateway_auth_enforced()
    api_key, api_key_id = _create_default_api_key()
    try:
        yield {"api_key": api_key}
    finally:
        if api_key_id:
            _revoke_api_key(_get_cluster_token(), api_key_id)
        _apply_api_translation(original)


@pytest.fixture
def translation_on(api_translation_env):
    """Enable apiTranslation and wait for the ExtProc to render the translator chain.

    Skip only when praxis-extproc isn't the dataplane (not this test's scenario);
    fail if it is but the chain never renders, so a regression isn't skipped."""
    if not extproc_deployment_uses_praxis(EXTPROC_DEPLOYMENT):
        pytest.skip("default tenant payload-processing is not praxis-extproc")
    log.info("Enabling apiTranslation on %s/%s", DEFAULT_TENANT_NS, DEFAULT_TENANT)
    if not _apply_api_translation(True, gate=True):
        pytest.fail(
            "apiTranslation enabled on a praxis-extproc dataplane but the translator chain "
            f"({', '.join(TRANSLATOR_KEYS)}) never rendered in {PLUGINS_CONFIGMAP} "
            f"within {RENDER_TIMEOUT}s"
        )
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
        invalid = _post(messages_url, "sk-invalid-not-real", _anthropic_body(), auth_denial_is_transient=False)
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
        # content_block_delta carries the generated text under the Anthropic streaming
        # contract; without it a gateway that drops all deltas would still pass the
        # envelope checks above, so require at least one.
        assert "content_block_delta" in names, f"expected generated content deltas, got {names}"

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
