#!/usr/bin/env bash

# Test script for listing available models with premium user
# This script tests premium-user token to verify model discovery

set -euo pipefail

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
PURPLE='\033[0;35m'
NC='\033[0m' # No Color

echo -e "${BLUE}🔍 MaaS Models List Test - Premium User${NC}"
echo "================================================"

# Test function to check models list
test_models_list() {
    local user_type="$1"
    local token="$2"
    local color="$3"
    
    echo -e "\n${color}=== Testing with ${user_type} ===${NC}"
    echo -e "${YELLOW}📝 Token length: ${#token}${NC}"
    echo -e "${YELLOW}📋 URL: http://localhost/v1/models${NC}"
    echo -e "${YELLOW}🔑 Authorization: Bearer [${user_type}-token]${NC}"
    echo ""
    
    # Test with timeout and status code
    local response_file=$(mktemp)
    local http_code=$(curl -s -w "%{http_code}" --max-time 10 \
        -H "Authorization: Bearer $token" \
        -o "$response_file" \
        http://localhost/v1/models)
    
    echo -e "${color}📊 HTTP Status: $http_code${NC}"
    
    if [ "$http_code" = "200" ]; then
        echo -e "${GREEN}✅ Success - Models list retrieved${NC}"
        echo -e "${GREEN}Response:${NC}"
        cat "$response_file" | jq '.' || {
            echo -e "${YELLOW}⚠️  Response is not valid JSON:${NC}"
            cat "$response_file"
        }
    elif [ "$http_code" = "401" ]; then
        echo -e "${RED}❌ Authentication Failed (401 Unauthorized)${NC}"
        echo -e "${YELLOW}Raw response:${NC}"
        cat "$response_file"
    elif [ "$http_code" = "403" ]; then
        echo -e "${YELLOW}🚫 Access Denied (403 Forbidden)${NC}"
        echo -e "${YELLOW}User tier may not have access to this endpoint${NC}"
        echo -e "${YELLOW}Response:${NC}"
        cat "$response_file"
    else
        echo -e "${YELLOW}⚠️  Unexpected status: $http_code${NC}"
        echo -e "${YELLOW}Response:${NC}"
        cat "$response_file"
    fi
    
    rm -f "$response_file"
}

# Create premium-user token
echo -e "${YELLOW}📝 Creating premium-user token...${NC}"
# Ensure premium-user service account exists
kubectl create serviceaccount premium-user -n maas-api 2>/dev/null || echo "ℹ️  premium-user service account already exists"
PREMIUM_TOKEN=$(kubectl create token premium-user -n maas-api --duration=1h --audience=maas-default-gateway-sa)
echo "✅ Premium-user token created"

# Test with premium-user
test_models_list "PREMIUM-USER" "$PREMIUM_TOKEN" "$PURPLE"

# Summary
echo -e "\n${BLUE}📊 Test Summary:${NC}"
echo "=============="
echo -e "${PURPLE}💎 Premium-user:${NC} Tests access to models with premium tier permissions"
echo ""
echo -e "${YELLOW}Expected behavior:${NC}"
echo "• Premium user should be able to list models (if auth is working)"
echo "• Both model-a and model-b should show with ready status"
echo "• HTTP 401 = Authentication system issue"
echo "• HTTP 403 = Authorization/tier access issue"

echo -e "\n${BLUE}✅ Premium user models list test completed${NC}"