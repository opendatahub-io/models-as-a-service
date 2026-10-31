"""Client credentials must not reach the KServe simulator backend.

``test_negative_security.py`` checks that deny responses do not contain
``sk-oai-`` keys, but nothing checks that the gateway strips the client's
``Authorization`` header before the request is forwarded upstream. This module
closes that gap with the echo-mode simulator (``echo-simulator``).

How it works:

* The simulator's ``--log-http`` flag logs every request it receives, headers
  included, without redaction. If the client's Bearer API key appears in the
  simulator logs, the gateway forwarded it.
* A canary in the prompt is expected in the same log entry. Finding it proves
  ``--log-http`` is active and that the right pod's logs were read, so a pass
  cannot come from logging silently not working.

``--log-http`` writes secrets to pod logs by design, so it stays out of the
default fixtures. The module fixture appends it for this run only, the same
change the opt-in ``echo-sim-log-http`` overlay makes, and restores the original
args afterwards, failing if the flag is still present. The test key is revoked
on teardown, so a key that did reach the logs is already dead.

``--log-http`` needs ``llm-d-inference-sim`` v0.9.0 or newer, which the echo-sim
fixture pins.
"""

import json
import os
import re
import subprocess
import time
import uuid
from datetime import UTC, datetime, timedelta

import pytest
import requests
from multitenancy_helpers import redact_sensitive
from test_helper import (
    ECHO_MODEL_NAME,
    ECHO_MODEL_PATH,
    ECHO_MODEL_REF,
    MODEL_NAMESPACE,
    OC_TIMEOUT,
    TIMEOUT,
    TLS_VERIFY,
    _create_api_key_raw,
    _create_sa_token,
    _create_test_auth_policy,
    _create_test_subscription,
    _delete_governance_and_wait,
    _gateway_url,
    _ns,
    _revoke_api_key,
    _run_oc,
    _sa_to_user,
    _wait_for_gateway_auth_enforced,
    _wait_for_model_ready,
    _wait_for_subscription_inference_ready,
)

# Serial: the fixture restarts the shared echo-simulator pod and creating the
# MaaSAuthPolicy regenerates the shared gateway AuthPolicy.
pytestmark = [pytest.mark.xdist_group("echo_auth_leak"), pytest.mark.serial]

LOG_HTTP_FLAG = "--log-http"
# KServe names the simulator Deployment after the LLMIS.
ECHO_DEPLOYMENT = f"{ECHO_MODEL_REF}-kserve"
ROLLOUT_TIMEOUT = int(os.environ.get("E2E_ECHO_ROLLOUT_TIMEOUT", "300"))


class _RedactedKey(str):
    """An API key that hides itself in repr().

    pytest prints fixture values in failure tracebacks with repr(), which would put
    the raw key in the terminal and JUnit report. str() and f-strings still give the
    real value, so request headers and ``in`` checks are unaffected.
    """

    def __repr__(self):
        return "'sk-oai-<redacted>'"


def _oc_json(*args):
    result = _run_oc(["oc", *args, "-o", "json"])
    assert result.returncode == 0, redact_sensitive(result.stderr)
    return json.loads(result.stdout)


def _main_container(llmis):
    """Return (index, container) for the simulator container in the LLMIS template."""
    for i, c in enumerate(llmis["spec"]["template"]["containers"]):
        if c["name"] == "main":
            return i, c
    pytest.fail(f"LLMIS {ECHO_MODEL_REF} has no 'main' container")


def _patch_main_args(index, args):
    patch = [{"op": "replace", "path": f"/spec/template/containers/{index}/args", "value": args}]
    result = _run_oc([
        "oc", "patch", "llminferenceservice", ECHO_MODEL_REF, "-n", MODEL_NAMESPACE,
        "--type=json", "-p", json.dumps(patch),
    ])
    assert result.returncode == 0, redact_sensitive(result.stderr)


