#!/bin/bash

# Keycloak PoC Setup Script
# This script sets up Keycloak for IDP-based token minting PoC

set -e

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "========================================="
echo "🔐 Keycloak PoC Setup"
echo "========================================="
echo ""

# Deploy Keycloak
echo "1️⃣ Deploying Keycloak..."
cd "$PROJECT_ROOT"
kubectl apply -k deployment/components/keycloak

echo "   Waiting for Keycloak route to be created..."
sleep 5

# Get Keycloak URL and patch deployment with dynamic hostname
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
    echo "   Patching deployment with dynamic hostname..."
    kubectl set env deployment/keycloak -n keycloak KC_HOSTNAME="${KEYCLOAK_URL}"
    echo "   ✅ Hostname configured: ${KEYCLOAK_URL}"
fi

echo "   Waiting for Keycloak to be ready..."
kubectl wait --for=condition=available deployment/keycloak -n keycloak --timeout=300s || \
  echo "   ⚠️  Keycloak taking longer than expected, continuing..."

# Wait for Keycloak to be fully ready
echo ""
echo "3️⃣ Waiting for Keycloak to be fully ready..."
for i in {1..60}; do
    # First check if the openid-configuration endpoint is accessible
    if curl -sSk -f "${KEYCLOAK_URL}/realms/master/.well-known/openid-configuration" > /dev/null 2>&1; then
        # Then verify the admin API is actually responding (not just returning 502)
        ADMIN_CHECK=$(curl -sSk -w "\n%{http_code}" -X GET "${KEYCLOAK_URL}/admin/realms" \
          -H "Content-Type: application/json" 2>/dev/null | tail -n1)
        if [ "$ADMIN_CHECK" == "401" ] || [ "$ADMIN_CHECK" == "200" ]; then
            # 401 means the API is working (just needs auth), 200 means it's fully ready
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

# Get admin token
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
        echo "   Response: $TOKEN_RESPONSE"
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

