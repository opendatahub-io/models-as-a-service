#!/usr/bin/env bash

# Automated test suite for Kind local deployment
# Tests: Basic connectivity, Auth, Rate Limiting, LLM Inference

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

# Configuration
BASE_URL="http://localhost"

# Test counters
TESTS_RUN=0
TESTS_PASSED=0
TESTS_FAILED=0

# Helper functions
print_header() {
    echo -e "\n${BLUE}================================================${NC}"
    echo -e "${BLUE}$1${NC}"
    echo -e "${BLUE}================================================${NC}\n"
}

print_test() {
    echo -e "${YELLOW}TEST: $1${NC}"
}

pass() {
    ((TESTS_PASSED++))
    ((TESTS_RUN++))
    echo -e "${GREEN}✅ PASS: $1${NC}"
}

fail() {
    ((TESTS_FAILED++))
    ((TESTS_RUN++))
    echo -e "${RED}❌ FAIL: $1${NC}"
}

# ==============================================================================
# Test Suite
# ==============================================================================

print_header "MaaS Kind Deployment - Automated Test Suite"

# ==============================================================================
# Setup: Create test users and RBAC
# ==============================================================================
print_header "Test Setup"

echo "Creating test service accounts..."
kubectl create sa free-user -n maas-api 2>/dev/null || true
kubectl create sa premium-user -n maas-api 2>/dev/null || true
kubectl create sa enterprise-user -n maas-api 2>/dev/null || true

echo "Setting up RBAC permissions..."
kubectl create clusterrole llm-model-access --verb=get,list,create --resource=llminferenceservices 2>/dev/null || true
kubectl create clusterrolebinding free-user-llm-access --clusterrole=llm-model-access --serviceaccount=maas-api:free-user 2>/dev/null || true
kubectl create clusterrolebinding premium-user-llm-access --clusterrole=llm-model-access --serviceaccount=maas-api:premium-user 2>/dev/null || true
kubectl create clusterrolebinding enterprise-user-llm-access --clusterrole=llm-model-access --serviceaccount=maas-api:enterprise-user 2>/dev/null || true

echo "Generating tokens..."
FREE_TOKEN=$(kubectl create token free-user -n maas-api --duration=1h --audience=maas-default-gateway-sa)
PREMIUM_TOKEN=$(kubectl create token premium-user -n maas-api --duration=1h --audience=maas-default-gateway-sa)
ENTERPRISE_TOKEN=$(kubectl create token enterprise-user -n maas-api --duration=1h --audience=maas-default-gateway-sa)

echo -e "${GREEN}✓ Setup complete${NC}\n"

# ==============================================================================
# Test 1: Basic Connectivity
# ==============================================================================
print_header "Test 1: Basic Connectivity"

print_test "Gateway is accessible"
if curl -s -o /dev/null -w "%{http_code}" ${BASE_URL}/v1/models | grep -qE "401|403|200"; then
    pass "Gateway responding"
else
    fail "Gateway not accessible"
fi

# ==============================================================================
# Test 2: Authentication
# ==============================================================================
print_header "Test 2: Authentication"

print_test "Unauthenticated request should fail (401/403)"
http_code=$(curl -s -o /dev/null -w "%{http_code}" ${BASE_URL}/v1/models)
if [ "$http_code" == "401" ] || [ "$http_code" == "403" ]; then
    pass "Unauthenticated request rejected ($http_code)"
else
    fail "Expected 401/403, got $http_code"
fi

print_test "Authenticated request should succeed (200)"
http_code=$(curl -s -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $FREE_TOKEN" ${BASE_URL}/v1/models)
if [ "$http_code" == "200" ]; then
    pass "Authenticated request accepted"
else
    fail "Expected 200, got $http_code"
fi

# ==============================================================================
# Test 3: Request Rate Limiting
# ==============================================================================
print_header "Test 3: Request Rate Limiting"

print_test "Free tier rate limit (5 req/2min)"
success_count=0
rate_limited_count=0

for i in {1..7}; do
    http_code=$(curl -s -o /dev/null -w "%{http_code}" \
        -H "Authorization: Bearer $FREE_TOKEN" \
        -H "Content-Type: application/json" \
        -d '{"model":"model-a","messages":[{"role":"user","content":"test"}],"max_tokens":5}' \
        ${BASE_URL}/llm/model-a/v1/chat/completions)
    
    if [ "$http_code" == "200" ]; then
        ((success_count++))
    elif [ "$http_code" == "429" ]; then
        ((rate_limited_count++))
    fi
    sleep 0.2
done

if [ $rate_limited_count -gt 0 ] && [ $success_count -le 5 ]; then
    pass "Rate limiting enforced ($success_count succeeded, $rate_limited_count blocked)"
else
    fail "Rate limiting not working ($success_count succeeded, $rate_limited_count blocked)"
fi

# ==============================================================================
# Test 4: Token-based Rate Limiting
# ==============================================================================
print_header "Test 4: Token-based Rate Limiting"

print_test "Free tier token limit (100 tokens/min)"
http_code=$(curl -s -o /dev/null -w "%{http_code}" \
    -H "Authorization: Bearer $FREE_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"model":"model-a","messages":[{"role":"user","content":"Write a long response"}],"max_tokens":150}' \
    ${BASE_URL}/llm/model-a/v1/chat/completions)

if [ "$http_code" == "429" ]; then
    pass "Token-based rate limiting enforced"
elif [ "$http_code" == "200" ]; then
    fail "Token limit not enforced (request succeeded)"
else
    fail "Unexpected response: $http_code"
fi

# ==============================================================================
# Test 5: LLM Inference
# ==============================================================================
print_header "Test 5: LLM Inference"

print_test "Premium user can make inference request"
response=$(curl -s \
    -H "Authorization: Bearer $PREMIUM_TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"model":"model-a","messages":[{"role":"user","content":"Hello"}],"max_tokens":10}' \
    ${BASE_URL}/llm/model-a/v1/chat/completions)

if echo "$response" | jq -e '.choices[0].message.content' &>/dev/null; then
    pass "LLM inference successful"
else
    fail "LLM inference failed"
fi

# ==============================================================================
# Test Summary
# ==============================================================================
print_header "Test Summary"

echo -e "Tests Run:    ${BLUE}${TESTS_RUN}${NC}"
echo -e "Tests Passed: ${GREEN}${TESTS_PASSED}${NC}"
echo -e "Tests Failed: ${RED}${TESTS_FAILED}${NC}"
echo ""

if [ $TESTS_FAILED -eq 0 ]; then
    echo -e "${GREEN}✅ All tests passed!${NC}"
    exit 0
else
    echo -e "${RED}❌ Some tests failed${NC}"
    exit 1
fi
