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

echo "Connected to cluster. Starting cleanup..."
echo ""

# 1. Delete DataScienceCluster instances
echo "1. Deleting DataScienceCluster instances..."
kubectl delete datasciencecluster --all -A --ignore-not-found --timeout=120s 2>/dev/null || true

# 2. Delete DSCInitialization instances
echo "2. Deleting DSCInitialization instances..."
kubectl delete dscinitialization --all -A --ignore-not-found --timeout=120s 2>/dev/null || true

# 3. Delete ODH Subscriptions from all namespaces
echo "3. Deleting ODH Subscriptions from all namespaces..."
# Find all ODH subscriptions
ODH_SUBS=$(kubectl get subscription -A -o json 2>/dev/null | \
    jq -r '.items[] | select(.spec.name | contains("opendatahub")) | "\(.metadata.namespace)/\(.metadata.name)"' 2>/dev/null || \
    kubectl get subscription -A | grep opendatahub-operator | awk '{print $1"/"$2}')

if [ -n "$ODH_SUBS" ]; then
    echo "$ODH_SUBS" | while IFS='/' read -r ns name; do
        if [ -n "$ns" ] && [ -n "$name" ]; then
            echo "   Deleting subscription $name in namespace $ns..."
            kubectl delete subscription "$name" -n "$ns" --ignore-not-found --timeout=60s 2>/dev/null || true
        fi
    done
else
    echo "   No ODH subscriptions found"
fi

# 4. Delete ODH CSVs from all namespaces
echo "4. Deleting ODH CSVs from all namespaces..."
# Find all namespaces with ODH CSVs
for ns in $(kubectl get namespace -o json 2>/dev/null | jq -r '.items[].metadata.name' 2>/dev/null || kubectl get namespace -o name | sed 's/namespace\///'); do
    # Check if namespace has ODH CSV
    ODH_CSVS=$(kubectl get csv -n "$ns" -o json 2>/dev/null | \
        jq -r '.items[] | select(.metadata.name | contains("opendatahub")) | .metadata.name' 2>/dev/null || \
        kubectl get csv -n "$ns" 2>/dev/null | grep opendatahub-operator | awk '{print $1}' || true)
    
    if [ -n "$ODH_CSVS" ]; then
        echo "$ODH_CSVS" | while read -r csv; do
            if [ -n "$csv" ]; then
                echo "   Deleting CSV $csv in namespace $ns..."
                # Remove finalizers first
                kubectl patch csv "$csv" -n "$ns" --type=json -p='[{"op": "remove", "path": "/metadata/finalizers"}]' 2>/dev/null || true
                # Delete the CSV
                kubectl delete csv "$csv" -n "$ns" --ignore-not-found --timeout=60s 2>/dev/null || true
            fi
        done
    fi
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
kubectl delete ns opendatahub --ignore-not-found --timeout=300s 2>/dev/null || true

# 9. Delete opendatahub-operator-system namespace (if exists)
echo "9. Deleting opendatahub-operator-system namespace..."
kubectl delete ns opendatahub-operator-system --ignore-not-found --timeout=120s 2>/dev/null || true

# Note: We don't delete kuadrant-system as it's used by RHCL (not just ODH)

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

# 12. Delete ODH CRDs (all of them)
if $INCLUDE_CRDS; then
    echo "12. Deleting ALL ODH and KServe CRDs..."
    
    # Get all ODH-related CRDs
    ODH_CRDS=$(kubectl get crd -o json 2>/dev/null | \
        jq -r '.items[] | select(.metadata.name | contains("opendatahub") or contains("kserve")) | .metadata.name' 2>/dev/null || \
        kubectl get crd | grep -E "opendatahub|kserve" | awk '{print $1}')
    
    if [ -n "$ODH_CRDS" ]; then
        echo "   Found $(echo "$ODH_CRDS" | wc -l) ODH/KServe CRDs to delete"
        echo "$ODH_CRDS" | while read -r crd; do
            if [ -n "$crd" ]; then
                echo "   Deleting CRD: $crd"
                # Remove finalizers first if any
                kubectl patch crd "$crd" --type=json -p='[{"op": "remove", "path": "/metadata/finalizers"}]' 2>/dev/null || true
                # Delete the CRD
                kubectl delete crd "$crd" --ignore-not-found --timeout=60s 2>/dev/null || true
            fi
        done
        echo "   ✅ All ODH/KServe CRDs deleted"
    else
        echo "   No ODH/KServe CRDs found"
    fi
else
    echo "12. Skipping CRD deletion (use --include-crds to remove CRDs)"
    echo "   To delete all CRDs, run: $0 --include-crds"
fi

# 13. Verification
echo ""
echo "13. Verifying cleanup..."
REMAINING_SUBS=$(kubectl get subscription -A 2>/dev/null | grep -i opendatahub | wc -l || echo "0")
REMAINING_CSVS=$(kubectl get csv -A 2>/dev/null | grep -i opendatahub | wc -l || echo "0")
REMAINING_CRDS=0
if $INCLUDE_CRDS; then
    REMAINING_CRDS=$(kubectl get crd 2>/dev/null | grep -E "opendatahub|kserve" | wc -l || echo "0")
fi

echo ""
echo "=== Cleanup Complete ==="
echo ""
echo "Verification:"
echo "  ODH Subscriptions remaining: $REMAINING_SUBS"
echo "  ODH CSVs remaining: $REMAINING_CSVS"
if $INCLUDE_CRDS; then
    echo "  ODH/KServe CRDs remaining: $REMAINING_CRDS"
else
    echo "  ODH/KServe CRDs: $(kubectl get crd 2>/dev/null | grep -E "opendatahub|kserve" | wc -l || echo "0") (not deleted - use --include-crds)"
fi
echo ""
echo "Manual verification commands:"
echo "  kubectl get subscription -A | grep -i odh"
echo "  kubectl get csv -A | grep -i odh"
echo "  kubectl get ns | grep -E 'odh|opendatahub|opendatahub-operator-system|llm'"
if $INCLUDE_CRDS; then
    echo "  kubectl get crd | grep -E 'opendatahub|kserve'"
fi

