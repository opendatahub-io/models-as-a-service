#!/bin/bash
#
# cleanup-odh.sh - Remove OpenDataHub/RHOAI MaaS resources and related operators
#
# This script removes:
# - DataScienceCluster and DSCInitialization custom resources
# - ODH operator Subscription and CSV
# - Custom CatalogSource (odh-custom-catalog)
# - ODH operator namespace (odh-operator)
# - OpenDataHub application namespace (opendatahub)
# - MaaS resources from RHOAI namespace (redhat-ods-applications)
# - MaaS subscription namespace (models-as-a-service)
# - Policy engine artifacts (Kuadrant/RHCL OLM resources, AuthConfig CRs)
# - Keycloak identity provider (if deployed)
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

# Detect operator type to find the right application namespace
MAAS_APP_NAMESPACE=""
if kubectl get subscription rhods-operator -n redhat-ods-operator &>/dev/null; then
    MAAS_APP_NAMESPACE="redhat-ods-applications"
    echo "Detected RHOAI operator (application namespace: $MAAS_APP_NAMESPACE)"
elif kubectl get subscription opendatahub-operator -A &>/dev/null; then
    MAAS_APP_NAMESPACE="opendatahub"
    echo "Detected ODH operator (application namespace: $MAAS_APP_NAMESPACE)"
else
    echo "No operator detected, will clean both namespaces"
fi
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

# 8b. Clean MaaS resources from RHOAI application namespace
# On RHOAI clusters, MaaS resources live in redhat-ods-applications which is
# operator-managed. We delete MaaS resources individually instead of the namespace.
cleanup_maas_resources() {
    local ns=$1
    if ! kubectl get namespace "$ns" &>/dev/null; then
        echo "   $ns not found, skipping"
        return 0
    fi

    echo "   Cleaning MaaS resources from $ns..."
    kubectl delete deployment maas-api maas-controller postgres -n "$ns" --ignore-not-found 2>/dev/null || true
    kubectl delete service maas-api postgres -n "$ns" --ignore-not-found 2>/dev/null || true
    kubectl delete secret maas-db-config postgres-creds -n "$ns" --ignore-not-found 2>/dev/null || true
    kubectl delete authpolicy maas-api-auth-policy -n "$ns" --ignore-not-found 2>/dev/null || true
    kubectl delete httproute maas-api-route -n "$ns" --ignore-not-found 2>/dev/null || true
    kubectl delete destinationrule maas-api-backend-tls -n "$ns" --ignore-not-found 2>/dev/null || true
    kubectl delete networkpolicy maas-api-cleanup-restrict maas-authorino-allow -n "$ns" --ignore-not-found 2>/dev/null || true
    kubectl delete cronjob maas-api-key-cleanup -n "$ns" --ignore-not-found 2>/dev/null || true
    kubectl delete role maas-api-db-secret maas-controller-leader-election-role -n "$ns" --ignore-not-found 2>/dev/null || true
    kubectl delete rolebinding maas-api-db-secret maas-controller-leader-election-rolebinding -n "$ns" --ignore-not-found 2>/dev/null || true
    kubectl delete serviceaccount maas-api maas-controller -n "$ns" --ignore-not-found 2>/dev/null || true
    echo "   ✅ MaaS resources cleaned from $ns"
}

if [[ "$MAAS_APP_NAMESPACE" == "redhat-ods-applications" ]]; then
    echo "8b. Cleaning MaaS resources from RHOAI namespace..."
    cleanup_maas_resources "redhat-ods-applications"
elif [[ -z "$MAAS_APP_NAMESPACE" ]]; then
    # No operator detected, clean both just in case
    echo "8b. Cleaning MaaS resources from both possible namespaces..."
    cleanup_maas_resources "redhat-ods-applications"
    cleanup_maas_resources "opendatahub"
fi

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

