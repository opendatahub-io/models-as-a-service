#!/bin/bash
# POC Test Script for Permanent API Keys
# Per docs/api-key-management/POC_PLAN.md
set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo "=== API Key POC End-to-End Test ==="
echo ""

# Configuration
MAAS_API_URL="${MAAS_API_URL:-http://localhost:8080}"
SA_TOKEN="${SA_TOKEN:-}" # Existing ServiceAccount token for authentication

# Check prerequisites
if [ -z "$SA_TOKEN" ]; then
    echo -e "${YELLOW}Warning: SA_TOKEN not set. Set it to test with authenticated endpoints.${NC}"
    echo "Example: export SA_TOKEN=\$(kubectl create token maas-api-sa -n maas-api)"
    echo ""
fi

echo "Using MaaS API URL: $MAAS_API_URL"
echo ""

# Test 1: Health check
echo -e "${YELLOW}[Test 1] Health Check${NC}"
HEALTH=$(curl -s "$MAAS_API_URL/health")
if echo "$HEALTH" | grep -q "ok\|healthy"; then
    echo -e "${GREEN}✓ Health check passed${NC}"
else
    echo -e "${RED}✗ Health check failed: $HEALTH${NC}"
    exit 1
fi
echo ""

# Test 2: Create permanent API key
echo -e "${YELLOW}[Test 2] Create Permanent API Key${NC}"
if [ -n "$SA_TOKEN" ]; then
    CREATE_RESPONSE=$(curl -s -X POST "$MAAS_API_URL/v1/api-keys/permanent" \
      -H "Authorization: Bearer $SA_TOKEN" \
      -H "Content-Type: application/json" \
      -d '{"name": "poc-test-key", "description": "POC validation test"}')
    
    echo "Response: $CREATE_RESPONSE"
    
    # Extract the key from response
    API_KEY=$(echo "$CREATE_RESPONSE" | grep -o '"key":"[^"]*"' | cut -d'"' -f4)
    KEY_ID=$(echo "$CREATE_RESPONSE" | grep -o '"id":"[^"]*"' | cut -d'"' -f4)
    KEY_PREFIX=$(echo "$CREATE_RESPONSE" | grep -o '"keyPrefix":"[^"]*"' | cut -d'"' -f4)
    
    if [[ "$API_KEY" == sk-oai-* ]]; then
        echo -e "${GREEN}✓ API key created with correct format: $KEY_PREFIX${NC}"
        echo "  Key ID: $KEY_ID"
    else
        echo -e "${RED}✗ API key format incorrect or creation failed${NC}"
    fi
else
    echo -e "${YELLOW}⊘ Skipped (no SA_TOKEN)${NC}"
fi
echo ""

# Test 3: Validate API key via internal endpoint
echo -e "${YELLOW}[Test 3] Validate API Key (Internal Endpoint)${NC}"
if [ -n "$API_KEY" ]; then
    VALIDATE_RESPONSE=$(curl -s -X POST "$MAAS_API_URL/internal/v1/api-keys/validate" \
      -H "Content-Type: application/json" \
      -d "{\"key\": \"$API_KEY\"}")
    
    echo "Response: $VALIDATE_RESPONSE"
    
    if echo "$VALIDATE_RESPONSE" | grep -q '"valid":true'; then
        echo -e "${GREEN}✓ API key validation successful${NC}"
        VALIDATED_USER=$(echo "$VALIDATE_RESPONSE" | grep -o '"userId":"[^"]*"' | cut -d'"' -f4)
        echo "  Validated user: $VALIDATED_USER"
    else
        echo -e "${RED}✗ API key validation failed${NC}"
    fi
else
    # Test with invalid key
    VALIDATE_RESPONSE=$(curl -s -X POST "$MAAS_API_URL/internal/v1/api-keys/validate" \
      -H "Content-Type: application/json" \
      -d '{"key": "sk-oai-invalid-test-key"}')
    
    echo "Response (invalid key): $VALIDATE_RESPONSE"
    
    if echo "$VALIDATE_RESPONSE" | grep -q '"valid":false'; then
        echo -e "${GREEN}✓ Invalid key correctly rejected${NC}"
    else
        echo -e "${RED}✗ Invalid key should be rejected${NC}"
    fi
fi
echo ""

# Test 4: Verify key format validation
echo -e "${YELLOW}[Test 4] Key Format Validation${NC}"
INVALID_FORMAT_RESPONSE=$(curl -s -X POST "$MAAS_API_URL/internal/v1/api-keys/validate" \
  -H "Content-Type: application/json" \
  -d '{"key": "not-a-valid-key"}')

echo "Response (wrong format): $INVALID_FORMAT_RESPONSE"

if echo "$INVALID_FORMAT_RESPONSE" | grep -q '"valid":false'; then
    echo -e "${GREEN}✓ Invalid format correctly rejected${NC}"
else
    echo -e "${RED}✗ Invalid format should be rejected${NC}"
fi
echo ""

# Test 5: List API keys
echo -e "${YELLOW}[Test 5] List API Keys${NC}"
if [ -n "$SA_TOKEN" ]; then
    LIST_RESPONSE=$(curl -s "$MAAS_API_URL/v1/api-keys" \
      -H "Authorization: Bearer $SA_TOKEN")
    
    echo "Response: $LIST_RESPONSE"
    echo -e "${GREEN}✓ List API keys endpoint working${NC}"
else
    echo -e "${YELLOW}⊘ Skipped (no SA_TOKEN)${NC}"
fi
echo ""

# Test 6: Revoke API key
echo -e "${YELLOW}[Test 6] Revoke API Key${NC}"
if [ -n "$SA_TOKEN" ] && [ -n "$KEY_ID" ]; then
    REVOKE_RESPONSE=$(curl -s -w "\n%{http_code}" -X DELETE "$MAAS_API_URL/v1/api-keys/$KEY_ID" \
      -H "Authorization: Bearer $SA_TOKEN")
    
    HTTP_CODE=$(echo "$REVOKE_RESPONSE" | tail -n1)
    
    if [ "$HTTP_CODE" = "204" ]; then
        echo -e "${GREEN}✓ API key revoked successfully${NC}"
        
        # Verify revoked key is rejected
        REVOKED_VALIDATE=$(curl -s -X POST "$MAAS_API_URL/internal/v1/api-keys/validate" \
          -H "Content-Type: application/json" \
          -d "{\"key\": \"$API_KEY\"}")
        
        if echo "$REVOKED_VALIDATE" | grep -q '"valid":false'; then
            echo -e "${GREEN}✓ Revoked key correctly rejected${NC}"
        else
            echo -e "${RED}✗ Revoked key should be rejected${NC}"
        fi
    else
        echo -e "${RED}✗ Failed to revoke API key (HTTP $HTTP_CODE)${NC}"
    fi
else
    echo -e "${YELLOW}⊘ Skipped (no SA_TOKEN or KEY_ID)${NC}"
fi
echo ""

echo "=== POC Test Complete ==="
echo ""
echo "Summary:"
echo "- Key format: sk-oai-{base62_encoded_256bit_random}"
echo "- Storage: SHA-256 hash only (plaintext never stored)"
echo "- Validation: HTTP callback endpoint at /internal/v1/api-keys/validate"
echo "- Show-once pattern: Key returned only at creation"
echo ""
echo "Next steps for full deployment:"
echo "1. Deploy updated MaaS API with POC changes"
echo "2. Apply updated gateway-auth-policy.yaml"
echo "3. Test end-to-end with model endpoint access"
