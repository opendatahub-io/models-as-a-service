#!/bin/bash
#
# Import test realm into an existing Keycloak instance.
#
# WARNING: Test realm contains hardcoded passwords - NOT for production.
#
# Prerequisites:
#   - Keycloak must be deployed (run ./scripts/setup-keycloak.sh first)
#
# Usage:
#   ./docs/samples/keycloak/test-realms/apply-test-realms.sh
#

set -euo pipefail

NAMESPACE="keycloak-system"
KEYCLOAK_NAME="maas-keycloak"
CONFIGMAP_NAME="keycloak-test-realms"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "WARNING: Deploying TEST realm with hardcoded passwords (dev/testing only)"
echo ""

if ! kubectl get keycloak "$KEYCLOAK_NAME" -n "$NAMESPACE" &>/dev/null; then
  echo "ERROR: Keycloak instance '$KEYCLOAK_NAME' not found in namespace '$NAMESPACE'" >&2
  echo "Deploy Keycloak first: ./scripts/setup-keycloak.sh" >&2
  exit 1
fi

echo "Creating ConfigMap with test realm..."

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
  maas-realm.json: |
$(sed 's/^/    /' "${SCRIPT_DIR}/maas-realm.json")
EOF

echo "  ConfigMap created"

echo ""
echo "Patching Keycloak instance to mount test realm..."

EXISTING_ARGS=$(kubectl get keycloak "$KEYCLOAK_NAME" -n "$NAMESPACE" \
  -o jsonpath='{.spec.unsupported.podTemplate.spec.containers[0].args}' 2>/dev/null || echo "[]")

if echo "$EXISTING_ARGS" | grep -q "import-realm"; then
  echo "  Keycloak already configured for realm import"
else
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
                "name": "'"${CONFIGMAP_NAME}"'"
              }
            }
          ]
        }
      }
    }
  }
}'
  echo "  Keycloak patched"
fi

echo ""
echo "Restarting Keycloak to import realm..."

kubectl rollout restart statefulset "${KEYCLOAK_NAME}" -n "$NAMESPACE"

echo "  Waiting for Keycloak to be ready..."
if ! kubectl rollout status statefulset "${KEYCLOAK_NAME}" -n "$NAMESPACE" --timeout=300s; then
  echo "  WARNING: Keycloak restart timeout" >&2
  echo "  Check: kubectl get pods -n $NAMESPACE -l app=keycloak" >&2
  exit 1
fi

sleep 10

KEYCLOAK_HOSTNAME=$(kubectl get httproute keycloak-route -n "$NAMESPACE" \
  -o jsonpath='{.spec.hostnames[0]}' 2>/dev/null || echo "keycloak.<cluster-domain>")

echo ""
echo "Test realm deployed!"
echo ""
echo "  Realm: maas"
echo "  Client: maas-cli (public)"
echo "  Users (password: letmein):"
echo "    - alice  -> premium-group"
echo "    - erin   -> enterprise-group"
echo "    - ada    -> admin-group"
echo ""
echo "  OIDC Issuer URL:"
echo "    https://${KEYCLOAK_HOSTNAME}/realms/maas"
echo ""
echo "  Token endpoint:"
echo "    https://${KEYCLOAK_HOSTNAME}/realms/maas/protocol/openid-connect/token"
echo ""
echo "  Test token request:"
echo "    curl -sk -X POST \"https://${KEYCLOAK_HOSTNAME}/realms/maas/protocol/openid-connect/token\" \\"
echo "      -d \"grant_type=password\" -d \"client_id=maas-cli\" \\"
echo "      -d \"username=alice\" -d \"password=letmein\" | jq -r '.access_token'"
echo ""
