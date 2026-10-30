"""Pinned Codex CLI install + non-interactive exec against a MaaS gateway.

Mirrors the Praxis pinned-Codex pattern (download + checksum + isolated
CODEX_HOME), but targets a live MaaS OpenAI-compatible model URL with an API
key instead of a local scripted backend.
"""

from __future__ import annotations

import hashlib
import logging
import os
import platform
import shutil
import subprocess
import tarfile
import tempfile
import urllib.request
from dataclasses import dataclass
from pathlib import Path

log = logging.getLogger(__name__)

# Keep in sync with fixtures/codex-cli/<ver>-*.sha256 and any CI install notes.
CODEX_VERSION = "0.144.1"
CODEX_VERSION_OUTPUT = f"codex-cli {CODEX_VERSION}"
CODEX_ARCHIVE = "codex-x86_64-unknown-linux-musl.tar.gz"
CODEX_BIN_NAME = "codex-x86_64-unknown-linux-musl"
CODEX_RELEASE_URL = (
    f"https://github.com/openai/codex/releases/download/rust-v{CODEX_VERSION}/{CODEX_ARCHIVE}"
)

_FIXTURES_DIR = Path(__file__).resolve().parents[1] / "fixtures" / "codex-cli"
_SHA256_FIXTURE = _FIXTURES_DIR / f"{CODEX_VERSION}-x86_64-unknown-linux-musl.sha256"

# Env overrides (Praxis-style): skip download when a binary is already staged.
CODEX_BIN_ENV = "MAAS_TEST_CODEX_BIN"
API_KEY_ENV = "MAAS_CODEX_API_KEY"

DEFAULT_PROMPT = "Reply with exactly PONG. Do not call tools or edit files."
DEFAULT_TIMEOUT_SEC = int(os.environ.get("E2E_CODEX_TIMEOUT", "180"))


@dataclass(frozen=True)
class CodexRunResult:
    returncode: int
    stdout: str
    stderr: str
    timed_out: bool
    codex_home: Path
    work_dir: Path


def _expected_sha256() -> str:
    line = _SHA256_FIXTURE.read_text(encoding="utf-8").strip().split()
    if not line:
        raise RuntimeError(f"empty checksum fixture: {_SHA256_FIXTURE}")
    return line[0]


def _cache_dir() -> Path:
    root = os.environ.get("ARTIFACT_DIR") or os.environ.get("ARTIFACTS_DIR") or ""
    if root:
        path = Path(root) / "codex-cli" / CODEX_VERSION
    else:
        path = Path(tempfile.gettempdir()) / "maas-e2e-codex-cli" / CODEX_VERSION
    path.mkdir(parents=True, exist_ok=True)
    return path


def ensure_codex_bin() -> Path:
    """Return an executable pinned Codex binary, downloading if needed."""
    override = os.environ.get(CODEX_BIN_ENV, "").strip()
    if override:
        path = Path(override)
        if not path.is_file() or not os.access(path, os.X_OK):
            raise RuntimeError(f"{CODEX_BIN_ENV}={override!r} is not an executable file")
        _assert_version(path)
        return path

    machine = platform.machine().lower()
    if machine not in ("x86_64", "amd64"):
        raise RuntimeError(
            f"pinned Codex e2e binary is x86_64-only for now; got machine={machine!r}"
        )

    cache = _cache_dir()
    archive_path = cache / CODEX_ARCHIVE
    bin_path = cache / CODEX_BIN_NAME
    expected = _expected_sha256()

    if not bin_path.is_file():
        if not archive_path.is_file() or _sha256_file(archive_path) != expected:
            log.info("Downloading pinned Codex CLI from %s", CODEX_RELEASE_URL)
            urllib.request.urlretrieve(CODEX_RELEASE_URL, archive_path)  # noqa: S310
        actual = _sha256_file(archive_path)
        if actual != expected:
            raise RuntimeError(
                f"Codex archive checksum mismatch: expected {expected}, got {actual}"
            )
        with tarfile.open(archive_path, "r:gz") as tar:
            # filter= was added in Python 3.12; CI may still be on 3.11.
            try:
                tar.extractall(path=cache, filter="data")
            except TypeError:
                tar.extractall(path=cache)
        extracted = cache / CODEX_BIN_NAME
        if not extracted.is_file():
            raise RuntimeError(f"Codex archive did not contain {CODEX_BIN_NAME}")
        extracted.chmod(0o755)
        if extracted.resolve() != bin_path.resolve():
            shutil.copy2(extracted, bin_path)
            bin_path.chmod(0o755)

    _assert_version(bin_path)
    return bin_path


