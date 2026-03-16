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
# - MaaS subscription namespace (models-as-a-service)
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

force_delete_namespace() {
    local ns=$1
    shift
    local cr_types=("$@")
    
    if ! kubectl get namespace "$ns" &>/dev/null; then
        echo "   $ns not found, skipping"
        return 0
    fi
    
    # Remove finalizers from CRs first (controller likely gone)
    for cr_type in "${cr_types[@]}"; do
        for name in $(kubectl get "$cr_type" -n "$ns" -o name 2>/dev/null); do
            kubectl patch "$name" -n "$ns" --type=json \
                -p='[{"op": "remove", "path": "/metadata/finalizers"}]' 2>/dev/null || true
        done
    done
    
    # Delete namespace
    kubectl delete ns "$ns" --ignore-not-found --timeout=30s 2>/dev/null || true
    
    # If still stuck, remove namespace finalizers
    if kubectl get namespace "$ns" &>/dev/null && command -v jq &>/dev/null; then
        kubectl get namespace "$ns" -o json | jq '.spec.finalizers = []' | \
            kubectl replace --raw "/api/v1/namespaces/$ns/finalize" -f - 2>/dev/null || true
    fi
    
    kubectl wait --for=delete namespace/"$ns" --timeout=30s 2>/dev/null || true
}

# 9. Delete models-as-a-service namespace (contains MaaS CRs)
echo "9. Deleting models-as-a-service namespace..."
force_delete_namespace "models-as-a-service" \
    "maasauthpolicies.maas.opendatahub.io" "maassubscriptions.maas.opendatahub.io"

# 10. Delete policy engine namespaces (Kuadrant or RHCL)
for policy_ns in kuadrant-system rh-connectivity-link; do
    echo "10. Deleting $policy_ns namespace (if installed)..."
    force_delete_namespace "$policy_ns" \
    "authorinos.operator.authorino.kuadrant.io" "kuadrants.kuadrant.io" "limitadors.limitador.kuadrant.io"
done

# 11. Delete llm namespace and model resources
echo "11. Deleting LLM models and namespace..."
force_delete_namespace "llm" "llminferenceservice" "inferenceservice" "maasmodelrefs.maas.opendatahub.io"

# 12. Delete gateway resources in openshift-ingress
echo "12. Deleting gateway resources..."
kubectl delete gateway maas-default-gateway -n openshift-ingress --ignore-not-found 2>/dev/null || true
kubectl delete envoyfilter -n openshift-ingress -l kuadrant.io/managed=true --ignore-not-found 2>/dev/null || true
kubectl delete envoyfilter kuadrant-auth-tls-fix -n openshift-ingress --ignore-not-found 2>/dev/null || true
kubectl delete authpolicy -n openshift-ingress --all --ignore-not-found 2>/dev/null || true
kubectl delete ratelimitpolicy -n openshift-ingress --all --ignore-not-found 2>/dev/null || true
kubectl delete tokenratelimitpolicy -n openshift-ingress --all --ignore-not-found 2>/dev/null || true

# 13. Delete MaaS RBAC (ClusterRoles, ClusterRoleBindings - can conflict with other managers)
echo "13. Deleting MaaS RBAC..."
kubectl delete clusterrolebinding maas-api maas-controller-rolebinding --ignore-not-found 2>/dev/null || true
kubectl delete clusterrole maas-api maas-controller-role --ignore-not-found 2>/dev/null || true

# 14. Optionally delete CRDs
if $INCLUDE_CRDS; then
    echo "14. Deleting ODH CRDs..."
    kubectl delete crd datascienceclusters.datasciencecluster.opendatahub.io --ignore-not-found 2>/dev/null || true
    kubectl delete crd dscinitializations.dscinitialization.opendatahub.io --ignore-not-found 2>/dev/null || true
    kubectl delete crd datasciencepipelinesapplications.datasciencepipelinesapplications.opendatahub.io --ignore-not-found 2>/dev/null || true
    # Add more CRDs as needed
else
    echo "14. Skipping CRD deletion (use --include-crds to remove CRDs)"
fi

echo ""
echo "=== Cleanup Complete ==="
echo ""
echo "Verify cleanup with:"
echo "  kubectl get subscription -A | grep -i odh"
echo "  kubectl get csv -A | grep -i odh"
echo "  kubectl get ns | grep -E 'odh|opendatahub|models-as-a-service|kuadrant|rh-connectivity-link|llm'"