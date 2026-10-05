#!/usr/bin/env bash
# Resolve a Python interpreter suitable for the e2e suite (pytest 9 → >= 3.10).
#
# Group-test runs on UBI9 where /usr/bin/python3 is still 3.9. Prefer python3.11 /
# python3.10 when present; as root on yum/dnf hosts, install python3.11 from AppStream.
#
# Source this file; it exports E2E_PYTHON (absolute or PATH name).
# Intentionally does not set -euo pipefail — callers own shell options (all entry
# scripts that source this already use set -euo pipefail).

_e2e_python_version() {
  "$1" -c 'import sys; print("%d.%d" % sys.version_info[:2])' 2>/dev/null
}

_e2e_python_at_least_310() {
  local ver major minor
  ver="$(_e2e_python_version "$1")" || return 1
  major="${ver%%.*}"
  minor="${ver#*.}"
  [[ "$major" -gt 3 || ( "$major" -eq 3 && "$minor" -ge 10 ) ]]
}

_e2e_find_python() {
  local cand
  for cand in python3.11 python3.10 python3; do
    if command -v "$cand" >/dev/null 2>&1 && _e2e_python_at_least_310 "$cand"; then
      command -v "$cand"
      return 0
    fi
  done
  return 1
}

# Already resolved in this shell — only reuse if it meets the floor.
if [[ -n "${E2E_PYTHON:-}" ]] && command -v "${E2E_PYTHON}" >/dev/null 2>&1 \
  && _e2e_python_at_least_310 "${E2E_PYTHON}"; then
  return 0 2>/dev/null || exit 0
fi
# Stale or too-old override: clear and continue discovery / install.
unset E2E_PYTHON

if E2E_PYTHON="$(_e2e_find_python)"; then
  export E2E_PYTHON
  return 0 2>/dev/null || exit 0
fi

# CI toolset (and similar) often run as root on UBI/RHEL 9 without python3.11 preinstalled.
if [[ "$(id -u)" -eq 0 ]]; then
  if command -v yum >/dev/null 2>&1; then
    echo "Installing python3.11 (e2e requires Python >= 3.10; system python3 is too old)..." >&2
    yum install -y --nodocs python3.11 >/dev/null
  elif command -v microdnf >/dev/null 2>&1; then
    echo "Installing python3.11 (e2e requires Python >= 3.10; system python3 is too old)..." >&2
    microdnf install -y python3.11 >/dev/null
  elif command -v dnf >/dev/null 2>&1; then
    echo "Installing python3.11 (e2e requires Python >= 3.10; system python3 is too old)..." >&2
    dnf install -y --nodocs python3.11 >/dev/null
  fi
fi

if E2E_PYTHON="$(_e2e_find_python)"; then
  export E2E_PYTHON
  return 0 2>/dev/null || exit 0
fi

echo "ERROR: e2e tests need Python >= 3.10 (pytest 9). Install python3.11 (RHEL/UBI 9 AppStream) or use a newer python3." >&2
return 1 2>/dev/null || exit 1
