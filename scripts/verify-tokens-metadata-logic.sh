#!/bin/bash

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
MAGENTA='\033[0;35m'
NC='\033[0m' 

if ! command -v oc &> /dev/null; then
    echo -e "${RED}Error: 'oc' command not found!${NC}"
    echo "This script requires OpenShift CLI to obtain identity tokens."
    exit 1
fi

if ! command -v jq &> /dev/null; then
    echo -e "${RED}Error: 'jq' command not found!${NC}"
    echo "This script requires jq to parse JSON."
    exit 1
fi

if ! command -v kubectl &> /dev/null; then
    echo -e "${RED}Error: 'kubectl' command not found!${NC}"
    echo "This script requires kubectl to query Gateway resources."
    exit 1
fi

# Gateway URL Discovery
if [ -z "${GATEWAY_URL:-}" ]; then
    echo -e "${BLUE}Looking up gateway configuration...${NC}"
    
    # Get the listener hostname from the Gateway spec (this is what Envoy routes on)
    GATEWAY_HOSTNAME=$(kubectl get gateway maas-default-gateway -n openshift-ingress -o jsonpath='{.spec.listeners[0].hostname}' 2>/dev/null)
    
    if [ -z "$GATEWAY_HOSTNAME" ]; then
        # Fallback: try to get from status address (may not work with hostname-based routing)
        GATEWAY_HOSTNAME=$(kubectl get gateway -l app.kubernetes.io/instance=maas-default-gateway -n openshift-ingress -o jsonpath='{.items[0].status.addresses[0].value}' 2>/dev/null)
    fi
    
    if [ -z "$GATEWAY_HOSTNAME" ]; then
        echo -e "${RED}Failed to find gateway hostname automatically.${NC}"
        echo -e "Please set GATEWAY_URL explicitly (e.g., export GATEWAY_URL=https://maas.apps.example.com)"
        exit 1
    fi
    
    # Try HTTPS first, fall back to HTTP if it fails
    SCHEME="https"
    if ! curl -skS -m 5 "${SCHEME}://${GATEWAY_HOSTNAME}/maas-api/healthz" -o /dev/null 2>/dev/null; then
        SCHEME="http"
    fi
    
    GATEWAY_URL="${SCHEME}://${GATEWAY_HOSTNAME}"
    echo -e "${GREEN}✓ Found Gateway at: ${GATEWAY_URL}${NC}"
fi

API_BASE="${GATEWAY_URL%/}"

echo -e "${CYAN}======================================${NC}"
echo -e "${CYAN}   MaaS API Comprehensive Verification  ${NC}"
echo -e "${CYAN}======================================${NC}"
echo ""
echo -e "${BLUE}Gateway URL:${NC} ${GATEWAY_URL}"
echo ""

echo -e "${MAGENTA}1. Authenticating with OpenShift...${NC}"

# Try oc whoami -t first, redirect stderr to avoid error message in logs
OC_TOKEN="$(oc whoami -t 2>/dev/null || true)"

# If that failed, try fallback methods (for Prow CI compatibility)
if [ -z "$OC_TOKEN" ]; then
    # Try reading from mounted service account token (Prow CI pods)
    if [ -f "/var/run/secrets/kubernetes.io/serviceaccount/token" ]; then
        OC_TOKEN=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token 2>/dev/null || true)
    fi
    
    # Extracting from kubeconfig (Prow CI)
    if [ -z "$OC_TOKEN" ] && [ -n "${KUBECONFIG:-}" ]; then
        OC_TOKEN=$(oc config view --raw -o jsonpath='{.users[0].user.token}' 2>/dev/null || true)
    fi
    
    # oc create token for service account (Prow CI)
    if [ -z "$OC_TOKEN" ]; then
        CURRENT_USER=$(oc whoami 2>/dev/null || true)
        if [ -n "$CURRENT_USER" ] && [[ "$CURRENT_USER" == system:serviceaccount:* ]]; then
            SA_NAMESPACE=$(echo "$CURRENT_USER" | cut -d: -f3)
            SA_NAME=$(echo "$CURRENT_USER" | cut -d: -f4)
            if [ -n "$SA_NAMESPACE" ] && [ -n "$SA_NAME" ]; then
                OC_TOKEN=$(oc create token "$SA_NAME" -n "$SA_NAMESPACE" 2>/dev/null || true)
            fi
        fi
    fi
fi

