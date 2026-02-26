#!/bin/bash
#
# cleanup-odh.sh - Remove OpenDataHub operator and all related resources
#
# This script removes:
# - DataScienceCluster and DSCInitialization custom resources
# - ODH operator Subscription and CSV
# - Custom CatalogSource (odh-custom-catalog)
# - ODH operator namespace (odh-operator)
# - OpenDataHub application namespace (opendatahub)
# - ODH CRDs (optional)
#
# Usage: ./cleanup-odh.sh [--include-crds]
#

set -euo pipefail

INCLUDE_CRDS=false

if [[ "${1:-}" == "--include-crds" ]]; then
    INCLUDE_CRDS=true
fi

echo "=== OpenDataHub Cleanup Script ==="
echo ""

# Check cluster connection
if ! kubectl cluster-info &>/dev/null; then
    echo "ERROR: Not connected to a cluster. Please run 'oc login' first."
    exit 1
fi

# jq required for force-removing stuck namespaces
if ! command -v jq &>/dev/null; then
    echo "WARNING: 'jq' not found. Stuck namespaces may not be force-removed (install jq for full cleanup)."
fi

echo "Connected to cluster. Starting cleanup..."
echo ""

# 1. Delete DataScienceCluster instances
echo "1. Deleting DataScienceCluster instances..."
kubectl delete datasciencecluster --all -A --ignore-not-found --timeout=120s 2>/dev/null || true

# 2. Delete DSCInitialization instances
echo "2. Deleting DSCInitialization instances..."
kubectl delete dscinitialization --all -A --ignore-not-found --timeout=120s 2>/dev/null || true

# 3. Delete ODH Subscription (check both possible namespaces)
echo "3. Deleting ODH Subscriptions..."
kubectl delete subscription opendatahub-operator -n odh-operator --ignore-not-found 2>/dev/null || true
kubectl delete subscription opendatahub-operator -n openshift-operators --ignore-not-found 2>/dev/null || true

# 4. Delete ODH CSVs
echo "4. Deleting ODH CSVs..."
# Delete by label if possible
kubectl delete csv -n odh-operator -l operators.coreos.com/opendatahub-operator.odh-operator --ignore-not-found 2>/dev/null || true
kubectl delete csv -n openshift-operators -l operators.coreos.com/opendatahub-operator.openshift-operators --ignore-not-found 2>/dev/null || true
# Also try by name prefix
for ns in odh-operator openshift-operators; do
    for csv in $(kubectl get csv -n "$ns" -o name 2>/dev/null | grep opendatahub-operator || true); do
        echo "   Deleting $csv in $ns..."
        kubectl delete "$csv" -n "$ns" --ignore-not-found 2>/dev/null || true
    done
done

# 5. Delete custom CatalogSource
echo "5. Deleting custom CatalogSource..."
kubectl delete catalogsource odh-custom-catalog -n openshift-marketplace --ignore-not-found 2>/dev/null || true

# 6. Delete OperatorGroup (if in dedicated namespace)
echo "6. Deleting ODH OperatorGroup..."
kubectl delete operatorgroup odh-operator-group -n odh-operator --ignore-not-found 2>/dev/null || true

# 7. Delete odh-operator namespace
echo "7. Deleting odh-operator namespace..."
kubectl delete ns odh-operator --ignore-not-found --timeout=120s 2>/dev/null || true

# 8. Delete opendatahub namespace (contains deployed components)
echo "8. Deleting opendatahub namespace..."
kubectl delete ns opendatahub --ignore-not-found --timeout=120s 2>/dev/null || true

