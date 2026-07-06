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
#   - Gateway with TLS certificate (service-ca provisioned)
#   - OpenShift Route for external access
#   - AITenant CR (triggers controller to create Tenant, maas-api, etc.)
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
    DEFAULT_HOSTNAME=$(oc get route maas-gateway-route -n openshift-ingress -o jsonpath='{.spec.host}' 2>/dev/null || \
                       oc get route -n openshift-ingress -o jsonpath='{.items[0].spec.host}' 2>/dev/null)

    if [ -n "$DEFAULT_HOSTNAME" ]; then
        CLUSTER_DOMAIN="${DEFAULT_HOSTNAME#*.}"
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

# Create Gateway options ConfigMap for service-ca TLS certificate provisioning
SERVICE_CA_SECRET="${TENANT_NAME}-gw-service-tls"

oc apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${TENANT_NAME}-gw-options
  namespace: ${GATEWAY_NAMESPACE}
data:
  service: |
    metadata:
      annotations:
        service.beta.openshift.io/serving-cert-secret-name: "${SERVICE_CA_SECRET}"
    spec:
      type: ClusterIP
EOF

# Create Gateway (without hostname field to avoid SNI filtering)
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
  infrastructure:
    parametersRef:
      group: ""
      kind: ConfigMap
      name: ${TENANT_NAME}-gw-options
  listeners:
  - name: https
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
        name: ${SERVICE_CA_SECRET}
EOF

# Wait for Gateway to be accepted
echo "Waiting for Gateway to be accepted..."
sleep 5
if oc get gateway "${TENANT_NAME}" -n "$GATEWAY_NAMESPACE" -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' 2>/dev/null | grep -q "True"; then
    echo "Gateway accepted"
else
    echo "Warning: Gateway may not be ready yet"
fi

# Create OpenShift Route for external access
GATEWAY_SERVICE_NAME="${TENANT_NAME}-openshift-default"

oc apply -f - <<EOF
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: ${TENANT_NAME}-route
  namespace: ${GATEWAY_NAMESPACE}
  labels:
    app.kubernetes.io/name: maas
    app.kubernetes.io/component: gateway
    app.kubernetes.io/instance: ${TENANT_NAME}
    gateway.networking.k8s.io/gateway-name: ${TENANT_NAME}
spec:
  host: "${GATEWAY_HOSTNAME}"
  to:
    kind: Service
    name: ${GATEWAY_SERVICE_NAME}
    weight: 100
  port:
    targetPort: 443
  tls:
    termination: reencrypt
    insecureEdgeTerminationPolicy: Redirect
EOF

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
echo "  Route:            ${TENANT_NAME}-route (${GATEWAY_NAMESPACE})"
echo "  AITenant:         ${TENANT_NAME} (${AITENANT_NAMESPACE})"
echo ""
echo "The MaaS controller will automatically create:"
echo "  Namespace:        ${TENANT_NAMESPACE}"
echo "  Tenant CR:        default-tenant (${TENANT_NAMESPACE})"
echo "  Deployment:       maas-api-${TENANT_NAME} (opendatahub)"
echo "  AuthPolicy:       ${TENANT_NAME}-maas-auth (${GATEWAY_NAMESPACE})"
echo ""
echo "Monitor status:"
echo "  oc get aitenant ${TENANT_NAME} -n ${AITENANT_NAMESPACE} -w"
echo "  oc get tenant default-tenant -n ${TENANT_NAMESPACE} -w"
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
