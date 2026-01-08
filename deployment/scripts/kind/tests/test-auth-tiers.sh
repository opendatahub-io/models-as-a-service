#!/usr/bin/env bash

# Auth tiers test script
# Tests access control: free user vs premium user across both models

set -euo pipefail

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
BLUE='\033[0;34m'
YELLOW='\033[1;33m'
PURPLE='\033[0;35m'
NC='\033[0m' # No Color

echo -e "${BLUE}🔐 MaaS Authentication & Authorization Test${NC}"
echo "=============================================="
echo "Testing access control across user tiers and models"
echo ""
echo -e "${PURPLE}Model Tiers:${NC}"
echo "  • model-a: free tier (accessible to all users)"
echo "  • model-b: premium tier (requires premium/enterprise access)"
echo ""
echo -e "${PURPLE}User Tiers:${NC}"
echo "  • free-user: Has access to free tier models only"
echo "  • premium-user: Has access to all models"
echo ""

# Function to test model access
test_model_access() {
    local user_type="$1"
    local token="$2" 
    local model="$3"
    local model_tier="$4"
    local expected_result="$5"  # "success" or "forbidden"
    
    echo -e "${BLUE}🧪 Testing: ${user_type} → ${model} (${model_tier})${NC}"
    
    local response
    local http_code
    
    response=$(curl -s -w "HTTP_CODE:%{http_code}" \
        -H "Authorization: Bearer $token" \
        -H "Content-Type: application/json" \
        -d "{\"model\":\"$model\",\"messages\":[{\"role\":\"user\",\"content\":\"Hello from $user_type\"}],\"max_tokens\":10}" \
        "http://localhost/llm/$model/v1/chat/completions")
    
    http_code=$(echo "$response" | sed -n 's/.*HTTP_CODE:\([0-9]*\).*/\1/p')
    response_body=$(echo "$response" | sed 's/HTTP_CODE:[0-9]*$//')
    
    case $http_code in
        200)
            if [ "$expected_result" = "success" ]; then
                echo -e "  ${GREEN}✅ SUCCESS${NC} - Access granted (200)"
                if echo "$response_body" | jq -e '.choices[0].message.content' > /dev/null 2>&1; then
                    local content=$(echo "$response_body" | jq -r '.choices[0].message.content' | head -c 50)
                    echo -e "  ${GREEN}💬 Response:${NC} \"$content...\""
                else
                    echo -e "  ${YELLOW}⚠️  Response format unexpected${NC}"
                fi
            else
                echo -e "  ${RED}❌ UNEXPECTED${NC} - Should have been denied but got 200"
                echo -e "  ${RED}🔍 This indicates a security issue!${NC}"
            fi
            ;;
        401)
            if [ "$expected_result" = "forbidden" ]; then
                echo -e "  ${GREEN}✅ BLOCKED${NC} - Access denied (401) ✓"
                echo -e "  ${GREEN}🔒 Authorization working correctly${NC}"
            else
                echo -e "  ${RED}❌ UNEXPECTED${NC} - Should have succeeded but got 401"
            fi
            ;;
        403)
            if [ "$expected_result" = "forbidden" ]; then
                echo -e "  ${GREEN}✅ BLOCKED${NC} - Access forbidden (403) ✓"
                echo -e "  ${GREEN}🔒 Authorization working correctly${NC}"
            else
                echo -e "  ${RED}❌ UNEXPECTED${NC} - Should have succeeded but got 403"
            fi
            ;;
        429)
            echo -e "  ${YELLOW}⚠️  RATE LIMITED${NC} - Too many requests (429)"
            echo -e "  ${YELLOW}🔄 Try again later or this indicates previous test traffic${NC}"
            ;;
        500)
            echo -e "  ${RED}❌ SERVER ERROR${NC} - Internal error (500)"
            echo -e "  ${RED}🐛 This indicates a system issue${NC}"
            ;;
        *)
            echo -e "  ${RED}❌ UNEXPECTED${NC} - HTTP $http_code"
            echo -e "  ${RED}📝 Response: $response_body${NC}"
            ;;
    esac
    echo ""
}

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# Create tokens
echo -e "${YELLOW}📝 Creating user tokens...${NC}"

