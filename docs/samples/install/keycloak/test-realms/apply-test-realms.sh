#!/bin/bash
#
# Deploy test realms to existing Keycloak instance
#
# ⚠️  WARNING: These test realms are for development/testing ONLY
#     They contain hardcoded passwords and insecure configurations
#     NOT for production use
#
# What this script does:
#   1. Creates ConfigMap with test realm JSONs
#   2. Patches Keycloak instance to mount the ConfigMap
#   3. Restarts Keycloak to trigger realm import
#   4. Waits for Keycloak to be ready
#
# Prerequisites:
#   - Keycloak must be deployed (run ./scripts/setup-keycloak.sh)
#
# Usage:
#   ./docs/samples/install/keycloak/test-realms/apply-test-realms.sh
#

set -euo pipefail

# Configuration
NAMESPACE="keycloak-system"
KEYCLOAK_NAME="maas-keycloak"
CONFIGMAP_NAME="keycloak-test-realms"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "⚠️  WARNING: Deploying TEST realms with hardcoded passwords"
echo "   These are for development/testing ONLY - NOT for production"
echo ""

# Check if Keycloak exists
if ! kubectl get keycloak "$KEYCLOAK_NAME" -n "$NAMESPACE" &>/dev/null; then
  echo "❌ ERROR: Keycloak instance '$KEYCLOAK_NAME' not found in namespace '$NAMESPACE'"
  echo ""
  echo "Deploy Keycloak first:"
  echo "  ./scripts/setup-keycloak.sh"
  exit 1
fi

echo "📋 Creating ConfigMap with test realms..."

# Create ConfigMap with realm JSONs
kubectl apply -n "$NAMESPACE" -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${CONFIGMAP_NAME}
  namespace: ${NAMESPACE}
  labels:
    app: keycloak
    purpose: test-realms
data:
  tenant-a-realm.json: |
$(sed 's/^/    /' "${SCRIPT_DIR}/tenant-a-realm.json")
  tenant-b-realm.json: |
$(sed 's/^/    /' "${SCRIPT_DIR}/tenant-b-realm.json")
EOF

echo "✓ ConfigMap created"

echo ""
echo "🔧 Patching Keycloak instance to mount test realms..."

# Check if Keycloak already has realm import configured
EXISTING_ARGS=$(kubectl get keycloak "$KEYCLOAK_NAME" -n "$NAMESPACE" \
  -o jsonpath='{.spec.unsupported.podTemplate.spec.containers[0].args}' 2>/dev/null || echo "[]")

if echo "$EXISTING_ARGS" | grep -q "import-realm"; then
  echo "  Keycloak already configured for realm import"
else
  # Patch Keycloak to enable realm import
  kubectl patch keycloak "$KEYCLOAK_NAME" -n "$NAMESPACE" --type=merge -p '
{
  "spec": {
    "unsupported": {
      "podTemplate": {
        "spec": {
          "containers": [
            {
              "name": "keycloak",
              "args": [
                "--verbose",
                "start",
                "--import-realm"
              ],
              "volumeMounts": [
                {
                  "name": "test-realms",
                  "mountPath": "/opt/keycloak/data/import"
                }
              ]
            }
          ],
          "volumes": [
            {
              "name": "test-realms",
              "configMap": {
                "name": "'${CONFIGMAP_NAME}'"
              }
            }
          ]
        }
      }
    }
  }
}'
  echo "✓ Keycloak patched"
fi

echo ""
echo "🔄 Restarting Keycloak to import realms..."

# Restart Keycloak StatefulSet to trigger realm import
kubectl rollout restart statefulset "${KEYCLOAK_NAME}" -n "$NAMESPACE"

echo "  Waiting for Keycloak to be ready..."

# Wait for rollout to complete
if ! kubectl rollout status statefulset "${KEYCLOAK_NAME}" -n "$NAMESPACE" --timeout=300s; then
  echo ""
  echo "⚠️  WARNING: Keycloak restart timeout"
  echo "  Check pod status: kubectl get pods -n $NAMESPACE"
  echo "  Check logs: kubectl logs -n $NAMESPACE -l app=keycloak"
  exit 1
fi

# Additional wait for pods to be fully ready
sleep 10

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "✅ Test Realms Deployed"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "Realms deployed:"
echo "  - tenant-a (Engineering, Site-Reliability, Project-Alpha)"
echo "  - tenant-b (Product-Security, Project-Omega)"
echo ""
echo "Test users (all with password: letmein):"
echo "  Tenant-A:"
echo "    - alice_lead (Engineering, Project-Alpha)"
echo "    - bob_sre (Site-Reliability)"
echo "  Tenant-B:"
echo "    - charlie_sec_lead (Product-Security, Project-Omega)"
echo "    - grace_dev (Project-Omega)"
echo ""
echo "OIDC Client: test-client (public, no authentication)"
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "📝 Verify Deployment"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# Get Keycloak hostname
KEYCLOAK_HOSTNAME=$(kubectl get httproute keycloak-route -n "$NAMESPACE" \
  -o jsonpath='{.spec.hostnames[0]}' 2>/dev/null || echo "keycloak.{cluster-domain}")

echo "Access Keycloak Admin Console:"
echo "  https://${KEYCLOAK_HOSTNAME}"
echo ""
echo "You should see 'tenant-a' and 'tenant-b' in the realm dropdown"
echo ""
echo "Test token generation:"
echo "  curl -k -X POST \\"
echo "    \"https://${KEYCLOAK_HOSTNAME}/realms/tenant-a/protocol/openid-connect/token\" \\"
echo "    -d \"grant_type=password\" \\"
echo "    -d \"client_id=test-client\" \\"
echo "    -d \"username=alice_lead\" \\"
echo "    -d \"password=letmein\" \\"
echo "    | jq ."
echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo "⚠️  Remember: These are TEST realms with insecure configurations"
echo "   See docs/samples/install/keycloak/test-realms/README.md for details"
echo ""
