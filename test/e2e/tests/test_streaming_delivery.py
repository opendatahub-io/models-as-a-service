"""
Streaming delivery e2e: tokens must reach the client as they are generated.

Why this file exists
--------------------
``test_smoke.py::test_chat_completions_streaming_sse`` proves the *shape* of a
stream: HTTP 200, ``text/event-stream``, parseable ``data:`` chunks, and a
terminal ``data: [DONE]``. All of that is still true when the response is fully
buffered and then flushed in a single burst at the end, so the shape assertions
cannot distinguish streaming from buffering.

That distinction is a product behaviour, not a detail. The payload-processing
ext_proc filter on the gateway sets ``response_body_mode``; under ``BUFFERED``
the whole response body is held until generation completes, so a client's
time-to-first-token equals the full generation time and streaming buys nothing.
Nothing in the MaaS suite noticed when that was the shipped behaviour.

This test measures delivery instead of shape: each SSE line is timestamped as it
is read off the socket, and the longest gap between consecutive ``data:`` chunks
must be close to the backend's per-token latency. A buffered response arrives in
one burst, so every gap collapses to ~0 no matter how slow the model is.

The largest gap rather than the first-to-last span: it does not depend on how many
tokens the backend produces, and intermediaries coalesce writes, so some gaps are
~0 even while the body is genuinely streamed. The fixture's echo mode and fixed
prompt make the chunk count deterministic as well.

The backend is the dedicated, paced ``fixtures/slow-stream`` simulator
(2s to first token, then 100ms per token). It is dedicated so this file cannot
slow down -- or be skewed by -- the shared simulators every other test uses.
Measured against an unpaced simulator this test is meaningless, which is why
``min_first_data_sec`` also asserts the fixture is still paced.

Requires:
  - GATEWAY_HOST and the usual test_helper environment (see its module docstring)
  - fixtures applied (``kustomize build test/e2e/fixtures/ | kubectl apply -f -``)
"""

import logging
import time
import uuid

import pytest
import requests
from test_helper import (
    MODEL_NAMESPACE,
    SLOW_STREAM_MODEL_ID,
    SLOW_STREAM_MODEL_PATH,
    SLOW_STREAM_MODEL_REF,
    TIMEOUT,
    TLS_VERIFY,
    _create_api_key_with_id,
    _create_sa_token,
    _create_test_auth_policy,
    _create_test_subscription,
    _delete_governance_and_wait,
    _delete_sa,
    _gateway_url,
    _ns,
    _poll_status,
    _revoke_api_key,
    _sa_to_user,
    _wait_for_gateway_auth_enforced,
    _wait_for_model_ready,
    _wait_for_subscription_inference_ready,
    assert_openai_sse_chat_stream,
)

log = logging.getLogger(__name__)

pytestmark = [
    pytest.mark.full_suite,
    pytest.mark.serial,
    pytest.mark.xdist_group("streaming_delivery"),
]

# The fixture is built with --time-to-first-token 2000ms --inter-token-latency 100ms.
FIXTURE_TTFT_SEC = 2.0
FIXTURE_INTER_TOKEN_SEC = 0.1

MAX_TOKENS = 64
# The fixture echoes the last message, so this fixed prompt (20 words) is what
# determines the response length: ~23 SSE chunks, delivered over ~1.9s at 100ms
# per token. Keeping it fixed matters -- a random-length response gives too few
# chunks to measure on some runs.
PROMPT = (
    "one two three four five six seven eight nine ten "
    "eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen nineteen twenty"
)
# The fixture emits a token every ~100ms, so at least one inter-chunk gap should be
# ~100ms. Requiring only half of that is decisive rather than marginal: buffered
# delivery leaves every gap at ~0, while batching on the way can shrink some gaps
# but not all of them. The largest gap, not the median, for that reason.
MIN_GAP_SEC = FIXTURE_INTER_TOKEN_SEC / 2
# The fixture waits 2s before emitting anything. Requiring at least 1s proves the
# backend is still paced; without it a fast backend would fail the gap assertion
# and look like a buffering regression. Measured from the moment the request is
# issued (see ``started_at``), because the gateway does not release the response
# headers until the first body bytes exist: the client's POST returns at TTFT, so
# relative to the response object the first chunk reads at ~0s either way.
MIN_FIRST_DATA_SEC = 1.0


def _stream_chat(api_key: str, *, max_tokens: int = MAX_TOKENS) -> requests.Response:
    """Open a streaming chat/completions request against the paced model."""
    return requests.post(
        f"{_gateway_url()}{SLOW_STREAM_MODEL_PATH}/v1/chat/completions",
        headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
        json={
            "model": SLOW_STREAM_MODEL_ID,
            "messages": [{"role": "user", "content": PROMPT}],
            "max_tokens": max_tokens,
            "stream": True,
        },
        # Connect/read timeout must comfortably exceed TTFT + generation.
        timeout=(TIMEOUT, TIMEOUT),
        verify=TLS_VERIFY,
        stream=True,
    )


