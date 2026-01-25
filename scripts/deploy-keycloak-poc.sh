#!/bin/bash

# Keycloak PoC Deployment Script
# This script deploys the MaaS platform with Keycloak-based token minting

set -e

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "========================================="
echo "🔐 Keycloak PoC Deployment"
echo "========================================="
echo ""

# Check if running on OpenShift
if ! kubectl api-resources | grep -q "route.openshift.io"; then
    echo "❌ This script is for OpenShift clusters only."
    exit 1
fi

# Step 1: Deploy Keycloak
echo "1️⃣ Deploying Keycloak..."
"$SCRIPT_DIR/setup-keycloak-poc.sh"

# Get Keycloak URL
KEYCLOAK_ROUTE=$(kubectl get route keycloak -n keycloak -o jsonpath='{.spec.host}' 2>/dev/null || echo "")
if [ -z "$KEYCLOAK_ROUTE" ]; then
    echo "   ⚠️  Keycloak route not found, will use port-forward"
    KEYCLOAK_URL="http://keycloak.keycloak.svc.cluster.local:8080"
else
    KEYCLOAK_URL="https://${KEYCLOAK_ROUTE}"
fi

echo ""
echo "2️⃣ Deploying MaaS platform with Keycloak integration..."

# Set environment variables for Keycloak
export KEYCLOAK_ENABLED=true
export KEYCLOAK_BASE_URL="${KEYCLOAK_INTERNAL_URL:-${KEYCLOAK_URL}}"
export KEYCLOAK_REALM="maas"
export KEYCLOAK_CLIENT_ID="maas-api"
export KEYCLOAK_CLIENT_SECRET="maas-api-secret"
export KEYCLOAK_AUDIENCE="maas-model-access"

echo "   Keycloak configuration:"
echo "     Base URL: $KEYCLOAK_BASE_URL"
echo "     Realm: $KEYCLOAK_REALM"
echo "     Client ID: $KEYCLOAK_CLIENT_ID"

# Deploy MaaS API with Keycloak configuration
cd "$PROJECT_ROOT"

# Patch the deployment to include Keycloak environment variables
MAAS_API_NAMESPACE=${MAAS_API_NAMESPACE:-opendatahub}
export MAAS_API_NAMESPACE

# Set the image to use
export MAAS_API_IMAGE="quay.io/maas/maas-api:keycloak-poc"

# First deploy base components
# Note: Assumes deploy-openshift.sh has already been run
# This script only makes Keycloak-specific updates

# Wait for maas-api deployment
echo ""
echo "3️⃣ Waiting for maas-api deployment..."
kubectl wait --for=condition=available deployment/maas-api -n "$MAAS_API_NAMESPACE" --timeout=300s || \
  echo "   ⚠️  maas-api taking longer than expected"

# Patch maas-api deployment with Keycloak environment variables
echo ""
echo "4️⃣ Configuring maas-api with Keycloak settings..."

# Set annotation to prevent operator from managing this deployment
kubectl annotate deployment maas-api -n "$MAAS_API_NAMESPACE" \
  opendatahub.io/managed=false --overwrite 2>/dev/null || true
echo "   ✅ Set annotation 'opendatahub.io/managed=false' to prevent operator management"

kubectl set env deployment/maas-api -n "$MAAS_API_NAMESPACE" \
  KEYCLOAK_ENABLED=true \
  KEYCLOAK_BASE_URL="${KEYCLOAK_BASE_URL}" \
  KEYCLOAK_REALM="maas" \
  KEYCLOAK_CLIENT_ID="maas-api" \
  KEYCLOAK_CLIENT_SECRET="maas-api-secret" \
  KEYCLOAK_AUDIENCE="maas-model-access"

# Also update the image if needed
kubectl set image deployment/maas-api -n "$MAAS_API_NAMESPACE" maas-api="${MAAS_API_IMAGE:-quay.io/maas/maas-api:keycloak-poc}" 2>/dev/null || echo "   Image already set"

# Wait for rollout
kubectl rollout status deployment/maas-api -n "$MAAS_API_NAMESPACE" --timeout=120s || \
  echo "   ⚠️  Deployment update taking longer than expected"

