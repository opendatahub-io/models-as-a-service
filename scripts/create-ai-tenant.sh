#!/bin/bash
#
# Create a new AITenant with isolated gateway and infrastructure.
#
# Usage:
#   ./scripts/create-ai-tenant.sh <tenant-name> [gateway-hostname]
#
# Examples:
#   ./scripts/create-ai-tenant.sh redteam
#   ./scripts/create-ai-tenant.sh blueteam blueteam-maas.apps.example.com
#
# This script creates:
#   - Gateway with LoadBalancer service and TLS certificate
#   - AITenant CR (triggers controller to create MaasTenantConfig, maas-api, etc.)
#

set -euo pipefail

TENANT_NAME=${1:-}
GATEWAY_HOSTNAME=${2:-}
GATEWAY_NAMESPACE="openshift-ingress"
AITENANT_NAMESPACE="ai-tenants"
HOSTNAME_AUTO_DETECTED=false

validate_dns1123_subdomain() {
    local value="$1"
    local field="$2"
    local -a labels
    local label

    if [ -z "$value" ] || [ "${#value}" -gt 253 ] || ! [[ "$value" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$ ]]; then
        echo "Error: ${field} must be a valid DNS-1123 subdomain and at most 253 characters"
        echo "Use lowercase alphanumeric characters, hyphens, and dots; each label must start and end with alphanumeric."
        return 1
    fi

    IFS='.' read -r -a labels <<< "$value"
    for label in "${labels[@]}"; do
        if [ "${#label}" -gt 63 ]; then
            echo "Error: ${field} labels must be at most 63 characters"
            return 1
        fi
    done
}

if [ -z "$TENANT_NAME" ]; then
    echo "Error: Tenant name is required"
    echo "Usage: $0 <tenant-name> [gateway-hostname]"
    exit 1
fi