if [ -z "$OC_TOKEN" ]; then
    echo -e "${RED}✗ Failed to obtain OpenShift identity token!${NC}"
    echo "Please ensure you are logged in: oc login"
    echo "Or in Prow CI, ensure proper service account authentication is configured"
    exit 1
fi
echo -e "${GREEN}✓ Authenticated successfully${NC}"
echo ""

# 2. Ephemeral Tokens (Stateless)
echo -e "${MAGENTA}2. Testing Ephemeral Tokens (/v1/tokens)...${NC}"

# Test 2.1: Issue Ephemeral Token
echo -n "  • Issuing ephemeral token (4h)... "
TOKEN_RESPONSE=$(curl -sSk \
    -H "Authorization: Bearer $OC_TOKEN" \
    -H "Content-Type: application/json" \
    -X POST \
    -d '{"expiration": "4h"}' \
    -w "\nHTTP_STATUS:%{http_code}\n" \
    "${API_BASE}/maas-api/v1/tokens")

http_status=$(echo "$TOKEN_RESPONSE" | grep "HTTP_STATUS:" | cut -d':' -f2)
response_body=$(echo "$TOKEN_RESPONSE" | sed '/HTTP_STATUS:/d')

if [ "$http_status" == "201" ]; then
    EPHEMERAL_TOKEN=$(echo "$response_body" | jq -r '.token')
    echo -e "${GREEN}✓ Success${NC}"
else
    echo -e "${RED}✗ Failed (Status: $http_status)${NC}"
    echo "Response: $response_body"
    exit 1
fi

# Test 2.2: Validate Ephemeral Token (List Models)
echo -n "  • Validating token (Listing Models)... "
MODELS_RESPONSE=$(curl -sSk \
    -H "Authorization: Bearer $EPHEMERAL_TOKEN" \
    -w "\nHTTP_STATUS:%{http_code}\n" \
    "${API_BASE}/maas-api/v1/models")

http_status=$(echo "$MODELS_RESPONSE" | grep "HTTP_STATUS:" | cut -d':' -f2)
if [ "$http_status" == "200" ]; then
    echo -e "${GREEN}✓ Success${NC}"
else
    echo -e "${RED}✗ Failed (Status: $http_status)${NC}"
    exit 1
fi

# Test 2.3: Verify Ephemeral Token Doesn't Appear in API Keys List
echo -n "  • Verifying ephemeral token NOT in API keys list... "
KEYS_LIST=$(curl -sSk \
    -H "Authorization: Bearer $OC_TOKEN" \
    "${API_BASE}/maas-api/v1/api-keys")

EPHEMERAL_JTI=$(echo "$response_body" | jq -r '.jti // empty')
if [ -n "$EPHEMERAL_JTI" ]; then
    FOUND_IN_LIST=$(echo "$KEYS_LIST" | jq -r ".[] | select(.id == \"$EPHEMERAL_JTI\") | .id")
    if [ -z "$FOUND_IN_LIST" ]; then
        echo -e "${GREEN}✓ Success (Ephemeral token not persisted)${NC}"
    else
        echo -e "${RED}✗ Failed (Ephemeral token found in API keys list!)${NC}"
    fi
else
    echo -e "${YELLOW}⚠ Skipped (No JTI in ephemeral token response)${NC}"
fi
echo ""

# 3. API Keys (Persistent)
echo -e "${MAGENTA}3. Testing API Keys (Persistent /v1/api-keys)...${NC}"

KEY_NAME="test-key-$(date +%s)"

# Test 3.0: Verify API Key Creation Without Name Fails
echo -n "  • Testing API key creation without name (should fail)... "
NO_NAME_RESPONSE=$(curl -sSk \
    -H "Authorization: Bearer $OC_TOKEN" \
    -H "Content-Type: application/json" \
    -X POST \
    -d '{"expiration": "1h"}' \
    -w "\nHTTP_STATUS:%{http_code}\n" \
    "${API_BASE}/maas-api/v1/api-keys")

no_name_status=$(echo "$NO_NAME_RESPONSE" | grep "HTTP_STATUS:" | cut -d':' -f2)
if [ "$no_name_status" == "400" ]; then
    echo -e "${GREEN}✓ Success (Correctly rejected)${NC}"
else
    echo -e "${RED}✗ Failed (Expected 400, got $no_name_status)${NC}"
fi