# Update AuthPolicy to use OIDC
echo ""
echo "5️⃣ Updating AuthPolicy to use OIDC authentication..."

# First, patch any existing AuthPolicy to remove old authorization keys BEFORE applying the new one
# This prevents auth issues from old kubernetesSubjectAccessReview keys
if kubectl get authpolicy gateway-auth-policy -n openshift-ingress &>/dev/null; then
    echo "   Patching existing AuthPolicy to remove old authorization keys..."
    kubectl patch authpolicy gateway-auth-policy -n openshift-ingress --type=json -p '[
      {
        "op": "remove",
        "path": "/spec/rules/authentication/service-accounts"
      },
      {
        "op": "remove",
        "path": "/spec/rules/authorization/tier-access/kubernetesSubjectAccessReview"
      },
      {
        "op": "replace",
        "path": "/spec/rules/authorization/tier-access/cache/key/selector",
        "value": "{auth.identity.userid}:{request.path}"
      }
    ]' 2>&1 || echo "   ⚠️  Patch failed (may not be needed if AuthPolicy is new)"
    
    # Set annotation to prevent operator from reverting changes
    kubectl annotate authpolicy gateway-auth-policy -n openshift-ingress \
      opendatahub.io/managed=false --overwrite 2>/dev/null || true
    echo "   ✅ Set annotation 'opendatahub.io/managed=false' to prevent operator management"
fi

# Apply the Keycloak AuthPolicy (this will replace or create the AuthPolicy)
kubectl apply -f deployment/base/policies/auth-policies/gateway-auth-policy-keycloak.yaml

# Ensure annotation is set after apply (in case it was overwritten)
kubectl annotate authpolicy gateway-auth-policy -n openshift-ingress \
  opendatahub.io/managed=false --overwrite 2>/dev/null || true

# Verify the authorization section doesn't have old keys
echo "   Verifying authorization section..."
HAS_OLD_AUTH=$(kubectl get authpolicy gateway-auth-policy -n openshift-ingress -o jsonpath='{.spec.rules.authorization.tier-access.kubernetesSubjectAccessReview}' 2>/dev/null || echo "")
if [ -n "$HAS_OLD_AUTH" ] && [ "$HAS_OLD_AUTH" != "null" ]; then
    echo "   ⚠️  WARNING: Old kubernetesSubjectAccessReview still present in authorization!"
    echo "   Attempting to remove it again..."
    kubectl patch authpolicy gateway-auth-policy -n openshift-ingress --type=json -p '[{"op": "remove", "path": "/spec/rules/authorization/tier-access/kubernetesSubjectAccessReview"}]' 2>&1
else
    echo "   ✅ Authorization section is clean (no old kubernetesSubjectAccessReview)"
fi


echo ""
echo "========================================="
echo "✅ Keycloak PoC Deployment Complete!"
echo "========================================="
echo ""
echo "Keycloak Configuration:"
echo "  URL: $KEYCLOAK_URL"
echo "  Realm: maas"
echo "  Client ID: maas-api"
echo ""
echo "To test the PoC:"
echo "1. Get your OpenShift token:"
echo "   OC_TOKEN=\$(oc whoami -t)"
echo ""
echo "2. Get a Keycloak token from maas-api:"
echo "   CLUSTER_DOMAIN=\$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"
echo "   HOST=\"maas.\${CLUSTER_DOMAIN}\""
echo "   TOKEN_RESPONSE=\$(curl -sSk \\"
echo "     -H \"Authorization: Bearer \${OC_TOKEN}\" \\"
echo "     -H \"Content-Type: application/json\" \\"
echo "     -X POST \\"
echo "     -d '{\"expiration\": \"1h\"}' \\"
echo "     \"\${HOST}/maas-api/v1/tokens\")"
echo "   TOKEN=\$(echo \$TOKEN_RESPONSE | jq -r .token)"
echo ""
echo "3. Use the token to access models:"
echo "   curl -sSk \\"
echo "     -H \"Authorization: Bearer \${TOKEN}\" \\"
echo "     -H \"Content-Type: application/json\" \\"
echo "     \"\${HOST}/maas-api/v1/models\""
echo ""