echo "  Creating free-user token..."
FREE_TOKEN=$(kubectl create token free-user -n maas-api --audience=maas-default-gateway-sa --duration=1h 2>/dev/null)
if [ -z "$FREE_TOKEN" ]; then
    echo -e "${RED}❌ Failed to create free-user token${NC}"
    exit 1
fi

echo "  Creating premium-user token..."  
PREMIUM_TOKEN=$(kubectl create token premium-user -n maas-api --audience=maas-default-gateway-sa --duration=1h 2>/dev/null)
if [ -z "$PREMIUM_TOKEN" ]; then
    echo -e "${RED}❌ Failed to create premium-user token${NC}"
    exit 1
fi

echo -e "${GREEN}✅ Both tokens created successfully${NC}"
echo ""

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo -e "${YELLOW}🧪 TESTING PHASE 1: Free User Access${NC}"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# Test free user access
test_model_access "free-user" "$FREE_TOKEN" "model-a" "free tier" "success"
test_model_access "free-user" "$FREE_TOKEN" "model-b" "premium tier" "forbidden"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo -e "${YELLOW}🧪 TESTING PHASE 2: Premium User Access${NC}"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# Test premium user access  
test_model_access "premium-user" "$PREMIUM_TOKEN" "model-a" "free tier" "success"
test_model_access "premium-user" "$PREMIUM_TOKEN" "model-b" "premium tier" "success"

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo -e "${YELLOW}🧪 TESTING PHASE 3: Model Discovery${NC}"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

# Test model discovery for both users
echo -e "${BLUE}🔍 Testing model discovery with free-user${NC}"
FREE_MODELS=$(curl -s -H "Authorization: Bearer $FREE_TOKEN" http://localhost/v1/models 2>/dev/null)
if echo "$FREE_MODELS" | jq -e '.data' > /dev/null 2>&1; then
    echo -e "${GREEN}✅ Model discovery successful${NC}"
    echo "Available models for free-user:"
    echo "$FREE_MODELS" | jq -r '.data[] | "  • \(.id) (ready: \(.ready // "unknown"))"'
else
    echo -e "${RED}❌ Model discovery failed for free-user${NC}"
fi
echo ""

echo -e "${BLUE}🔍 Testing model discovery with premium-user${NC}"
PREMIUM_MODELS=$(curl -s -H "Authorization: Bearer $PREMIUM_TOKEN" http://localhost/v1/models 2>/dev/null)
if echo "$PREMIUM_MODELS" | jq -e '.data' > /dev/null 2>&1; then
    echo -e "${GREEN}✅ Model discovery successful${NC}"
    echo "Available models for premium-user:"
    echo "$PREMIUM_MODELS" | jq -r '.data[] | "  • \(.id) (ready: \(.ready // "unknown"))"'
else
    echo -e "${RED}❌ Model discovery failed for premium-user${NC}"
fi
echo ""

echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo -e "${GREEN}📊 TEST SUMMARY${NC}"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""
echo -e "${BLUE}Expected Results:${NC}"
echo "  ✅ free-user → model-a: SUCCESS (access granted)"
echo "  ✅ free-user → model-b: BLOCKED (access denied)" 
echo "  ✅ premium-user → model-a: SUCCESS (access granted)"
echo "  ✅ premium-user → model-b: SUCCESS (access granted)"
echo ""
echo -e "${GREEN}🔒 Authentication & Authorization Test Complete!${NC}"
echo ""
echo -e "${YELLOW}💡 Notes:${NC}"
echo "  • This test validates the tier-based access control system"
echo "  • Free users should only access free-tier models"
echo "  • Premium users should access all models"
echo "  • Any unexpected results indicate security policy issues"