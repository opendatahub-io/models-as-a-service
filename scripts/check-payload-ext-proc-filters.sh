#!/usr/bin/env bash
# Check that payload-processing EnvoyFilter is shaped correctly AND that
# ext_proc filters are present in the live gateway Envoy config.
#
# Catches the RHCL failure mode where EnvoyFilter YAML exists but HTTP_FILTER
# inserts never match (e.g. missing positive priority) -> 404 NR on body-routed /v1/*,
# and Istio's InferencePool filter (envoy.filters.http.ext_proc) running ahead of
# ipp-pre, where the EPP is silently skipped. Every listener chain carrying ipp must
# read ipp-pre -> auth -> ipp -> EPP -> router.
#
# Usage:
#   ./scripts/check-payload-ext-proc-filters.sh
#   GATEWAY_NAMESPACE=openshift-ingress GATEWAY_NAME=maas-default-gateway ./scripts/check-payload-ext-proc-filters.sh
#   GATEWAY_NAME=partner EF_NAME=payload-processing-partner ./scripts/check-payload-ext-proc-filters.sh
#   CONFIG_DUMP=config_dump.json ./scripts/check-payload-ext-proc-filters.sh   # saved dump, no cluster
#
# Requires: oc/kubectl, python3

set -euo pipefail

GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-openshift-ingress}"
GATEWAY_NAME="${GATEWAY_NAME:-maas-default-gateway}"
EF_NAME="${EF_NAME:-payload-processing}"
MIN_PRIORITY="${MIN_PRIORITY:-10}"
CONFIG_DUMP="${CONFIG_DUMP:-}"

KUBECTL="${KUBECTL:-}"
if [[ -z "$KUBECTL" ]]; then
  if command -v oc >/dev/null 2>&1; then
    KUBECTL=oc
  else
    KUBECTL=kubectl
  fi
fi

fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "OK: $*"; }

check_chains() {
  python3 - "$1" <<'PY'
import json, sys

PRE = "envoy.filters.http.ext_proc.ipp-pre"
IPP = "envoy.filters.http.ext_proc.ipp"
EPP = "envoy.filters.http.ext_proc"
WASM = "envoy.filters.http.wasm"
ROUTER = "envoy.filters.http.router"

with open(sys.argv[1]) as f:
    data = json.load(f)

chains = []  # (listener, [filter names]) from active listener state only
errors = []  # listeners Envoy rejected

def walk(o, listener="?"):
    if isinstance(o, dict):
        if "error_state" in o:
            errors.append((o.get("name", listener), o["error_state"].get("details", "")))
        if "filter_chains" in o and "name" in o:
            listener = o["name"]
        if "http_filters" in o:
            chains.append((listener, [f.get("name", "") for f in o["http_filters"]]))
        for k, v in o.items():
            if k not in ("warming_state", "draining_state", "error_state"):
                walk(v, listener)
    elif isinstance(o, list):
        for v in o:
            walk(v, listener)

walk(data)
failed = False
for name, details in errors:
    print(f"FAIL: listener {name} rejected by Envoy: {details}", file=sys.stderr)
    failed = True
if not chains:
    print("FAIL: no http_filters found in config_dump", file=sys.stderr)
    sys.exit(1)

maas = [(l, names) for l, names in chains if IPP in names or PRE in names]
if not maas:
    print("FAIL: missing required ext_proc filters on every listener:", PRE, IPP, file=sys.stderr)
    print("hint: EnvoyFilter inserts may not be matching (check priority / auth anchor).", file=sys.stderr)
    sys.exit(1)

def auth_index(names):
    for i, n in enumerate(names):
        if n == WASM or n.startswith("extensions.istio.io/wasmplugin/"):
            return i
    return -1

for listener, names in maas:
    print(f"filter chain ({listener}):")
    for n in names:
        print(f"  - {n}")
    problems = [f"{n} x{names.count(n)}" for n in (PRE, IPP, EPP) if names.count(n) > 1]
    problems += [f"missing {n}" for n in (PRE, IPP, EPP, ROUTER) if n not in names]
    if not problems:
        order = [names.index(n) for n in (PRE, IPP, EPP, ROUTER)]
        auth = auth_index(names)
        if auth >= 0:
            order.insert(1, auth)
        if order != sorted(order):
            problems.append("bad order, expected ipp-pre -> auth -> ipp -> " + EPP + " -> router")
    if problems:
        print(f"FAIL: {listener}: " + "; ".join(problems), file=sys.stderr)
        failed = True

for listener, names in chains:
    if EPP in names and IPP not in names:
        print(f"WARN: {listener} runs {EPP} without MaaS ext_proc filters", file=sys.stderr)

if failed:
    sys.exit(1)
print(f"OK: ext_proc filters present with correct relative order on {len(maas)} chain(s)")
PY
}

if [[ -n "$CONFIG_DUMP" ]]; then
  echo "== ${CONFIG_DUMP} =="
  check_chains "$CONFIG_DUMP"
  echo "All checks passed."
  exit 0
fi

echo "== EnvoyFilter ${GATEWAY_NAMESPACE}/${EF_NAME} =="
if ! "$KUBECTL" get envoyfilter "$EF_NAME" -n "$GATEWAY_NAMESPACE" >/dev/null 2>&1; then
  fail "EnvoyFilter not found — ext_proc cannot run (body-routed /v1/* -> 404 NR)"
fi

priority="$("$KUBECTL" get envoyfilter "$EF_NAME" -n "$GATEWAY_NAMESPACE" -o jsonpath='{.spec.priority}' 2>/dev/null || true)"
if [[ -z "$priority" ]]; then
  fail "spec.priority is missing; need >= ${MIN_PRIORITY} so inserts apply after Kuadrant wasm"
fi
if (( priority < MIN_PRIORITY )); then
  fail "spec.priority=${priority}; need >= ${MIN_PRIORITY}"
fi
ok "spec.priority=${priority}"

selected="$("$KUBECTL" get envoyfilter "$EF_NAME" -n "$GATEWAY_NAMESPACE" \
  -o jsonpath='{.spec.workloadSelector.labels.gateway\.networking\.k8s\.io/gateway-name}' 2>/dev/null || true)"
[[ "$selected" == "$GATEWAY_NAME" ]] || fail "workloadSelector gateway-name=${selected:-empty}; expected ${GATEWAY_NAME}"
ok "workloadSelector -> Gateway/${GATEWAY_NAME}"

echo "== Live gateway http_filters =="
pods="$("$KUBECTL" get pods -n "$GATEWAY_NAMESPACE" \
  -l "gateway.networking.k8s.io/gateway-name=${GATEWAY_NAME}" \
  --field-selector=status.phase=Running -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true)"
[[ -n "$pods" ]] || fail "no Running pod for Gateway/${GATEWAY_NAME} in ${GATEWAY_NAMESPACE}"

dump="$(mktemp)"
trap 'rm -f "$dump"' EXIT

rc=0
for pod in $pods; do
  echo "-- ${pod}"
  "$KUBECTL" exec -n "$GATEWAY_NAMESPACE" "$pod" -c istio-proxy -- \
    pilot-agent request GET 'config_dump?resource=dynamic_listeners' >"$dump" \
    || fail "could not fetch Envoy config_dump from ${pod}"
  check_chains "$dump" || rc=1
done
(( rc == 0 )) || fail "filter chain check failed on at least one gateway pod"

echo "All checks passed."
