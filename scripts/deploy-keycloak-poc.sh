#!/bin/bash

# Keycloak PoC Deployment Script
# This script deploys the MaaS platform with Keycloak-based token minting
# 
# Prerequisites:
#   1. Run ./scripts/deploy-rhoai-stable.sh --operator-type odh --operator-catalog quay.io/opendatahub/opendatahub-operator-catalog:latest --channel fast
#   2. Run ./scripts/deploy-openshift.sh
#   3. Then run this script to apply Keycloak overlay

set -e

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

source "$SCRIPT_DIR/deployment-helpers.sh" 2>/dev/null || true

echo "========================================="
echo "🔐 Keycloak PoC Deployment"
echo "========================================="
echo ""

# Check if running on OpenShift
if ! kubectl api-resources | grep -q "route.openshift.io"; then
    echo "❌ This script is for OpenShift clusters only."
    exit 1
fi

# Check prerequisites
echo "📋 Checking prerequisites..."
REQUIRED_TOOLS=("jq" "kubectl" "kustomize")
for tool in "${REQUIRED_TOOLS[@]}"; do
    if ! command -v "$tool" &> /dev/null; then
        echo "❌ Required tool '$tool' not found. Please install it first."
        exit 1
    fi
done
echo "   ✅ All required tools found"
echo ""

# Determine namespaces
MAAS_API_NAMESPACE=${MAAS_API_NAMESPACE:-opendatahub}
export MAAS_API_NAMESPACE

# ============================================================================
# Step 0: Configure Authorino for Debug Logging (for convenience)
# ============================================================================
echo "0️⃣ Configuring Authorino debug logging (for convenience)..."
if kubectl get authorino authorino -n kuadrant-system &>/dev/null; then
    kubectl patch authorino authorino -n kuadrant-system --type=merge --patch '{
      "spec": {
        "logLevel": "Debug"
      }
    }' 2>&1 || echo "   ⚠️  Failed to patch Authorino log level (non-fatal)"
    echo "   ✅ Authorino log level set to Debug"
else
    echo "   ⚠️  Authorino CR not found, skipping log level configuration"
fi

