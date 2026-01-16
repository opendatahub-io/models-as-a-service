#!/usr/bin/env bash

# Test script for token rate limiting
# Free tier: 100 tokens per 1 minute
# This script waits for reset, then tests the token rate limiting functionality

set -euo pipefail

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${BLUE}🪙 MaaS Token Rate Limiting Test${NC}"
echo "==================================="
echo "Free tier limits: 100 tokens per 1 minute"
echo ""

# Ask user if they want to wait for token rate limit reset
echo -e "${YELLOW}⚠️  Token rate limits may be active from previous tests${NC}"
echo "Free tier: 100 tokens per 1 minute"
echo ""
read -p "Wait 70 seconds for token rate limit reset? (y/N): " -n 1 -r
echo

if [[ $REPLY =~ ^[Yy]$ ]]; then
    echo -e "${YELLOW}⏳ Waiting 70 seconds for token rate limit reset...${NC}"
    for i in {70..1}; do
        printf "\r⏱️  Countdown: %d seconds remaining..." $i
        sleep 1
    done
    echo -e "\n✅ Reset period complete"
else
    echo -e "${BLUE}ℹ️  Proceeding without reset - some requests may fail due to existing token limits${NC}"
fi

# Create fresh token
echo -e "\n${YELLOW}📝 Creating fresh free-user token...${NC}"
TOKEN=$(kubectl create token free-user -n maas-api --duration=1h --audience=maas-default-gateway-sa)
echo "✅ Token created"

echo -e "\n${YELLOW}🧪 Testing token rate limits...${NC}"
echo "Strategy: Send requests with progressively longer prompts to accumulate tokens"
echo "URL: http://localhost/llm/model-a/v1/chat/completions"
echo ""

# Test with progressively larger prompts to hit token limits
SUCCESS_COUNT=0
RATE_LIMITED_COUNT=0
TOTAL_TOKENS_USED=0

# Prompts designed to consume different amounts of tokens
PROMPTS=(
    "Hi"  # ~5 tokens response
    "Hello, how are you today?"  # ~15 tokens response  
    "Please write a short story about a cat"  # ~30 tokens response
    "Explain the concept of machine learning in detail"  # ~50 tokens response
    "Write a comprehensive analysis of artificial intelligence and its impact on society"  # ~80+ tokens response
    "Tell me everything about quantum computing"  # Should exceed limit
    "Another request"  # Should be blocked
)

for i in "${!PROMPTS[@]}"; do
    REQUEST_NUM=$((i + 1))
    PROMPT="${PROMPTS[$i]}"
    
    echo -e "${BLUE}Request $REQUEST_NUM:${NC}"
    echo -e "  💬 Prompt: \"$PROMPT\""
    
    RESPONSE=$(curl -s -w "HTTP_CODE:%{http_code}" \
        -H "Authorization: Bearer $TOKEN" \
        -H "Content-Type: application/json" \
        -d "{\"model\":\"model-a\",\"messages\":[{\"role\":\"user\",\"content\":\"$PROMPT\"}]}" \
        http://localhost/llm/model-a/v1/chat/completions)
    
    HTTP_CODE=$(echo "$RESPONSE" | grep -o "HTTP_CODE:[0-9]*" | cut -d: -f2)
    BODY=$(echo "$RESPONSE" | sed 's/HTTP_CODE:[0-9]*$//')
    
    if [ "$HTTP_CODE" = "200" ]; then
        echo -e "  ${GREEN}✅ Success (200)${NC}"
        
        # Extract and display token usage
        if echo "$BODY" | jq -e '.usage.total_tokens' >/dev/null 2>&1; then
            TOKENS=$(echo "$BODY" | jq -r '.usage.total_tokens')
            TOTAL_TOKENS_USED=$((TOTAL_TOKENS_USED + TOKENS))
            echo -e "  📊 Tokens this request: $TOKENS"
            echo -e "  📈 Total tokens used: $TOTAL_TOKENS_USED"
            
            # Show preview of response
            CONTENT=$(echo "$BODY" | jq -r '.choices[0].message.content' | head -c 100)
            echo -e "  💭 Response preview: \"${CONTENT}...\""
        fi
        
        SUCCESS_COUNT=$((SUCCESS_COUNT + 1))
        
        # Check if we're approaching the limit
        if [ $TOTAL_TOKENS_USED -gt 80 ]; then
            echo -e "  ${YELLOW}⚠️  Approaching token limit (100)${NC}"
        fi
        
    elif [ "$HTTP_CODE" = "429" ]; then
        echo -e "  ${RED}🚫 Token Rate Limited (429)${NC}"
        echo -e "  📋 Response: Too Many Requests (token limit exceeded)"
        echo -e "  📊 Total tokens consumed before limit: $TOTAL_TOKENS_USED"
        RATE_LIMITED_COUNT=$((RATE_LIMITED_COUNT + 1))
    else
        echo -e "  ${YELLOW}⚠️  Other (${HTTP_CODE})${NC}"
        echo -e "  📋 Response: $(echo "$BODY" | head -c 200)"
    fi
    
    echo ""
    sleep 1  # Brief pause between requests
done

# Summary
echo -e "${BLUE}📊 Test Results Summary:${NC}"
echo "========================="
echo -e "✅ Successful requests: ${GREEN}$SUCCESS_COUNT${NC}"
echo -e "🚫 Token rate limited requests: ${RED}$RATE_LIMITED_COUNT${NC}"
echo -e "📊 Total tokens consumed: ${YELLOW}$TOTAL_TOKENS_USED${NC}"
echo -e "📝 Token limit: 100 per minute"
echo ""

if [ $TOTAL_TOKENS_USED -gt 80 ] && [ $RATE_LIMITED_COUNT -gt 0 ]; then
    echo -e "${GREEN}🎉 Token rate limiting test PASSED!${NC}"
    echo "Token rate limiting is working as expected"
    echo "Limit was hit around $TOTAL_TOKENS_USED tokens (under 100 limit)"
elif [ $TOTAL_TOKENS_USED -lt 100 ] && [ $RATE_LIMITED_COUNT -eq 0 ]; then
    echo -e "${YELLOW}⚠️  Test completed without hitting token limit${NC}"
    echo "This might indicate the responses are shorter than expected"
    echo "Or the token counting might be different than assumed"
else
    echo -e "${YELLOW}⚠️  Unexpected results - may need investigation${NC}"
    echo "This could indicate policy changes or system issues"
fi

echo -e "\n${BLUE}✅ Token rate limit test completed${NC}"
echo "Wait 60+ seconds before running again to test multiple times"