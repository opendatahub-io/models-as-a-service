#!/bin/bash
# =============================================================================
# MaaS Auth Debug Script
# =============================================================================
#
# Collects auth-related resources and validates connectivity to the maas-api
# subscription selector endpoint. Use this to diagnose 403/401 issues and
# DNS/connectivity problems (e.g. "no such host" for maas-api.*.svc.cluster.local).
#
# Output: All collected info is printed to stdout at the end.
#
# Usage:
#   ./test/e2e/scripts/debug_auth.sh
#   MAAS_NAMESPACE=opendatahub ./test/e2e/scripts/debug_auth.sh
#
# Note: First run may take 30-60s while ephemeral curl/busybox images are pulled.
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
OUTPUT=""

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

main() {
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
  _run "maas-api deployment (maas-api-namespace env)" \
    "kubectl get deployment maas-api -n $MAAS_NAMESPACE -o jsonpath='{.spec.template.spec.containers[?(@.name==\"maas-api\")].env}' 2>/dev/null | jq -r '.[] | select(.name==\"GATEWAY_NAMESPACE\" or .name==\"GATEWAY_NAME\") | \"\(.name)=\(.value)\"' 2>/dev/null || true"
  _append ""

  _section "maas-controller"
  _run "maas-controller pods" "kubectl get pods -n $MAAS_NAMESPACE -l app=maas-controller -o wide 2>/dev/null || true"
  _run "maas-controller MAAS_API_NAMESPACE" \
    "kubectl get deployment maas-controller -n $MAAS_NAMESPACE -o jsonpath='{.spec.template.spec.containers[0].env}' 2>/dev/null | jq -r '.[] | select(.name==\"MAAS_API_NAMESPACE\") | \"\(.name)=\(.value)\"' 2>/dev/null || echo 'N/A'"
  _append ""

  _section "Kuadrant AuthPolicies"
  _run "AuthPolicies (all namespaces)" "kubectl get authpolicies -A -o wide 2>/dev/null || true"
  _run "AuthPolicies in openshift-ingress" "kubectl get authpolicies -n openshift-ingress -o wide 2>/dev/null || true"
  _run "AuthPolicy subscription selector URL" \
    "kubectl get authpolicies -n $MAAS_NAMESPACE -o yaml 2>/dev/null | grep -E 'url:|maas-api\.' | head -5 || echo 'N/A'"
  _append ""

  _section "TokenRateLimitPolicies"
  _run "TokenRateLimitPolicies (all namespaces)" "kubectl get tokenratelimitpolicies -A -o wide 2>/dev/null || true"
  _append ""

  _section "MaaS CRs"
  _run "MaaSAuthPolicies" "kubectl get maasauthpolicies -n $MAAS_NAMESPACE -o wide 2>/dev/null || true"
  _run "MaaSSubscriptions" "kubectl get maassubscriptions -n $MAAS_NAMESPACE -o wide 2>/dev/null || true"
  _run "MaaSModels" "kubectl get maasmodels -n $MAAS_NAMESPACE -o wide 2>/dev/null || true"
  _append ""

  _section "Gateway / HTTPRoutes"
  _run "Gateway" "kubectl get gateway -n openshift-ingress maas-default-gateway -o wide 2>/dev/null || kubectl get gateway -A 2>/dev/null | head -10 || true"
  _run "HTTPRoutes (maas-api)" "kubectl get httproute maas-api-route -n $MAAS_NAMESPACE -o wide 2>/dev/null || true"
  _run "HTTPRoutes (model routes)" "kubectl get httproutes -A -l 'gateway.networking.k8s.io/gateway-name=maas-default-gateway' -o wide 2>/dev/null | head -20 || true"
  _append ""

  _section "Authorino"
  _run "Authorino pods" "kubectl get pods -n $AUTHORINO_NAMESPACE -l 'app.kubernetes.io/name=authorino' --no-headers 2>/dev/null; kubectl get pods -n openshift-ingress -l 'app.kubernetes.io/name=authorino' --no-headers 2>/dev/null; echo '---'; kubectl get pods -A -l 'app.kubernetes.io/name=authorino' -o wide 2>/dev/null || true"
  _append ""

  _section "NetworkPolicies (opendatahub namespace)"
  _run "NetworkPolicies list" "kubectl get networkpolicies -n $MAAS_NAMESPACE -o wide 2>/dev/null || true"
  _run "NetworkPolicies full YAML" "kubectl get networkpolicies -n $MAAS_NAMESPACE -o yaml 2>/dev/null || true"
  _append ""

  # Determine maas-api namespace (where the service actually lives)
  local maas_api_ns
  maas_api_ns=$(kubectl get deployment maas-controller -n $MAAS_NAMESPACE -o jsonpath='{.spec.template.spec.containers[0].env}' 2>/dev/null | jq -r '.[] | select(.name=="MAAS_API_NAMESPACE") | .value' 2>/dev/null || echo "$MAAS_NAMESPACE")
  [[ -z "$maas_api_ns" ]] && maas_api_ns="$MAAS_NAMESPACE"

  local sub_select_url="https://maas-api.${maas_api_ns}.svc.cluster.local:8443/v1/subscriptions/select"
  _section "Subscription Selector Endpoint Validation"
  _append "Expected URL (from maas-controller config): $sub_select_url"
  _append ""

  # Hit the endpoint from within the cluster using an ephemeral curl pod.
  # Run from Authorino's namespace to simulate Authorino's network/DNS perspective.
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

  _section "maas-api Logs (last 100 lines)"
  _run "maas-api logs" "kubectl logs -n $MAAS_NAMESPACE -l app.kubernetes.io/name=maas-api --tail=100 --all-containers=true 2>/dev/null || true"
  _append ""

  _section "maas-controller Logs (last 100 lines)"
  _run "maas-controller logs" "kubectl logs -n $MAAS_NAMESPACE -l app=maas-controller --tail=100 --all-containers=true 2>/dev/null || true"
  _append ""

  _section "Authorino Logs (last 50 lines, auth-related)"
  _run "Authorino logs" "kubectl logs -n $AUTHORINO_NAMESPACE -l app.kubernetes.io/name=authorino --tail=50 --all-containers=true 2>/dev/null || kubectl logs -n openshift-ingress -l app.kubernetes.io/name=authorino --tail=50 --all-containers=true 2>/dev/null || true"
  _append ""

  _section "DNS Resolution Check (from cluster, same ns as Authorino)"
  _append "Resolving: maas-api.${maas_api_ns}.svc.cluster.local"
  local dns_out
  dns_out=$(kubectl run "debug-dns-$(date +%s)" --rm --restart=Never --image=busybox:1.36 -n "$curl_ns" -- \
    nslookup "maas-api.${maas_api_ns}.svc.cluster.local" 2>&1) || dns_out="nslookup failed"
  _append "$dns_out"
  _append ""

  # Print everything to stdout
  echo "$OUTPUT"
}

main "$@"