# Configure Authorino to trust self-signed certificates for OIDC discovery
# This is required for clusters with self-signed certificates (e.g., OpenShift routes)
# Authorino (written in Go) needs the cluster CA certificates to verify self-signed certs
# We combine: system CA + OpenShift service CA + ingress operator CA
# And mount it at the standard Go CA bundle locations via Authorino CR volumes field
echo "   Configuring Authorino to trust self-signed certificates for OIDC discovery..."
if kubectl get authorino authorino -n kuadrant-system &>/dev/null; then
    echo "   Building combined CA bundle (system CA + service CA + ingress CA)..."
    
    # Wait for Authorino pod to exist so we can extract system CA
    echo "   Waiting for Authorino pod..."
    AUTHORINO_POD=""
    for i in {1..30}; do
        AUTHORINO_POD=$(kubectl get pods -n kuadrant-system -l app=authorino -o name 2>/dev/null | head -1 | sed 's|pod/||' || echo "")
        if [ -n "$AUTHORINO_POD" ]; then
            break
        fi
        sleep 2
    done
    
    if [ -z "$AUTHORINO_POD" ]; then
        echo "   ⚠️  Authorino pod not found, skipping TLS configuration"
    else
        # Extract system CA bundle from Authorino pod
        echo "   Extracting system CA bundle from Authorino pod..."
        SYSTEM_CA=$(kubectl exec "$AUTHORINO_POD" -n kuadrant-system -- cat /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem 2>/dev/null || echo "")
        
        # Get OpenShift service CA
        echo "   Getting OpenShift service CA..."
        SERVICE_CA=$(kubectl get configmap openshift-service-ca.crt -n openshift-config -o jsonpath='{.data.service-ca\.crt}' 2>/dev/null || echo "")
        
        # Get ingress operator CA (needed for route certificates)
        echo "   Getting ingress operator CA..."
        INGRESS_CA=$(kubectl get secret router-ca -n openshift-ingress-operator -o jsonpath='{.data.tls\.crt}' 2>/dev/null | base64 -d 2>/dev/null || echo "")
        
        # Combine all CA certificates
        if [ -n "$SYSTEM_CA" ] && [ -n "$SERVICE_CA" ] && [ -n "$INGRESS_CA" ]; then
            COMBINED_CA="${SYSTEM_CA}"$'\n'"${SERVICE_CA}"$'\n'"${INGRESS_CA}"
            
            # Create combined CA bundle ConfigMap
            echo "   Creating combined CA bundle ConfigMap..."
            echo "$COMBINED_CA" | kubectl create configmap combined-ca-bundle -n kuadrant-system \
              --from-file=ca-bundle.crt=/dev/stdin \
              --dry-run=client -o yaml 2>/dev/null | kubectl apply -f - 2>&1 || \
              echo "$COMBINED_CA" > /tmp/combined-ca-bundle.pem && \
              kubectl create configmap combined-ca-bundle -n kuadrant-system \
                --from-file=ca-bundle.crt=/tmp/combined-ca-bundle.pem \
                --dry-run=client -o yaml | kubectl apply -f - 2>&1
            
            echo "   ✅ Combined CA bundle ConfigMap created"
            
            # Configure Authorino CR to mount combined CA bundle at both standard locations
            # Go HTTP client checks both /etc/ssl/certs/ca-certificates.crt and /etc/pki/tls/certs/ca-bundle.crt
            echo "   Configuring Authorino CR to mount combined CA bundle..."
            HAS_COMBINED_CA=$(kubectl get authorino authorino -n kuadrant-system -o jsonpath='{.spec.volumes.items[*].name}' 2>/dev/null | grep -o "combined-ca-bundle" || echo "")
            
            if [ -z "$HAS_COMBINED_CA" ]; then
                kubectl patch authorino authorino -n kuadrant-system --type=json -p='[
                  {
                    "op": "add",
                    "path": "/spec/volumes/items/-",
                    "value": {
                      "name": "combined-ca-bundle",
                      "configMaps": ["combined-ca-bundle"],
                      "mountPath": "/etc/ssl/certs",
                      "items": [
                        {
                          "key": "ca-bundle.crt",
                          "path": "ca-certificates.crt"
                        }
                      ]
                    }
                  },
                  {
                    "op": "add",
                    "path": "/spec/volumes/items/-",
                    "value": {
                      "name": "combined-ca-bundle-rhel",
                      "configMaps": ["combined-ca-bundle"],
                      "mountPath": "/etc/pki/tls/certs",
                      "items": [
                        {
                          "key": "ca-bundle.crt",
                          "path": "ca-bundle.crt"
                        }
                      ]
                    }
                  }
                ]' 2>&1 || echo "   ⚠️  Failed to patch Authorino CR (may need manual configuration)"
                
                echo "   ✅ Authorino CR configured with combined CA bundle volumes"
                echo "   Waiting for Authorino operator to reconcile..."
                sleep 10
                kubectl wait --for=condition=Ready authorino/authorino -n kuadrant-system --timeout=120s 2>&1 || \
                  echo "   ⚠️  Authorino reconciliation taking longer than expected, continuing..."
            else
                echo "   ✅ Combined CA bundle already configured in Authorino CR"
            fi
        else
            echo "   ⚠️  Could not extract all required CA certificates"
            echo "      System CA: $([ -n "$SYSTEM_CA" ] && echo "✅" || echo "❌")"
            echo "      Service CA: $([ -n "$SERVICE_CA" ] && echo "✅" || echo "❌")"
            echo "      Ingress CA: $([ -n "$INGRESS_CA" ] && echo "✅" || echo "❌")"
        fi
    fi
else
    echo "   ⚠️  Authorino CR not found, skipping TLS configuration"
fi
echo ""

# Step 1: Deploy Keycloak
# ============================================================================
echo "1️⃣ Deploying Keycloak..."
cd "$PROJECT_ROOT"
kubectl apply -k deployment/components/keycloak

echo "   Waiting for Keycloak route to be created..."
sleep 5

# Get Keycloak URL and configure hostname
echo ""
echo "2️⃣ Getting Keycloak URL and configuring hostname..."
KEYCLOAK_ROUTE=$(kubectl get route keycloak -n keycloak -o jsonpath='{.spec.host}' 2>/dev/null || echo "")
if [ -z "$KEYCLOAK_ROUTE" ]; then
    echo "   ⚠️  Route not found, using port-forward..."
    KEYCLOAK_URL="http://localhost:8080"
    echo "   Starting port-forward in background..."
    kubectl port-forward -n keycloak svc/keycloak 8080:8080 &
    PORT_FORWARD_PID=$!
    sleep 5
else
    KEYCLOAK_URL="https://${KEYCLOAK_ROUTE}"
    echo "   Keycloak URL: $KEYCLOAK_URL"
    echo "   Configuring hostname..."
    kubectl set env deployment/keycloak -n keycloak KC_HOSTNAME="${KEYCLOAK_URL}"
    echo "   ✅ Hostname configured: ${KEYCLOAK_URL}"
    
    # Wait for rollout to complete after hostname change
    # This ensures the new pods with correct hostname are ready before we create realms
    echo "   Waiting for Keycloak rollout to complete after hostname change..."
    kubectl rollout status deployment/keycloak -n keycloak --timeout=300s 2>&1 || {
        echo "   ⚠️  Rollout taking longer than expected, but continuing..."
        echo "   ⚠️  Note: Realm creation may fail if Keycloak has not fully restarted"
    }
    echo "   ✅ Keycloak rollout complete"