# 10. Delete policy engine workload CRs (before operator cleanup)
# This allows operators to cleanly delete Deployments/ReplicaSets before we delete the operators themselves
echo "10. Deleting policy engine workload CRs..."
for policy_ns in kuadrant-system rh-connectivity-link; do
    if kubectl get namespace "$policy_ns" &>/dev/null; then
        echo "   Deleting workload CRs in $policy_ns..."
        # Delete high-level CRs to trigger operator cleanup of owned resources
        kubectl delete kuadrant --all -n "$policy_ns" --ignore-not-found --timeout=60s 2>/dev/null || true
        kubectl delete limitador --all -n "$policy_ns" --ignore-not-found --timeout=60s 2>/dev/null || true
        kubectl delete authorino --all -n "$policy_ns" --ignore-not-found --timeout=60s 2>/dev/null || true

        # Wait for CRs to be fully deleted (finalizers processed) before removing operators
        # This prevents orphaned resources if we delete operators while finalizers are still running
        echo "   Waiting for CR finalizers to complete in $policy_ns..."
        timeout=60
        deadline=$((SECONDS + timeout))
        remaining=1
        while [[ $SECONDS -lt $deadline ]]; do
            # Count remaining CRs (wc -l counts all lines, subtract 1 for header if present)
            count=$(kubectl get kuadrant,limitador,authorino -n "$policy_ns" --ignore-not-found 2>/dev/null | wc -l)
            remaining=$((count > 0 ? count - 1 : 0))
            if [[ $remaining -eq 0 ]]; then
                echo "   ✅ All workload CRs deleted from $policy_ns"
                break
            fi
            sleep 2
        done
        if [[ $remaining -gt 0 ]]; then
            echo "   ⚠️  Warning: $remaining CR(s) still exist in $policy_ns after ${timeout}s (finalizers may be stuck)"
        fi
    fi
done

# 11. Delete policy engine OLM resources (before namespace deletion)
echo "11. Cleaning up policy engine OLM resources..."
# Kuadrant cleanup
if kubectl get namespace kuadrant-system &>/dev/null; then
    echo "   Cleaning up Kuadrant OLM resources..."
    # Delete Subscriptions (triggers CSV cleanup by OLM)
    kubectl delete subscription --all -n kuadrant-system --ignore-not-found --timeout=60s 2>/dev/null || true
    # Delete CSVs explicitly (includes dependent operators: authorino, limitador, dns-operator)
    kubectl delete csv -n kuadrant-system --all --ignore-not-found --timeout=60s 2>/dev/null || true
    # Delete CatalogSource (created in kuadrant-system namespace, not marketplace)
    kubectl delete catalogsource kuadrant-operator-catalog -n kuadrant-system --ignore-not-found 2>/dev/null || true
    echo "   ✅ Kuadrant OLM resources cleaned"
fi
# RHCL cleanup
if kubectl get namespace rh-connectivity-link &>/dev/null; then
    echo "   Cleaning up RHCL OLM resources..."
    kubectl delete subscription --all -n rh-connectivity-link --ignore-not-found --timeout=60s 2>/dev/null || true
    kubectl delete csv -n rh-connectivity-link --all --ignore-not-found --timeout=60s 2>/dev/null || true
    # Note: RHCL uses redhat-operators catalog (not a custom catalog), so no catalog deletion needed
    echo "   ✅ RHCL OLM resources cleaned"
fi

# 11b. Delete AuthConfig CRs cluster-wide
# Old AuthConfig CRs can block new policy engine installs if the CRD schema changes.
echo "11b. Deleting AuthConfig CRs..."
kubectl delete authconfig --all --all-namespaces --ignore-not-found 2>/dev/null || true

# 12. Delete policy engine namespaces (Kuadrant or RHCL)
for policy_ns in kuadrant-system rh-connectivity-link; do
    echo "12. Deleting $policy_ns namespace (if installed)..."
    force_delete_namespace "$policy_ns" \
    "authorinos.operator.authorino.kuadrant.io" "kuadrants.kuadrant.io" "limitadors.limitador.kuadrant.io"
done

