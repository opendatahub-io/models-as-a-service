#!/usr/bin/env python3
"""Fail when a PR adds E2E tests without an explicit coverage tier.

New tests under test/e2e/tests/ must carry one of:
  - smoke      → runs in /group-test (E2E_SMOKE_ONLY=true)
  - full_suite → intentionally off the smoke path; still runs when smoke-only is off
  - nightly    → pass-3 / scheduled extended coverage

Existing unmarked tests are grandfathered: only tests added relative to --base
are checked. Class/module-level markers count for nested methods.

Also used by run_e2e_tests.sh (--list-new): print pytest node ids for newly
added tests so smoke-only CI can run curated smoke ∪ PR-new (via the dynamic
pr_new marker) without rewriting source to add @pytest.mark.smoke.

Usage:
  python test/e2e/scripts/check_new_e2e_test_tiers.py --base origin/main
  python test/e2e/scripts/check_new_e2e_test_tiers.py --base "$BASE_SHA"
  python test/e2e/scripts/check_new_e2e_test_tiers.py --base "$BASE_SHA" --list-new
"""

from __future__ import annotations

import argparse
import ast
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[3]
TESTS_DIR = REPO_ROOT / "test" / "e2e" / "tests"
# Markers that satisfy the new-test gate (any one is enough).
TIER_MARKERS = frozenset({"smoke", "full_suite", "nightly"})