fi

echo "   Waiting for Keycloak to be ready..."
kubectl wait --for=condition=available deployment/keycloak -n keycloak --timeout=300s || \
  echo "   ⚠️  Keycloak taking longer than expected, continuing..."

# Wait for Keycloak to be fully ready
echo ""
echo "3️⃣ Waiting for Keycloak to be fully ready..."
for i in {1..60}; do
    if curl -sSk -f "${KEYCLOAK_URL}/realms/master/.well-known/openid-configuration" > /dev/null 2>&1; then
        ADMIN_CHECK=$(curl -sSk -w "\n%{http_code}" -X GET "${KEYCLOAK_URL}/admin/realms" \
          -H "Content-Type: application/json" 2>/dev/null | tail -n1)
        if [ "$ADMIN_CHECK" == "401" ] || [ "$ADMIN_CHECK" == "200" ]; then
            echo "   ✅ Keycloak is ready (admin API responding)"
            break
        fi
    fi
    if [ $i -eq 60 ]; then
        echo "   ⚠️  Keycloak health check failed after 120 seconds, but continuing..."
    else
        echo "   Waiting for Keycloak to be ready... ($i/60)"
        sleep 2
    fi
done

# ============================================================================
# Step 2: Get Admin Token
# ============================================================================
echo ""
echo "4️⃣ Getting admin token..."
ADMIN_TOKEN=""
for i in {1..10}; do
    TOKEN_RESPONSE=$(curl -sSk -X POST "${KEYCLOAK_URL}/realms/master/protocol/openid-connect/token" \
      -H "Content-Type: application/x-www-form-urlencoded" \
      -d "username=admin" \
      -d "password=admin" \
      -d "grant_type=password" \
      -d "client_id=admin-cli" 2>/dev/null)
    
    ADMIN_TOKEN=$(echo "$TOKEN_RESPONSE" | jq -r '.access_token // empty' 2>/dev/null)
    
    if [ -n "$ADMIN_TOKEN" ] && [ "$ADMIN_TOKEN" != "null" ] && [ "$ADMIN_TOKEN" != "" ]; then
        echo "   ✅ Admin token obtained"
        break
    fi
    
    if [ $i -eq 10 ]; then
        echo "   ❌ Failed to get admin token after 10 attempts"
        exit 1
    else
        echo "   Retrying admin token request... ($i/10)"
        sleep 2
    fi
done

if [ -z "$ADMIN_TOKEN" ] || [ "$ADMIN_TOKEN" == "null" ] || [ "$ADMIN_TOKEN" == "" ]; then
    echo "   ❌ Failed to get admin token"
    exit 1
fi

# ============================================================================
# Step 3: Configure Keycloak Realm
# ============================================================================
# Helper function to create a user and assign to group
create_user() {
    local KEYCLOAK_URL=$1
    local ADMIN_TOKEN=$2
    local USERNAME=$3
    local GROUP_NAME=$4
    local FIRST_NAME=$5
    local LAST_NAME=$6
    
    USER_RESPONSE=$(curl -sSk -w "\n%{http_code}" -X POST "${KEYCLOAK_URL}/admin/realms/maas/users" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" \
      -H "Content-Type: application/json" \
      -d "{
        \"username\": \"${USERNAME}\",
        \"enabled\": true,
        \"email\": \"${USERNAME}@example.com\",
        \"firstName\": \"${FIRST_NAME}\",
        \"lastName\": \"${LAST_NAME}\",
        \"credentials\": [{
          \"type\": \"password\",
          \"value\": \"password\",
          \"temporary\": false
        }]
      }")
    
    HTTP_CODE=$(echo "$USER_RESPONSE" | tail -n1)
    if [ "$HTTP_CODE" != "201" ] && [ "$HTTP_CODE" != "409" ]; then
        echo "   ⚠️  User '${USERNAME}' creation returned: $HTTP_CODE"
        return
    fi
    
    USER_ID=$(curl -sSk -X GET "${KEYCLOAK_URL}/admin/realms/maas/users?username=${USERNAME}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" | jq -r '.[0].id // empty')
    
    if [ -z "$USER_ID" ] || [ "$USER_ID" == "null" ]; then
        echo "   ⚠️  Could not find user ID for ${USERNAME}"
        return
    fi
    
    GROUP_ID=$(curl -sSk -X GET "${KEYCLOAK_URL}/admin/realms/maas/groups" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" | jq -r ".[] | select(.name==\"${GROUP_NAME}\") | .id // empty")
    
    if [ -n "$GROUP_ID" ] && [ "$GROUP_ID" != "null" ]; then
        curl -sSk -X PUT "${KEYCLOAK_URL}/admin/realms/maas/users/${USER_ID}/groups/${GROUP_ID}" \
          -H "Authorization: Bearer ${ADMIN_TOKEN}" > /dev/null 2>&1
        echo "   ✅ Created ${USERNAME} and assigned to ${GROUP_NAME}"
    else
        echo "   ⚠️  Group '${GROUP_NAME}' not found for ${USERNAME}"
    fi
}