def _sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def _assert_version(bin_path: Path) -> None:
    proc = subprocess.run(
        [str(bin_path), "--version"],
        check=False,
        capture_output=True,
        text=True,
        timeout=30,
    )
    version = (proc.stdout or proc.stderr or "").strip()
    if proc.returncode != 0 or version != CODEX_VERSION_OUTPUT:
        raise RuntimeError(
            f"unexpected Codex version from {bin_path}: "
            f"rc={proc.returncode} out={version!r} expected={CODEX_VERSION_OUTPUT!r}"
        )


def write_codex_home(*, base_url: str, model: str) -> Path:
    """Create an isolated CODEX_HOME with a MaaS chat-completions provider."""
    home = Path(tempfile.mkdtemp(prefix="maas-codex-home-"))
    # wire_api=chat: MaaS simulators expose OpenAI chat/completions, not Responses.
    config = f"""model = "{model}"
model_provider = "maas"
approval_policy = "never"
sandbox_mode = "read-only"
web_search = "disabled"

[model_providers.maas]
name = "MaaS gateway"
base_url = "{base_url.rstrip("/")}"
wire_api = "chat"
env_key = "{API_KEY_ENV}"
"""
    (home / "config.toml").write_text(config, encoding="utf-8")
    return home


def run_codex_exec(
    *,
    codex_bin: Path | str,
    api_key: str,
    model_v1: str,
    model: str,
    prompt: str = DEFAULT_PROMPT,
    timeout_sec: int = DEFAULT_TIMEOUT_SEC,
) -> CodexRunResult:
    """Run `codex exec` non-interactively against model_v1 (…/v1)."""
    bin_path = Path(codex_bin)
    codex_home = write_codex_home(base_url=model_v1, model=model)
    work_dir = Path(tempfile.mkdtemp(prefix="maas-codex-work-"))
    (work_dir / "README.md").write_text(
        "# MaaS Codex e2e workspace\n\nNo tools required; answer the prompt only.\n",
        encoding="utf-8",
    )

    cmd = [
        str(bin_path),
        "exec",
        "--ephemeral",
        "--skip-git-repo-check",
        "--sandbox",
        "read-only",
        "--json",
        prompt,
    ]
    env = {
        "HOME": str(codex_home),
        "CODEX_HOME": str(codex_home),
        API_KEY_ENV: api_key,
        "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
        "RUST_LOG": os.environ.get("RUST_LOG", "error"),
    }
    # Propagate TLS skip only if the cluster uses it for Python clients too;
    # Codex/reqwest may still enforce certs — failures surface in stderr.
    if os.environ.get("E2E_SKIP_TLS_VERIFY", "").lower() == "true":
        env["NODE_TLS_REJECT_UNAUTHORIZED"] = "0"

    log.info("Running Codex: %s (model=%s base=%s)", " ".join(cmd), model, model_v1)
    try:
        proc = subprocess.run(
            cmd,
            check=False,
            capture_output=True,
            text=True,
            timeout=timeout_sec,
            cwd=str(work_dir),
            env=env,
        )
        return CodexRunResult(
            returncode=proc.returncode,
            stdout=proc.stdout or "",
            stderr=proc.stderr or "",
            timed_out=False,
            codex_home=codex_home,
            work_dir=work_dir,
        )
    except subprocess.TimeoutExpired as exc:
        return CodexRunResult(
            returncode=-1,
            stdout=(exc.stdout or "") if isinstance(exc.stdout, str) else "",
            stderr=(exc.stderr or "") if isinstance(exc.stderr, str) else f"timed out after {timeout_sec}s",
            timed_out=True,
            codex_home=codex_home,
            work_dir=work_dir,
        )
