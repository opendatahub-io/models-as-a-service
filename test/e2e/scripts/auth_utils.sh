#!/bin/bash
# =============================================================================
# MaaS Auth Utils - E2E debugging and artifact collection
# =============================================================================
#
# Provides utilities for auth debugging, Authorino configuration, and
# artifact collection for Prow/CI. Use for diagnosing 403/401 issues,
# DNS/connectivity problems, and collecting logs for analysis.
#
# Collected artifacts (under $ARTIFACT_DIR):
#   authorino-debug.log        - Authorino pod logs (token-redacted)
#   cluster-state.log          - Cluster snapshot (nodes, namespaces, policies, CRs)
#   maas-debug-report.log      - Full MaaS debug report
#   maas-crs/                  - Full YAML of MaaS custom resources:
#     maasmodelrefs.yaml         - MaaSModelRef definitions
#     maasauthpolicies.yaml      - MaaSAuthPolicy definitions
#     maassubscriptions.yaml     - MaaSSubscription definitions
#     externalmodels.yaml        - ExternalModel definitions
#   pod-logs/                  - Per-pod logs from the deployment namespace
#
# Usage:
#   source test/e2e/scripts/auth_utils.sh
#   patch_authorino_debug
#   collect_e2e_artifacts
#
# Or run the full auth debug report:
#   ./test/e2e/scripts/auth_utils.sh
#
# Environment:
#   DEPLOYMENT_NAMESPACE       - MaaS API and controller namespace (default: opendatahub)
#   MAAS_SUBSCRIPTION_NAMESPACE - MaaS CRs namespace (default: models-as-a-service)
#   AUTHORINO_NAMESPACE        - Authorino namespace (default: kuadrant-system)
#   OPERATOR_NAMESPACE         - RHOAI operator namespace (default: redhat-ods-operator)
#   APPLICATIONS_NAMESPACE     - RHOAI applications namespace (default: redhat-ods-applications)
#   GATEWAY_NAMESPACE          - Gateway/ingress namespace (default: openshift-ingress)
#   LLM_NAMESPACE              - LLM workload namespace (default: llm)
#   ISTIO_NAMESPACE            - Istio/service mesh namespace (default: istio-system)
#   ARTIFACT_DIR               - Prow artifact dir; also ARTIFACTS, LOG_DIR (default: test/e2e/reports)
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
DEPLOYMENT_NAMESPACE="${DEPLOYMENT_NAMESPACE:-opendatahub}"
MAAS_SUBSCRIPTION_NAMESPACE="${MAAS_SUBSCRIPTION_NAMESPACE:-models-as-a-service}"
AUTHORINO_NAMESPACE="${AUTHORINO_NAMESPACE:-kuadrant-system}"
OPERATOR_NAMESPACE="${OPERATOR_NAMESPACE:-redhat-ods-operator}"
APPLICATIONS_NAMESPACE="${APPLICATIONS_NAMESPACE:-redhat-ods-applications}"
GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-openshift-ingress}"
LLM_NAMESPACE="${LLM_NAMESPACE:-llm}"
ISTIO_NAMESPACE="${ISTIO_NAMESPACE:-istio-system}"
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
  [[ -s "$outfile" ]] && echo "  Saved to $outfile" || true
}

# -----------------------------------------------------------------------------
# Collect full MaaS CR YAML definitions to artifact dir
# Mirrors the CRD list from red-hat-data-services/must-gather:
#   gather_models_as_a_service
# -----------------------------------------------------------------------------
MAAS_CRDS=(
  "maasmodelrefs.maas.opendatahub.io"
  "maasauthpolicies.maas.opendatahub.io"
  "maassubscriptions.maas.opendatahub.io"
  "externalmodels.maas.opendatahub.io"
)