echo ""
echo "5️⃣ Creating maas realm..."
REALM_RESPONSE=$(curl -sSk -w "\n%{http_code}" -X POST "${KEYCLOAK_URL}/admin/realms" \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "realm": "maas",
    "enabled": true,
    "displayName": "MaaS Realm"
  }')

HTTP_CODE=$(echo "$REALM_RESPONSE" | tail -n1)
if [ "$HTTP_CODE" == "201" ] || [ "$HTTP_CODE" == "409" ]; then
    echo "   ✅ Realm created or already exists"
else
    echo "   ⚠️  Realm creation returned: $HTTP_CODE"
fi

echo ""
echo "6️⃣ Creating maas-api client..."
for i in {1..5}; do
    CLIENT_RESPONSE=$(curl -sSk -w "\n%{http_code}" -X POST "${KEYCLOAK_URL}/admin/realms/maas/clients" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" \
      -H "Content-Type: application/json" \
      -d '{
        "clientId": "maas-api",
        "enabled": true,
        "publicClient": false,
        "secret": "maas-api-secret",
        "serviceAccountsEnabled": true,
        "authorizationServicesEnabled": true,
        "protocol": "openid-connect",
        "standardFlowEnabled": false,
        "directAccessGrantsEnabled": true,
        "implicitFlowEnabled": false,
        "attributes": {
          "access.token.lifespan": "14400",
          "token-exchange-enabled": "true"
        }
      }' 2>/dev/null)
    
    HTTP_CODE=$(echo "$CLIENT_RESPONSE" | tail -n1)
    
    if [ "$HTTP_CODE" == "201" ] || [ "$HTTP_CODE" == "409" ]; then
        echo "   ✅ Client created or already exists"
        break
    elif [ "$HTTP_CODE" == "502" ] || [ "$HTTP_CODE" == "503" ] || [ "$HTTP_CODE" == "504" ]; then
        if [ $i -lt 5 ]; then
            echo "   ⚠️  Keycloak not ready yet (HTTP $HTTP_CODE), retrying... ($i/5)"
            sleep 3
        else
            echo "   ⚠️  Client creation returned: $HTTP_CODE after 5 attempts"
        fi
    else
        echo "   ⚠️  Client creation returned: $HTTP_CODE"
        break
    fi
done

