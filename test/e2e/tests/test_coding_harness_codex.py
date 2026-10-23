"""Codex coding-agent harness against a live MaaS gateway (phase 0/1).

Spawns a pinned Codex CLI configured for the MaaS OpenAI-compatible chat
endpoint (simulator or real model) using a minted API key.

Validation draft: this module is intentionally *not* marked ``nightly`` so it
runs in the normal Konflux /group-test e2e passes. After CI validation, flip
``pytestmark`` to include ``pytest.mark.nightly`` so it only runs in pass 3
when ``MAAS_PYTEST_INCLUDE_NIGHTLY=true`` (see test/e2e/README.md).

Optional env:
  MAAS_TEST_CODEX_BIN   Absolute path to a pre-staged Codex binary (skips download)
  E2E_CODEX_TIMEOUT     Seconds for ``codex exec`` (default 180)
"""

from __future__ import annotations

import logging
import os

import pytest
import requests

from conftest import TLS_VERIFY
from harness.codex import ensure_codex_bin, run_codex_exec
from test_helper import MODEL_NAME as SERVED_MODEL_NAME

log = logging.getLogger(__name__)

pytestmark = [
    pytest.mark.xdist_group("coding_harness"),
    pytest.mark.slow,
]


def _artifact_dir() -> str:
    return (
        os.environ.get("ARTIFACT_DIR")
        or os.environ.get("ARTIFACTS_DIR")
        or os.environ.get("LOG_DIR")
        or ""
    )


def _write_artifacts(stdout: str, stderr: str) -> None:
    root = _artifact_dir()
    if not root:
        return
    os.makedirs(root, exist_ok=True)
    out = os.path.join(root, "codex-exec.stdout.log")
    err = os.path.join(root, "codex-exec.stderr.log")
    with open(out, "w", encoding="utf-8") as f:
        f.write(stdout)
    with open(err, "w", encoding="utf-8") as f:
        f.write(stderr)
    log.info("Wrote Codex logs to %s and %s", out, err)


@pytest.fixture(scope="module")
def codex_bin() -> str:
    return str(ensure_codex_bin())


def test_codex_exec_reaches_maas_chat_completions(
    codex_bin: str,
    model_v1: str,
    api_key: str,
    api_key_headers: dict,
):
    """
    Given a ready MaaS model URL and API key,
    When pinned Codex runs a one-shot ``exec`` over chat/completions,
    Then Codex exits successfully (gateway + auth + wire_api=chat path works).
    """
    # Fail fast if the gateway itself cannot serve chat/completions.
    probe = requests.post(
        f"{model_v1}/chat/completions",
        headers=api_key_headers,
        json={
            "model": SERVED_MODEL_NAME,
            "messages": [{"role": "user", "content": "ping"}],
            "max_tokens": 8,
            "stream": False,
        },
        timeout=30,
        verify=TLS_VERIFY,
    )
    assert probe.status_code == 200, (
        f"preflight chat/completions failed: {probe.status_code} {probe.text[:500]}"
    )

    result = run_codex_exec(
        codex_bin=codex_bin,
        api_key=api_key,
        model_v1=model_v1,
        model=SERVED_MODEL_NAME,
    )
    _write_artifacts(result.stdout, result.stderr)

    assert not result.timed_out, (
        f"Codex timed out\n--- stdout ---\n{result.stdout[-4000:]}\n"
        f"--- stderr ---\n{result.stderr[-4000:]}"
    )
    assert result.returncode == 0, (
        f"Codex exited {result.returncode}\n--- stdout ---\n{result.stdout[-4000:]}\n"
        f"--- stderr ---\n{result.stderr[-4000:]}"
    )

    combined = f"{result.stdout}\n{result.stderr}".lower()
    # Soft signal that the client completed a model turn; exact "PONG" is not
    # required because the vLLM simulator does not follow instructions reliably.
    assert any(
        token in combined
        for token in ("pong", "agent_message", "message", "turn.completed", "completed")
    ), (
        "Codex succeeded but output lacked an expected completion marker; "
        f"inspect artifacts\n--- stdout ---\n{result.stdout[-2000:]}"
    )