def test_sse_response_is_delivered_incrementally():
    """
    Given a paced model behind the gateway and a valid API key, a streaming
    request must deliver tokens progressively rather than in one burst at
    completion.

    Fails if the response body is buffered anywhere in front of the model --
    typically ``response_body_mode: BUFFERED`` on the payload-processing ext_proc
    filter, which holds the body until generation finishes.
    """
    suffix = uuid.uuid4().hex[:8]
    auth_name = f"e2e-stream-auth-{suffix}"
    subscription_name = f"e2e-stream-sub-{suffix}"
    sa_name = f"e2e-stream-sa-{suffix}"
    key_name = f"e2e-stream-key-{suffix}"
    ns = _ns()

    sa_token = None
    api_key = None
    key_id = None

    try:
        sa_token = _create_sa_token(sa_name, namespace=MODEL_NAMESPACE)
        sa_user = _sa_to_user(sa_name, namespace=MODEL_NAMESPACE)

        # Governance first. The MaaSModelRef ships Pending at
        # "GovernanceAttached=False" and only publishes its endpoint once a
        # subscription and auth policy pair exists, so waiting for the model
        # before creating them would always time out.
        log.info("Creating auth policy and subscription for %s...", SLOW_STREAM_MODEL_REF)
        _create_test_auth_policy(auth_name, SLOW_STREAM_MODEL_REF, users=[sa_user])
        _create_test_subscription(
            subscription_name,
            SLOW_STREAM_MODEL_REF,
            users=[sa_user],
            # A 20-token echo is well inside the default budget, but the
            # non-streaming probe below plus retries make an explicit ceiling
            # cheaper than diagnosing a 429 as a streaming failure.
            token_limit=10_000,
        )

        log.info("Waiting for the paced model %s to be ready...", SLOW_STREAM_MODEL_REF)
        _wait_for_model_ready(SLOW_STREAM_MODEL_REF, namespace=MODEL_NAMESPACE)
        _wait_for_gateway_auth_enforced()
        # A Ready model and an enforced gateway policy are not enough: the TRLP
        # derived from the subscription reconciles separately, and until it does
        # the gateway answers 403 "subscription rate limiting policies are not
        # ready" -- which would look like an auth failure here.
        _wait_for_subscription_inference_ready(
            subscription_name,
            SLOW_STREAM_MODEL_REF,
            namespace=ns,
            model_namespace=MODEL_NAMESPACE,
        )

        api_key, key_id = _create_api_key_with_id(
            sa_token, name=key_name, subscription=subscription_name
        )

        # Non-streaming first: proves auth/routing work before timing anything,
        # so a 401/403 is never reported as a streaming failure.
        _poll_status(
            api_key,
            200,
            path=SLOW_STREAM_MODEL_PATH,
            model_name=SLOW_STREAM_MODEL_ID,
        )

        log.info(
            "Streaming a %d-word echo from a backend paced at %.1fs TTFT / %.0fms per token...",
            len(PROMPT.split()),
            FIXTURE_TTFT_SEC,
            FIXTURE_INTER_TOKEN_SEC * 1000,
        )
        started_at = time.monotonic()
        with _stream_chat(api_key) as streamed:
            events = assert_openai_sse_chat_stream(
                streamed,
                min_inter_chunk_gap_sec=MIN_GAP_SEC,
                min_first_data_sec=MIN_FIRST_DATA_SEC,
                started_at=started_at,
            )

        log.info(
            "✅ %d SSE lines delivered incrementally (largest gap >= %.3fs required)",
            len(events),
            MIN_GAP_SEC,
        )

    finally:
        if key_id:
            try:
                _revoke_api_key(sa_token, key_id)
            except Exception:  # noqa: BLE001 - cleanup must not mask the real failure
                log.warning("Failed to revoke API key %s during cleanup", key_name, exc_info=True)
        try:
            _delete_governance_and_wait(
                subscriptions=[(subscription_name, ns)],
                auth_policies=[(auth_name, ns)],
            )
        except Exception:  # noqa: BLE001
            log.warning("Failed to delete governance CRs during cleanup", exc_info=True)
        try:
            _delete_sa(sa_name, namespace=MODEL_NAMESPACE)
        except Exception:  # noqa: BLE001
            log.warning("Failed to delete service account %s during cleanup", sa_name, exc_info=True)
