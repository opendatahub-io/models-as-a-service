"""
E2E tests for large chat/completions I/O — streaming mode (RHOAIENG-94274).

Validates that large payloads survive the round trip through gateway filters
(WASM, ext_proc, Envoy buffer limits) on the **streaming** code path. The
non-streaming path is covered by TestLargeIO in test_echo_large_io.py.

Streaming exercises a different code path than blocking: chunked transfer
encoding, streaming ext_proc interceptors, and wasm streaming body filters.
A gateway that handles the non-streaming path correctly may still buffer or
truncate chunks on the streaming path — these tests catch exactly that.

Prerequisites:
  Echo fixture (RHOAIENG-94273) deployed at cluster setup time:
    kustomize build test/e2e/fixtures | kubectl apply -f -
  The echo-sim LLMIS mirrors the last user message back verbatim, making
  response content a deterministic function of the request.

Payload size: LARGE_WORDS words (~4.3MB) — satisfies the ≥1-4MB AC.
echo-sim context window is 1700000 tokens; 800K words × 2 = 1.6M tokens.

Environment variables:
  E2E_LARGE_IO         Set to "1" to enable (gated for CI time budget).
  E2E_LARGE_IO_TIMEOUT Request timeout in seconds (default: 300).
  E2E_LARGE_IO_WORDS   Prompt size in words (default: 800000, ~4.3MB).

Epic: RHOAIENG-94270
Story: RHOAIENG-94274
"""

import json
import logging
import os
import time

import pytest
import requests

from test_helper import (
    ECHO_MODEL_NAME,
    ECHO_MODEL_PATH,
    ECHO_MODEL_REF,
    MODEL_NAMESPACE,
    TLS_VERIFY,
    _create_api_key,
    _create_sa_token,
    _create_test_auth_policy,
    _create_test_subscription,
    _delete_governance_and_wait,
    _gateway_url,
    _ns,
    _sa_to_user,
    _wait_for_gateway_auth_enforced,
    _wait_for_model_ready,
    _wait_for_subscription_inference_ready,
)

log = logging.getLogger(__name__)

LARGE_IO_ENABLED = os.environ.get("E2E_LARGE_IO", "").lower() in ("1", "true")
LARGE_IO_TIMEOUT = int(os.environ.get("E2E_LARGE_IO_TIMEOUT", "300"))

