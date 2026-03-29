#!/bin/bash
#
# Deploy Keycloak identity provider for MaaS external OIDC support.
#
# This script provides a POC-grade Keycloak deployment for development and testing.
# For production use, configure external database, high availability, and proper TLS.
#
# What this script does:
#   1. Installs Keycloak operator (if not present)
#   2. Creates keycloak-system namespace
#   3. Deploys Keycloak instance
#   4. Creates HTTPRoute for external access via Gateway API
#   5. No realms configured by default (configure via Admin Console)
#
# Namespace:
#   - Uses keycloak-system namespace (separate from MaaS)
#
# Environment variables:
#   GATEWAY_NAME        Gateway to use for HTTPRoute (default: maas-default-gateway)
#   GATEWAY_NAMESPACE   Gateway namespace (default: openshift-ingress)
#   KEYCLOAK_INSTANCES  Number of Keycloak instances (default: 1)
#
# Usage:
#   ./scripts/setup-keycloak.sh
#
# Access Keycloak Admin Console:
#   1. Get admin password:
#      kubectl get secret -n keycloak-system maas-keycloak-initial-admin \
#        -o jsonpath='{.data.password}' | base64 -d
#   2. Navigate to: https://keycloak.{cluster-domain}
#   3. Login as: admin / {password-from-step-1}
#

set -euo pipefail

# Configuration
NAMESPACE="keycloak-system"
KEYCLOAK_NAME="maas-keycloak"
GATEWAY_NAME="${GATEWAY_NAME:-maas-default-gateway}"
GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-openshift-ingress}"
KEYCLOAK_INSTANCES="${KEYCLOAK_INSTANCES:-1}"
# Validate KEYCLOAK_INSTANCES (prevent injection)
if ! [[ "$KEYCLOAK_INSTANCES" =~ ^[0-9]+$ ]] || [ "$KEYCLOAK_INSTANCES" -lt 1 ]; then
  echo "ERROR: KEYCLOAK_INSTANCES must be a positive integer (got: '$KEYCLOAK_INSTANCES')" >&2
  exit 1
fi

OPERATOR_TIMEOUT="${OPERATOR_TIMEOUT:-180}"
KEYCLOAK_TIMEOUT="${KEYCLOAK_TIMEOUT:-300}"

echo "🔐 Deploying Keycloak identity provider for MaaS OIDC support..."
echo ""

# Ensure namespace exists
if ! kubectl get namespace "$NAMESPACE" &>/dev/null; then
  echo "📦 Creating namespace '$NAMESPACE'..."
  kubectl create namespace "$NAMESPACE"
fi

#──────────────────────────────────────────────────────────────────────────────
# Install Keycloak Operator
#──────────────────────────────────────────────────────────────────────────────

echo "🔧 Checking Keycloak operator installation..."

if kubectl get subscription keycloak-operator -n "$NAMESPACE" &>/dev/null; then
  echo "  ✓ Keycloak operator already installed"
else
  echo "  Installing Keycloak operator from community-operators catalog..."

  # Create OperatorGroup
  kubectl apply -n "$NAMESPACE" -f - <<EOF
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: keycloak-operator-group
  namespace: ${NAMESPACE}
spec:
  targetNamespaces:
  - ${NAMESPACE}
EOF

  # Create Subscription
  kubectl apply -n "$NAMESPACE" -f - <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: keycloak-operator
  namespace: ${NAMESPACE}
spec:
  channel: fast
  name: keycloak-operator
  source: community-operators
  sourceNamespace: openshift-marketplace
  installPlanApproval: Automatic
EOF

  echo "  Waiting for operator to be installed (timeout: ${OPERATOR_TIMEOUT}s)..."

  # Wait for CRD to appear
  WAIT_START=$(date +%s)
  while ! kubectl get crd keycloaks.k8s.keycloak.org &>/dev/null; do
    ELAPSED=$(($(date +%s) - WAIT_START))
    if [ "$ELAPSED" -gt "$OPERATOR_TIMEOUT" ]; then
      echo "  ⚠️  Timeout waiting for Keycloak operator CRD" >&2
      echo "  Check operator logs: kubectl logs -n $NAMESPACE -l app.kubernetes.io/name=keycloak-operator" >&2
      exit 1
    fi
    echo -n "."
    sleep 5
  done
  echo ""

  # Wait for operator deployment to be ready
  echo "  Waiting for operator deployment to be ready..."
  if ! kubectl wait --for=condition=available \
      deployment -l app.kubernetes.io/name=keycloak-operator \
      -n "$NAMESPACE" --timeout=120s 2>/dev/null; then
    echo "  ⚠️  Warning: Operator deployment not ready, but CRD exists. Continuing..." >&2
  fi

  echo "  ✓ Keycloak operator installed successfully"
