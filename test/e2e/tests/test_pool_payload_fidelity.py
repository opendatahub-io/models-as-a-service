"""
Payload fidelity through a pool-backed echo model.

On a pool-backed model the endpoint picker (EPP) and the payload-processing ext_proc
(ipp) both process the response body. A fault in that pairing does not show in the
status: on Envoy 1.36 (OCP 4.22) a response can end early and still arrive as a 200
that carries the picker's response header (envoy#47449). Every request here is
therefore judged by its payload. The echo simulator returns the prompt verbatim, so the
completion must equal the prompt byte for byte.

The shared simulator fixtures are served without a scheduler and never reach the EPP,
so this module creates its own pool-backed echo model. Requests are body-routed, the
route that stays when per-model paths are turned off
(serving.kserve.io/model-based-routing-only), and the one whose picker binding the
fixture checks.

It covers the request shapes that complete on Envoy 1.36: a small non-streaming
completion and a long SSE stream. Larger non-streaming bodies and short streams fail
there intermittently and are not covered here.

Environment Variables:
- E2E_EPP_POOL_TIMEOUT: Seconds to wait for the first response picked by the pool's EPP
  (default: 300)
- E2E_EPP_ROUTE_TIMEOUT: Seconds to wait until every gateway replica binds the pool's
  route to its EPP (default: 120)
"""

from __future__ import annotations

import json
import logging
import random
import uuid
from collections.abc import Callable
from dataclasses import dataclass, field

import pytest
import requests
from multitenancy_helpers import (
    DEFAULT_GATEWAY_NAME,
    GATEWAY_NAMESPACE,
    wait_for_llmisvc_backend_ready,
    wait_until,
)
from test_gateway_filter_chain import (
    EPP_RESPONSE_HEADER,
    IPP,
    POOL_READY_TIMEOUT,
    ROUTE_BIND_TIMEOUT,
    _listener_state,
    _log_pool_diagnostics,
    _pool_routes,
)
from test_helper import (
    MODEL_NAMESPACE,
    _create_api_key,
    _create_llmis,
    _create_maas_model_ref,
    _create_sa_token,
    _create_test_auth_policy,
    _create_test_subscription,
    _delete_cr,
    _delete_sa,
    _gateway_url,
    _sa_to_user,
    _wait_for_cr_absent,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
    _wait_for_model_ready,
    assert_openai_sse_chat_stream,
    chat,
)

log = logging.getLogger(__name__)

pytestmark = [
    pytest.mark.full_suite,
    pytest.mark.serial,
    # Same lane as test_inference_priority.py, the other module that puts a pool on the
    # default gateway, so the two pools never coexist even with several serial workers.
    pytest.mark.xdist_group("inference_priority"),
]

SMALL_PROMPT_BYTES = 2_000
LARGE_PROMPT_BYTES = 470_000
REQUESTS_PER_SHAPE = 5
# A 470 KB echo bills about 46k tokens, prompt and completion together.
TOKEN_BUDGET = 10_000_000
# (connect, read). The read timeout also covers the wait for the first byte, and a
# tenant that buffers response bodies held a 470 KB stream for up to 15 s.
REQUEST_TIMEOUT = (10, 120)
# Long words keep a 470 KB prompt at about 23k tokens, so its stream is about 23k SSE
# events (6.1 MB). The simulator streams one event per token.
WORDS = [
    "incomprehensibilities", "counterrevolutionaries", "electroencephalograph",
    "uncharacteristically", "telecommunications", "disproportionately",
    "interchangeability", "misrepresentations",
]


@dataclass(frozen=True)
class PoolEcho:
    """The fixture's model and its API key.

    The key stays out of repr: pytest prints test and helper arguments in failure
    tracebacks, and the runner keeps them in its JUnit and HTML reports.
    """

    llmis: str
    model: str
    api_key: str = field(repr=False)

    def complete(self, prompt: str, stream: bool = False) -> requests.Response:
        """Body-routed chat completion: the model is only named in the body."""
        headers = {"Authorization": f"Bearer {self.api_key}", "Content-Type": "application/json"}
        return chat(prompt, f"{_gateway_url()}/v1", headers, self.model, stream=stream, timeout=REQUEST_TIMEOUT)

    def redact(self, text: str) -> str:
        return text.replace(self.api_key, "<redacted>")