# Helper function to create a user and assign to group
create_user() {
    local KEYCLOAK_URL=$1
    local ADMIN_TOKEN=$2
    local USERNAME=$3
    local GROUP_NAME=$4
    local FIRST_NAME=$5
    local LAST_NAME=$6
    
    # Create user
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
    
    # Get user ID
    USER_ID=$(curl -sSk -X GET "${KEYCLOAK_URL}/admin/realms/maas/users?username=${USERNAME}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}" | jq -r '.[0].id // empty')
    
    if [ -z "$USER_ID" ] || [ "$USER_ID" == "null" ]; then
        echo "   ⚠️  Could not find user ID for ${USERNAME}"
        return
    fi
    
    # Get group ID
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

# Create realm
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
    echo "   Response: $(echo "$REALM_RESPONSE" | head -n-1)"
fi

# Create client for token exchange
echo ""
echo "6️⃣ Creating maas-api client for token exchange..."
CLIENT_RESPONSE=""
HTTP_CODE=""
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
            echo "   Response: $(echo "$CLIENT_RESPONSE" | head -n-1)"
        fi
    else
        echo "   ⚠️  Client creation returned: $HTTP_CODE"
        echo "   Response: $(echo "$CLIENT_RESPONSE" | head -n-1)"
        break
    fi
done

# Get client UUID
CLIENT_UUID=$(curl -sSk -X GET "${KEYCLOAK_URL}/admin/realms/maas/clients?clientId=maas-api" \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" | jq -r '.[0].id')

# Enable token exchange for maas-api client
if [ -n "$CLIENT_UUID" ] && [ "$CLIENT_UUID" != "null" ]; then
    echo ""
    echo "   Enabling token exchange for maas-api client..."
    # Get current client configuration
    CLIENT_CONFIG=$(curl -sSk -X GET "${KEYCLOAK_URL}/admin/realms/maas/clients/${CLIENT_UUID}" \
      -H "Authorization: Bearer ${ADMIN_TOKEN}")
    
    # Update client to enable token exchange
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

# Create client for model access (audience)
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
else
    echo "   ⚠️  Model access client creation returned: $HTTP_CODE"
fi

# Configure token exchange permission
echo ""
echo "8️⃣ Configuring token exchange permissions..."
# Enable token exchange feature at realm level
# Get current realm config first to preserve other settings
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
else
    echo "   ⚠️  Failed to enable token exchange at realm level (HTTP $REALM_UPDATE_HTTP_CODE)"
fi

# Create tier groups
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
    else
        echo "   ⚠️  Group '${GROUP_NAME}' creation returned: $HTTP_CODE"
    fi
done

# Create tier users
echo ""
echo "🔟 Creating tier users..."
# Free tier users
for i in 1 2; do
    USERNAME="free-user-${i}"
    create_user "${KEYCLOAK_URL}" "${ADMIN_TOKEN}" "${USERNAME}" "tier-free-users" "Free" "User${i}"
done

# Premium tier users
for i in 1 2; do
    USERNAME="premium-user-${i}"
    create_user "${KEYCLOAK_URL}" "${ADMIN_TOKEN}" "${USERNAME}" "tier-premium-users" "Premium" "User${i}"
done

# Enterprise tier users
for i in 1 2; do
    USERNAME="enterprise-user-${i}"
    create_user "${KEYCLOAK_URL}" "${ADMIN_TOKEN}" "${USERNAME}" "tier-enterprise-users" "Enterprise" "User${i}"
done

echo "   ✅ All tier users created"

# Add group mapper to maas-api client
echo ""
echo "1️⃣1️⃣ Adding group mapper to maas-api client..."
CLIENT_ID=$(curl -sSk -X GET "${KEYCLOAK_URL}/admin/realms/maas/clients?clientId=maas-api" \
  -H "Authorization: Bearer ${ADMIN_TOKEN}" | jq -r '.[0].id // empty')

if [ -n "$CLIENT_ID" ] && [ "$CLIENT_ID" != "null" ]; then
    # Check if mapper already exists
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
        else
            echo "   ⚠️  Group mapper creation returned: $HTTP_CODE"
        fi
    else
        echo "   ✅ Group mapper already exists"
    fi
else
    echo "   ⚠️  Could not find maas-api client ID"
fi

# Apply AuthPolicy with operator management disabled
echo ""
echo "1️⃣2️⃣ Applying AuthPolicy with Keycloak configuration..."
AUTH_POLICY_FILE="${PROJECT_ROOT}/deployment/base/policies/auth-policies/gateway-auth-policy-keycloak.yaml"

if [ ! -f "$AUTH_POLICY_FILE" ]; then
    echo "   ⚠️  AuthPolicy file not found: $AUTH_POLICY_FILE"
else
    # Get the Keycloak realm URL for the issuer
    KEYCLOAK_ISSUER_URL="${KEYCLOAK_URL}/realms/maas"
    
    # Create a temporary file with the updated issuer URL
    TEMP_AUTH_POLICY=$(mktemp)
    
    # Update issuerUrl using sed
    sed "s|issuerUrl:.*|issuerUrl: ${KEYCLOAK_ISSUER_URL}|" "$AUTH_POLICY_FILE" > "$TEMP_AUTH_POLICY"
    
    # Apply the AuthPolicy
    if kubectl apply -f "$TEMP_AUTH_POLICY" 2>/dev/null; then
        echo "   ✅ AuthPolicy applied successfully"
    else
        echo "   ⚠️  Failed to apply AuthPolicy, trying patch instead..."
        # Patch the issuerUrl if the AuthPolicy already exists
        kubectl patch authpolicy gateway-auth-policy -n openshift-ingress --type=json \
          -p="[{\"op\": \"replace\", \"path\": \"/rules/authentication/keycloak/jwt/issuerUrl\", \"value\": \"${KEYCLOAK_ISSUER_URL}\"}]" 2>/dev/null || true
    fi
    
    # Always set the annotation to prevent operator management
    kubectl annotate authpolicy gateway-auth-policy -n openshift-ingress \
      opendatahub.io/managed=false --overwrite 2>/dev/null || true
    
    echo "   ✅ Annotation 'opendatahub.io/managed=false' set to prevent operator management"
    echo "   ✅ Issuer URL set to: ${KEYCLOAK_ISSUER_URL}"
    
    # Clean up temp file
    rm -f "$TEMP_AUTH_POLICY"
fi

# Also update the maas-api AuthPolicy
echo ""
echo "1️⃣3️⃣ Applying maas-api AuthPolicy with Keycloak configuration..."
MAAS_API_AUTH_POLICY_FILE="${PROJECT_ROOT}/deployment/base/maas-api/policies/auth-policy.yaml"

if [ ! -f "$MAAS_API_AUTH_POLICY_FILE" ]; then
    echo "   ⚠️  maas-api AuthPolicy file not found: $MAAS_API_AUTH_POLICY_FILE"
else
    # Get the Keycloak realm URL for the issuer
    KEYCLOAK_ISSUER_URL="${KEYCLOAK_URL}/realms/maas"
    
    # Check which namespace the AuthPolicy is in (could be maas-api or opendatahub)
    MAAS_API_NAMESPACE=""
    if kubectl get authpolicy maas-api-auth-policy -n opendatahub &>/dev/null; then
        MAAS_API_NAMESPACE="opendatahub"
    elif kubectl get authpolicy maas-api-auth-policy -n maas-api &>/dev/null; then
        MAAS_API_NAMESPACE="maas-api"
    fi
    
    if [ -n "$MAAS_API_NAMESPACE" ]; then
        echo "   Found maas-api-auth-policy in namespace: $MAAS_API_NAMESPACE"
        echo "   Removing openshift-identities and updating to use Keycloak authentication..."
        
        # First, remove openshift-identities if it exists
        echo "   Removing openshift-identities from authentication..."
        kubectl patch authpolicy maas-api-auth-policy -n "$MAAS_API_NAMESPACE" --type=json \
          -p='[{"op": "remove", "path": "/spec/rules/authentication/openshift-identities"}]' 2>&1 || echo "   ⚠️  openshift-identities may not exist (continuing...)"
        
        # Apply from file (more reliable than patching) - this replaces the entire spec
        # First update the namespace in the file temporarily
        TEMP_MAAS_API_POLICY=$(mktemp)
        # Update both issuerUrl and namespace
        sed -e "s|issuerUrl:.*|issuerUrl: ${KEYCLOAK_ISSUER_URL}|" \
            -e "s|namespace:.*|namespace: ${MAAS_API_NAMESPACE}|" \
            "$MAAS_API_AUTH_POLICY_FILE" > "$TEMP_MAAS_API_POLICY"
        
        if kubectl apply -f "$TEMP_MAAS_API_POLICY" 2>/dev/null; then
            echo "   ✅ maas-api AuthPolicy applied successfully from file"
        else
            echo "   ⚠️  Failed to apply from file, trying patch instead..."
            # Fallback to patch if apply fails
            kubectl patch authpolicy maas-api-auth-policy -n "$MAAS_API_NAMESPACE" --type=json \
              -p="[{\"op\": \"remove\", \"path\": \"/spec/rules/authentication/openshift-identities\"}, {\"op\": \"add\", \"path\": \"/spec/rules/authentication/keycloak\", \"value\": {\"jwt\": {\"issuerUrl\": \"${KEYCLOAK_ISSUER_URL}\"}, \"defaults\": {\"userid\": {\"expression\": \"auth.identity.preferred_username\"}}, \"cache\": {\"key\": {\"selector\": \"context.request.http.headers.authorization.@case:lower\"}, \"ttl\": 600}}}, {\"op\": \"replace\", \"path\": \"/spec/rules/response/success/headers/X-MaaS-Username/plain/selector\", \"value\": \"auth.identity.preferred_username\"}, {\"op\": \"replace\", \"path\": \"/spec/rules/response/success/headers/X-MaaS-Group/plain/selector\", \"value\": \"auth.identity.groups\"}]" 2>&1 || echo "   ⚠️  Patch also failed"
        fi
        
        rm -f "$TEMP_MAAS_API_POLICY"
        
        # Verify openshift-identities was removed
        HAS_OPENSHIFT_ID=$(kubectl get authpolicy maas-api-auth-policy -n "$MAAS_API_NAMESPACE" -o jsonpath='{.spec.rules.authentication.openshift-identities}' 2>/dev/null || echo "")
        if [ -n "$HAS_OPENSHIFT_ID" ] && [ "$HAS_OPENSHIFT_ID" != "null" ]; then
            echo "   ⚠️  WARNING: openshift-identities still present! Attempting to remove again..."
            kubectl patch authpolicy maas-api-auth-policy -n "$MAAS_API_NAMESPACE" --type=json \
              -p='[{"op": "remove", "path": "/spec/rules/authentication/openshift-identities"}]' 2>&1
        else
            echo "   ✅ openshift-identities removed successfully"
        fi
        
        # Always set the annotation to prevent operator management
        kubectl annotate authpolicy maas-api-auth-policy -n "$MAAS_API_NAMESPACE" \
          opendatahub.io/managed=false --overwrite 2>/dev/null || true
        
        # Verify the response section was updated correctly
        ACTUAL_USERNAME=$(kubectl get authpolicy maas-api-auth-policy -n "$MAAS_API_NAMESPACE" -o jsonpath='{.spec.rules.response.success.headers.X-MaaS-Username.plain.selector}' 2>/dev/null || echo "")
        ACTUAL_GROUP=$(kubectl get authpolicy maas-api-auth-policy -n "$MAAS_API_NAMESPACE" -o jsonpath='{.spec.rules.response.success.headers.X-MaaS-Group.plain.selector}' 2>/dev/null || echo "")
        
        if [ "$ACTUAL_USERNAME" == "auth.identity.preferred_username" ] && [ "$ACTUAL_GROUP" == "auth.identity.groups" ]; then
            echo "   ✅ maas-api AuthPolicy updated correctly with Keycloak fields"
        else
            echo "   ⚠️  WARNING: Response section may not be correct!"
            echo "      Username selector: ${ACTUAL_USERNAME:-<not found>}"
            echo "      Group selector: ${ACTUAL_GROUP:-<not found>}"
            echo "      Expected: auth.identity.preferred_username and auth.identity.groups"
        fi
        
        echo "   ✅ Annotation 'opendatahub.io/managed=false' set to prevent operator management"
        echo "   ✅ Issuer URL set to: ${KEYCLOAK_ISSUER_URL}"
    else
        # Apply from file if AuthPolicy doesn't exist yet
        TEMP_MAAS_API_POLICY=$(mktemp)
        # Default to opendatahub namespace if not found
        MAAS_API_NAMESPACE="opendatahub"
        sed -e "s|issuerUrl:.*|issuerUrl: ${KEYCLOAK_ISSUER_URL}|" \
            -e "s|namespace:.*|namespace: ${MAAS_API_NAMESPACE}|" \
            "$MAAS_API_AUTH_POLICY_FILE" > "$TEMP_MAAS_API_POLICY"
        
        if kubectl apply -f "$TEMP_MAAS_API_POLICY" 2>/dev/null; then
            echo "   ✅ maas-api AuthPolicy applied successfully"
        else
            echo "   ⚠️  Failed to apply maas-api AuthPolicy"
        fi
        
        rm -f "$TEMP_MAAS_API_POLICY"
    fi
fi

# Update maas-api deployment to use the new image and enable Keycloak
echo ""
echo "1️⃣4️⃣ Updating maas-api deployment image and enabling Keycloak..."
MAAS_API_NAMESPACE=${MAAS_API_NAMESPACE:-opendatahub}
MAAS_API_IMAGE="quay.io/maas/maas-api:keycloak-poc"

if kubectl get deployment maas-api -n "$MAAS_API_NAMESPACE" &>/dev/null; then
    echo "   Updating maas-api image to: $MAAS_API_IMAGE"
    
    # Set annotation to prevent operator from managing this deployment
    kubectl annotate deployment maas-api -n "$MAAS_API_NAMESPACE" \
      opendatahub.io/managed=false --overwrite 2>/dev/null || true
    
    # Update the image
    kubectl set image deployment/maas-api -n "$MAAS_API_NAMESPACE" maas-api="$MAAS_API_IMAGE" 2>/dev/null || true
    
    echo "   Enabling Keycloak in maas-api deployment..."
    # Use kubectl set env which handles both adding and updating env vars
    # This is more reliable than patching and works whether vars exist or not
    if kubectl set env deployment/maas-api -n "$MAAS_API_NAMESPACE" \
      KEYCLOAK_ENABLED=true \
      KEYCLOAK_BASE_URL="${KEYCLOAK_URL}" \
      KEYCLOAK_REALM=maas \
      KEYCLOAK_CLIENT_ID=maas-api \
      KEYCLOAK_CLIENT_SECRET=maas-api-secret \
      KEYCLOAK_AUDIENCE=maas-model-access \
      DEBUG_MODE=true 2>&1; then
        echo "   ✅ Keycloak environment variables set successfully"
    else
        echo "   ⚠️  Failed to set environment variables, trying patch method..."
        # Fallback to patch if set env fails
        kubectl patch deployment maas-api -n "$MAAS_API_NAMESPACE" --type='json' -p="[
          {
            \"op\": \"add\",
            \"path\": \"/spec/template/spec/containers/0/env/-\",
            \"value\": {
              \"name\": \"KEYCLOAK_ENABLED\",
              \"value\": \"true\"
            }
          },
          {
            \"op\": \"add\",
            \"path\": \"/spec/template/spec/containers/0/env/-\",
            \"value\": {
              \"name\": \"KEYCLOAK_BASE_URL\",
              \"value\": \"${KEYCLOAK_URL}\"
            }
          },
          {
            \"op\": \"add\",
            \"path\": \"/spec/template/spec/containers/0/env/-\",
            \"value\": {
              \"name\": \"KEYCLOAK_REALM\",
              \"value\": \"maas\"
            }
          },
          {
            \"op\": \"add\",
            \"path\": \"/spec/template/spec/containers/0/env/-\",
            \"value\": {
              \"name\": \"KEYCLOAK_CLIENT_ID\",
              \"value\": \"maas-api\"
            }
          },
          {
            \"op\": \"add\",
            \"path\": \"/spec/template/spec/containers/0/env/-\",
            \"value\": {
              \"name\": \"KEYCLOAK_CLIENT_SECRET\",
              \"value\": \"maas-api-secret\"
            }
          },
          {
            \"op\": \"add\",
            \"path\": \"/spec/template/spec/containers/0/env/-\",
            \"value\": {
              \"name\": \"KEYCLOAK_AUDIENCE\",
              \"value\": \"maas-model-access\"
            }
          },
          {
            \"op\": \"add\",
            \"path\": \"/spec/template/spec/containers/0/env/-\",
            \"value\": {
              \"name\": \"DEBUG_MODE\",
              \"value\": \"true\"
            }
          }
        ]" 2>&1 || echo "   ⚠️  Patch also failed"
    fi
    
    # Verify the environment variables were set
    echo "   Verifying environment variables..."
    KEYCLOAK_ENABLED_CHECK=$(kubectl get deployment maas-api -n "$MAAS_API_NAMESPACE" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="KEYCLOAK_ENABLED")].value}' 2>/dev/null || echo "")
    DEBUG_MODE_CHECK=$(kubectl get deployment maas-api -n "$MAAS_API_NAMESPACE" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="DEBUG_MODE")].value}' 2>/dev/null || echo "")
    
    if [ "$KEYCLOAK_ENABLED_CHECK" == "true" ] && [ "$DEBUG_MODE_CHECK" == "true" ]; then
        echo "   ✅ Environment variables verified: KEYCLOAK_ENABLED=true, DEBUG_MODE=true"
    else
        echo "   ⚠️  WARNING: Environment variables may not be set correctly!"
        echo "      KEYCLOAK_ENABLED: ${KEYCLOAK_ENABLED_CHECK:-<not found>}"
        echo "      DEBUG_MODE: ${DEBUG_MODE_CHECK:-<not found>}"
    fi
    
    echo "   Waiting for rollout to complete..."
    kubectl rollout status deployment/maas-api -n "$MAAS_API_NAMESPACE" --timeout=120s 2>/dev/null || \
      echo "   ⚠️  Rollout may still be in progress"
    
    echo "   ✅ maas-api deployment updated with Keycloak configuration"
    echo "   ✅ Annotation 'opendatahub.io/managed=false' set to prevent operator management"
