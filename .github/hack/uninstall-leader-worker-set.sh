#!/usr/bin/env bash
# Completely remove the LeaderWorkerSet (LWS) operator from the cluster.
#
# Prerequisites: oc/kubectl with cluster-admin
# Usage: ./hack/uninstall-leader-worker-set.sh

set -euo pipefail

echo "========================================="
echo "Uninstalling LeaderWorkerSet operator"
echo "========================================="
echo ""

# 1. Delete all LeaderWorkerSet CRs (operator creates these for LLMInferenceService multi-node)
echo "1. Deleting LeaderWorkerSet resources..."
if oc get leaderworkerset -A --no-headers 2>/dev/null | grep -q .; then
  oc delete leaderworkerset -A --all --timeout=120s --ignore-not-found=true || true
  echo "   Waiting for LeaderWorkerSet cleanup..."
  sleep 5
else
  echo "   No LeaderWorkerSet resources found"
fi

# 2. Delete LeaderWorkerSetOperator CR
echo "2. Deleting LeaderWorkerSetOperator CR..."
oc delete leaderworkersetoperator cluster -n openshift-lws-operator --timeout=60s --ignore-not-found=true || true

# 3. Delete Subscription
echo "3. Deleting LWS Subscription..."
oc delete subscription leader-worker-set -n openshift-lws-operator --timeout=60s --ignore-not-found=true || true

# 4. Delete CSV (by label or name prefix)
echo "4. Deleting LWS CSV..."
for csv in $(oc get csv -n openshift-lws-operator --no-headers 2>/dev/null | grep -E 'leader-worker-set|leaderworkerset' | awk '{print $1}'); do
  oc delete csv "$csv" -n openshift-lws-operator --timeout=60s --ignore-not-found=true || true
done

# 5. Delete OperatorGroup
echo "5. Deleting LWS OperatorGroup..."
oc delete operatorgroup leader-worker-set -n openshift-lws-operator --timeout=60s --ignore-not-found=true || true

# 6. Delete namespace
echo "6. Deleting openshift-lws-operator namespace..."
oc delete namespace openshift-lws-operator --timeout=300s --ignore-not-found=true || true

# 7. Delete CRDs (removes the API types entirely)
echo "7. Deleting LeaderWorkerSet CRDs..."
oc delete crd leaderworkersets.leaderworkerset.x-k8s.io --timeout=60s --ignore-not-found=true || true
oc delete crd leaderworkersetoperators.operator.openshift.io --timeout=60s --ignore-not-found=true || true

echo ""
echo "========================================="
echo "LeaderWorkerSet operator removed"
echo "========================================="
echo ""
echo "Verify:"
echo "  oc get subscription,csv,operatorgroup -n openshift-lws-operator"
echo "  oc get crd | grep leaderworkerset"
echo "  oc get namespace openshift-lws-operator"
echo ""