def _wait_for_rollout(*, log_http: bool):
    """Wait until the Deployment carries the wanted spec and that spec is fully rolled out.

    Polling the Deployment template first matters: right after the LLMIS patch the
    controller has not updated the Deployment yet, and ``rollout status`` would
    report the old, already-complete rollout.
    """
    deadline = time.monotonic() + ROLLOUT_TIMEOUT
    while True:
        deploy = _oc_json("get", "deployment", ECHO_DEPLOYMENT, "-n", MODEL_NAMESPACE)
        main = next(c for c in deploy["spec"]["template"]["spec"]["containers"] if c["name"] == "main")
        if (LOG_HTTP_FLAG in main.get("args", [])) == log_http:
            break
        if time.monotonic() > deadline:
            pytest.fail(f"Deployment {ECHO_DEPLOYMENT} never picked up log_http={log_http}")
        time.sleep(3)

    result = _run_oc(
        ["oc", "rollout", "status", f"deployment/{ECHO_DEPLOYMENT}", "-n", MODEL_NAMESPACE,
         f"--timeout={ROLLOUT_TIMEOUT}s"],
        timeout=ROLLOUT_TIMEOUT + 30,
    )
    if result.returncode != 0:
        # The usual cause is an image older than v0.9.0: the pod crashloops on "unknown flag".
        pytest.fail(
            f"Rollout of {ECHO_DEPLOYMENT} failed (log_http={log_http}); if the pod reports "
            f"'unknown flag: --log-http', the echo-sim image predates llm-d-inference-sim v0.9.0: "
            f"{redact_sensitive(result.stderr)}"
        )


def _simulator_logs(since: datetime) -> str:
    # Not _run_oc: it decodes stdout strictly as UTF-8, and --log-http writes
    # response bodies verbatim, so a compressed body puts raw bytes in the log.
    result = subprocess.run(
        ["oc", "logs", f"deployment/{ECHO_DEPLOYMENT}", "-n", MODEL_NAMESPACE, "-c", "main",
         f"--since-time={since.strftime('%Y-%m-%dT%H:%M:%SZ')}"],
        capture_output=True,
        check=False,
        timeout=OC_TIMEOUT,
    )
    stderr = result.stderr.decode("utf-8", errors="replace")
    assert result.returncode == 0, redact_sensitive(stderr)
    return result.stdout.decode("utf-8", errors="replace")


def _request_entry(logs: str, marker: str) -> str:
    """Return the klog entry (request line plus its headers and body) containing marker."""
    for entry in re.split(r"\n(?=[IWEF]\d{4} )", logs):
        if marker in entry:
            return entry
    return ""


def _echo_completion(api_key: str, prompt: str) -> requests.Response:
    return requests.post(
        f"{_gateway_url()}{ECHO_MODEL_PATH}/v1/chat/completions",
        # identity keeps the response body uncompressed, so --log-http logs it as text.
        headers={
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
            "Accept-Encoding": "identity",
        },
        json={
            "model": ECHO_MODEL_NAME,
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": 16,
        },
        timeout=TIMEOUT,
        verify=TLS_VERIFY,
    )


@pytest.fixture(scope="module")
def log_http_enabled():
    """Turn on --log-http for this module only; always restore the original spec."""
    llmis = _oc_json("get", "llminferenceservice", ECHO_MODEL_REF, "-n", MODEL_NAMESPACE)
    index, main = _main_container(llmis)
    original_args = list(main.get("args", []))
    assert LOG_HTTP_FLAG not in original_args, (
        f"{LOG_HTTP_FLAG} is already set on {ECHO_MODEL_REF}, probably from the echo-sim-log-http "
        f"overlay; restore with: kustomize build test/e2e/fixtures/echo-sim | oc apply -f -"
    )

    try:
        _patch_main_args(index, original_args + [LOG_HTTP_FLAG])
        _wait_for_rollout(log_http=True)
        yield
    finally:
        _patch_main_args(index, original_args)
        _wait_for_rollout(log_http=False)
        restored = _main_container(
            _oc_json("get", "llminferenceservice", ECHO_MODEL_REF, "-n", MODEL_NAMESPACE)
        )[1]
        assert LOG_HTTP_FLAG not in restored.get("args", []), (
            f"cleanup failed: {LOG_HTTP_FLAG} still set on {ECHO_MODEL_REF}"
        )


