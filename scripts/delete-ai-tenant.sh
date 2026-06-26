#!/bin/bash
#
# Delete an AITenant and all its infrastructure resources.
#
# Usage:
#   ./scripts/delete-ai-tenant.sh <tenant-name>
#
# Example:
#   ./scripts/delete-ai-tenant.sh redteam
#

set -euo pipefail

TENANT_NAME=${1:-}
GATEWAY_NAMESPACE="openshift-ingress"
AITENANT_NAMESPACE="ai-tenants"

if [ -z "$TENANT_NAME" ]; then
    echo "Error: Tenant name is required"
    echo "Usage: $0 <tenant-name>"
    exit 1
fi

TENANT_NAMESPACE="ai-tenant-${TENANT_NAME}"

echo "Deleting tenant: $TENANT_NAME"

# Delete AITenant CR (finalizer will clean up some resources)
echo "  Deleting AITenant CR..."
kubectl delete aitenant "$TENANT_NAME" -n "$AITENANT_NAMESPACE" --ignore-not-found=true --wait=false

# Delete Gateway infrastructure
echo "  Deleting Gateway..."
kubectl delete gateway "$TENANT_NAME" -n "$GATEWAY_NAMESPACE" --ignore-not-found=true

echo "  Deleting Route..."
kubectl delete route "${TENANT_NAME}-route" -n "$GATEWAY_NAMESPACE" --ignore-not-found=true

echo "  Deleting Gateway options ConfigMap..."
kubectl delete configmap "${TENANT_NAME}-gw-options" -n "$GATEWAY_NAMESPACE" --ignore-not-found=true

# Delete any AuthPolicies
echo "  Deleting AuthPolicies..."
kubectl delete authpolicy "${TENANT_NAME}-maas-auth" -n "$GATEWAY_NAMESPACE" --ignore-not-found=true 2>/dev/null || true
kubectl delete authpolicy "${TENANT_NAME}-gateway-maas-auth" -n "$GATEWAY_NAMESPACE" --ignore-not-found=true 2>/dev/null || true

# Wait for finalizer cleanup
echo "  Waiting for finalizer cleanup (30s)..."
sleep 30

# Check if resources were cleaned up
REMAINING=0

if kubectl get namespace "$TENANT_NAMESPACE" &>/dev/null; then
    echo "  WARNING: Tenant namespace still exists, deleting manually..."
    kubectl delete namespace "$TENANT_NAMESPACE" --ignore-not-found=true --wait=false
    REMAINING=1
fi

if kubectl get deployment "maas-api-${TENANT_NAME}" -n opendatahub &>/dev/null; then
    echo "  WARNING: maas-api deployment still exists, deleting manually..."
    kubectl delete deployment "maas-api-${TENANT_NAME}" -n opendatahub --ignore-not-found=true
    REMAINING=1
fi

if kubectl get service "maas-api-${TENANT_NAME}" -n opendatahub &>/dev/null; then
    echo "  WARNING: maas-api service still exists, deleting manually..."
    kubectl delete service "maas-api-${TENANT_NAME}" -n opendatahub --ignore-not-found=true
    REMAINING=1
fi

if [ $REMAINING -eq 1 ]; then
    echo ""
    echo "Some resources required manual cleanup. Waiting for deletion (30s)..."
    sleep 30
fi

echo ""
echo "Tenant deletion complete: $TENANT_NAME"
echo ""
echo "Verify cleanup:"
echo "  kubectl get aitenant $TENANT_NAME -n $AITENANT_NAMESPACE"
echo "  kubectl get gateway $TENANT_NAME -n $GATEWAY_NAMESPACE"
echo "  kubectl get namespace $TENANT_NAMESPACE"
echo "  kubectl get deployment maas-api-$TENANT_NAME -n opendatahub"