def _prompt(size: int, seed: int) -> str:
    """Seeded ASCII prompt of the given size in bytes."""
    rnd = random.Random(seed)
    words, length = [], 0
    while length < size:
        word = rnd.choice(WORDS)
        words.append(word)
        length += len(word) + 1
    return " ".join(words)[:size].rstrip()


def _message_content(response: requests.Response) -> str:
    return response.json()["choices"][0]["message"]["content"]


def _stream_content(response: requests.Response) -> str:
    events = assert_openai_sse_chat_stream(response)
    parts = []
    for event in events:
        if event.startswith("data: {"):
            for choice in json.loads(event[len("data: "):]).get("choices") or []:
                parts.append((choice.get("delta") or {}).get("content") or "")
    return "".join(parts)


def _problem(response: requests.Response, prompt: str,
             content_of: Callable[[requests.Response], str]) -> str | None:
    """Why the response is not the picker-handled echo of the prompt, or None."""
    if response.status_code != 200:
        return f"HTTP {response.status_code}: {response.text[:300]}"
    if response.headers.get(EPP_RESPONSE_HEADER) != "true":
        return f"response skipped the endpoint picker: {dict(response.headers)}"
    try:
        content = content_of(response)
    except (AssertionError, ValueError, LookupError, requests.RequestException) as exc:
        return f"no complete echo in the body: {exc!r}"
    if content == prompt:
        return None
    offset = next((i for i, (a, b) in enumerate(zip(prompt, content)) if a != b),
                  min(len(prompt), len(content)))
    return f"sent {len(prompt)} bytes, got {len(content)} back, first difference at offset {offset}"


def _round_trip(pool: PoolEcho, prompt: str, stream: bool = False) -> str | None:
    """Send the prompt and say why it did not come back whole, or None.

    A failed request is returned as text rather than raised, so no traceback reaches
    the frame whose arguments carry the key.
    """
    content_of = _stream_content if stream else _message_content
    try:
        with pool.complete(prompt, stream=stream) as response:
            return _problem(response, prompt, content_of)
    except requests.RequestException as exc:
        return f"request failed: {type(exc).__name__}: {exc}"


def _check(pool: PoolEcho, prompt: str, stream: bool = False):
    """Fail with the pool's state logged unless the prompt comes back whole."""
    problem = _round_trip(pool, prompt, stream)
    if problem:
        _log_pool_diagnostics(pool.llmis)
        pytest.fail(pool.redact(problem))


def _log_ipp_modes():
    """Log the ipp processing modes on the gateway listeners, so a run says what it tested."""
    modes = set()
    for chains, _ in _listener_state(DEFAULT_GATEWAY_NAME).values():
        for _, filters in chains:
            for http_filter in filters:
                if http_filter.get("name") == IPP:
                    mode = (http_filter.get("typed_config") or {}).get("processing_mode") or {}
                    modes.add(json.dumps(mode, sort_keys=True))
    log.info("%s processing modes on %s: %s", IPP, DEFAULT_GATEWAY_NAME, sorted(modes) or "no ipp filter")


