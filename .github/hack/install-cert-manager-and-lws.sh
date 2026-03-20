#!/usr/bin/env bash
# Install cert-manager and LeaderWorkerSet (LWS) operators on OpenShift.
# cert-manager is installed first (LWS depends on it per Red Hat docs).
#
# Prerequisites: oc/kubectl, cluster-admin, cluster with redhat-operators catalog.
# For clusters without Red Hat entitlement, use upstream LWS per docs/content/install/platform-setup.md.
#
# Usage: ./hack/install-cert-manager-and-lws.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# From .github/hack/ go up two levels to project root
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
DATA_DIR="${REPO_ROOT}/scripts/data"

wait_subscription() {
  local ns="$1"
  local name="$2"
  echo "  Waiting for Subscription $ns/$name..."
  kubectl wait subscription.operators.coreos.com --timeout=300s -n "$ns" "$name" \
    --for=jsonpath='{.status.currentCSV}' 2>/dev/null || {
    echo "ERROR: Subscription $ns/$name did not get currentCSV in time"
    return 1
  }
  local csv
  csv=$(kubectl get subscription.operators.coreos.com -n "$ns" "$name" -o jsonpath='{.status.currentCSV}')
  while ! kubectl get -n "$ns" csv "$csv" >/dev/null 2>&1; do
    sleep 1
  done
  echo "  Waiting for CSV $csv to succeed..."
  kubectl wait -n "$ns" --for=jsonpath='{.status.phase}'=Succeeded csv "$csv" --timeout=300s
}

echo "=== Installing cert-manager and LeaderWorkerSet operators ==="
echo ""

# 1. cert-manager (required first)
echo "1. Installing cert-manager operator..."
kubectl apply -f "${DATA_DIR}/cert-manager-subscription.yaml"
wait_subscription "cert-manager-operator" "openshift-cert-manager-operator"
echo "   cert-manager ready."
echo ""

# 2. LeaderWorkerSet
echo "2. Installing LeaderWorkerSet operator..."
kubectl apply -f "${DATA_DIR}/lws-subscription.yaml"
wait_subscription "openshift-lws-operator" "leader-worker-set"
echo "   LeaderWorkerSet operator ready."
echo ""

# 3. Activate LWS API (LeaderWorkerSetOperator CR)
echo "3. Activating LeaderWorkerSet API..."
kubectl apply -f "${DATA_DIR}/lws-operator-cr.yaml"
echo "   LeaderWorkerSetOperator CR applied."
echo ""

echo "=== Done ==="
echo ""
echo "Verify:"
echo "  oc get pods -n cert-manager-operator"
echo "  oc get pods -n openshift-lws-operator"
echo "  oc get crd leaderworkersets.leaderworkerset.x-k8s.io"
