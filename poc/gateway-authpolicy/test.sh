#!/usr/bin/env bash
# PoC test script: Gateway-level AuthPolicy for MaaS inference
#
# Tests 5 scenarios:
#   1. Happy path — valid API key + subscription → 200 (or 429 from TRLP, not 401/403)
#   2. Bad API key → 401
#   3. Valid key but no subscription for this model → 403
#   4. Coke/Pepsi hostname isolation — valid key used on wrong gateway → 403
#   5. Body route stub — X-Gateway-Model-Name header (simulates Approach C / BBR WASM peek)
#
# Prerequisites:
#   - kubectl access to a dev cluster with MaaS + Kuadrant installed
#   - 01-gateway-authpolicy.yaml applied (with substitutions)
#   - At least one MaaSModelRef with a known HTTPRoute
#   - VALID_KEY and INVALID_KEY set below
#
# Usage:
#   export GATEWAY_HOST=maas.apps.your-cluster.example.com
#   export MAAS_API_NS=maas-api
#   export MODEL_NS=llm
#   export MODEL_NAME=granite-3b
#   export VALID_KEY=sk-oai-xxxxxxxxxxxx
#   export INVALID_KEY=sk-oai-invalid
#   ./test.sh

set -euo pipefail

GATEWAY_HOST="${GATEWAY_HOST:-maas.apps.your-cluster.example.com}"
MODEL_NS="${MODEL_NS:-llm}"
MODEL_NAME="${MODEL_NAME:-granite-3b}"
VALID_KEY="${VALID_KEY:-sk-oai-REPLACE_ME}"
INVALID_KEY="${INVALID_KEY:-sk-oai-invalid}"

# Path-based inference URL (current MaaS routing)
PATH_URL="https://${GATEWAY_HOST}/${MODEL_NS}/${MODEL_NAME}/v1/chat/completions"
# Body-based inference URL (BBR / Approach C)
BODY_URL="https://${GATEWAY_HOST}/v1/chat/completions"
# Models listing (should bypass auth check)
MODELS_URL="https://${GATEWAY_HOST}/v1/models"

PAYLOAD='{"model":"'"${MODEL_NS}/${MODEL_NAME}"'","messages":[{"role":"user","content":"ping"}],"max_tokens":1}'

PASS=0
FAIL=0

check() {
  local desc="$1"
  local expected="$2"
  local actual="$3"
  if echo "$actual" | grep -q "HTTP/$expected\|< HTTP/[0-9.]* $expected"; then
    echo "  ✓ PASS: $desc (HTTP $expected)"
    ((PASS++)) || true
  else
    echo "  ✗ FAIL: $desc — expected HTTP $expected, got: $(echo "$actual" | grep -oP 'HTTP/[0-9.]+ \d+' | head -1)"
    ((FAIL++)) || true
  fi
}

echo ""
echo "═══════════════════════════════════════════════════════════"
echo " MaaS Gateway-Level AuthPolicy PoC — Test Suite"
echo "═══════════════════════════════════════════════════════════"
echo " Gateway:    ${GATEWAY_HOST}"
echo " Model:      ${MODEL_NS}/${MODEL_NAME}"
echo " Path URL:   ${PATH_URL}"
echo " Body URL:   ${BODY_URL}"
echo ""

# ─── Scenario 1: Happy path (path-based route) ───────────────────────────────
echo "[ Scenario 1 ] Valid API key + subscription — path-based route"
echo "  Expect: 200 (or 429 from TRLP, but NOT 401/403)"
RESP=$(curl -sk -o /dev/null -w "%{http_code}" \
  -H "Authorization: Bearer ${VALID_KEY}" \
  -H "Content-Type: application/json" \
  -d "$PAYLOAD" \
  "$PATH_URL")
if [[ "$RESP" == "200" || "$RESP" == "429" ]]; then
  echo "  ✓ PASS: got HTTP $RESP (auth passed)"
  ((PASS++)) || true
else
  echo "  ✗ FAIL: got HTTP $RESP (expected 200 or 429)"
  ((FAIL++)) || true
fi

# ─── Scenario 2: Bad API key → 401 ──────────────────────────────────────────
echo ""
echo "[ Scenario 2 ] Invalid API key — path-based route"
echo "  Expect: 401"
RESP=$(curl -sk -o /dev/null -w "%{http_code}" \
  -H "Authorization: Bearer ${INVALID_KEY}" \
  -H "Content-Type: application/json" \
  -d "$PAYLOAD" \
  "$PATH_URL")
[[ "$RESP" == "401" ]] && { echo "  ✓ PASS: HTTP 401"; ((PASS++)) || true; } \
  || { echo "  ✗ FAIL: got HTTP $RESP (expected 401)"; ((FAIL++)) || true; }