# Enable token exchange for maas-api client
CLIENT_UUID=$(curl -sSk -X GET "${KEYCLOAK_URL}/admin/realms/maas/clients?clientId=maas-api" \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" | jq -r '.[0].id')

if [ -n "$CLIENT_UUID" ] && [ "$CLIENT_UUID" != "null" ]; then
    echo "   Enabling token exchange for maas-api client..."
    CLIENT_CONFIG=$(curl -sSk -X GET "${KEYCLOAK_URL}/admin/realms/maas/clients/${CLIENT_UUID}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}")
    
    CLIENT_UPDATE=$(echo "$CLIENT_CONFIG" | jq '.attributes["token-exchange-enabled"] = "true"')
    
    UPDATE_RESPONSE=$(curl -sSk -w "\n%{http_code}" -X PUT "${KEYCLOAK_URL}/admin/realms/maas/clients/${CLIENT_UUID}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" \
      -H "Content-Type: application/json" \
      -d "$CLIENT_UPDATE" 2>/dev/null)
    
    UPDATE_HTTP_CODE=$(echo "$UPDATE_RESPONSE" | tail -n1)
    if [ "$UPDATE_HTTP_CODE" == "204" ] || [ "$UPDATE_HTTP_CODE" == "200" ]; then
        echo "   ✅ Token exchange enabled for maas-api client"
    else
        echo "   ⚠️  Failed to enable token exchange (HTTP $UPDATE_HTTP_CODE)"
    fi
fi

echo ""
echo "7️⃣ Creating maas-model-access client (audience)..."
MODEL_CLIENT_RESPONSE=$(curl -sSk -w "\n%{http_code}" -X POST "${KEYCLOAK_URL}/admin/realms/maas/clients" \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "clientId": "maas-model-access",
    "enabled": true,
    "publicClient": true,
    "protocol": "openid-connect",
    "standardFlowEnabled": false,
    "directAccessGrantsEnabled": false,
    "implicitFlowEnabled": false,
    "attributes": {
      "access.token.lifespan": "14400"
    }
  }')

HTTP_CODE=$(echo "$MODEL_CLIENT_RESPONSE" | tail -n1)
if [ "$HTTP_CODE" == "201" ] || [ "$HTTP_CODE" == "409" ]; then
    echo "   ✅ Model access client created or already exists"
fi

echo ""
echo "8️⃣ Configuring token exchange permissions..."
REALM_CONFIG=$(curl -sSk -X GET "${KEYCLOAK_URL}/admin/realms/maas" \
  -H "Authorization: Bearer ${ADMIN_TOKEN}")
REALM_UPDATE=$(echo "$REALM_CONFIG" | jq '.tokenExchangeEnabled = true')
REALM_UPDATE_RESPONSE=$(curl -sSk -w "\n%{http_code}" -X PUT "${KEYCLOAK_URL}/admin/realms/maas" \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" \
  -H "Content-Type: application/json" \
  -d "$REALM_UPDATE" 2>/dev/null)
REALM_UPDATE_HTTP_CODE=$(echo "$REALM_UPDATE_RESPONSE" | tail -n1)
if [ "$REALM_UPDATE_HTTP_CODE" == "204" ] || [ "$REALM_UPDATE_HTTP_CODE" == "200" ]; then
    echo "   ✅ Token exchange enabled at realm level"
fi

echo ""
echo "9️⃣ Creating tier groups..."
TIERS=("tier-free-users" "tier-premium-users" "tier-enterprise-users")
for GROUP_NAME in "${TIERS[@]}"; do
    GROUP_RESPONSE=$(curl -sSk -w "\n%{http_code}" -X POST "${KEYCLOAK_URL}/admin/realms/maas/groups" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" \
      -H "Content-Type: application/json" \
      -d "{\"name\": \"${GROUP_NAME}\"}")
    
    HTTP_CODE=$(echo "$GROUP_RESPONSE" | tail -n1)
    if [ "$HTTP_CODE" == "201" ] || [ "$HTTP_CODE" == "409" ]; then
        echo "   ✅ Group '${GROUP_NAME}' created or already exists"
    fi
done

echo ""
echo "🔟 Creating tier users..."
for i in 1 2; do
    create_user "${KEYCLOAK_URL}" "${ADMIN_TOKEN}" "free-user-${i}" "tier-free-users" "Free" "User${i}"
    create_user "${KEYCLOAK_URL}" "${ADMIN_TOKEN}" "premium-user-${i}" "tier-premium-users" "Premium" "User${i}"
    create_user "${KEYCLOAK_URL}" "${ADMIN_TOKEN}" "enterprise-user-${i}" "tier-enterprise-users" "Enterprise" "User${i}"
done

echo ""
echo "1️⃣1️⃣ Adding group mapper to maas-api client..."
CLIENT_ID=$(curl -sSk -X GET "${KEYCLOAK_URL}/admin/realms/maas/clients?clientId=maas-api" \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" | jq -r '.[0].id // empty')

if [ -n "$CLIENT_ID" ] && [ "$CLIENT_ID" != "null" ]; then
    EXISTING_MAPPER=$(curl -sSk -X GET "${KEYCLOAK_URL}/admin/realms/maas/clients/${CLIENT_ID}/protocol-mappers/models" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" | jq -r '.[] | select(.name=="groups") | .name // empty')
    
    if [ -z "$EXISTING_MAPPER" ] || [ "$EXISTING_MAPPER" == "null" ]; then
        MAPPER_RESPONSE=$(curl -sSk -w "\n%{http_code}" -X POST "${KEYCLOAK_URL}/admin/realms/maas/clients/${CLIENT_ID}/protocol-mappers/models" \
          -H "Authorization: Bearer ${ADMIN_TOKEN}" \
          -H "Content-Type: application/json" \
          -d '{
            "name": "groups",
            "protocol": "openid-connect",
            "protocolMapper": "oidc-group-membership-mapper",
            "consentRequired": false,
            "config": {
              "multivalued": "true",
              "full.path": "false",
              "access.token.claim": "true",
              "id.token.claim": "true",
              "userinfo.token.claim": "true",
              "claim.name": "groups"
            }
          }' 2>/dev/null)
        
        HTTP_CODE=$(echo "$MAPPER_RESPONSE" | tail -n1)
        if [ "$HTTP_CODE" == "201" ] || [ "$HTTP_CODE" == "409" ]; then
            echo "   ✅ Group mapper added"
        fi
    else
        echo "   ✅ Group mapper already exists"
    fi
fi

# ============================================================================
# Step 4: Apply Keycloak Configuration using Kustomize
# ============================================================================
echo ""
echo "1️⃣2️⃣ Applying Keycloak configuration with Kustomize..."

# Set up environment variables for envsubst
export KEYCLOAK_ISSUER_URL="${KEYCLOAK_URL}/realms/maas"
export KEYCLOAK_BASE_URL="${KEYCLOAK_URL}"
export KEYCLOAK_REALM="maas"
export KEYCLOAK_CLIENT_ID="maas-api"
export KEYCLOAK_CLIENT_SECRET="maas-api-secret"
export KEYCLOAK_AUDIENCE="maas-model-access"
export MAAS_API_IMAGE="${MAAS_API_IMAGE:-quay.io/maas/maas-api:keycloak-poc}"

# Apply AuthPolicies using envsubst
echo "   Applying gateway-auth-policy (Keycloak-only)..."
# First, completely remove old authentication and authorization sections
if kubectl get authpolicy gateway-auth-policy -n openshift-ingress &>/dev/null; then
    echo "   Removing old service-accounts authentication and authorization sections..."
    kubectl patch authpolicy gateway-auth-policy -n openshift-ingress --type=json \
      -p='[{"op": "remove", "path": "/spec/rules/authentication/service-accounts"}]' 2>/dev/null || echo "   ⚠️  service-accounts may not exist (continuing...)"
    kubectl patch authpolicy gateway-auth-policy -n openshift-ingress --type=json \
      -p='[{"op": "remove", "path": "/spec/rules/authorization"}]' 2>/dev/null || echo "   ⚠️  authorization may not exist (continuing...)"
fi

# Apply the new Keycloak-only policy (this will replace/merge with existing)
kubectl apply --server-side=true --force-conflicts -f <(envsubst '$KEYCLOAK_ISSUER_URL $MAAS_API_NAMESPACE' < \
  "$PROJECT_ROOT/deployment/overlays/keycloak/policies/gateway-auth-policy-keycloak.yaml.template")

kubectl annotate authpolicy gateway-auth-policy -n openshift-ingress \
  opendatahub.io/managed=false --overwrite 2>/dev/null || true

# Verify the policy is Keycloak-only
echo "   Verifying gateway-auth-policy is Keycloak-only..."
HAS_SERVICE_ACCOUNTS=$(kubectl get authpolicy gateway-auth-policy -n openshift-ingress -o jsonpath='{.spec.rules.authentication.service-accounts}' 2>/dev/null || echo "")
HAS_AUTHORIZATION=$(kubectl get authpolicy gateway-auth-policy -n openshift-ingress -o jsonpath='{.spec.rules.authorization}' 2>/dev/null || echo "")

if [ -n "$HAS_SERVICE_ACCOUNTS" ] && [ "$HAS_SERVICE_ACCOUNTS" != "null" ]; then
    echo "   ⚠️  WARNING: service-accounts still present! Attempting to remove again..."
    kubectl patch authpolicy gateway-auth-policy -n openshift-ingress --type=json \
      -p='[{"op": "remove", "path": "/spec/rules/authentication/service-accounts"}]' 2>&1
fi

if [ -n "$HAS_AUTHORIZATION" ] && [ "$HAS_AUTHORIZATION" != "null" ]; then
    echo "   ⚠️  WARNING: authorization section still present! Attempting to remove again..."
    kubectl patch authpolicy gateway-auth-policy -n openshift-ingress --type=json \
      -p='[{"op": "remove", "path": "/spec/rules/authorization"}]' 2>&1
fi

if [ -z "$HAS_SERVICE_ACCOUNTS" ] || [ "$HAS_SERVICE_ACCOUNTS" == "null" ]; then
    if [ -z "$HAS_AUTHORIZATION" ] || [ "$HAS_AUTHORIZATION" == "null" ]; then
        echo "   ✅ Gateway-auth-policy is Keycloak-only"
    fi
fi
  opendatahub.io/managed=false --overwrite 2>/dev/null || true

echo "   Applying maas-api-auth-policy..."
# First, explicitly remove openshift-identities if it exists (before applying new policy)
MAAS_API_AUTH_NAMESPACE=""
if kubectl get authpolicy maas-api-auth-policy -n opendatahub &>/dev/null; then
    MAAS_API_AUTH_NAMESPACE="opendatahub"
elif kubectl get authpolicy maas-api-auth-policy -n maas-api &>/dev/null; then
    MAAS_API_AUTH_NAMESPACE="maas-api"
fi

if [ -n "$MAAS_API_AUTH_NAMESPACE" ]; then
    echo "   Removing openshift-identities from existing AuthPolicy..."
    kubectl patch authpolicy maas-api-auth-policy -n "$MAAS_API_AUTH_NAMESPACE" --type=json \
      -p='[{"op": "remove", "path": "/spec/rules/authentication/openshift-identities"}]' 2>/dev/null || \
      echo "   ⚠️  openshift-identities may not exist (continuing...)"
fi

# Apply the new AuthPolicy
kubectl apply --server-side=true --force-conflicts -f <(envsubst '$KEYCLOAK_ISSUER_URL $MAAS_API_NAMESPACE' < \
  "$PROJECT_ROOT/deployment/overlays/keycloak/policies/maas-api-auth-policy.yaml.template")

# Re-determine namespace after apply (in case it was created)
if [ -z "$MAAS_API_AUTH_NAMESPACE" ]; then
    if kubectl get authpolicy maas-api-auth-policy -n opendatahub &>/dev/null; then
        MAAS_API_AUTH_NAMESPACE="opendatahub"
    elif kubectl get authpolicy maas-api-auth-policy -n maas-api &>/dev/null; then
        MAAS_API_AUTH_NAMESPACE="maas-api"
    fi
fi

if [ -n "$MAAS_API_AUTH_NAMESPACE" ]; then
    kubectl annotate authpolicy maas-api-auth-policy -n "$MAAS_API_AUTH_NAMESPACE" \
      opendatahub.io/managed=false --overwrite 2>/dev/null || true
    
    # Verify openshift-identities was removed
    echo "   Verifying openshift-identities removal..."
    HAS_OPENSHIFT_ID=$(kubectl get authpolicy maas-api-auth-policy -n "$MAAS_API_AUTH_NAMESPACE" -o jsonpath='{.spec.rules.authentication.openshift-identities}' 2>/dev/null || echo "")
    if [ -n "$HAS_OPENSHIFT_ID" ] && [ "$HAS_OPENSHIFT_ID" != "null" ]; then
        echo "   ⚠️  WARNING: openshift-identities still present! Attempting to remove again..."
        kubectl patch authpolicy maas-api-auth-policy -n "$MAAS_API_AUTH_NAMESPACE" --type=json \
          -p='[{"op": "remove", "path": "/spec/rules/authentication/openshift-identities"}]' 2>&1
    else
        echo "   ✅ openshift-identities removed successfully"
    fi
    
    # Verify response headers are correct
    echo "   Verifying response headers..."
    ACTUAL_USERNAME=$(kubectl get authpolicy maas-api-auth-policy -n "$MAAS_API_AUTH_NAMESPACE" -o jsonpath='{.spec.rules.response.success.headers.X-MaaS-Username.plain.selector}' 2>/dev/null || echo "")
    ACTUAL_GROUP=$(kubectl get authpolicy maas-api-auth-policy -n "$MAAS_API_AUTH_NAMESPACE" -o jsonpath='{.spec.rules.response.success.headers.X-MaaS-Group.plain.selector}' 2>/dev/null || echo "")
    
    if [ "$ACTUAL_USERNAME" == "auth.identity.preferred_username" ] && [ "$ACTUAL_GROUP" == "auth.identity.groups" ]; then
        echo "   ✅ Response headers configured correctly"
    else
        echo "   ⚠️  WARNING: Response headers may not be correct!"
        echo "      Username selector: ${ACTUAL_USERNAME:-<not found>}"
        echo "      Group selector: ${ACTUAL_GROUP:-<not found>}"
        echo "      Expected: auth.identity.preferred_username and auth.identity.groups"
    fi
    
    # Ensure groups header format is correct for parseGroupsHeader
    # Authorino's plain.selector on arrays may output in unexpected format
    # We ensure it uses plain.selector (not expression) for proper serialization
    echo "   Ensuring groups header uses plain.selector format..."
    CURRENT_GROUP_SELECTOR=$(kubectl get authpolicy maas-api-auth-policy -n "$MAAS_API_AUTH_NAMESPACE" -o jsonpath='{.spec.rules.response.success.headers.X-MaaS-Group.plain.selector}' 2>/dev/null || echo "")
    if [ "$CURRENT_GROUP_SELECTOR" != "auth.identity.groups" ]; then
        echo "   Patching groups header to use plain.selector..."
        kubectl patch authpolicy maas-api-auth-policy -n "$MAAS_API_AUTH_NAMESPACE" --type=json \
          -p='[{"op": "replace", "path": "/spec/rules/response/success/headers/X-MaaS-Group/plain/selector", "value": "auth.identity.groups"}]' 2>/dev/null || \
          echo "   ⚠️  Failed to patch groups header (may need manual fix)"
    fi
fi
# Apply maas-api deployment patch using envsubst
echo "   Applying maas-api deployment configuration..."
kubectl apply --server-side=true --force-conflicts -f <(envsubst '$MAAS_API_NAMESPACE $MAAS_API_IMAGE $KEYCLOAK_BASE_URL $KEYCLOAK_REALM $KEYCLOAK_CLIENT_ID $KEYCLOAK_CLIENT_SECRET $KEYCLOAK_AUDIENCE' < \
  "$PROJECT_ROOT/deployment/overlays/keycloak/maas-api-env-patch.yaml.template")

# Set annotation on deployment
kubectl annotate deployment maas-api -n "$MAAS_API_NAMESPACE" \
  opendatahub.io/managed=false --overwrite 2>/dev/null || true

# Wait for rollout
echo "   Waiting for maas-api rollout..."
kubectl rollout status deployment/maas-api -n "$MAAS_API_NAMESPACE" --timeout=180s 2>&1 || \
  echo "   ⚠️  Rollout may still be in progress"

# ============================================================================
# Step 5: Validation Output
# ============================================================================
echo ""
echo "========================================="
echo "✅ Keycloak PoC Deployment Complete!"
echo "========================================="
echo ""
echo "📋 Configuration Summary:"
echo "   Keycloak URL: $KEYCLOAK_URL"
echo "   Realm: maas"
echo "   Issuer URL: $KEYCLOAK_ISSUER_URL"
echo "   MaaS API Namespace: $MAAS_API_NAMESPACE"
echo ""
echo "👥 Test Users (all passwords: 'password'):"
echo "   Free Tier: free-user-1, free-user-2"
echo "   Premium Tier: premium-user-1, premium-user-2"
echo "   Enterprise Tier: enterprise-user-1, enterprise-user-2"
echo ""
echo "🔑 Client Credentials:"
echo "   Client ID: maas-api"
echo "   Client Secret: maas-api-secret"
echo ""
echo "🧪 Validation Steps:"
echo ""
echo "1️⃣ Get a Keycloak user token:"
echo "   CLUSTER_DOMAIN=\$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"
echo "   KEYCLOAK_URL=\"$KEYCLOAK_URL\""
echo "   USER_TOKEN=\$(curl -sSk -X POST \"\${KEYCLOAK_URL}/realms/maas/protocol/openid-connect/token\" \\"
echo "     -H \"Content-Type: application/x-www-form-urlencoded\" \\"
echo "     -d \"username=free-user-1\" \\"
echo "     -d \"password=password\" \\"
echo "     -d \"grant_type=password\" \\"
echo "     -d \"client_id=maas-api\" \\"
echo "     -d \"client_secret=maas-api-secret\" | jq -r '.access_token')"
echo ""
echo "2️⃣ Mint a MaaS token:"
echo "   HOST=\"maas.\${CLUSTER_DOMAIN}\""
echo "   MAAS_TOKEN=\$(curl -sSk -X POST \"https://\${HOST}/maas-api/v1/tokens\" \\"
echo "     -H \"Authorization: Bearer \${USER_TOKEN}\" \\"
echo "     -H \"Content-Type: application/json\" \\"
echo "     -d '{\"expiration\": \"1h\"}' | jq -r '.token')"
echo ""
echo "3️⃣ List available models:"
echo "   curl -sSk -H \"Authorization: Bearer \${MAAS_TOKEN}\" \\"
echo "     \"https://\${HOST}/maas-api/v1/models\" | jq ."
echo ""
echo "4️⃣ Make a model completion request:"
echo "   MODEL_URL=\$(curl -sSk -H \"Authorization: Bearer \${MAAS_TOKEN}\" \\"
echo "     \"https://\${HOST}/maas-api/v1/models\" | jq -r '.data[0].url')"
echo "   MODEL_ID=\$(curl -sSk -H \"Authorization: Bearer \${MAAS_TOKEN}\" \\"
echo "     \"https://\${HOST}/maas-api/v1/models\" | jq -r '.data[0].id')"
echo "   curl -sSk -X POST \"\${MODEL_URL}/v1/chat/completions\" \\"
echo "     -H \"Authorization: Bearer \${MAAS_TOKEN}\" \\"
echo "     -H \"Content-Type: application/json\" \\"
echo "     -d \"{\\\"model\\\": \\\"\${MODEL_ID}\\\", \\\"messages\\\": [{\\\"role\\\": \\\"user\\\", \\\"content\\\": \\\"Hello\\\"}], \\\"max_tokens\\\": 100}\" | jq ."
echo ""
echo "💡 Quick Test Script:"
echo "   Run: ./scripts/test-keycloak-poc.sh"
echo ""
if [ -n "$PORT_FORWARD_PID" ]; then
    echo "⚠️  Port-forward is running in background (PID: $PORT_FORWARD_PID)"
    echo "   To stop it: kill $PORT_FORWARD_PID"
    echo ""
fi
