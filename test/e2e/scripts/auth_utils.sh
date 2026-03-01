#!/bin/bash
# =============================================================================
# MaaS Auth Utils - E2E debugging and artifact collection
# =============================================================================
#
# Provides utilities for auth debugging, Authorino configuration, and
# artifact collection for Prow/CI. Use for diagnosing 403/401 issues,
# DNS/connectivity problems, and collecting logs for analysis.
#
# Usage:
#   source test/e2e/scripts/auth_utils.sh
#   patch_authorino_debug
#   collect_e2e_artifacts
#
# Or run the full auth debug report:
#   MAAS_NAMESPACE=opendatahub ./test/e2e/scripts/auth_utils.sh
#
# Environment:
#   MAAS_NAMESPACE     - Namespace where maas-api/maas-controller run (default: opendatahub)
#   AUTHORINO_NAMESPACE - Namespace for Authorino (default: kuadrant-system)
#   ARTIFACT_DIR       - Prow artifact dir; also ARTIFACTS, LOG_DIR (default: test/e2e/reports)
#
# =============================================================================

set -euo pipefail

# Find project root
_find_root() {
  local dir="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
  while [[ "$dir" != "/" && ! -e "$dir/.git" ]]; do
    dir="$(dirname "$dir")"
  done
  [[ -e "$dir/.git" ]] && printf '%s\n' "$dir" || echo "."
}

PROJECT_ROOT="$(_find_root)"
MAAS_NAMESPACE="${MAAS_NAMESPACE:-opendatahub}"
AUTHORINO_NAMESPACE="${AUTHORINO_NAMESPACE:-kuadrant-system}"
# OpenShift CI/Prow use ARTIFACT_DIR; respect ARTIFACTS_DIR if already set by caller
ARTIFACTS_DIR="${ARTIFACTS_DIR:-${ARTIFACT_DIR:-${ARTIFACTS:-${LOG_DIR:-$PROJECT_ROOT/test/e2e/reports}}}}"

# -----------------------------------------------------------------------------
# Redact token-like values from log output (JWT, Bearer tokens, token fields)
# -----------------------------------------------------------------------------
redact_tokens() {
  sed -E \
    -e 's/eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+/****REDACTED_JWT****/g' \
    -e 's/"token":"[^"]*"/"token":"****"/g' \
    -e 's/"token": "[^"]*"/"token": "****"/g' \
    -e 's/(Bearer )[^[:space:]]+/\1****/g' \
    -e 's/("spec":\s*\{[^}]*"token":\s*)"[^"]*"/\1"****"/g' \
    -e 's/token=[A-Za-z0-9_-]+\.?[A-Za-z0-9_-]*\.?[A-Za-z0-9_-]*/token=****/g' \
    2>/dev/null || cat
}

