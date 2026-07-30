#!/usr/bin/env bash
# Check that payload-processing EnvoyFilter is shaped correctly AND that
# ext_proc filters are present in the live gateway Envoy config.
#
# Catches the RHCL failure mode where EnvoyFilter YAML exists but HTTP_FILTER
# inserts never match (e.g. missing positive priority) → 404 NR on body-routed /v1/*.
#
# Usage:
#   ./scripts/check-payload-ext-proc-filters.sh
#   GATEWAY_NAMESPACE=openshift-ingress GATEWAY_NAME=maas-default-gateway ./scripts/check-payload-ext-proc-filters.sh
#   GATEWAY_NAME=partner EF_NAME=payload-processing-partner ./scripts/check-payload-ext-proc-filters.sh
#
# Requires: oc/kubectl, python3, curl (for local port-forward to Envoy admin)

set -euo pipefail

GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-openshift-ingress}"
GATEWAY_NAME="${GATEWAY_NAME:-maas-default-gateway}"
EF_NAME="${EF_NAME:-payload-processing}"
MIN_PRIORITY="${MIN_PRIORITY:-10}"
REQUIRED_FILTERS=(
  "envoy.filters.http.ext_proc.ipp-pre"
  "envoy.filters.http.ext_proc.ipp"
)

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

echo "== EnvoyFilter ${GATEWAY_NAMESPACE}/${EF_NAME} =="
if ! "$KUBECTL" get envoyfilter "$EF_NAME" -n "$GATEWAY_NAMESPACE" >/dev/null 2>&1; then
  fail "EnvoyFilter not found — ext_proc cannot run (body-routed /v1/* → 404 NR)"
fi

priority="$("$KUBECTL" get envoyfilter "$EF_NAME" -n "$GATEWAY_NAMESPACE" -o jsonpath='{.spec.priority}' 2>/dev/null || true)"
if [[ -z "$priority" ]]; then
  fail "spec.priority is missing; need >= ${MIN_PRIORITY} so inserts apply after Kuadrant wasm"
fi
if (( priority < MIN_PRIORITY )); then
  fail "spec.priority=${priority}; need >= ${MIN_PRIORITY}"
fi
ok "spec.priority=${priority}"

target="$("$KUBECTL" get envoyfilter "$EF_NAME" -n "$GATEWAY_NAMESPACE" -o jsonpath='{.spec.targetRefs[0].name}' 2>/dev/null || true)"
[[ "$target" == "$GATEWAY_NAME" ]] || fail "targetRefs[0].name=${target:-empty}; expected ${GATEWAY_NAME}"
ok "targetRefs → Gateway/${GATEWAY_NAME}"

echo "== Live gateway http_filters =="
pod="$("$KUBECTL" get pods -n "$GATEWAY_NAMESPACE" \
  -l "gateway.networking.k8s.io/gateway-name=${GATEWAY_NAME}" \
  -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
[[ -n "$pod" ]] || fail "no Running pod for Gateway/${GATEWAY_NAME} in ${GATEWAY_NAMESPACE}"

local_port=15000
pf_log="$(mktemp)"
dump="$(mktemp)"
cleanup() {
  kill "$pf_pid" 2>/dev/null || true
  wait "$pf_pid" 2>/dev/null || true
  rm -f "$pf_log" "$dump"
}
trap cleanup EXIT

"$KUBECTL" port-forward -n "$GATEWAY_NAMESPACE" "pod/${pod}" "${local_port}:15000" >"$pf_log" 2>&1 &
pf_pid=$!

# Wait for admin port
for _ in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:${local_port}/ready" >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done
curl -fsS "http://127.0.0.1:${local_port}/config_dump?resource=dynamic_listeners" -o "$dump" \
  || fail "could not fetch Envoy config_dump from ${pod} (see ${pf_log})"

python3 - "$dump" "${REQUIRED_FILTERS[@]}" <<'PY'
import json, sys
path = sys.argv[1]
required = sys.argv[2:]
with open(path) as f:
    data = json.load(f)

chains = []

def walk(o):
    if isinstance(o, dict):
        if "http_filters" in o:
            names = [f.get("name", "") for f in o["http_filters"]]
            chains.append(names)
        for v in o.values():
            walk(v)
    elif isinstance(o, list):
        for v in o:
            walk(v)

walk(data)
if not chains:
    print("FAIL: no http_filters found in config_dump", file=sys.stderr)
    sys.exit(1)

# Prefer a chain that already has kuadrant wasm / wasmplugin (auth-bearing listener)
def score(names):
    s = 0
    joined = " ".join(names)
    if "envoy.filters.http.wasm" in joined or "wasmplugin" in joined:
        s += 10
    if "envoy.filters.http.router" in names:
        s += 1
    return s

chains.sort(key=score, reverse=True)
names = chains[0]
print("filter chain:")
for n in names:
    print(f"  - {n}")

missing = [r for r in required if r not in names]
if missing:
    print("FAIL: missing required ext_proc filters:", ", ".join(missing), file=sys.stderr)
    print("hint: EnvoyFilter inserts may not be matching (check priority / auth anchor).", file=sys.stderr)
    sys.exit(1)

# Ordering: ipp-pre before auth (wasm|wasmplugin), ipp after auth, before router
def idx(exact):
    try:
        return names.index(exact)
    except ValueError:
        return -1

def idx_substr(substr):
    for i, n in enumerate(names):
        if substr in n:
            return i
    return -1

pre = idx("envoy.filters.http.ext_proc.ipp-pre")
ipp = idx("envoy.filters.http.ext_proc.ipp")
auth = idx("envoy.filters.http.wasm")
if auth < 0:
    auth = idx_substr("wasmplugin")
router = idx("envoy.filters.http.router")

if pre < 0 or ipp < 0 or router < 0:
    print("FAIL: unexpected filter names", file=sys.stderr)
    sys.exit(1)
if auth >= 0 and not (pre < auth < ipp < router):
    print(f"FAIL: bad order pre={pre} auth={auth} ipp={ipp} router={router}", file=sys.stderr)
    print("expected: ipp-pre → auth → ipp → router", file=sys.stderr)
    sys.exit(1)
if auth < 0 and not (pre < ipp < router):
    print(f"FAIL: bad order pre={pre} ipp={ipp} router={router}", file=sys.stderr)
    sys.exit(1)

print("OK: ext_proc filters present with correct relative order")
PY

echo "All checks passed."