# ─── Scenario 3: No subscription → 403 ──────────────────────────────────────
echo ""
echo "[ Scenario 3 ] Valid key with no subscription for this model — path-based route"
echo "  Expect: 403"
echo "  NOTE: requires a key that is valid but has no subscription for ${MODEL_NS}/${MODEL_NAME}"
echo "  Set NO_SUB_KEY env var — skipping if not set"
if [[ -n "${NO_SUB_KEY:-}" ]]; then
  RESP=$(curl -sk -o /dev/null -w "%{http_code}" \
    -H "Authorization: Bearer ${NO_SUB_KEY}" \
    -H "Content-Type: application/json" \
    -d "$PAYLOAD" \
    "$PATH_URL")
  [[ "$RESP" == "403" ]] && { echo "  ✓ PASS: HTTP 403"; ((PASS++)) || true; } \
    || { echo "  ✗ FAIL: got HTTP $RESP (expected 403)"; ((FAIL++)) || true; }
else
  echo "  ⚠ SKIP: NO_SUB_KEY not set"
fi

# ─── Scenario 4: Coke/Pepsi hostname isolation ───────────────────────────────
echo ""
echo "[ Scenario 4 ] Hostname tenant isolation (Coke key on Pepsi gateway)"
echo "  Expect: 403 (once overrides tenant check is implemented with real lookup)"
echo "  NOTE: This is a STUB in Phase 1 — the OPA rule allows all until tenant"
echo "        lookup endpoint is wired in. Expected result is currently 200/429."
echo "  Set PEPSI_HOST env var to a different gateway hostname to test routing"
if [[ -n "${PEPSI_HOST:-}" && -n "${COKE_KEY:-}" ]]; then
  RESP=$(curl -sk -o /dev/null -w "%{http_code}" \
    -H "Authorization: Bearer ${COKE_KEY}" \
    -H "Content-Type: application/json" \
    -d "$PAYLOAD" \
    "https://${PEPSI_HOST}/${MODEL_NS}/${MODEL_NAME}/v1/chat/completions")
  if [[ "$RESP" == "403" ]]; then
    echo "  ✓ PASS: HTTP 403 — tenant isolation enforced"
    ((PASS++)) || true
  elif [[ "$RESP" == "200" || "$RESP" == "429" ]]; then
    echo "  ⚠ STUB: HTTP $RESP — tenant OPA rule is a stub, isolation not yet enforced"
    echo "    → Action: wire /internal/v1/tenants/validate into the overrides block"
  else
    echo "  ✗ FAIL: got HTTP $RESP"
    ((FAIL++)) || true
  fi
else
  echo "  ⚠ SKIP: set PEPSI_HOST and COKE_KEY to test cross-tenant isolation"
fi

# ─── Scenario 5: Body-based route (BBR stub) ─────────────────────────────────
echo ""
echo "[ Scenario 5 ] Body-based route with X-Gateway-Model-Name header (BBR / Approach C stub)"
echo "  Simulates what Kuadrant WASM body-peek (Approach C) will provide."
echo "  Expect: 200 (or 429 from TRLP) — same as path-based if header is correct"
RESP=$(curl -sk -o /dev/null -w "%{http_code}" \
  -H "Authorization: Bearer ${VALID_KEY}" \
  -H "Content-Type: application/json" \
  -H "X-Gateway-Model-Name: ${MODEL_NS}/${MODEL_NAME}" \
  -d "$PAYLOAD" \
  "$BODY_URL")
if [[ "$RESP" == "200" || "$RESP" == "429" ]]; then
  echo "  ✓ PASS: got HTTP $RESP — Gateway policy correctly resolves model from header"
  ((PASS++)) || true
elif [[ "$RESP" == "403" ]]; then
  echo "  ✗ FAIL: HTTP 403 — subscription check likely failed (wrong model from path fallback?)"
  echo "    → Check: does the policy fall back to path for /v1/chat/completions when header present?"
  ((FAIL++)) || true
else
  echo "  ✗ FAIL: got HTTP $RESP"
  ((FAIL++)) || true
fi

# ─── Summary ─────────────────────────────────────────────────────────────────
echo ""
echo "═══════════════════════════════════════════════════════════"
echo " Results: ${PASS} passed, ${FAIL} failed"
echo "═══════════════════════════════════════════════════════════"

# ─── Kuadrant policy status check ────────────────────────────────────────────
echo ""
echo "[ Policy status ]"
kubectl get authpolicy -n openshift-ingress maas-gateway-auth-poc \
  -o jsonpath='{.status.conditions}' 2>/dev/null | python3 -m json.tool 2>/dev/null \
  || echo "  (kubectl not available or policy not found)"

[[ "$FAIL" -eq 0 ]] && exit 0 || exit 1