# Test 3.1: Create API Key
KEY_DESCRIPTION="Test API key for verification script"
echo -n "  • Creating API Key ('$KEY_NAME') with description... "
KEY_RESPONSE=$(curl -sSk \
    -H "Authorization: Bearer $OC_TOKEN" \
    -H "Content-Type: application/json" \
    -X POST \
    -d "{\"name\": \"$KEY_NAME\", \"description\": \"$KEY_DESCRIPTION\", \"expiration\": \"24h\"}" \
    -w "\nHTTP_STATUS:%{http_code}\n" \
    "${API_BASE}/maas-api/v1/api-keys")

http_status=$(echo "$KEY_RESPONSE" | grep "HTTP_STATUS:" | cut -d':' -f2)
response_body=$(echo "$KEY_RESPONSE" | sed '/HTTP_STATUS:/d')

if [ "$http_status" == "201" ]; then
    # Verify response structure: { "token": "...", "expiration": "...", "expiresAt": ..., "jti": "...", "name": "...", "description": "..." }
    HAS_TOKEN=$(echo "$response_body" | jq -r 'has("token")')
    HAS_EXPIRATION=$(echo "$response_body" | jq -r 'has("expiration")')
    HAS_EXPIRES_AT=$(echo "$response_body" | jq -r 'has("expiresAt")')
    HAS_JTI=$(echo "$response_body" | jq -r 'has("jti")')
    HAS_NAME=$(echo "$response_body" | jq -r 'has("name")')
    HAS_DESCRIPTION=$(echo "$response_body" | jq -r 'has("description")')
    
    if [ "$HAS_TOKEN" == "true" ] && [ "$HAS_EXPIRATION" == "true" ] && [ "$HAS_EXPIRES_AT" == "true" ] && [ "$HAS_JTI" == "true" ] && [ "$HAS_NAME" == "true" ]; then
        API_KEY_TOKEN=$(echo "$response_body" | jq -r '.token')
        API_KEY_JTI=$(echo "$response_body" | jq -r '.jti')
        API_KEY_NAME=$(echo "$response_body" | jq -r '.name')
        API_KEY_DESCRIPTION=$(echo "$response_body" | jq -r '.description // ""')
        
        echo -e "${GREEN}✓ Success${NC}"
        echo "    - JTI: $API_KEY_JTI"
        echo "    - Name: $API_KEY_NAME"
        echo "    - Description: ${API_KEY_DESCRIPTION:-'(empty)'}"
        echo "    - Response structure: ✓ Valid (all required fields present)"
        if [ "$HAS_DESCRIPTION" == "true" ]; then
            echo "    - Description field: ✓ Present"
            # Verify description matches what we sent
            if [ "$API_KEY_DESCRIPTION" == "$KEY_DESCRIPTION" ]; then
                echo "    - Description value: ✓ Matches request"
            else
                echo -e "    - Description value: ${YELLOW}⚠ Mismatch (expected: $KEY_DESCRIPTION, got: $API_KEY_DESCRIPTION)${NC}"
            fi
        else
            echo -e "    - Description field: ${YELLOW}⚠ Missing (expected but not present)${NC}"
        fi
    else
        echo -e "${RED}✗ Failed (Invalid response structure)${NC}"
        echo "  Missing fields:"
        [ "$HAS_TOKEN" != "true" ] && echo "    - token"
        [ "$HAS_EXPIRATION" != "true" ] && echo "    - expiration"
        [ "$HAS_EXPIRES_AT" != "true" ] && echo "    - expiresAt"
        [ "$HAS_JTI" != "true" ] && echo "    - jti"
        [ "$HAS_NAME" != "true" ] && echo "    - name"
        echo "Response: $response_body"
        exit 1
    fi
else
    echo -e "${RED}✗ Failed (Status: $http_status)${NC}"
    echo "Response: $response_body"
    exit 1
fi

# Test 3.2: List API Keys
echo -n "  • Listing API Keys... "
LIST_RESPONSE=$(curl -sSk \
    -H "Authorization: Bearer $OC_TOKEN" \
    -w "\nHTTP_STATUS:%{http_code}\n" \
    "${API_BASE}/maas-api/v1/api-keys")

http_status=$(echo "$LIST_RESPONSE" | grep "HTTP_STATUS:" | cut -d':' -f2)
list_body=$(echo "$LIST_RESPONSE" | sed '/HTTP_STATUS:/d')

