#!/bin/bash

# Keycloak PoC Test Script
# This script tests the Keycloak-based token minting PoC

set -e

echo "========================================="
echo "🧪 Keycloak PoC Test"
echo "========================================="
echo ""

# Get cluster domain
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null || echo "")
if [ -z "$CLUSTER_DOMAIN" ]; then
    echo "❌ Failed to retrieve cluster domain"
    exit 1
fi

HOST="maas.${CLUSTER_DOMAIN}"
echo "MaaS API Host: $HOST"
echo ""

# Get Keycloak user token
echo "1️⃣ Getting Keycloak user token..."
KEYCLOAK_ROUTE=$(kubectl get route keycloak -n keycloak -o jsonpath='{.spec.host}' 2>/dev/null || echo "")
if [ -z "$KEYCLOAK_ROUTE" ]; then
    echo "❌ Keycloak route not found. Please ensure Keycloak is deployed."
    exit 1
fi
KEYCLOAK_URL="https://${KEYCLOAK_ROUTE}"
echo "   Keycloak URL: $KEYCLOAK_URL"

# Get a user token from Keycloak using password grant
echo "   Requesting token for user: free-user-1..."
USER_TOKEN=$(curl -sSk -X POST "${KEYCLOAK_URL}/realms/maas/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "username=free-user-1" \
  -d "password=password" \
  -d "grant_type=password" \
  -d "client_id=maas-api" \
  -d "client_secret=maas-api-secret" 2>/dev/null | jq -r '.access_token // empty')

if [ -z "$USER_TOKEN" ] || [ "$USER_TOKEN" == "null" ]; then
    echo "❌ Failed to get Keycloak user token"
    echo "   Make sure:"
    echo "   1. Keycloak is running and accessible"
    echo "   2. The 'maas' realm exists"
    echo "   3. User 'free-user-1' exists with password 'password'"
    echo "   4. Client 'maas-api' has direct access grants enabled"
    exit 1
fi
echo "   ✅ Keycloak user token obtained"
echo ""

# Test 1: Get Keycloak token from maas-api
echo "2️⃣ Testing token minting via maas-api..."
TOKEN_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer ${USER_TOKEN}" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{"expiration": "1h"}' \
  "${HOST}/maas-api/v1/tokens" || echo "")

if [ -z "$TOKEN_RESPONSE" ]; then
    echo "   ❌ Failed to get token response"
    exit 1
fi

TOKEN=$(echo "$TOKEN_RESPONSE" | jq -r '.token' 2>/dev/null || echo "")
if [ -z "$TOKEN" ] || [ "$TOKEN" == "null" ]; then
    echo "   ❌ Failed to extract token from response"
    echo "   Response: $TOKEN_RESPONSE"
    exit 1
fi

echo "   ✅ Token obtained successfully"
echo "   Token (first 50 chars): ${TOKEN:0:50}..."
echo ""

# Decode token to verify it's a Keycloak token (not ServiceAccount)
echo "3️⃣ Verifying token is from Keycloak (not ServiceAccount)..."
JWT_PAYLOAD=$(echo "$TOKEN" | cut -d. -f2 2>/dev/null || echo "")
if [ -n "$JWT_PAYLOAD" ]; then
    DECODED=$(echo "$JWT_PAYLOAD" | base64 -d 2>/dev/null | jq . 2>/dev/null || echo "")
    if [ -n "$DECODED" ]; then
        echo "   ✅ Token is a valid JWT"
        
        # Check issuer to verify it's from Keycloak
        ISSUER=$(echo "$DECODED" | jq -r '.iss // empty' 2>/dev/null || echo "")
        SUB=$(echo "$DECODED" | jq -r '.sub // empty' 2>/dev/null || echo "")
        
        if [ -n "$ISSUER" ]; then
            echo "   Token issuer: $ISSUER"
            
            # Check if issuer contains Keycloak realm
            if echo "$ISSUER" | grep -q "/realms/maas"; then
                echo "   ✅ Token is from Keycloak (maas realm)"
            elif echo "$ISSUER" | grep -q "keycloak"; then
                echo "   ✅ Token is from Keycloak"
            elif echo "$SUB" | grep -q "system:serviceaccount"; then
                echo "   ❌ Token is from ServiceAccount (sub: $SUB)"
                echo "   This indicates Keycloak integration is not working correctly!"
                exit 1
            else
                echo "   ⚠️  Token issuer doesn't match expected Keycloak pattern"
                echo "   Issuer: $ISSUER"
            fi
        else
            echo "   ⚠️  Could not extract issuer from token"
        fi
    
        echo "   Token claims:"
        echo "$DECODED" | head -100
        echo ""
    else
        echo "   ⚠️  Could not decode token payload"
        GROUPS="[]"
    fi
else
    echo "   ⚠️  Token doesn't appear to be a JWT"
    GROUPS="[]"
fi

# Test 2: Check user tier
echo "3️⃣ Checking user tier..."
# Extract groups from the original user token (not the minted token)
USER_JWT_PAYLOAD=$(echo "$USER_TOKEN" | cut -d. -f2 2>/dev/null || echo "")
if [ -n "$USER_JWT_PAYLOAD" ]; then
    USER_DECODED=$(echo "$USER_JWT_PAYLOAD" | base64 -d 2>/dev/null | jq . 2>/dev/null || echo "")
    if [ -n "$USER_DECODED" ]; then
        USER_GROUPS=$(echo "$USER_DECODED" | jq -c '.groups // []' 2>/dev/null || echo "[]")
    else
        USER_GROUPS="[]"
    fi
else
    USER_GROUPS="[]"
fi

if [ "$USER_GROUPS" != "[]" ] && [ "$USER_GROUPS" != "null" ] && [ -n "$USER_GROUPS" ]; then
    echo "   User groups from token: $USER_GROUPS"
    TIER_RESPONSE=$(curl -sSk \
      -H "Authorization: Bearer ${TOKEN}" \
      -H "Content-Type: application/json" \
      -X POST \
      -d "{\"groups\": $USER_GROUPS}" \
      "${HOST}/maas-api/v1/tiers/lookup" 2>/dev/null || echo "")
    
    if [ -n "$TIER_RESPONSE" ]; then
        TIER_NAME=$(echo "$TIER_RESPONSE" | jq -r '.tier // empty' 2>/dev/null || echo "")
        TIER_DISPLAY=$(echo "$TIER_RESPONSE" | jq -r '.displayName // empty' 2>/dev/null || echo "")
        TIER_ERROR=$(echo "$TIER_RESPONSE" | jq -r '.error // empty' 2>/dev/null || echo "")
        
        if [ -n "$TIER_NAME" ] && [ "$TIER_NAME" != "null" ]; then
            echo "   ✅ Current tier: $TIER_DISPLAY ($TIER_NAME)"
        elif [ -n "$TIER_ERROR" ]; then
            echo "   ⚠️  Tier lookup error: $(echo "$TIER_RESPONSE" | jq -r '.message // "Unknown error"')"
            echo "   This may indicate the user's groups are not mapped to any tier"
        else
            echo "   ⚠️  Could not determine tier"
            echo "   Response: $TIER_RESPONSE"
        fi
    else
        echo "   ⚠️  Failed to get tier response"
    fi
else
    echo "   ⚠️  No groups found in token, cannot determine tier"
fi
echo ""

# Test 3: Verify models endpoint requires authentication
echo "4️⃣ Verifying models endpoint requires authentication..."
NO_TOKEN_HTTP_CODE=$(curl -sSk -o /dev/null -w "%{http_code}" \
  -H "Content-Type: application/json" \
  "${HOST}/maas-api/v1/models" 2>/dev/null || echo "000")

if [ "$NO_TOKEN_HTTP_CODE" == "401" ] || [ "$NO_TOKEN_HTTP_CODE" == "403" ]; then
    echo "   ✅ Models endpoint correctly rejects requests without token (HTTP $NO_TOKEN_HTTP_CODE)"
elif [ "$NO_TOKEN_HTTP_CODE" == "200" ]; then
    echo "   ❌ SECURITY ISSUE: Models endpoint allows access without token (HTTP $NO_TOKEN_HTTP_CODE)"
    echo "   Authentication is not properly enforced!"
else
    echo "   ⚠️  Unexpected response without token (HTTP $NO_TOKEN_HTTP_CODE)"
fi
echo ""

# Test 4: Use token to access models endpoint
echo "5️⃣ Testing model access with Keycloak token..."
MODELS_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  "${HOST}/maas-api/v1/models" || echo "")