if ! [[ "$TENANT_NAME" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || [ "${#TENANT_NAME}" -gt 41 ]; then
    echo "Error: tenant name must be a valid DNS-1123 label and at most 41 characters"
    echo "Use lowercase alphanumeric characters and hyphens, starting and ending with alphanumeric."
    exit 1
fi

TENANT_NAMESPACE="ai-tenant-${TENANT_NAME}"

# Auto-detect cluster domain if hostname not provided
if [ -z "$GATEWAY_HOSTNAME" ]; then
    CLUSTER_DOMAIN=$(oc get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null)

    if [ -n "$CLUSTER_DOMAIN" ]; then
        GATEWAY_HOSTNAME="${TENANT_NAME}-maas.${CLUSTER_DOMAIN}"
        HOSTNAME_AUTO_DETECTED=true
    else
        echo "Error: Could not auto-detect cluster domain"
        echo "Please provide hostname: $0 $TENANT_NAME <gateway-hostname>"
        exit 1
    fi
fi

if ! validate_dns1123_subdomain "$GATEWAY_HOSTNAME" "gateway hostname"; then
    exit 1
fi

if [ "$HOSTNAME_AUTO_DETECTED" = true ]; then
    echo "Auto-detected hostname: $GATEWAY_HOSTNAME"
fi

echo "Creating tenant: $TENANT_NAME"
echo "  Gateway hostname: $GATEWAY_HOSTNAME"
echo "  Tenant namespace: $TENANT_NAMESPACE"

# Ensure ai-tenants namespace exists
oc get namespace "$AITENANT_NAMESPACE" &>/dev/null || oc create namespace "$AITENANT_NAMESPACE"

# Detect TLS certificate (reuse from main gateway if available, otherwise use self-signed)
TLS_SECRET_NAME=$(oc get gateway maas-default-gateway -n "$GATEWAY_NAMESPACE" \
    -o jsonpath='{.spec.listeners[0].tls.certificateRefs[0].name}' 2>/dev/null || echo "")

if [ -z "$TLS_SECRET_NAME" ]; then
    echo "Warning: Could not detect TLS certificate from main gateway, using default"
    TLS_SECRET_NAME="router-certs-default"
fi

echo "Using TLS certificate: $TLS_SECRET_NAME"

# Create Gateway with LoadBalancer service (default Gateway API pattern)
# Note: Gateway name must match tenant name (AITenant controller defaults gatewayRef.name to tenant name)
oc apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: ${TENANT_NAME}
  namespace: ${GATEWAY_NAMESPACE}
  labels:
    app.kubernetes.io/component: gateway
    app.kubernetes.io/instance: ${TENANT_NAME}
    app.kubernetes.io/name: maas
    opendatahub.io/managed: "false"
  annotations:
    opendatahub.io/managed: "false"
    security.opendatahub.io/authorino-tls-bootstrap: "true"
spec:
  gatewayClassName: openshift-default
  listeners:
  - name: https
    hostname: ${GATEWAY_HOSTNAME}
    port: 443
    protocol: HTTPS
    allowedRoutes:
      namespaces:
        from: All
    tls:
      mode: Terminate
      certificateRefs:
      - group: ""
        kind: Secret
        name: ${TLS_SECRET_NAME}
EOF

# Wait for Gateway to be accepted and programmed
echo "Waiting for Gateway to be accepted..."
for i in {1..30}; do
    if oc get gateway "${TENANT_NAME}" -n "$GATEWAY_NAMESPACE" -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' 2>/dev/null | grep -q "True"; then
        echo "Gateway accepted"
        break
    fi
    sleep 2
done

echo "Waiting for Gateway to be programmed (LoadBalancer provisioning)..."
echo "This may take 1-2 minutes on cloud providers..."
for i in {1..60}; do
    if oc get gateway "${TENANT_NAME}" -n "$GATEWAY_NAMESPACE" -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null | grep -q "True"; then
        echo "Gateway programmed"
        break
    fi
    sleep 2
done

# Create AITenant CR
# Note: gatewayRef is optional - controller defaults to {name: <aitenant-name>, namespace: openshift-ingress}
# Note: tenantNamespace is derived as ai-tenant-<name> for non-default tenants (PR #992)
oc apply -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: AITenant
metadata:
  name: ${TENANT_NAME}
  namespace: ${AITENANT_NAMESPACE}
EOF

echo ""
echo "Tenant creation initiated successfully"
echo ""
echo "Resources created:"
echo "  Gateway:          ${TENANT_NAME} (${GATEWAY_NAMESPACE})"
echo "  AITenant:         ${TENANT_NAME} (${AITENANT_NAMESPACE})"
echo ""
echo "The MaaS controller will automatically create:"
echo "  Namespace:        ${TENANT_NAMESPACE}"
echo "  MaasTenantConfig: default-tenant (${TENANT_NAMESPACE})"
echo "  Deployment:       maas-api-${TENANT_NAME} (opendatahub)"
echo "  AuthPolicy:       ${TENANT_NAME}-maas-auth (${GATEWAY_NAMESPACE})"
echo ""
echo "Gateway configuration:"
echo "  Hostname:         ${GATEWAY_HOSTNAME}"
echo "  Service type:     LoadBalancer (Gateway API standard)"
echo ""
echo "Monitor status:"
echo "  oc get aitenant ${TENANT_NAME} -n ${AITENANT_NAMESPACE} -w"
echo "  oc get maastenantconfig default-tenant -n ${TENANT_NAMESPACE} -w"
echo "  oc get gateway ${TENANT_NAME} -n ${GATEWAY_NAMESPACE}"
echo ""
echo "Grant tenant-admin access with a standard RoleBinding, for example:"
echo "  oc create rolebinding ${TENANT_NAME}-tenant-admin \\"
echo "    --role=aitenant-${TENANT_NAME}-tenant-admin \\"
echo "    --user=<user@example.com> \\"
echo "    -n ${TENANT_NAMESPACE}"
echo ""
echo "See docs/content/configuration-and-management/tenant-rbac.md for group, ServiceAccount, and AITenant read-access examples."
echo ""
echo "Access tenant gateway:"
echo "  https://${GATEWAY_HOSTNAME}/maas-api/v1/models"