def _run_git(*args: str) -> str:
    result = subprocess.run(
        ["git", *args],
        cwd=REPO_ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    if result.returncode != 0:
        raise RuntimeError(
            f"git {' '.join(args)} failed: {result.stderr.strip() or result.stdout.strip()}"
        )
    return result.stdout


def _marker_names_from_decorator(node: ast.AST) -> set[str]:
    """Extract pytest.mark.<name> from a decorator expression."""
    names: set[str] = set()
    target = node
    # @pytest.mark.smoke()  → Call
    if isinstance(target, ast.Call):
        target = target.func
    # pytest.mark.smoke
    if (
        isinstance(target, ast.Attribute)
        and isinstance(target.value, ast.Attribute)
        and target.value.attr == "mark"
        and isinstance(target.value.value, ast.Name)
        and target.value.value.id == "pytest"
    ):
        names.add(target.attr)
    return names


def _markers_from_pytestmark(assign_value: ast.AST) -> set[str]:
    """Markers from module/class pytestmark = ... assignments."""
    names: set[str] = set()
    values: list[ast.AST]
    if isinstance(assign_value, (ast.List, ast.Tuple)):
        values = list(assign_value.elts)
    else:
        values = [assign_value]
    for elt in values:
        names |= _marker_names_from_decorator(elt)
    return names


def _collect_tests(source: str, path: Path) -> dict[str, set[str]]:
    """Map qualified test id → effective tier markers (module ∪ class ∪ function)."""
    try:
        tree = ast.parse(source, filename=str(path))
    except SyntaxError as exc:
        raise RuntimeError(f"failed to parse {path}: {exc}") from exc

    module_markers: set[str] = set()
    for node in tree.body:
        if isinstance(node, ast.Assign):
            for target in node.targets:
                if isinstance(target, ast.Name) and target.id == "pytestmark":
                    module_markers |= _markers_from_pytestmark(node.value)
        elif isinstance(node, ast.AnnAssign) and isinstance(node.target, ast.Name):
            if node.target.id == "pytestmark" and node.value is not None:
                module_markers |= _markers_from_pytestmark(node.value)

    tests: dict[str, set[str]] = {}

    def add_function(fn: ast.FunctionDef | ast.AsyncFunctionDef, prefix: str, inherited: set[str]) -> None:
        if not fn.name.startswith("test_"):
            return
        markers = set(inherited)
        for dec in fn.decorator_list:
            markers |= _marker_names_from_decorator(dec)
        qual = f"{prefix}::{fn.name}" if prefix else fn.name
        tests[qual] = markers & TIER_MARKERS

    for node in tree.body:
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            add_function(node, "", module_markers)
        elif isinstance(node, ast.ClassDef):
            class_markers = set(module_markers)
            for dec in node.decorator_list:
                class_markers |= _marker_names_from_decorator(dec)
            for stmt in node.body:
                if isinstance(stmt, ast.Assign):
                    for target in stmt.targets:
                        if isinstance(target, ast.Name) and target.id == "pytestmark":
                            class_markers |= _markers_from_pytestmark(stmt.value)
                elif isinstance(stmt, ast.AnnAssign) and isinstance(stmt.target, ast.Name):
                    if stmt.target.id == "pytestmark" and stmt.value is not None:
                        class_markers |= _markers_from_pytestmark(stmt.value)
            for stmt in node.body:
                if isinstance(stmt, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    add_function(stmt, node.name, class_markers)

    return tests


def _git_show(ref: str, rel_path: str) -> str | None:
    result = subprocess.run(
        ["git", "show", f"{ref}:{rel_path}"],
        cwd=REPO_ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    if result.returncode != 0:
        return None
    return result.stdout


def _is_pytest_test_module(path: Path) -> bool:
    """Match pytest default python_files (test_*.py / *_test.py)."""
    return path.suffix == ".py" and (
        path.name.startswith("test_") or path.name.endswith("_test.py")
    )


def _decode_git_path(raw: bytes) -> str:
    return raw.decode("utf-8", errors="surrogateescape").strip()


def _changed_test_modules(base: str) -> list[tuple[Path, str]]:
    """Return (head_path, base_rel_path) for changed pytest modules.

    Renames/copies use the pre-image path for the base lookup so existing tests
    are not treated as newly added after a file move.
    """
    # -z avoids Git quoting; --name-status exposes rename source paths.
    result = subprocess.run(
        [
            "git",
            "diff",
            "-z",
            "--name-status",
            "--diff-filter=ACMR",
            f"{base}...HEAD",
            "--",
            str(TESTS_DIR),
        ],
        cwd=REPO_ROOT,
        capture_output=True,
        check=False,
    )
    if result.returncode != 0:
        err = (result.stderr or result.stdout or b"").decode("utf-8", errors="replace").strip()
        raise RuntimeError(f"git diff -z --name-status failed: {err}")

    entries = [part for part in result.stdout.split(b"\0") if part]
    changed: list[tuple[Path, str]] = []
    i = 0
    while i < len(entries):
        status = _decode_git_path(entries[i])
        if not status:
            i += 1
            continue
        kind = status[0]
        if kind in {"R", "C"}:
            if i + 2 >= len(entries):
                raise RuntimeError(f"truncated git name-status rename/copy record: {status!r}")
            old_rel = _decode_git_path(entries[i + 1])
            new_rel = _decode_git_path(entries[i + 2])
            i += 3
            head_path = REPO_ROOT / new_rel
            if _is_pytest_test_module(head_path):
                changed.append((head_path, old_rel))
            continue
        if i + 1 >= len(entries):
            raise RuntimeError(f"truncated git name-status record: {status!r}")
        rel = _decode_git_path(entries[i + 1])
        i += 2
        head_path = REPO_ROOT / rel
        if _is_pytest_test_module(head_path):
            changed.append((head_path, rel))
    return changed


def _pytest_node_id(path: Path, qual: str) -> str:
    """Node id as pytest reports it when cwd is test/e2e/."""
    rel = path.resolve().relative_to((REPO_ROOT / "test" / "e2e").resolve()).as_posix()
    return f"{rel}::{qual}"


def iter_new_tests(base: str) -> list[tuple[str, set[str]]]:
    """Return (pytest_node_id, tier_markers) for tests added since base."""
    changed = _changed_test_modules(base)
    new: list[tuple[str, set[str]]] = []
    for path, base_rel in sorted(changed, key=lambda item: item[0].as_posix()):
        head_src = path.read_text(encoding="utf-8")
        head_tests = _collect_tests(head_src, path)
        base_src = _git_show(base, base_rel)
        base_tests = _collect_tests(base_src, path) if base_src is not None else {}
        for qual, tiers in sorted(head_tests.items()):
            if qual in base_tests:
                continue
            new.append((_pytest_node_id(path, qual), tiers))
    return new


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--base",
        required=True,
        help="Git ref/SHA to diff against (merge base parent, e.g. origin/main or $BASE_SHA)",
    )
    parser.add_argument(
        "--list-new",
        action="store_true",
        help="Print pytest node ids for newly added tests (one per line) and exit 0",
    )
    args = parser.parse_args()
    base = args.base

    try:
        _run_git("rev-parse", "--verify", base)
    except RuntimeError as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 2

    try:
        new_tests = iter_new_tests(base)
    except RuntimeError as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 2

    if args.list_new:
        for node_id, _tiers in new_tests:
            print(node_id)
        return 0

    changed = _changed_test_modules(base)
    if not changed and not new_tests:
        print("No changed pytest test modules under test/e2e/tests/; tier gate OK.")
        return 0

    violations: list[str] = []
    for node_id, tiers in new_tests:
        if tiers:
            continue
        violations.append(f"  {node_id}")

    print(
        f"Checked {len(new_tests)} newly added E2E test function(s) "
        f"across {len(changed)} file(s)."
    )
    if not violations:
        print("All new E2E tests declare a coverage tier (smoke | full_suite | nightly).")
        return 0

    print("ERROR: new E2E tests missing a coverage tier marker:", file=sys.stderr)
    for item in violations:
        print(item, file=sys.stderr)
    print(
        "\nMark PR-critical low-churn coverage with @pytest.mark.smoke.\n"
        "Mark everything else new with @pytest.mark.full_suite (or @pytest.mark.nightly).\n"
        "Module- or class-level markers apply to nested test methods.\n"
        "Smoke-only CI also runs new tests once via the dynamic pr_new marker "
        "(no need to temporarily mark them smoke).",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    sys.exit(main())