# 8b. Force-remove opendatahub if stuck terminating (MaaS CRs often have finalizers)
if kubectl get namespace opendatahub &>/dev/null; then
    echo "   opendatahub stuck terminating, removing finalizers..."
    # Remove finalizers from MaaS CRs (common blockers)
    for name in $(kubectl get maasauthpolicies.maas.opendatahub.io -n opendatahub --no-headers 2>/dev/null | awk '{print $1}'); do
        echo "   Removing finalizers from MaaSAuthPolicy $name..."
        kubectl patch maasauthpolicies.maas.opendatahub.io "$name" -n opendatahub --type=json -p='[{"op": "remove", "path": "/metadata/finalizers"}]' 2>/dev/null || true
    done
    for name in $(kubectl get maasmodels.maas.opendatahub.io -n opendatahub --no-headers 2>/dev/null | awk '{print $1}'); do
        echo "   Removing finalizers from MaaSModel $name..."
        kubectl patch maasmodels.maas.opendatahub.io "$name" -n opendatahub --type=json -p='[{"op": "remove", "path": "/metadata/finalizers"}]' 2>/dev/null || true
    done
    for name in $(kubectl get maassubscriptions.maas.opendatahub.io -n opendatahub --no-headers 2>/dev/null | awk '{print $1}'); do
        echo "   Removing finalizers from MaaSSubscription $name..."
        kubectl patch maassubscriptions.maas.opendatahub.io "$name" -n opendatahub --type=json -p='[{"op": "remove", "path": "/metadata/finalizers"}]' 2>/dev/null || true
    done
    # Remove finalizers from namespace itself (requires jq)
    if kubectl get namespace opendatahub &>/dev/null && command -v jq &>/dev/null; then
        echo "   Removing finalizers from opendatahub namespace..."
        kubectl get namespace opendatahub -o json | jq '.spec.finalizers = []' | \
            kubectl replace --raw "/api/v1/namespaces/opendatahub/finalize" -f -
    elif kubectl get namespace opendatahub &>/dev/null; then
        echo "   WARNING: Install 'jq' to force-remove namespace finalizers. Namespace may remain terminating."
    fi
    echo "   Waiting for opendatahub namespace to be removed..."
    for i in {1..30}; do
        if ! kubectl get namespace opendatahub &>/dev/null; then
            echo "   opendatahub namespace removed."
            break
        fi
        sleep 2
    done
fi

# 9. Delete policy engine namespaces (Kuadrant or RHCL)
for policy_ns in kuadrant-system rh-connectivity-link; do
  echo "9. Deleting $policy_ns namespace (if installed)..."
  kubectl delete ns "$policy_ns" --ignore-not-found --timeout=60s 2>/dev/null || true

  # Force-remove if stuck terminating
  if kubectl get namespace "$policy_ns" &>/dev/null; then
    echo "   $policy_ns stuck terminating, removing finalizers..."
    for name in $(kubectl get authorinos.operator.authorino.kuadrant.io -n "$policy_ns" --no-headers 2>/dev/null | awk '{print $1}'); do
      kubectl patch authorinos.operator.authorino.kuadrant.io "$name" -n "$policy_ns" --type=json -p='[{"op": "remove", "path": "/metadata/finalizers"}]' 2>/dev/null || true
    done
    for name in $(kubectl get kuadrants.kuadrant.io -n "$policy_ns" --no-headers 2>/dev/null | awk '{print $1}'); do
      kubectl patch kuadrants.kuadrant.io "$name" -n "$policy_ns" --type=json -p='[{"op": "remove", "path": "/metadata/finalizers"}]' 2>/dev/null || true
    done
    for name in $(kubectl get limitadors.limitador.kuadrant.io -n "$policy_ns" --no-headers 2>/dev/null | awk '{print $1}'); do
      kubectl patch limitadors.limitador.kuadrant.io "$name" -n "$policy_ns" --type=json -p='[{"op": "remove", "path": "/metadata/finalizers"}]' 2>/dev/null || true
    done
    if kubectl get namespace "$policy_ns" &>/dev/null && command -v jq &>/dev/null; then
      kubectl get namespace "$policy_ns" -o json | jq '.spec.finalizers = []' | \
        kubectl replace --raw "/api/v1/namespaces/$policy_ns/finalize" -f -
    fi
  fi