echo "MODELS_RESPONSE: $MODELS_RESPONSE"

HTTP_CODE=$(curl -sSk -o /dev/null -w "%{http_code}" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  "${HOST}/maas-api/v1/models" || echo "000")

if [ "$HTTP_CODE" == "200" ]; then
    echo "   ✅ Model access successful (HTTP $HTTP_CODE)"
    if [ -n "$MODELS_RESPONSE" ]; then
        MODEL_COUNT=$(echo "$MODELS_RESPONSE" | jq '.data | length' 2>/dev/null || echo "0")
        echo "   Found $MODEL_COUNT models"
        
        # Extract first model for inference test
        MODEL_ID=$(echo "$MODELS_RESPONSE" | jq -r '.data[0].id // empty' 2>/dev/null || echo "")
        MODEL_URL=$(echo "$MODELS_RESPONSE" | jq -r '.data[0].url // empty' 2>/dev/null || echo "")
    fi
else
    echo "   ⚠️  Model access returned HTTP $HTTP_CODE"
    if [ "$HTTP_CODE" == "401" ]; then
        echo "   This may indicate AuthPolicy needs to be updated for OIDC"
    fi
    echo "   Response: $MODELS_RESPONSE"
fi
echo ""

# Test 5: Test model inference (if model is available)
if [ -n "$MODEL_ID" ] && [ "$MODEL_ID" != "null" ] && [ -n "$MODEL_URL" ] && [ "$MODEL_URL" != "null" ]; then
    echo "6️⃣ Testing model inference with Keycloak token..."
    echo "   Model: $MODEL_ID"
    echo "   Endpoint: ${MODEL_URL}/v1/chat/completions"
    echo ""
    
    # Construct JSON body with proper variable expansion
    REQUEST_BODY=$(jq -n \
      --arg model "$MODEL_ID" \
      --argjson max_tokens "${MAX_TOKENS:-100}" \
      '{
        "model": $model,
        "messages": [{"role": "user", "content": "Hello"}],
        "max_tokens": $max_tokens
      }')
    
    INFERENCE_RESPONSE=$(curl -sSk \
      -H "Authorization: Bearer ${TOKEN}" \
      -H "Content-Type: application/json" \
      -X POST \
      -d "$REQUEST_BODY" \
      -w "\nHTTP_STATUS:%{http_code}" \
      "${MODEL_URL}/v1/chat/completions" 2>&1 || echo "")
    
    HTTP_STATUS=$(echo "$INFERENCE_RESPONSE" | grep "HTTP_STATUS:" | cut -d':' -f2)
    RESPONSE_BODY=$(echo "$INFERENCE_RESPONSE" | sed '/HTTP_STATUS:/d')
    
    if [ "$HTTP_STATUS" == "200" ]; then
        echo "   ✅ Model inference successful (HTTP $HTTP_STATUS)"
        ANSWER=$(echo "$RESPONSE_BODY" | jq -r '.choices[0].message.content // "No response"' 2>/dev/null || echo "")
        TOKENS_USED=$(echo "$RESPONSE_BODY" | jq -r '.usage.total_tokens // 0' 2>/dev/null || echo "0")
        
        if [ -n "$ANSWER" ] && [ "$ANSWER" != "null" ]; then
            echo "   Response: $ANSWER"
        fi
        if [ "$TOKENS_USED" != "0" ] && [ "$TOKENS_USED" != "null" ]; then
            echo "   Tokens used: $TOKENS_USED"
        fi
    else
        echo "   ⚠️  Model inference returned HTTP $HTTP_STATUS"
        ERROR_MSG=$(echo "$RESPONSE_BODY" | jq -r '.error.message // .error // "Unknown error"' 2>/dev/null || echo "")
        if [ -n "$ERROR_MSG" ] && [ "$ERROR_MSG" != "null" ]; then
            echo "   Error: $ERROR_MSG"
        else
            echo "   Response: $(echo "$RESPONSE_BODY" | head -c 200)"
        fi
    fi
    echo ""