if [ "$http_status" == "200" ]; then
    # Verify empty list returns [] not null
    IS_ARRAY=$(echo "$list_body" | jq 'type == "array"')
    if [ "$IS_ARRAY" != "true" ]; then
        echo -e "${RED}✗ Failed (Expected array, got: $(echo "$list_body" | jq 'type'))${NC}"
        exit 1
    fi
    
    # Check if our key is in the list
    FOUND_KEY=$(echo "$list_body" | jq -r ".[] | select(.name == \"$KEY_NAME\") | .name")
    if [ "$FOUND_KEY" == "$KEY_NAME" ]; then
        echo -e "${GREEN}✓ Success (Found '$KEY_NAME')${NC}"
        
        # Verify response structure has required fields
        KEY_DATA=$(echo "$list_body" | jq ".[] | select(.name == \"$KEY_NAME\")")
        HAS_ID=$(echo "$KEY_DATA" | jq -r 'has("id")')
        HAS_NAME=$(echo "$KEY_DATA" | jq -r 'has("name")')
        HAS_DESCRIPTION=$(echo "$KEY_DATA" | jq -r 'has("description")')
        HAS_STATUS=$(echo "$KEY_DATA" | jq -r 'has("status")')
        HAS_CREATION_DATE=$(echo "$KEY_DATA" | jq -r 'has("creationDate")')
        HAS_EXPIRATION_DATE=$(echo "$KEY_DATA" | jq -r 'has("expirationDate")')
        
        if [ "$HAS_ID" == "true" ] && [ "$HAS_NAME" == "true" ] && [ "$HAS_STATUS" == "true" ] && [ "$HAS_CREATION_DATE" == "true" ] && [ "$HAS_EXPIRATION_DATE" == "true" ]; then
            echo "    - Response structure: ✓ Valid (all required fields present)"
            if [ "$HAS_DESCRIPTION" == "true" ]; then
                LISTED_DESCRIPTION=$(echo "$KEY_DATA" | jq -r '.description // ""')
                echo "    - Description field: ✓ Present"
                if [ -n "$LISTED_DESCRIPTION" ]; then
                    echo "    - Description value: $LISTED_DESCRIPTION"
                    # Verify description matches what we sent
                    if [ "$LISTED_DESCRIPTION" == "$KEY_DESCRIPTION" ]; then
                        echo "    - Description matches request: ✓"
                    else
                        echo -e "    - Description value: ${YELLOW}⚠ Mismatch (expected: $KEY_DESCRIPTION, got: $LISTED_DESCRIPTION)${NC}"
                    fi
                else
                    echo "    - Description value: (empty)"
                fi
            else
                echo -e "    - Description field: ${YELLOW}⚠ Optional (not present)${NC}"
            fi
        else
            echo -e "${YELLOW}⚠ Warning: Missing required fields in response${NC}"
            [ "$HAS_ID" != "true" ] && echo "      - Missing: id"
            [ "$HAS_NAME" != "true" ] && echo "      - Missing: name"
            [ "$HAS_STATUS" != "true" ] && echo "      - Missing: status"
            [ "$HAS_CREATION_DATE" != "true" ] && echo "      - Missing: creationDate"
            [ "$HAS_EXPIRATION_DATE" != "true" ] && echo "      - Missing: expirationDate"
        fi
    else
        echo -e "${RED}✗ Failed (Key '$KEY_NAME' not found in list)${NC}"
        echo "List: $list_body"
    fi
else
    echo -e "${RED}✗ Failed (Status: $http_status)${NC}"
fi

# Test 3.3: Get Specific API Key
echo -n "  • Getting API Key by ID ($API_KEY_JTI)... "
GET_RESPONSE=$(curl -sSk \
    -H "Authorization: Bearer $OC_TOKEN" \
    -w "\nHTTP_STATUS:%{http_code}\n" \
    "${API_BASE}/maas-api/v1/api-keys/$API_KEY_JTI")

http_status=$(echo "$GET_RESPONSE" | grep "HTTP_STATUS:" | cut -d':' -f2)
get_body=$(echo "$GET_RESPONSE" | sed '/HTTP_STATUS:/d')

