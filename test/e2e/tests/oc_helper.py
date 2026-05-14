"""Shared `oc` invocation helpers for E2E tests (absolute binary, timeouts, stdin closed)."""

from __future__ import annotations

import json
import os
import shutil
import subprocess


_OC_TIMEOUT = int(os.environ.get("E2E_OC_TIMEOUT", "60"))


def oc_bin() -> str:
    path = shutil.which("oc")
    if not path:
        raise RuntimeError("`oc` binary not found in PATH")
    return path


def oc_run(
    args: list[str],
    *,
    timeout: int | None = None,
) -> subprocess.CompletedProcess[str]:
    """Run `oc` with capture; does not raise on non-zero exit."""
    return subprocess.run(
        [oc_bin(), *args],
        capture_output=True,
        text=True,
        timeout=_OC_TIMEOUT if timeout is None else timeout,
        stdin=subprocess.DEVNULL,
        check=False,
    )


def oc_not_found(exc: subprocess.CalledProcessError) -> bool:
    """True when `oc` failed because the requested API object was not found."""
    combined = (exc.stderr or "") + (exc.stdout or "")
    return "(NotFound)" in combined


def oc_json(args: list[str]) -> dict:
    """Run `oc` and parse JSON; raises CalledProcessError on failure."""
    result = oc_run(args)
    if result.returncode != 0:
        raise subprocess.CalledProcessError(
            result.returncode,
            [oc_bin(), *args],
            result.stdout,
            result.stderr,
        )
    return json.loads(result.stdout)