# -----------------------------------------------------------------------------
# Patch Authorino to log_level DEBUG for troubleshooting
# -----------------------------------------------------------------------------
patch_authorino_debug() {
  echo "Patching Authorino to log_level DEBUG..."
  local authorino_name
  authorino_name=$(kubectl get authorino -n "$AUTHORINO_NAMESPACE" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [[ -n "$authorino_name" ]]; then
    if kubectl patch authorino "$authorino_name" -n "$AUTHORINO_NAMESPACE" --type=merge -p='{"spec":{"logLevel":"debug"}}' 2>/dev/null; then
      echo "✅ Authorino patched to log_level DEBUG"
      kubectl rollout status deployment/authorino -n "$AUTHORINO_NAMESPACE" --timeout=120s 2>/dev/null || true
    else
      echo "⚠️  Failed to patch Authorino (may not support logLevel)"
    fi
  else
    echo "⚠️  No Authorino CR found in $AUTHORINO_NAMESPACE"
  fi
}

# -----------------------------------------------------------------------------
# Collect Authorino logs to authorino-debug.log with token redaction
# -----------------------------------------------------------------------------
collect_authorino_logs_redacted() {
  local outfile="${1:-$ARTIFACTS_DIR/authorino-debug.log}"
  mkdir -p "$(dirname "$outfile")"
  : > "$outfile"
  echo "Collecting Authorino logs (token-redacted) to $outfile"
  for ns in "$AUTHORINO_NAMESPACE" openshift-ingress; do
    for label in "app.kubernetes.io/name=authorino" "authorino-resource=authorino"; do
      if kubectl get pods -n "$ns" -l "$label" --no-headers 2>/dev/null | head -1 | grep -q .; then
        {
          echo "--- Authorino logs from $ns (label=$label) ---"
          kubectl logs -n "$ns" -l "$label" --tail=2000 --all-containers=true 2>/dev/null || true
        } | redact_tokens >> "$outfile"
        break
      fi
    done
  done
  [[ -s "$outfile" ]] && echo "  Saved to $outfile"
}

# -----------------------------------------------------------------------------
# Collect cluster state (key resources) to artifact dir
# -----------------------------------------------------------------------------
collect_cluster_state() {
  local outdir="${1:-$ARTIFACTS_DIR}"
  mkdir -p "$outdir"
  echo "Collecting cluster state to $outdir"
  {
    echo "=== Cluster state $(date -Iseconds 2>/dev/null || date) ==="
    kubectl get nodes -o wide 2>/dev/null || true
    kubectl get ns 2>/dev/null || true
    echo ""
    echo "--- MaaS namespace ($MAAS_NAMESPACE) ---"
    kubectl get all -n "$MAAS_NAMESPACE" 2>/dev/null || true
    echo ""
    echo "--- AuthPolicies ---"
    kubectl get authpolicies -A 2>/dev/null || true
    echo ""
    echo "--- TokenRateLimitPolicies ---"
    kubectl get tokenratelimitpolicies -A 2>/dev/null || true
    echo ""
    echo "--- MaaS CRs ---"
    kubectl get maasmodels,maasauthpolicies,maassubscriptions -n "$MAAS_NAMESPACE" 2>/dev/null || true
    echo ""
    echo "--- HTTPRoutes ---"
    kubectl get httproutes -A 2>/dev/null | head -30 || true
    echo ""
    echo "--- Gateway ---"
    kubectl get gateway -A 2>/dev/null || true
  } > "$outdir/cluster-state.log" 2>&1
  echo "  Saved to $outdir/cluster-state.log"
}

# -----------------------------------------------------------------------------
# Collect logs from all pods in the maas namespace
# -----------------------------------------------------------------------------
collect_namespace_pod_logs() {
  local ns="${1:-$MAAS_NAMESPACE}"
  local outdir="${2:-$ARTIFACTS_DIR/pod-logs}"
  mkdir -p "$outdir"
  echo "Collecting pod logs from namespace $ns to $outdir"
  for pod in $(kubectl get pods -n "$ns" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
    kubectl logs -n "$ns" "$pod" --all-containers --tail=500 2>/dev/null | redact_tokens > "${outdir}/${pod}.log" || true
  done
  local count
  count=$(ls -1 "$outdir"/*.log 2>/dev/null | wc -l || echo 0)
  echo "  Saved $count pod log file(s) to $outdir"
}

# -----------------------------------------------------------------------------
# Main artifact collection: Authorino logs, cluster state, namespace pod logs
# -----------------------------------------------------------------------------
collect_e2e_artifacts() {
  mkdir -p "$ARTIFACTS_DIR"
  echo ""
  echo "========== E2E Artifact Collection =========="
  echo "Artifact dir: $ARTIFACTS_DIR"
  collect_authorino_logs_redacted "$ARTIFACTS_DIR/authorino-debug.log"
  collect_cluster_state "$ARTIFACTS_DIR"
  collect_namespace_pod_logs "$MAAS_NAMESPACE" "$ARTIFACTS_DIR/pod-logs"
  echo "=============================================="
}

# -----------------------------------------------------------------------------
# Full auth debug report (original debug_auth.sh behavior)
# -----------------------------------------------------------------------------
run_auth_debug_report() {
  local OUTPUT=""

  _append() {
    OUTPUT+="$1"
    OUTPUT+=$'\n'
  }

  _section() {
    _append ""
    _append "========================================"
    _append "$1"
    _append "========================================"
    _append ""
  }

  _run() {
    local label="$1"
    shift
    _append "--- $label ---"
    _append "$(eval "$*" 2>&1 || true)"
    _append ""
  }

  _section "Cluster / Namespace Info"
  _run "Current context" "kubectl config current-context 2>/dev/null || echo 'N/A'"
  _run "Logged-in user" "oc whoami 2>/dev/null || echo 'Not logged in'"
  _run "Cluster domain" "oc get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null || echo 'N/A'"
  _append "MAAS_NAMESPACE: $MAAS_NAMESPACE"
  _append "AUTHORINO_NAMESPACE: $AUTHORINO_NAMESPACE"
  _append ""

  _section "MaaS API Deployment"
  _run "maas-api pods" "kubectl get pods -n $MAAS_NAMESPACE -l app.kubernetes.io/name=maas-api -o wide 2>/dev/null || true"
  _run "maas-api service" "kubectl get svc maas-api -n $MAAS_NAMESPACE -o wide 2>/dev/null || true"
  _append ""

  _section "maas-controller"
  _run "maas-controller pods" "kubectl get pods -n $MAAS_NAMESPACE -l app=maas-controller -o wide 2>/dev/null || true"
  _run "maas-controller MAAS_API_NAMESPACE" \
    "kubectl get deployment maas-controller -n $MAAS_NAMESPACE -o jsonpath='{.spec.template.spec.containers[0].env}' 2>/dev/null | jq -r '.[] | select(.name==\"MAAS_API_NAMESPACE\") | \"\(.name)=\(.value // .valueFrom.fieldRef.fieldPath // \"N/A\")\"' 2>/dev/null || echo 'N/A'"
  _append ""

  _section "Kuadrant AuthPolicies"
  _run "AuthPolicies (all namespaces)" "kubectl get authpolicies -A -o wide 2>/dev/null || true"
  _append ""

  _section "MaaS CRs"
  _run "MaaSAuthPolicies" "kubectl get maasauthpolicies -n $MAAS_NAMESPACE -o wide 2>/dev/null || true"
  _run "MaaSSubscriptions" "kubectl get maassubscriptions -n $MAAS_NAMESPACE -o wide 2>/dev/null || true"
  _run "MaaSModels" "kubectl get maasmodels -n $MAAS_NAMESPACE -o wide 2>/dev/null || true"
  _append ""

  _section "Gateway / HTTPRoutes"
  _run "Gateway" "kubectl get gateway -n openshift-ingress maas-default-gateway -o wide 2>/dev/null || kubectl get gateway -A 2>/dev/null | head -10 || true"
  _run "HTTPRoutes (maas-api)" "kubectl get httproute maas-api-route -n $MAAS_NAMESPACE -o wide 2>/dev/null || true"
  _append ""

  _section "Authorino"
  _run "Authorino pods" "kubectl get pods -n $AUTHORINO_NAMESPACE -l 'app.kubernetes.io/name=authorino' --no-headers 2>/dev/null; kubectl get pods -n openshift-ingress -l 'app.kubernetes.io/name=authorino' --no-headers 2>/dev/null; echo '---'; kubectl get pods -A -l 'app.kubernetes.io/name=authorino' -o wide 2>/dev/null || true"
  _append ""

  # Determine maas-api namespace
  local maas_api_ns
  maas_api_ns=$(kubectl get deployment maas-controller -n $MAAS_NAMESPACE -o jsonpath='{.spec.template.spec.containers[0].env}' 2>/dev/null | jq -r '.[] | select(.name=="MAAS_API_NAMESPACE") | .value' 2>/dev/null || echo "$MAAS_NAMESPACE")
  [[ -z "$maas_api_ns" ]] && maas_api_ns="$MAAS_NAMESPACE"

  local sub_select_url="https://maas-api.${maas_api_ns}.svc.cluster.local:8443/v1/subscriptions/select"
  _section "Subscription Selector Endpoint Validation"
  _append "Expected URL (from maas-controller config): $sub_select_url"
  _append ""

  local curl_ns="$AUTHORINO_NAMESPACE"
  if ! kubectl get namespace "$curl_ns" &>/dev/null; then
    curl_ns="openshift-ingress"
  fi
  if ! kubectl get namespace "$curl_ns" &>/dev/null; then
    curl_ns="$MAAS_NAMESPACE"
  fi

  _append "--- Connectivity test (from $curl_ns, simulates Authorino) ---"
  _append "curl -vsk -m 10 -X POST '$sub_select_url' -H 'Content-Type: application/json' -d '{}'"
  _append ""
  local curl_out
  curl_out=$(kubectl run "debug-curl-$(date +%s)" --rm --restart=Never --image=curlimages/curl:latest -n "$curl_ns" -- \
    curl -vsk -m 10 -X POST "$sub_select_url" -H "Content-Type: application/json" -d '{}' 2>&1) || curl_out="kubectl run failed or timed out"
  _append "$curl_out"
  _append ""

  _section "DNS Resolution Check"
  _append "Resolving: maas-api.${maas_api_ns}.svc.cluster.local"
  local dns_out
  dns_out=$(kubectl run "debug-dns-$(date +%s)" --rm --restart=Never --image=busybox:1.36 -n "$curl_ns" -- \
    nslookup "maas-api.${maas_api_ns}.svc.cluster.local" 2>&1) || dns_out="nslookup failed"
  _append "$dns_out"
  _append ""

  echo "$OUTPUT"
}

# -----------------------------------------------------------------------------
# Main: when run directly, do full artifact collection + auth debug report
# -----------------------------------------------------------------------------
main() {
  if [[ "${1:-}" == "--collect-only" ]]; then
    collect_e2e_artifacts
    return 0
  fi
  if [[ "${1:-}" == "--patch-authorino" ]]; then
    patch_authorino_debug
    return 0
  fi
  # Default: collect artifacts, then print auth debug report
  collect_e2e_artifacts
  echo ""
  echo "========== Auth Debug Report =========="
  run_auth_debug_report
}

# Run main only when executed directly (not sourced)
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
