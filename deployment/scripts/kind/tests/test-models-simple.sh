#!/usr/bin/env bash

# Simple model test script
# Tests both model-a and model-b with basic chat completion requests

set -euo pipefail

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${BLUE}🤖 MaaS Simple Model Test${NC}"
echo "==========================="
echo "Testing both model-a (free tier) and model-b (premium tier)"
echo ""

# Create premium token (has access to both models)
echo -e "${YELLOW}📝 Creating premium user token...${NC}"
TOKEN=$(kubectl create token premium-user -n maas-api --audience=maas-default-gateway-sa --duration=1h)
if [ -z "$TOKEN" ]; then
    echo -e "${RED}❌ Failed to create token${NC}"
    exit 1
fi
echo -e "${GREEN}✅ Token created successfully${NC}"
echo ""

# Test model-a (free tier)
echo -e "${BLUE}🧪 Testing model-a (free tier)${NC}"
echo "Sending message: 'Hello, how are you?'"
echo ""

RESPONSE_A=$(curl -s -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"model":"model-a","messages":[{"role":"user","content":"Hello, how are you?"}],"max_tokens":50}' \
    http://localhost/llm/model-a/v1/chat/completions)

if echo "$RESPONSE_A" | jq -e '.choices[0].message.content' > /dev/null 2>&1; then
    echo -e "${GREEN}✅ model-a response:${NC}"
    echo "$RESPONSE_A" | jq -r '.choices[0].message.content'
    echo ""
    echo -e "${BLUE}📊 model-a usage:${NC}"
    echo "  Prompt tokens: $(echo "$RESPONSE_A" | jq -r '.usage.prompt_tokens')"
    echo "  Completion tokens: $(echo "$RESPONSE_A" | jq -r '.usage.completion_tokens')"
    echo "  Total tokens: $(echo "$RESPONSE_A" | jq -r '.usage.total_tokens')"
else
    echo -e "${RED}❌ model-a failed or returned unexpected response:${NC}"
    echo "$RESPONSE_A"
fi

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# Test model-b (premium tier)
echo -e "${BLUE}🧪 Testing model-b (premium tier)${NC}"
echo "Sending message: 'Tell me a short joke'"
echo ""

RESPONSE_B=$(curl -s -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"model":"model-b","messages":[{"role":"user","content":"Tell me a short joke"}],"max_tokens":50}' \
    http://localhost/llm/model-b/v1/chat/completions)

if echo "$RESPONSE_B" | jq -e '.choices[0].message.content' > /dev/null 2>&1; then
    echo -e "${GREEN}✅ model-b response:${NC}"
    echo "$RESPONSE_B" | jq -r '.choices[0].message.content'
    echo ""
    echo -e "${BLUE}📊 model-b usage:${NC}"
    echo "  Prompt tokens: $(echo "$RESPONSE_B" | jq -r '.usage.prompt_tokens')"
    echo "  Completion tokens: $(echo "$RESPONSE_B" | jq -r '.usage.completion_tokens')"
    echo "  Total tokens: $(echo "$RESPONSE_B" | jq -r '.usage.total_tokens')"
else
    echo -e "${RED}❌ model-b failed or returned unexpected response:${NC}"
    echo "$RESPONSE_B"
fi

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# Test model discovery
echo -e "${BLUE}🔍 Testing model discovery${NC}"
MODELS_RESPONSE=$(curl -s -H "Authorization: Bearer $TOKEN" http://localhost/v1/models)

if echo "$MODELS_RESPONSE" | jq -e '.data' > /dev/null 2>&1; then
    echo -e "${GREEN}✅ Available models:${NC}"
    echo "$MODELS_RESPONSE" | jq -r '.data[] | "  \(.id) (ready: \(.ready // "unknown"))"'
else
    echo -e "${RED}❌ Failed to list models:${NC}"
    echo "$MODELS_RESPONSE"
fi

echo ""
echo -e "${GREEN}🎉 Test completed!${NC}"
echo ""
echo "💡 Tips:"
echo "  - Both models use the same simulator but different configurations"
echo "  - model-a: free tier (accessible to all users)"
echo "  - model-b: premium tier (requires premium/enterprise access)"
echo "  - Responses are simulated with random content"