fi

#──────────────────────────────────────────────────────────────────────────────
# Auto-detect cluster configuration
#──────────────────────────────────────────────────────────────────────────────

echo ""
echo "🔍 Auto-detecting cluster configuration..."

# Get cluster domain from OpenShift ingress config
CLUSTER_DOMAIN=""
if kubectl get ingresses.config.openshift.io cluster &>/dev/null 2>&1; then
  CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster \
    -o jsonpath='{.spec.domain}' 2>/dev/null || true)
fi

if [[ -z "${CLUSTER_DOMAIN}" ]]; then
  echo "  ❌ ERROR: Could not auto-detect cluster domain" >&2
  echo "  Please ensure you are on an OpenShift cluster with ingress configured" >&2
  exit 1
fi

KEYCLOAK_HOSTNAME="keycloak.${CLUSTER_DOMAIN}"
echo "  Cluster domain: ${CLUSTER_DOMAIN}"
echo "  Keycloak hostname: ${KEYCLOAK_HOSTNAME}"

#──────────────────────────────────────────────────────────────────────────────
# Deploy Keycloak Instance
#──────────────────────────────────────────────────────────────────────────────

echo ""
echo "🚀 Deploying Keycloak instance..."

# Check if Keycloak already exists
if kubectl get keycloak "$KEYCLOAK_NAME" -n "$NAMESPACE" &>/dev/null; then
  echo "  Keycloak instance '$KEYCLOAK_NAME' already exists in namespace $NAMESPACE"

  # Check if hostname needs updating
  CURRENT_HOSTNAME=$(kubectl get keycloak "$KEYCLOAK_NAME" -n "$NAMESPACE" \
    -o jsonpath='{.spec.hostname.hostname}' 2>/dev/null || echo "")

  if [[ "$CURRENT_HOSTNAME" != "$KEYCLOAK_HOSTNAME" ]]; then
    echo "  Updating hostname from '$CURRENT_HOSTNAME' to '$KEYCLOAK_HOSTNAME'..."
    PATCH_JSON=$(jq -n --arg hostname "$KEYCLOAK_HOSTNAME" '{spec:{hostname:{hostname:$hostname}}}')
    kubectl patch keycloak "$KEYCLOAK_NAME" -n "$NAMESPACE" --type=merge -p "$PATCH_JSON"
  fi
else
  echo "  Creating Keycloak instance..."
  echo "  Instances: ${KEYCLOAK_INSTANCES}"
  echo "  Hostname: ${KEYCLOAK_HOSTNAME}"
  echo "  ⚠️  Using POC configuration (HTTP enabled, single instance)"
  echo ""

  kubectl apply -n "$NAMESPACE" -f - <<EOF
apiVersion: k8s.keycloak.org/v2alpha1
kind: Keycloak
metadata:
  name: ${KEYCLOAK_NAME}
  namespace: ${NAMESPACE}
  labels:
    app: keycloak
    purpose: maas-oidc
spec:
  # WARNING: HTTP enabled for POC/development only
  # Production deployments should use TLS termination at Gateway
  # (cert-manager with Let's Encrypt or enterprise CA)
  instances: ${KEYCLOAK_INSTANCES}
  hostname:
    hostname: ${KEYCLOAK_HOSTNAME}
    strict: false
  proxy:
    headers: xforwarded
  http:
    httpEnabled: true