done

# 10. Delete llm namespace and model resources
echo "10. Deleting LLM models and namespace..."
if kubectl get ns llm &>/dev/null; then
    # Delete LLMInferenceService resources first (they have finalizers)
    echo "   Deleting LLMInferenceService resources..."
    kubectl delete llminferenceservice --all -n llm --ignore-not-found --timeout=30s 2>/dev/null || true
    
    # If deletion timed out, force remove finalizers
    for resource in $(kubectl get llminferenceservice -n llm -o name 2>/dev/null || true); do
        echo "   Removing finalizers from $resource..."
        kubectl patch "$resource" -n llm --type=json -p='[{"op": "remove", "path": "/metadata/finalizers"}]' 2>/dev/null || true
    done
    
    # Delete KServe InferenceService resources (also have finalizers)
    echo "   Deleting InferenceService resources..."
    kubectl delete inferenceservice --all -n llm --ignore-not-found --timeout=30s 2>/dev/null || true
    
    # If deletion timed out, force remove finalizers
    for resource in $(kubectl get inferenceservice -n llm -o name 2>/dev/null || true); do
        echo "   Removing finalizers from $resource..."
        kubectl patch "$resource" -n llm --type=json -p='[{"op": "remove", "path": "/metadata/finalizers"}]' 2>/dev/null || true
    done
    
    # Now delete the namespace
    echo "   Deleting llm namespace..."
    kubectl delete ns llm --ignore-not-found --timeout=120s 2>/dev/null || true
else
    echo "   llm namespace not found, skipping"
fi

# 11. Delete gateway resources in openshift-ingress
echo "11. Deleting gateway resources..."
kubectl delete gateway maas-default-gateway -n openshift-ingress --ignore-not-found 2>/dev/null || true
kubectl delete envoyfilter -n openshift-ingress -l kuadrant.io/managed=true --ignore-not-found 2>/dev/null || true
kubectl delete envoyfilter kuadrant-auth-tls-fix -n openshift-ingress --ignore-not-found 2>/dev/null || true
kubectl delete authpolicy -n openshift-ingress --all --ignore-not-found 2>/dev/null || true
kubectl delete ratelimitpolicy -n openshift-ingress --all --ignore-not-found 2>/dev/null || true
kubectl delete tokenratelimitpolicy -n openshift-ingress --all --ignore-not-found 2>/dev/null || true

# 12. Delete MaaS RBAC (ClusterRoles, ClusterRoleBindings - can conflict with other managers)
echo "12. Deleting MaaS RBAC..."
kubectl delete clusterrolebinding maas-api maas-controller-rolebinding --ignore-not-found 2>/dev/null || true
kubectl delete clusterrole maas-api maas-controller-role --ignore-not-found 2>/dev/null || true

# 13. Optionally delete CRDs
if $INCLUDE_CRDS; then
    echo "13. Deleting ODH CRDs..."
    kubectl delete crd datascienceclusters.datasciencecluster.opendatahub.io --ignore-not-found 2>/dev/null || true
    kubectl delete crd dscinitializations.dscinitialization.opendatahub.io --ignore-not-found 2>/dev/null || true
    kubectl delete crd datasciencepipelinesapplications.datasciencepipelinesapplications.opendatahub.io --ignore-not-found 2>/dev/null || true
    # Add more CRDs as needed
else
    echo "13. Skipping CRD deletion (use --include-crds to remove CRDs)"
fi

echo ""
echo "=== Cleanup Complete ==="
echo ""
echo "Verify cleanup with:"
echo "  kubectl get subscription -A | grep -i odh"
echo "  kubectl get csv -A | grep -i odh"
echo "  kubectl get ns | grep -E 'odh|opendatahub|kuadrant|rh-connectivity-link|llm'"