@pytest.fixture(scope="module")
def pool_echo():
    """A pool-backed echo model, granted to a dedicated service account only."""
    suffix = uuid.uuid4().hex[:8]
    llmis = f"e2e-pool-echo-{suffix}"
    policy = f"e2e-pool-echo-access-{suffix}"
    subscription = f"e2e-pool-echo-sub-{suffix}"
    sa_name = f"e2e-pool-echo-{suffix}"
    served_id = f"test/e2e-pool-echo-{suffix}"
    model = f"publishers/{MODEL_NAMESPACE}/models/{served_id}"
    picker_host = f"{llmis}-epp-service.{MODEL_NAMESPACE}."

    try:
        log.info("Creating pool-backed echo model %s/%s", MODEL_NAMESPACE, llmis)
        _create_llmis(
            llmis, MODEL_NAMESPACE, DEFAULT_GATEWAY_NAME, GATEWAY_NAMESPACE,
            model_name=served_id, scheduler=True, echo=True,
            # KServe gives every service the same route rule names, and istiod before
            # 1.29.9 binds InferencePool pickers by rule name, so another pool on the
            # gateway could take over this model's routes.
            spec_annotations={"serving.kserve.io/route-rule-naming": "unique"},
        )
        wait_for_llmisvc_backend_ready(llmis, MODEL_NAMESPACE, DEFAULT_GATEWAY_NAME)
        _create_maas_model_ref(llmis, MODEL_NAMESPACE, llmis)

        sa_token = _create_sa_token(sa_name, duration="30m")
        sa_user = _sa_to_user(sa_name)
        _create_test_auth_policy(policy, llmis, users=[sa_user])
        _create_test_subscription(subscription, llmis, users=[sa_user], token_limit=TOKEN_BUDGET)
        _wait_for_maas_auth_policy_phase(policy)
        _wait_for_maas_subscription_phase(subscription)
        _wait_for_model_ready(llmis, namespace=MODEL_NAMESPACE)
        pool = PoolEcho(llmis, model, _create_api_key(sa_token, name=f"e2e-pool-echo-{suffix}",
                                                       subscription=subscription))

        # Every replica binds the body-routed rule for this model to its own picker.
        routes = wait_until(lambda: _pool_routes(DEFAULT_GATEWAY_NAME, picker_host), ROUTE_BIND_TIMEOUT,
                            f"not every {DEFAULT_GATEWAY_NAME} replica bound endpoint picker {picker_host}")
        for pod, route in routes.items():
            assert route.model == model, f"{pod}: picker-bound route matches model {route.model!r}, expected {model!r}"
        _log_ipp_modes()

        # Auth needs to propagate and the pool fails open until its picker serves.
        def echoed():
            problem = _round_trip(pool, "pool echo ready")
            assert problem is None, pool.redact(problem)
            return True

        wait_until(echoed, POOL_READY_TIMEOUT, "pool echo model never answered through its endpoint picker")
        yield pool
    except Exception:
        _log_pool_diagnostics(llmis)
        raise
    finally:
        _delete_cr("maassubscription", subscription)
        _delete_cr("maasauthpolicy", policy)
        _delete_cr("maasmodelref", llmis, namespace=MODEL_NAMESPACE)
        _delete_cr("llminferenceservice", llmis, namespace=MODEL_NAMESPACE)
        _delete_sa(sa_name)
        _wait_for_cr_absent("maassubscription", subscription)
        _wait_for_cr_absent("maasauthpolicy", policy)
        _wait_for_cr_absent("maasmodelref", llmis, namespace=MODEL_NAMESPACE)
        # LLMIS teardown can lag the default 30s CR-absent wait under parallel load.
        _wait_for_cr_absent("llminferenceservice", llmis, namespace=MODEL_NAMESPACE, timeout=180)


class TestPoolPayloadFidelity:
    """Responses from a pool-backed model reach the client whole."""

    def test_small_completion_round_trips(self, pool_echo):
        """2 KB non-streaming completions come back byte for byte."""
        for seed in range(REQUESTS_PER_SHAPE):
            _check(pool_echo, _prompt(SMALL_PROMPT_BYTES, seed))

    def test_large_stream_round_trips(self, pool_echo):
        """470 KB prompts stream back byte for byte, ending with [DONE]."""
        for seed in range(REQUESTS_PER_SHAPE):
            _check(pool_echo, _prompt(LARGE_PROMPT_BYTES, seed), stream=True)