else
    echo "   ⚠️  maas-api deployment not found in namespace $MAAS_API_NAMESPACE"
    echo "   The deployment will use the new image when it's created"
fi

echo ""
echo "========================================="
echo "✅ Keycloak PoC Setup Complete!"
echo "========================================="
echo ""
echo "Keycloak URL: $KEYCLOAK_URL"
echo "Realm: maas"
echo "Admin Username: admin"
echo "Admin Password: admin"
echo ""
echo "Tier Users (all passwords: 'password'):"
echo "  Free: free-user-1, free-user-2"
echo "  Premium: premium-user-1, premium-user-2"
echo "  Enterprise: enterprise-user-1, enterprise-user-2"
echo ""
echo "Client Credentials:"
echo "  Client ID: maas-api"
echo "  Client Secret: maas-api-secret"
echo ""
echo "To get Keycloak service account token:"
echo "  curl -X POST \"${KEYCLOAK_URL}/realms/maas/protocol/openid-connect/token\" \\"
echo "    -d 'client_id=maas-api' \\"
echo "    -d 'client_secret=maas-api-secret' \\"
echo "    -d 'grant_type=client_credentials'"
echo ""
if [ -n "$PORT_FORWARD_PID" ]; then
    echo "⚠️  Port-forward is running in background (PID: $PORT_FORWARD_PID)"
    echo "   To stop it: kill $PORT_FORWARD_PID"
fi