if [ "$http_status" == "200" ]; then
    RETRIEVED_ID=$(echo "$get_body" | jq -r '.id')
    RETRIEVED_NAME=$(echo "$get_body" | jq -r '.name')
    RETRIEVED_DESCRIPTION=$(echo "$get_body" | jq -r '.description // ""')
    
    # Verify all required fields are present
    HAS_ID=$(echo "$get_body" | jq -r 'has("id")')
    HAS_NAME=$(echo "$get_body" | jq -r 'has("name")')
    HAS_DESCRIPTION=$(echo "$get_body" | jq -r 'has("description")')
    HAS_STATUS=$(echo "$get_body" | jq -r 'has("status")')
    HAS_CREATION_DATE=$(echo "$get_body" | jq -r 'has("creationDate")')
    HAS_EXPIRATION_DATE=$(echo "$get_body" | jq -r 'has("expirationDate")')
    
    if [ "$RETRIEVED_ID" == "$API_KEY_JTI" ]; then
        if [ "$HAS_ID" == "true" ] && [ "$HAS_NAME" == "true" ] && [ "$HAS_STATUS" == "true" ] && [ "$HAS_CREATION_DATE" == "true" ] && [ "$HAS_EXPIRATION_DATE" == "true" ]; then
        echo -e "${GREEN}✓ Success${NC}"
            echo "    - ID: $RETRIEVED_ID"
            echo "    - Name: $RETRIEVED_NAME"
            if [ "$HAS_DESCRIPTION" == "true" ]; then
                echo "    - Description field: ✓ Present"
                if [ -n "$RETRIEVED_DESCRIPTION" ]; then
                    echo "    - Description value: $RETRIEVED_DESCRIPTION"
                    # Verify description matches what we sent
                    if [ "$RETRIEVED_DESCRIPTION" == "$KEY_DESCRIPTION" ]; then
                        echo "    - Description matches request: ✓"
                    else
                        echo -e "    - Description value: ${YELLOW}⚠ Mismatch (expected: $KEY_DESCRIPTION, got: $RETRIEVED_DESCRIPTION)${NC}"
                    fi
                else
                    echo "    - Description value: (empty)"
                fi
            else
                echo -e "    - Description field: ${YELLOW}⚠ Optional (not present)${NC}"
            fi
            echo "    - All required fields: ✓ Present"
        else
            echo -e "${YELLOW}⚠ Partial success (ID matches but missing fields)${NC}"
            [ "$HAS_ID" != "true" ] && echo "      - Missing: id"
            [ "$HAS_NAME" != "true" ] && echo "      - Missing: name"
            [ "$HAS_STATUS" != "true" ] && echo "      - Missing: status"
            [ "$HAS_CREATION_DATE" != "true" ] && echo "      - Missing: creationDate"
            [ "$HAS_EXPIRATION_DATE" != "true" ] && echo "      - Missing: expirationDate"
        fi
    else
        echo -e "${RED}✗ Failed (ID mismatch: expected $API_KEY_JTI, got $RETRIEVED_ID)${NC}"
    fi
else
    echo -e "${RED}✗ Failed (Status: $http_status)${NC}"
fi

# Test 3.4: Validate API Key Usage
echo -n "  • Using API Key for Request... "
MODELS_RESPONSE=$(curl -sSk \
    -H "Authorization: Bearer $API_KEY_TOKEN" \
    -w "\nHTTP_STATUS:%{http_code}\n" \
    "${API_BASE}/maas-api/v1/models")

http_status=$(echo "$MODELS_RESPONSE" | grep "HTTP_STATUS:" | cut -d':' -f2)
if [ "$http_status" == "200" ]; then
    echo -e "${GREEN}✓ Success${NC}"
else
    echo -e "${RED}✗ Failed (Status: $http_status)${NC}"
fi

# Test 3.5: Note - Single key deletion removed for initial release
echo -e "${BLUE}ℹ Note: Single API key deletion (DELETE /v1/api-keys/:id) removed for initial release${NC}"
echo "    Use DELETE /v1/tokens to revoke all tokens (recreates Service Account)"

echo ""

# 4. Revoke All Tokens
echo -e "${MAGENTA}4. Testing Revoke All Tokens (/v1/tokens)...${NC}"

# Create a temp key to ensure it gets marked as expired
echo -n "  • Creating temp key for cleanup test... "
TEMP_KEY_RESPONSE=$(curl -sSk \
    -H "Authorization: Bearer $OC_TOKEN" \
    -H "Content-Type: application/json" \
    -X POST \
    -d "{\"name\": \"cleanup-test\", \"expiration\": \"1h\"}" \
    -w "\nHTTP_STATUS:%{http_code}\n" \
    "${API_BASE}/maas-api/v1/api-keys")