# ~800k words / ~4.3MB — satisfies the ≥1-4MB AC in RHOAIENG-94274.
# echo-sim's context window is 1700000 tokens; 800K words × 2 (prompt + echo)
# = 1.6M tokens, comfortably within the limit.
LARGE_WORDS = int(os.environ.get("E2E_LARGE_IO_WORDS", "800000"))
SMALL_WORDS = max(1000, LARGE_WORDS // 800)  # ~0.1% of LARGE_WORDS, min 1000

if LARGE_WORDS <= SMALL_WORDS:
    raise ValueError(
        f"E2E_LARGE_IO_WORDS ({LARGE_WORDS}) must be larger than SMALL_WORDS ({SMALL_WORDS}); "
        "increase E2E_LARGE_IO_WORDS (default 800000)"
    )

# Each streaming round trip bills ~40k tokens (prompt + mirrored completion).
# Ten million per minute keeps rate limiting out of the way without relying on
# the unlimited field, which older CRDs reject with "Required value".
TOKEN_BUDGET = 10_000_000

pytestmark = [
    pytest.mark.serial,
    pytest.mark.xdist_group("large_io"),
    pytest.mark.skipif(
        not LARGE_IO_ENABLED,
        reason="Large I/O streaming tests require E2E_LARGE_IO=1 (CI time budget)",
    ),
]


def _lorem(words: int) -> str:
    """Whitespace-delimited filler so the echo-sim tokenizer sees real words."""
    return (("lorem ipsum dolor sit amet " * ((words // 5) + 1)).strip()).rsplit(" ", 1)[0]


def _echo_completion_stream(api_key: str, prompt: str) -> requests.Response:
    """POST chat/completions with stream=True to the echo model.

    Uses LARGE_IO_TIMEOUT (300s default) because the shared TIMEOUT (45s) is too
    tight for a ~4.3MB payload traversing the full gateway filter chain.
    """
    return requests.post(
        f"{_gateway_url()}{ECHO_MODEL_PATH}/v1/chat/completions",
        headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
        json={
            "model": ECHO_MODEL_NAME,
            "messages": [{"role": "user", "content": prompt}],
            "stream": True,
        },
        timeout=LARGE_IO_TIMEOUT,
        verify=TLS_VERIFY,
        stream=True,
    )


def _collect_stream_content(response: requests.Response) -> tuple[list, str]:
    """Parse an SSE response; return (raw_event_lines, assembled_content_string).

    Joins delta.content fields across all chunks in order. Raw event lines are
    returned so callers can independently assert on the [DONE] sentinel and
    chunk count.
    """
    content_type = response.headers.get("Content-Type", "")
    assert "text/event-stream" in content_type, (
        f"Expected Content-Type text/event-stream, got: {content_type!r}"
    )

    deadline = time.monotonic() + LARGE_IO_TIMEOUT
    events = []
    parts = []
    for line in response.iter_lines(decode_unicode=True):
        if time.monotonic() > deadline:
            raise AssertionError(
                f"Stream collection exceeded {LARGE_IO_TIMEOUT}s deadline after {len(events)} events"
            )
        if not line:
            continue
        events.append(line)
        if not line.startswith("data: ") or line == "data: [DONE]":
            continue
        try:
            chunk = json.loads(line[6:])
            for choice in chunk.get("choices", []):
                content = choice.get("delta", {}).get("content")
                if content:
                    parts.append(content)
        except json.JSONDecodeError as exc:
            raise AssertionError(
                f"Unparseable SSE event (length={len(line)}): {exc}"
            ) from exc
    return events, "".join(parts)


@pytest.fixture(scope="module")
def large_io_access():
    """Grant a dedicated service account access to the echo model and mint an API key.

    The echo-sim fixture ships LLMIS + MaaSModelRef only (like distinct/distinct-2).
    This fixture creates the MaaSAuthPolicy and MaaSSubscription for the duration of
    the module and tears them down when the module completes.
    """
    ns = _ns()
    sa_name = "e2e-large-io-sa"
    auth_name = "e2e-large-io-auth"
    subscription_name = "e2e-large-io-sub"

    oc_token = _create_sa_token(sa_name, namespace=MODEL_NAMESPACE)
    sa_user = _sa_to_user(sa_name, namespace=MODEL_NAMESPACE)

    try:
        _create_test_auth_policy(auth_name, ECHO_MODEL_REF, users=[sa_user])
        # Use an explicit token budget rather than unlimited=True: the unlimited
        # field is a newer CRD addition and older clusters reject the subscription
        # with "spec.modelRefs[0].tokenRateLimits: Required value".
        _create_test_subscription(
            subscription_name,
            ECHO_MODEL_REF,
            users=[sa_user],
            token_limit=TOKEN_BUDGET,
            window="1m",
        )

        # Wait for the MaaSModelRef to reconcile its endpoint; without this the
        # first request can race and return 404/403 before the route is up.
        _wait_for_model_ready(ECHO_MODEL_REF, namespace=MODEL_NAMESPACE, timeout=120)
        _wait_for_gateway_auth_enforced()

        # The TRLP derived from the subscription reconciles separately from the
        # model and the gateway AuthPolicy. Until it is ready the gateway rejects
        # every request with 403 "subscription rate limiting policies are not ready".
        _wait_for_subscription_inference_ready(
            subscription_name,
            ECHO_MODEL_REF,
            namespace=ns,
            model_namespace=MODEL_NAMESPACE,
            timeout=180,
        )

        yield _create_api_key(oc_token, subscription=subscription_name)
    finally:
        _delete_governance_and_wait(
            subscriptions=[(subscription_name, ns)],
            auth_policies=[(auth_name, ns)],
        )


@pytest.mark.models
class TestLargeIOStream:
    """Large payload tests for the streaming (SSE) chat/completions code path.

    Each test sends a prompt to the echo-sim, collects the SSE stream, and
    reassembles the content from delta.content fields. The echo contract guarantees
    that a correct, unmodified round trip produces content == prompt exactly.

    Non-streaming coverage is provided by TestLargeIO in test_echo_large_io.py.
    """

    def test_stream_returns_200_and_done_sentinel(self, large_io_access):
        """Gateway must return HTTP 200, deliver at least one data chunk, and close with [DONE].

        A missing [DONE] or an empty event list indicates the gateway dropped or
        truncated the streaming response before it completed.
        """
        prompt = _lorem(LARGE_WORDS)

        with _echo_completion_stream(large_io_access, prompt) as r:
            assert r.status_code == 200, f"{r.status_code}: {r.headers}"
            events, _ = _collect_stream_content(r)

        data_chunks = [e for e in events if e.startswith("data: {")]
        assert data_chunks, f"Stream carried no data chunks. All events: {events[:10]}"
        assert events and events[-1] == "data: [DONE]", (
            f"Stream did not close with [DONE] as the final event. Last 3 events: {events[-3:]}"
        )

    def test_stream_content_equals_prompt(self, large_io_access):
        """Assembled SSE content must be byte-identical to the original prompt.

        Any truncation, mutation, or injection by a gateway filter (ext_proc, wasm,
        Envoy body buffer) on the streaming code path shows up as a mismatch here.
        """
        prompt = _lorem(LARGE_WORDS)

        with _echo_completion_stream(large_io_access, prompt) as r:
            assert r.status_code == 200, f"{r.status_code}: {r.headers}"
            _, content = _collect_stream_content(r)

        assert len(content) == len(prompt), (
            f"stream truncated or padded: sent {len(prompt)} bytes, got {len(content)}"
        )
        assert content == prompt, "stream body was mutated in transit"

    def test_stream_size_tracks_prompt_size(self, large_io_access):
        """Growing the prompt must grow the assembled stream content proportionally.

        A streaming buffer limit would show up as the larger response being clamped
        at a fixed ceiling while the smaller one passes unaffected. Both assembled
        contents must match their respective prompts exactly.
        """
        small = _lorem(SMALL_WORDS)
        large = _lorem(LARGE_WORDS)

        with _echo_completion_stream(large_io_access, small) as r:
            assert r.status_code == 200, f"{r.status_code} {str(r.headers)}"
            _, small_content = _collect_stream_content(r)

        with _echo_completion_stream(large_io_access, large) as r:
            assert r.status_code == 200, f"{r.status_code} {str(r.headers)}"
            _, large_content = _collect_stream_content(r)

        assert small_content == small, (
            f"small prompt mutated: sent {len(small)} bytes, got {len(small_content)}"
        )
        assert large_content == large, (
            f"large prompt mutated: sent {len(large)} bytes, got {len(large_content)}"
        )
        assert len(large_content) > len(small_content), (
            f"stream size did not grow with prompt: "
            f"small={len(small_content)} bytes, large={len(large_content)} bytes"
        )