# 13. Delete Keycloak identity provider (if installed)
echo "13. Deleting Keycloak namespace (if installed)..."
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && cd ../.. && pwd)"
if [[ -f "${SCRIPT_DIR}/scripts/cleanup-keycloak.sh" ]]; then
    # Pass --delete-crds if --include-crds was specified for this script
    if $INCLUDE_CRDS; then
        "${SCRIPT_DIR}/scripts/cleanup-keycloak.sh" --force --delete-crds 2>/dev/null || true
    else
        "${SCRIPT_DIR}/scripts/cleanup-keycloak.sh" --force 2>/dev/null || true
    fi
else
    # Fallback if cleanup script not found - direct cleanup
    force_delete_namespace "keycloak-system" "keycloaks.k8s.keycloak.org"
    if $INCLUDE_CRDS; then
        kubectl delete crd keycloaks.k8s.keycloak.org --ignore-not-found 2>/dev/null || true
        kubectl delete crd keycloakrealmimports.k8s.keycloak.org --ignore-not-found 2>/dev/null || true
    fi
fi

# 14. Delete llm namespace and model resources
echo "14. Deleting LLM models and namespace..."
force_delete_namespace "llm" "llminferenceservice" "inferenceservice" "maasmodelrefs.maas.opendatahub.io"

# 15. Delete gateway resources in openshift-ingress
echo "15. Deleting gateway resources..."
kubectl delete gateway maas-default-gateway -n openshift-ingress --ignore-not-found 2>/dev/null || true
kubectl delete envoyfilter -n openshift-ingress -l kuadrant.io/managed=true --ignore-not-found 2>/dev/null || true
kubectl delete envoyfilter kuadrant-auth-tls-fix -n openshift-ingress --ignore-not-found 2>/dev/null || true
kubectl delete authpolicy -n openshift-ingress --all --ignore-not-found 2>/dev/null || true
kubectl delete ratelimitpolicy -n openshift-ingress --all --ignore-not-found 2>/dev/null || true
kubectl delete tokenratelimitpolicy -n openshift-ingress --all --ignore-not-found 2>/dev/null || true
kubectl delete gatewayclass openshift-default --ignore-not-found 2>/dev/null || true

# 16. Delete MaaS RBAC (ClusterRoles, ClusterRoleBindings - can conflict with other managers)
echo "16. Deleting MaaS RBAC..."
kubectl delete clusterrolebinding maas-api maas-controller-rolebinding --ignore-not-found 2>/dev/null || true
kubectl delete clusterrole maas-api maas-controller-role --ignore-not-found 2>/dev/null || true

# 17. Delete CRDs
# Always delete KServe/MaaS CRDs to prevent storedVersions schema conflicts on reinstall.
# ODH-internal CRDs are only deleted with --include-crds.
echo "17. Deleting KServe/MaaS CRDs (always removed to prevent version conflicts)..."
for crd in $(kubectl get crd -o name 2>/dev/null | grep -E 'serving\.kserve\.io|maas\.opendatahub\.io'); do
    echo "   Deleting $crd"
    kubectl delete "$crd" --ignore-not-found --timeout=30s 2>/dev/null || true
done

if $INCLUDE_CRDS; then
    echo "17b. Deleting all ODH CRDs..."
    for crd in $(kubectl get crd -o name 2>/dev/null | grep -E 'opendatahub\.io|trustyai\.opendatahub'); do
        echo "   Deleting $crd"
        kubectl delete "$crd" --ignore-not-found --timeout=30s 2>/dev/null || true
    done
else
    echo "17b. Skipping ODH-internal CRD deletion (use --include-crds to remove all)"
fi

echo ""
echo "=== Cleanup Complete ==="
echo ""
echo "Verify cleanup with:"
echo "  kubectl get subscription -A | grep -i odh"
echo "  kubectl get csv -A | grep -i odh"
echo "  kubectl get ns | grep -E 'odh|opendatahub|models-as-a-service|kuadrant|rh-connectivity-link|keycloak-system|llm'
  kubectl get deployment maas-api maas-controller postgres -n redhat-ods-applications 2>/dev/null || echo '  (no MaaS resources in redhat-ods-applications)'"