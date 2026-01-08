#!/usr/bin/env bash

# Test script for request rate limiting
# Free tier: 5 requests per 2 minutes
# This script waits for reset, then tests the rate limiting functionality

set -euo pipefail

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${BLUE}🚦 MaaS Rate Limiting Test${NC}"
echo "================================"
echo "Free tier limits: 5 requests per 2 minutes"
echo ""

# Ask user if they want to wait for rate limit reset
echo -e "${YELLOW}⚠️  Rate limits may be active from previous tests${NC}"
echo "Free tier: 5 requests per 2 minutes"
echo ""
read -p "Wait 130 seconds for rate limit reset? (y/N): " -n 1 -r
echo

if [[ $REPLY =~ ^[Yy]$ ]]; then
    echo -e "${YELLOW}⏳ Waiting 130 seconds for rate limit reset...${NC}"
    for i in {130..1}; do
        printf "\r⏱️  Countdown: %d seconds remaining..." $i
        sleep 1
    done
    echo -e "\n✅ Reset period complete"
else
    echo -e "${BLUE}ℹ️  Proceeding without reset - some requests may fail due to existing rate limits${NC}"
fi

# Create fresh token
echo -e "\n${YELLOW}📝 Creating fresh free-user token...${NC}"
TOKEN=$(kubectl create token free-user -n maas-api --duration=1h --audience=maas-default-gateway-sa)
echo "✅ Token created"

echo -e "\n${YELLOW}🧪 Testing rate limits (expecting 5 success + rate limiting)...${NC}"
echo "URL: http://localhost/llm/model-a/v1/chat/completions"
echo ""

# Test 8 requests to demonstrate rate limiting
SUCCESS_COUNT=0
RATE_LIMITED_COUNT=0

for i in {1..8}; do
    echo -e "${BLUE}Request $i:${NC}"
    
    RESPONSE=$(curl -s -w "HTTP_CODE:%{http_code}" \
        -H "Authorization: Bearer $TOKEN" \
        -H "Content-Type: application/json" \
        -d "{\"model\":\"model-a\",\"messages\":[{\"role\":\"user\",\"content\":\"Test request $i\"}]}" \
        http://localhost/llm/model-a/v1/chat/completions)
    
    HTTP_CODE=$(echo "$RESPONSE" | grep -o "HTTP_CODE:[0-9]*" | cut -d: -f2)
    BODY=$(echo "$RESPONSE" | sed 's/HTTP_CODE:[0-9]*$//')
    
    if [ "$HTTP_CODE" = "200" ]; then
        echo -e "  ${GREEN}✅ Success (200)${NC}"
        # Show token usage if available
        if echo "$BODY" | jq -e '.usage.total_tokens' >/dev/null 2>&1; then
            TOKENS=$(echo "$BODY" | jq -r '.usage.total_tokens')
            echo -e "  📊 Tokens used: $TOKENS"
        fi
        SUCCESS_COUNT=$((SUCCESS_COUNT + 1))
    elif [ "$HTTP_CODE" = "429" ]; then
        echo -e "  ${RED}🚫 Rate Limited (429)${NC}"
        echo -e "  📋 Response: Too Many Requests"
        RATE_LIMITED_COUNT=$((RATE_LIMITED_COUNT + 1))
    else
        echo -e "  ${YELLOW}⚠️  Other (${HTTP_CODE})${NC}"
        echo -e "  📋 Response: $BODY"
    fi
    
    echo ""
    sleep 0.5  # Brief pause between requests
done

# Summary
echo -e "${BLUE}📊 Test Results Summary:${NC}"
echo "========================"
echo -e "✅ Successful requests: ${GREEN}$SUCCESS_COUNT${NC}"
echo -e "🚫 Rate limited requests: ${RED}$RATE_LIMITED_COUNT${NC}"
echo -e "📝 Expected: ~5 success, ~3 rate limited"
echo ""

if [ $SUCCESS_COUNT -ge 4 ] && [ $SUCCESS_COUNT -le 6 ] && [ $RATE_LIMITED_COUNT -ge 2 ]; then
    echo -e "${GREEN}🎉 Rate limiting test PASSED!${NC}"
    echo "Rate limiting is working as expected"
else
    echo -e "${YELLOW}⚠️  Unexpected results - may need investigation${NC}"
    echo "This could indicate policy changes or system issues"
fi

echo -e "\n${BLUE}✅ Rate limit test completed${NC}"
echo "You can run this script multiple times (with 2+ minute gaps)"