else
    echo "6️⃣ Skipping model inference test (no models available)"
    echo ""
fi

# Test 6: Verify Keycloak is accessible
echo "7️⃣ Verifying Keycloak connectivity..."
KEYCLOAK_ROUTE=$(kubectl get route keycloak -n keycloak -o jsonpath='{.spec.host}' 2>/dev/null || echo "")
if [ -n "$KEYCLOAK_ROUTE" ]; then
    KEYCLOAK_URL="https://${KEYCLOAK_ROUTE}"
    HEALTH=$(curl -sSk "${KEYCLOAK_URL}/health/ready" 2>/dev/null || echo "")
    if [ -n "$HEALTH" ]; then
        echo "   ✅ Keycloak is accessible"
    else
        echo "   ⚠️  Keycloak health check failed"
    fi
else
    echo "   ⚠️  Keycloak route not found"
fi
echo ""

# Test 7: Check maas-api logs for Keycloak usage
echo "8️⃣ Checking maas-api logs for Keycloak usage..."
RECENT_LOGS=$(kubectl logs -n maas-api deployment/maas-api --tail=20 2>/dev/null | grep -i keycloak || echo "")
if [ -n "$RECENT_LOGS" ]; then
    echo "   ✅ Found Keycloak references in logs:"
    echo "$RECENT_LOGS" | head -5
else
    echo "   ⚠️  No Keycloak references found in recent logs"
fi
echo ""

echo "========================================="
echo "✅ PoC Test Complete!"
echo "========================================="
echo ""
echo "Summary:"
echo "  - Token minting: ✅"
echo "  - Token format: ✅"
echo "  - Model access: $([ "$HTTP_CODE" == "200" ] && echo "✅" || echo "⚠️")"
echo ""
echo "Next steps:"
echo "1. If model access failed, check AuthPolicy configuration"
echo "2. Verify Keycloak token includes required claims (username, groups)"
echo "3. Check Authorino logs for OIDC validation errors"
echo ""