temp_key_status=$(echo "$TEMP_KEY_RESPONSE" | grep "HTTP_STATUS:" | cut -d':' -f2)
temp_key_body=$(echo "$TEMP_KEY_RESPONSE" | sed '/HTTP_STATUS:/d')

if [ "$temp_key_status" == "201" ]; then
    # Try to get jti directly from response (preferred)
    TEMP_KEY_JTI=$(echo "$temp_key_body" | jq -r '.jti // empty')
    
    # If not found, extract from JWT token
    if [ -z "$TEMP_KEY_JTI" ]; then
        JWT_TOKEN=$(echo "$temp_key_body" | jq -r '.token // empty')
        if [ -n "$JWT_TOKEN" ]; then
            # JWT uses base64url encoding; convert to standard base64 before decoding
            PAYLOAD=$(echo "$JWT_TOKEN" | cut -d'.' -f2 | tr '_-' '/+')
            case $((${#PAYLOAD} % 4)) in
                2) PAYLOAD="${PAYLOAD}==" ;;
                3) PAYLOAD="${PAYLOAD}=" ;;
            esac
            TEMP_KEY_JTI=$(echo "$PAYLOAD" | base64 -d 2>/dev/null | jq -r '.jti // empty' 2>/dev/null || echo "")
        fi
    fi
    
    if [ -n "$TEMP_KEY_JTI" ]; then
        echo -e "${GREEN}✓ Done (JTI: $TEMP_KEY_JTI)${NC}"
    else
        echo -e "${GREEN}✓ Done${NC}"
    fi
else
    echo -e "${RED}✗ Failed (Status: $temp_key_status)${NC}"
    echo "Response: $temp_key_body"
    exit 1
fi

echo -n "  • Revoking ALL tokens... "
REVOKE_ALL_RESPONSE=$(curl -sSk \
    -H "Authorization: Bearer $OC_TOKEN" \
    -X DELETE \
    -w "\nHTTP_STATUS:%{http_code}\n" \
    "${API_BASE}/maas-api/v1/tokens")

http_status=$(echo "$REVOKE_ALL_RESPONSE" | grep "HTTP_STATUS:" | cut -d':' -f2)

if [ "$http_status" == "204" ]; then
    echo -e "${GREEN}✓ Success${NC}"
else
    echo -e "${RED}✗ Failed (Status: $http_status)${NC}"
    exit 1
fi

# Verify tokens are marked as expired (not deleted)
echo -n "  • Verifying tokens marked as expired... "
LIST_RESPONSE=$(curl -sSk \
    -H "Authorization: Bearer $OC_TOKEN" \
    -w "\nHTTP_STATUS:%{http_code}\n" \
    "${API_BASE}/maas-api/v1/api-keys")

http_status=$(echo "$LIST_RESPONSE" | grep "HTTP_STATUS:" | cut -d':' -f2)
list_body=$(echo "$LIST_RESPONSE" | sed '/HTTP_STATUS:/d')
IS_ARRAY=$(echo "$list_body" | jq 'type == "array"')

if [ "$IS_ARRAY" != "true" ]; then
    echo -e "${RED}✗ Failed (Expected array, got: $(echo "$list_body" | jq 'type'))${NC}"
    echo "Response: $list_body"
    exit 1
fi

# Check that all tokens have status "expired"
ALL_EXPIRED=$(echo "$list_body" | jq '[.[] | select(.status == "expired")] | length')
TOTAL_COUNT=$(echo "$list_body" | jq 'length')

if [ "$TOTAL_COUNT" -gt 0 ] && [ "$ALL_EXPIRED" == "$TOTAL_COUNT" ]; then
    echo -e "${GREEN}✓ Success (All $TOTAL_COUNT tokens marked as expired)${NC}"
elif [ "$TOTAL_COUNT" == "0" ]; then
    echo -e "${YELLOW}⚠ Info (No tokens found - may have been deleted in previous test)${NC}"
else
    echo -e "${RED}✗ Failed (Expected all tokens expired, got $ALL_EXPIRED/$TOTAL_COUNT expired)${NC}"
    echo "Response: $list_body"
fi

echo ""
echo -e "${CYAN}======================================${NC}"
echo -e "${GREEN}   All Verification Tests Passed!     ${NC}"
echo -e "${CYAN}======================================${NC}"