collect_maas_crs() {
  local outdir="${1:-$ARTIFACTS_DIR/maas-crs}"
  mkdir -p "$outdir"
  echo "Collecting MaaS CR definitions to $outdir"

  local ns_list=""
  for crd in "${MAAS_CRDS[@]}"; do
    local nss
    nss=$(kubectl get "$crd" --all-namespaces -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{end}' 2>/dev/null || true)
    ns_list+=" $nss"
  done
  ns_list=$(echo "$ns_list" | tr ' ' '\n' | sort -u | grep -v '^$' || true)

  if [[ -z "$ns_list" ]]; then
    echo "  No MaaS CRs found in any namespace"
    echo "No MaaS CRs found at $(date -Iseconds 2>/dev/null || date)" > "$outdir/no-crs-found.log"
    return 0
  fi

  local total=0
  for crd in "${MAAS_CRDS[@]}"; do
    local short_name="${crd%%.*}"
    local outfile="$outdir/${short_name}.yaml"
    : > "$outfile"
    for ns in $ns_list; do
      local yaml
      yaml=$(kubectl get "$crd" -n "$ns" -o yaml 2>/dev/null || true)
      if [[ -n "$yaml" ]] && ! echo "$yaml" | grep -q 'items: \[\]'; then
        {
          echo "# --- namespace: $ns ---"
          echo "$yaml"
          echo ""
        } | redact_tokens >> "$outfile"
        total=$((total + 1))
      fi
    done
    if [[ ! -s "$outfile" ]]; then
      rm -f "$outfile"
    fi
  done
  echo "  Saved CRs from $(echo "$ns_list" | wc -w | tr -d ' ') namespace(s) to $outdir ($total resource group(s))"
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
    echo "--- MaaS deployment namespace ($DEPLOYMENT_NAMESPACE) ---"
    kubectl get all -n "$DEPLOYMENT_NAMESPACE" 2>/dev/null || true
    echo ""
    echo "--- RHOAI Operator namespace ($OPERATOR_NAMESPACE) ---"
    kubectl get pods,deployments,csv -n "$OPERATOR_NAMESPACE" -o wide 2>/dev/null || true
    echo ""
    echo "--- RHOAI Applications namespace ($APPLICATIONS_NAMESPACE) ---"
    kubectl get pods,deployments,services -n "$APPLICATIONS_NAMESPACE" -o wide 2>/dev/null || true
    echo ""
    echo "--- DSC / DSCI ---"
    kubectl get datasciencecluster,dscinitialization -o wide 2>/dev/null || true
    echo ""
    echo "--- Gateway namespace ($GATEWAY_NAMESPACE) ---"
    kubectl get pods,services -n "$GATEWAY_NAMESPACE" -o wide 2>/dev/null || true
    echo ""
    echo "--- AuthPolicies ---"
    kubectl get authpolicies -A 2>/dev/null || true
    echo ""
    echo "--- TokenRateLimitPolicies ---"
    kubectl get tokenratelimitpolicies -A 2>/dev/null || true
    echo ""
    echo "--- MaaS CRs ---"
    kubectl get maasmodelrefs -n "$DEPLOYMENT_NAMESPACE" 2>/dev/null || true
    kubectl get maasauthpolicies,maassubscriptions -n "$MAAS_SUBSCRIPTION_NAMESPACE" 2>/dev/null || true
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
  local ns="${1:-$DEPLOYMENT_NAMESPACE}"
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
  collect_maas_crs "$ARTIFACTS_DIR/maas-crs"
  local ns
  for ns in \
    "$DEPLOYMENT_NAMESPACE" \
    "$MAAS_SUBSCRIPTION_NAMESPACE" \
    "$OPERATOR_NAMESPACE" \
    "$APPLICATIONS_NAMESPACE" \
    "$AUTHORINO_NAMESPACE" \
    "$GATEWAY_NAMESPACE" \
    "$LLM_NAMESPACE" \
    "$ISTIO_NAMESPACE" \
  ; do
    if kubectl get namespace "$ns" &>/dev/null; then
      collect_namespace_pod_logs "$ns" "$ARTIFACTS_DIR/pod-logs/$ns"
    else
      echo "  Skipping namespace $ns (not found)"
    fi
  done
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
  _append "DEPLOYMENT_NAMESPACE: $DEPLOYMENT_NAMESPACE"
  _append "MAAS_SUBSCRIPTION_NAMESPACE: $MAAS_SUBSCRIPTION_NAMESPACE"
  _append "AUTHORINO_NAMESPACE: $AUTHORINO_NAMESPACE"
  _append ""

  _section "MaaS API Deployment"
  _run "maas-api pods" "kubectl get pods -n $DEPLOYMENT_NAMESPACE -l app.kubernetes.io/name=maas-api -o wide 2>/dev/null || true"
  _run "maas-api service" "kubectl get svc maas-api -n $DEPLOYMENT_NAMESPACE -o wide 2>/dev/null || true"
  _append ""

  _section "maas-controller"
  _run "maas-controller pods" "kubectl get pods -n $DEPLOYMENT_NAMESPACE -l app=maas-controller -o wide 2>/dev/null || true"

  local env_display
  env_display=$(kubectl get deployment maas-controller -n $DEPLOYMENT_NAMESPACE -o jsonpath='{.spec.template.spec.containers[0].env}' 2>/dev/null | jq -r '.[] | select(.name=="MAAS_API_NAMESPACE") | if .value then "\(.name)=\(.value)" elif .valueFrom.fieldRef.fieldPath then "\(.name)=\(.valueFrom.fieldRef.fieldPath) (resolves to: '"$DEPLOYMENT_NAMESPACE"')" else "\(.name)=N/A" end' 2>/dev/null || echo 'MAAS_API_NAMESPACE=N/A')
  _run "maas-controller MAAS_API_NAMESPACE" "echo '$env_display'"
  _append ""

  _section "Kuadrant Policies"
  _run "AuthPolicies (all namespaces)" "kubectl get authpolicies -A -o wide 2>/dev/null || true"
  _run "TokenRateLimitPolicies (all namespaces)" "kubectl get tokenratelimitpolicies -A -o wide 2>/dev/null || true"
  _append ""

  _section "MaaS CRs"
  _run "MaaSAuthPolicies" "kubectl get maasauthpolicies -n $MAAS_SUBSCRIPTION_NAMESPACE -o wide 2>/dev/null || true"
  _run "MaaSSubscriptions" "kubectl get maassubscriptions -n $MAAS_SUBSCRIPTION_NAMESPACE -o wide 2>/dev/null || true"
  _run "MaaSSubscription status details" "kubectl get maassubscriptions -n $MAAS_SUBSCRIPTION_NAMESPACE -o jsonpath='{range .items[*]}{.metadata.name}: {.status.phase} - {.status.conditions[?(@.type==\"Ready\")].message}{\"\\n\"}{end}' 2>/dev/null || true"
  _run "MaaSModelRefs (all namespaces)" "kubectl get maasmodelrefs -A -o wide 2>/dev/null || true"
  _append ""

  _section "Test User Information"
  local test_token
  test_token=$(oc whoami -t 2>/dev/null || echo "")
  if [[ -n "$test_token" ]]; then
    _append "Test user token available: yes"

    # Try to get user info from token review
    local user_info
    user_info=$(kubectl create --dry-run=server --raw /apis/authentication.k8s.io/v1/tokenreviews -f - <<EOF 2>/dev/null | jq -r '.status.user // empty'
{
  "apiVersion": "authentication.k8s.io/v1",
  "kind": "TokenReview",
  "spec": {
    "token": "$test_token"
  }
}
EOF
)
    if [[ -n "$user_info" ]]; then
      local username groups
      username=$(echo "$user_info" | jq -r '.username // "N/A"')
      groups=$(echo "$user_info" | jq -r '.groups // [] | join(", ")')
      _append "  Username: $username"
      _append "  Groups: $groups"
    else
      _append "  Could not retrieve user info from token"
    fi
  else
    _append "No test token available (not logged in via oc)"
  fi
  _append ""

  _section "Subscription → Model Mapping"
  local subscriptions_json sub_mapping
  subscriptions_json=$(kubectl get maassubscriptions -n $MAAS_SUBSCRIPTION_NAMESPACE -o json 2>/dev/null | jq -r '.items // []' 2>/dev/null)
  if [[ -n "$subscriptions_json" ]] && [[ "$subscriptions_json" != "[]" ]]; then
    sub_mapping=$(echo "$subscriptions_json" | jq -r '.[] |
      "Subscription: " + .metadata.name +
      "\n  Owner users: " + ((.spec.owner.users // []) | join(", ") | if . == "" then "(none)" else . end) +
      "\n  Owner groups: " + ((.spec.owner.groups // [] | map(.name)) | join(", ") | if . == "" then "(none)" else . end) +
      "\n  Models: " + ((.spec.modelRefs // [] | map(.namespace + "/" + .name)) | join(", ") | if . == "" then "(none)" else . end)' 2>/dev/null)
    if [[ -n "$sub_mapping" ]]; then
      _append "$sub_mapping"
    else
      _append "Failed to parse subscription data"
    fi
  else
    _append "No subscriptions found in $MAAS_SUBSCRIPTION_NAMESPACE"
  fi
  _append ""

  _section "Available Models (MaaSModelRefs)"
  local models_json model_listing
  models_json=$(kubectl get maasmodelrefs -A -o json 2>/dev/null | jq -r '.items // []' 2>/dev/null)
  if [[ -n "$models_json" ]] && [[ "$models_json" != "[]" ]]; then
    _append "Model Reference → Model ID / Endpoint"
    model_listing=$(echo "$models_json" | jq -r '.[] |
      "  " + .metadata.namespace + "/" + .metadata.name +
      " → " + (.spec.modelRef.name // "N/A") +
      " (" + (.status.phase // "unknown") + ")" +
      if .status.endpoint then "\n    Endpoint: " + .status.endpoint else "" end' 2>/dev/null)
    if [[ -n "$model_listing" ]]; then
      _append "$model_listing"
    else
      _append "Failed to parse model data"
    fi
  else
    _append "No MaaSModelRefs found"
  fi
  _append ""

  _section "Gateway / HTTPRoutes"
  _run "Gateway" "kubectl get gateway -n openshift-ingress maas-default-gateway -o wide 2>/dev/null || kubectl get gateway -A 2>/dev/null | head -10 || true"
  _run "HTTPRoutes (maas-api)" "kubectl get httproute maas-api-route -n $DEPLOYMENT_NAMESPACE -o wide 2>/dev/null || true"
  _append ""

  _section "Authorino"
  _run "Authorino pods" "kubectl get pods -n $AUTHORINO_NAMESPACE -l 'app.kubernetes.io/name=authorino' --no-headers 2>/dev/null; kubectl get pods -n openshift-ingress -l 'app.kubernetes.io/name=authorino' --no-headers 2>/dev/null; echo '---'; kubectl get pods -A -l 'app.kubernetes.io/name=authorino' -o wide 2>/dev/null || true"
  _append ""

  # Determine maas-api namespace from controller deployment
  local maas_api_ns
  local env_json
  env_json=$(kubectl get deployment maas-controller -n $DEPLOYMENT_NAMESPACE -o jsonpath='{.spec.template.spec.containers[0].env}' 2>/dev/null || echo "[]")

  # Try to get direct .value first
  maas_api_ns=$(echo "$env_json" | jq -r '.[] | select(.name=="MAAS_API_NAMESPACE") | .value // empty' 2>/dev/null)

  # If empty, check if using fieldRef (downward API)
  if [[ -z "$maas_api_ns" ]]; then
    local field_path
    field_path=$(echo "$env_json" | jq -r '.[] | select(.name=="MAAS_API_NAMESPACE") | .valueFrom.fieldRef.fieldPath // empty' 2>/dev/null)
    if [[ "$field_path" == "metadata.namespace" ]]; then
      # Using downward API - the value is the controller's namespace
      maas_api_ns="$DEPLOYMENT_NAMESPACE"
    fi
  fi

  # Fallback to deployment namespace if still empty
  [[ -z "$maas_api_ns" ]] && maas_api_ns="$DEPLOYMENT_NAMESPACE" || true

  local sub_select_url="https://maas-api.${maas_api_ns}.svc.cluster.local:8443/internal/v1/subscriptions/select"
  _section "Subscription Selector Endpoint Validation"
  _append "Expected URL (from maas-controller config): $sub_select_url"
  _append "  (MAAS_API_NAMESPACE resolved to: $maas_api_ns)"
  _append ""

  # Verify actual AuthPolicy configuration
  _append "--- Sample AuthPolicy subscription-info configuration ---"
  local sample_policy_json
  sample_policy_json=$(kubectl get authpolicies -A -l 'app.kubernetes.io/managed-by=maas-controller' -o json 2>/dev/null | jq -r '.items[0] // empty' 2>/dev/null)

  if [[ -n "$sample_policy_json" ]]; then
    local policy_name policy_ns
    policy_name=$(echo "$sample_policy_json" | jq -r '.metadata.name // "unknown"')
    policy_ns=$(echo "$sample_policy_json" | jq -r '.metadata.namespace // "unknown"')
    _append "  Inspecting: $policy_ns/$policy_name"

    local actual_url
    actual_url=$(echo "$sample_policy_json" | jq -r '.spec.rules.metadata."subscription-info".http.url // "N/A"' 2>/dev/null)
    _append "  Actual URL in AuthPolicy: $actual_url"

    local request_body
    request_body=$(echo "$sample_policy_json" | jq -r '.spec.rules.metadata."subscription-info".http.body.expression // "N/A"' 2>/dev/null)
    if echo "$request_body" | grep -q "requestedModel"; then
      _append "  ✅ Request body includes requestedModel field"
      # Extract the model reference from the body
      local model_ref
      model_ref=$(echo "$request_body" | grep -o '"requestedModel"[^"]*"[^"]*"' | sed 's/.*"\([^"]*\)".*/\1/' || echo "")
      if [[ -n "$model_ref" ]]; then
        _append "  Model reference: $model_ref"
      fi
    else
      _append "  ❌ Request body MISSING requestedModel field (should include model namespace/name)"
    fi
    _append "  Request body preview:"
    _append "$(echo "$request_body" | head -5 | sed 's/^/    /')"
  else
    _append "  No managed AuthPolicies found"
  fi
  _append ""

  local curl_ns="$AUTHORINO_NAMESPACE"
  if ! kubectl get namespace "$curl_ns" &>/dev/null; then
    curl_ns="openshift-ingress"
  fi
  if ! kubectl get namespace "$curl_ns" &>/dev/null; then
    curl_ns="$DEPLOYMENT_NAMESPACE"
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

  _section "Configuration Summary"
  _append "This summary helps compare local vs CI runs:"
  _append ""
  local total_models total_subs total_authpolicies total_kuadrant_authpolicies
  total_models=$(echo "$models_json" | jq '. | length' 2>/dev/null || echo "0")
  total_subs=$(echo "$subscriptions_json" | jq '. | length' 2>/dev/null || echo "0")
  total_authpolicies=$(kubectl get maasauthpolicies -n $MAAS_SUBSCRIPTION_NAMESPACE -o json 2>/dev/null | jq -r '.items | length' 2>/dev/null || echo "0")
  total_kuadrant_authpolicies=$(kubectl get authpolicies -A -l 'app.kubernetes.io/managed-by=maas-controller' -o json 2>/dev/null | jq -r '.items | length' 2>/dev/null || echo "0")

  _append "  MaaSModelRefs (all namespaces): $total_models"
  _append "  MaaSSubscriptions ($MAAS_SUBSCRIPTION_NAMESPACE): $total_subs"
  _append "  MaaSAuthPolicies ($MAAS_SUBSCRIPTION_NAMESPACE): $total_authpolicies"
  _append "  Generated Kuadrant AuthPolicies: $total_kuadrant_authpolicies"
  _append ""
  _append "  Subscription selector URL: $sub_select_url"
  _append "  Test user: $(oc whoami 2>/dev/null || echo 'N/A')"
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
  # Default: collect artifacts, then print auth debug report (also saved to file)
  collect_e2e_artifacts
  echo ""
  echo "========== MaaS Debug Report =========="
  local report
  report=$(run_auth_debug_report)
  echo "$report"
  mkdir -p "$ARTIFACTS_DIR"
  echo "$report" > "$ARTIFACTS_DIR/maas-debug-report.log"
  echo "MaaS debug report saved to $ARTIFACTS_DIR/maas-debug-report.log"
}

# Run main only when executed directly (not sourced)
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  main "$@"
fi