@pytest.fixture(scope="module")
def echo_key():
    """Grant a service account access to the echo model and mint a dedicated API key.

    The key is revoked on teardown, so if it does leak into logs it is already dead.
    """
    ns = _ns()
    sa_name = "e2e-echo-leak-sa"
    auth_name = "e2e-echo-leak-auth"
    subscription_name = "e2e-echo-leak-sub"

    oc_token = _create_sa_token(sa_name, namespace=MODEL_NAMESPACE)
    sa_user = _sa_to_user(sa_name, namespace=MODEL_NAMESPACE)
    key_id = None
    try:
        _create_test_auth_policy(auth_name, ECHO_MODEL_REF, users=[sa_user])
        # A token budget rather than unlimited=True: older MaaSSubscription CRDs
        # (before #1536) reject unlimited. These requests are tiny.
        _create_test_subscription(subscription_name, ECHO_MODEL_REF, users=[sa_user], token_limit=100000)
        # The MaaSModelRef stays Pending until a subscription and auth policy pair
        # exists; without this the first request can race the route.
        _wait_for_model_ready(ECHO_MODEL_REF, namespace=MODEL_NAMESPACE, timeout=120)
        _wait_for_gateway_auth_enforced()
        # Without this the first request can 403 before the rate-limit policy is ready.
        _wait_for_subscription_inference_ready(subscription_name, ECHO_MODEL_REF, namespace=ns)

        r = _create_api_key_raw(oc_token, name=f"e2e-echo-leak-{uuid.uuid4().hex[:8]}",
                                subscription=subscription_name)
        assert r.status_code in (200, 201), f"API key mint failed: {r.status_code} {redact_sensitive(r.text)}"
        body = r.json()
        key_id = body["id"]
        assert body["key"].startswith("sk-oai-"), "mint did not return an sk-oai- key"
        yield _RedactedKey(body["key"])
    finally:
        if key_id:
            _revoke_api_key(oc_token, key_id)
        _delete_governance_and_wait(
            subscriptions=[(subscription_name, ns)],
            auth_policies=[(auth_name, ns)],
        )


@pytest.mark.security
class TestClientAuthNotForwarded:
    """The gateway must strip the client's credential before forwarding to the model."""

    def test_client_bearer_not_in_simulator_logs(self, log_http_enabled, echo_key):
        """The client's API key must never appear in what the simulator received."""
        canary = f"leak-canary-{uuid.uuid4()}"
        # Small backdate absorbs clock skew between this host and the node.
        since = datetime.now(UTC) - timedelta(seconds=5)

        r = _echo_completion(echo_key, canary)
        assert r.status_code == 200, f"{r.status_code}: {redact_sensitive(r.text)}"

        logs = _simulator_logs(since)
        # Asserting on precomputed booleans keeps pytest's assertion rewriting from
        # printing the raw key and logs into the failure output and JUnit report.
        canary_logged = canary in logs
        key_leaked = echo_key in logs
        # Positive control: without it, a pass could just mean nothing was logged.
        assert canary_logged, (
            f"canary not found in {ECHO_DEPLOYMENT} logs; --log-http is not active or the wrong "
            f"pod was read. Log tail: {redact_sensitive(logs[-500:])}"
        )
        assert not key_leaked, (
            f"client API key reached the simulator backend. Request as logged by the simulator: "
            f"{redact_sensitive(_request_entry(logs, canary), max_length=2000)}"
        )

    def test_authorization_not_echoed_in_response(self, echo_key):
        """The response must not reflect the Authorization header back to the caller."""
        prompt = f"leak-probe-{uuid.uuid4()}"
        r = _echo_completion(echo_key, prompt)
        assert r.status_code == 200, f"{r.status_code}: {redact_sensitive(r.text)}"

        body = r.text
        # Booleans for the same reason as above: keep the raw key out of pytest output.
        key_reflected = echo_key in body
        bearer_reflected = "Bearer " in body
        header_reflected = "authorization" in body.lower()
        assert not key_reflected, f"API key reflected in response: {redact_sensitive(body)}"
        assert not bearer_reflected, f"Bearer token reflected in response: {redact_sensitive(body)}"
        assert not header_reflected, (
            f"Authorization header reflected in response: {redact_sensitive(body)}"
        )
