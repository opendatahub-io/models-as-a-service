#!/usr/bin/env python3
"""Fail when a PR adds E2E tests without an explicit coverage tier.

New tests under test/e2e/tests/ must carry one of:
  - smoke      → runs in /group-test (E2E_SMOKE_ONLY=true)
  - full_suite → intentionally off the smoke path; still runs when smoke-only is off
  - nightly    → pass-3 / scheduled extended coverage

Existing unmarked tests are grandfathered: only tests added relative to --base
are checked. Class/module-level markers count for nested methods.

Usage:
  python test/e2e/scripts/check_new_e2e_test_tiers.py --base origin/main
  python test/e2e/scripts/check_new_e2e_test_tiers.py --base "$BASE_SHA"
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


def _changed_test_files(base: str) -> list[Path]:
    out = _run_git("diff", "--name-only", "--diff-filter=ACMR", f"{base}...HEAD", "--", str(TESTS_DIR))
    files: list[Path] = []
    for line in out.splitlines():
        path = REPO_ROOT / line.strip()
        if path.suffix == ".py" and path.name.startswith("test_"):
            files.append(path)
    return files


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--base",
        required=True,
        help="Git ref/SHA to diff against (merge base parent, e.g. origin/main or $BASE_SHA)",
    )
    args = parser.parse_args()
    base = args.base

    try:
        _run_git("rev-parse", "--verify", base)
    except RuntimeError as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 2

    changed = _changed_test_files(base)
    if not changed:
        print("No changed test_*.py files under test/e2e/tests/; tier gate OK.")
        return 0

    violations: list[str] = []
    checked = 0
    for path in sorted(changed):
        rel = path.relative_to(REPO_ROOT).as_posix()
        head_src = path.read_text(encoding="utf-8")
        head_tests = _collect_tests(head_src, path)
        base_src = _git_show(base, rel)
        base_tests = _collect_tests(base_src, path) if base_src is not None else {}

        for qual, tiers in sorted(head_tests.items()):
            if qual in base_tests:
                continue  # grandfathered / pre-existing
            checked += 1
            if tiers:
                continue
            violations.append(f"  {rel}::{qual}")

    print(f"Checked {checked} newly added E2E test function(s) across {len(changed)} file(s).")
    if not violations:
        print("All new E2E tests declare a coverage tier (smoke | full_suite | nightly).")
        return 0

    print("ERROR: new E2E tests missing a coverage tier marker:", file=sys.stderr)
    for item in violations:
        print(item, file=sys.stderr)
    print(
        "\nMark PR-critical low-churn coverage with @pytest.mark.smoke.\n"
        "Mark everything else new with @pytest.mark.full_suite (or @pytest.mark.nightly).\n"
        "Module- or class-level markers apply to nested test methods.",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    sys.exit(main())