EOF

  echo "  Waiting for Keycloak to be ready (timeout: ${KEYCLOAK_TIMEOUT}s)..."

  # Wait for Keycloak StatefulSet to be created
  WAIT_START=$(date +%s)
  while ! kubectl get statefulset -n "$NAMESPACE" -l app=keycloak 2>/dev/null | grep -q "${KEYCLOAK_NAME}"; do
    ELAPSED=$(($(date +%s) - WAIT_START))
    if [ "$ELAPSED" -gt 60 ]; then
      echo "  ⚠️  StatefulSet not created after 60s, checking Keycloak status..." >&2
      kubectl get keycloak "$KEYCLOAK_NAME" -n "$NAMESPACE" -o yaml
      break
    fi
    echo -n "."
    sleep 3
  done
  echo ""

  # Wait for Keycloak pods to be ready
  if ! kubectl wait --for=condition=ready pod \
      -l app=keycloak \
      -n "$NAMESPACE" --timeout="${KEYCLOAK_TIMEOUT}s" 2>/dev/null; then
    echo "  ⚠️  Warning: Keycloak pods not ready within timeout" >&2
    echo "  Check pod status: kubectl get pods -n $NAMESPACE -l app=keycloak" >&2
    echo "  Check logs: kubectl logs -n $NAMESPACE -l app=keycloak" >&2
  else
    echo "  ✓ Keycloak instance deployed successfully"
  fi
fi

#──────────────────────────────────────────────────────────────────────────────
# Create HTTPRoute for External Access
#──────────────────────────────────────────────────────────────────────────────

echo ""
echo "🌐 Configuring external access via Gateway API..."

# Check if Gateway exists
if ! kubectl get gateway "$GATEWAY_NAME" -n "$GATEWAY_NAMESPACE" &>/dev/null; then
  echo "  ⚠️  Warning: Gateway '$GATEWAY_NAME' not found in namespace '$GATEWAY_NAMESPACE'" >&2
  echo "  HTTPRoute will be created but may not work until Gateway is available" >&2
  echo "  Deploy MaaS first with: ./scripts/deploy.sh" >&2
fi

# Create or update HTTPRoute
if kubectl get httproute keycloak-route -n "$NAMESPACE" &>/dev/null; then
  echo "  Updating existing HTTPRoute..."
  PATCH_JSON=$(jq -n --arg hostname "$KEYCLOAK_HOSTNAME" '{spec:{hostnames:[$hostname]}}')
  kubectl patch httproute keycloak-route -n "$NAMESPACE" --type=merge -p "$PATCH_JSON"
else
  echo "  Creating HTTPRoute..."
  kubectl apply -n "$NAMESPACE" -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: keycloak-route
  namespace: ${NAMESPACE}
  labels:
    app: keycloak
    purpose: maas-oidc
spec:
  parentRefs:
  - name: ${GATEWAY_NAME}
    namespace: ${GATEWAY_NAMESPACE}
  hostnames:
  - ${KEYCLOAK_HOSTNAME}
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /
    backendRefs:
    - name: ${KEYCLOAK_NAME}-service
      port: 8080
EOF
fi

echo "  ✓ HTTPRoute configured"

#──────────────────────────────────────────────────────────────────────────────
# Get Admin Credentials
#──────────────────────────────────────────────────────────────────────────────

echo ""
echo "✅ Keycloak deployment complete!"
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "📋 Access Information"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Admin Console URL:"
echo "  https://${KEYCLOAK_HOSTNAME}"
echo ""
echo "Admin Credentials:"
echo "  Retrieve with commands below"
echo ""
echo "Get admin username:"
echo "  kubectl get secret ${KEYCLOAK_NAME}-initial-admin -n ${NAMESPACE} \\"
echo "    -o jsonpath='{.data.username}' | base64 -d"
echo ""
echo "Get admin password:"
echo "  kubectl get secret ${KEYCLOAK_NAME}-initial-admin -n ${NAMESPACE} \\"
echo "    -o jsonpath='{.data.password}' | base64 -d"
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "📝 Next Steps"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "1. Access the Admin Console and login with credentials above"
echo ""
echo "2. Create a realm:"
echo "   - Click 'Create Realm'"
echo "   - Configure groups (must match MaaS subscription groups)"
echo "   - Add users and assign to groups"
echo "   - Create OIDC client for MaaS integration"
echo ""
echo "3. Configure OIDC client:"
echo "   - Client authentication: ON"
echo "   - Valid redirect URIs: https://${CLUSTER_DOMAIN}/*"
echo "   - Client scopes: Add 'groups' mapper"
echo ""
echo "4. Optional: Import test realms for development:"
echo "   - See docs/samples/install/keycloak/test-realms/"
echo "   - ⚠️  Test realms contain hardcoded passwords - NOT for production"
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
