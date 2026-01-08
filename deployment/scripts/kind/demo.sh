#!/usr/bin/env bash

# Interactive demo for Kind local deployment
# Shows: Gateway connectivity, Authentication, Rate Limiting, LLM Inference

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

BASE_URL="http://localhost"

# Helper functions
print_header() {
    echo -e "\n${BLUE}========================================${NC}"
    echo -e "${BLUE}$1${NC}"
    echo -e "${BLUE}========================================${NC}\n"
}

print_step() {
    echo -e "${CYAN}>>> $1${NC}"
}

print_success() {
    echo -e "${GREEN}✓ $1${NC}"
}

print_error() {
    echo -e "${RED}✗ $1${NC}"
}

print_warning() {
    echo -e "${YELLOW}⚠ $1${NC}"
}

wait_for_enter() {
    echo -e "\n${YELLOW}Press Enter to continue...${NC}"
    read
}

# ==============================================================================
# Main Menu
# ==============================================================================
show_menu() {
    clear
    echo -e "${BLUE}╔════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${BLUE}║  MaaS Platform - Kind Local Development Demo              ║${NC}"
    echo -e "${BLUE}╚════════════════════════════════════════════════════════════╝${NC}\n"

    echo "Select a demo to run:"
    echo ""
    echo "  1) Quick Start - Basic connectivity and inference"
    echo "  2) Authentication & Authorization - 3-tier access control"
    echo "  3) Rate Limiting - Request and token-based limits"
    echo "  4) Full Demo - All features (interactive)"
    echo "  5) Exit"
    echo ""
    echo -n "Enter choice [1-5]: "
}

# ==============================================================================
# Demo 1: Quick Start
# ==============================================================================
demo_quick_start() {
    print_header "Demo 1: Quick Start - Basic Connectivity"

    echo "This demo shows:"
    echo "  • Gateway accessibility on localhost:80"
    echo "  • Model listing"
    echo "  • Simple LLM inference"
    echo ""
    wait_for_enter

    # Setup
    print_step "Setting up test user..."
    kubectl create sa demo-user -n maas-api 2>/dev/null || true
    kubectl create clusterrole llm-model-access --verb=get,list,create --resource=llminferenceservices 2>/dev/null || true
    kubectl create clusterrolebinding demo-user-llm-access --clusterrole=llm-model-access --serviceaccount=maas-api:demo-user 2>/dev/null || true
    TOKEN=$(kubectl create token demo-user -n maas-api --duration=1h --audience=maas-default-gateway-sa)
    print_success "Demo user created"
    echo ""
    wait_for_enter

    # Test 1: List models
    print_step "Listing available models..."
    response=$(curl -s -H "Authorization: Bearer $TOKEN" ${BASE_URL}/v1/models)
    echo "$response" | jq '.'
    wait_for_enter

    # Test 2: Simple inference
    print_step "Making a simple inference request..."
    response=$(curl -s \
        -H "Authorization: Bearer $TOKEN" \
        -H "Content-Type: application/json" \
        -d '{"model":"model-a","messages":[{"role":"user","content":"Say hello!"}],"max_tokens":20}' \
        ${BASE_URL}/llm/model-a/v1/chat/completions)

    if echo "$response" | jq -e '.choices[0].message.content' &>/dev/null; then
        print_success "Inference successful!"
        echo ""
        echo "Response:"
        echo "$response" | jq -r '.choices[0].message.content'
    else
        print_error "Inference failed"
        echo "$response" | jq '.'
    fi

    wait_for_enter
}

# ==============================================================================
# Demo 2: Authentication & Authorization
# ==============================================================================
demo_auth() {
    print_header "Demo 2: Authentication & Authorization"

    echo "This demo shows:"
    echo "  • Unauthenticated requests → 401 Unauthorized"
    echo "  • Service account token authentication"
    echo "  • RBAC-based authorization"
    echo "  • 3-tier access control (Free/Premium/Enterprise)"
    echo "  • Tier-based model access (model-b restricted to premium+ tiers)"
    echo ""
    wait_for_enter

    # Test 1: Unauthenticated
    print_step "Test 1: Unauthenticated request (should fail)"
    response=$(curl -s -w "\n%{http_code}" ${BASE_URL}/v1/models 2>/dev/null)
    http_code=$(echo "$response" | tail -n 1)

    if [ "$http_code" == "401" ] || [ "$http_code" == "403" ]; then
        print_success "Correctly rejected with $http_code"
    else
        print_warning "Unexpected response: $http_code"
    fi
    wait_for_enter

    # Test 2: Create users
    print_step "Test 2: Creating 3-tier users (Free, Premium, Enterprise)"
    kubectl create sa free-user -n maas-api 2>/dev/null || echo "  free-user exists"
    kubectl create sa premium-user -n maas-api 2>/dev/null || echo "  premium-user exists"
    kubectl create sa enterprise-user -n maas-api 2>/dev/null || echo "  enterprise-user exists"

    print_step "Granting RBAC permissions..."
    kubectl create clusterrole llm-model-access --verb=get,list,create --resource=llminferenceservices 2>/dev/null || true
    kubectl create clusterrolebinding free-user-llm-access --clusterrole=llm-model-access --serviceaccount=maas-api:free-user 2>/dev/null || true
    kubectl create clusterrolebinding premium-user-llm-access --clusterrole=llm-model-access --serviceaccount=maas-api:premium-user 2>/dev/null || true
    kubectl create clusterrolebinding enterprise-user-llm-access --clusterrole=llm-model-access --serviceaccount=maas-api:enterprise-user 2>/dev/null || true

    FREE_TOKEN=$(kubectl create token free-user -n maas-api --duration=1h --audience=maas-default-gateway-sa)
    PREMIUM_TOKEN=$(kubectl create token premium-user -n maas-api --duration=1h --audience=maas-default-gateway-sa)
    ENTERPRISE_TOKEN=$(kubectl create token enterprise-user -n maas-api --duration=1h --audience=maas-default-gateway-sa)

    print_success "Users created with proper RBAC"
    echo ""
    echo "Tokens generated:"
    echo "  Free:       ${FREE_TOKEN:0:30}..."
    echo "  Premium:    ${PREMIUM_TOKEN:0:30}..."
    echo "  Enterprise: ${ENTERPRISE_TOKEN:0:30}..."
    wait_for_enter

    # Test 3: Authenticated access
    print_step "Test 3: Authenticated requests (should succeed)"

    for tier in "Free" "Premium" "Enterprise"; do
        case $tier in
            "Free") TOKEN=$FREE_TOKEN ;;
            "Premium") TOKEN=$PREMIUM_TOKEN ;;
            "Enterprise") TOKEN=$ENTERPRISE_TOKEN ;;
        esac

        http_code=$(curl -s -o /dev/null -w "%{http_code}" \
            -H "Authorization: Bearer $TOKEN" \
            ${BASE_URL}/v1/models)

        if [ "$http_code" == "200" ]; then
            print_success "$tier user: Access granted (200)"
        else
            print_error "$tier user: Access denied ($http_code)"
        fi
    done

    wait_for_enter

    # Test 4: Model discovery
    print_step "Test 4: Model Discovery - Listing KServe InferenceServices"
    echo ""
    echo "Listing available models through /v1/models endpoint:"
    echo ""

    response=$(curl -s -H "Authorization: Bearer $FREE_TOKEN" ${BASE_URL}/v1/models)

    if echo "$response" | jq -e '.data' &>/dev/null; then
        echo "$response" | jq -r '.data[] | "  - \(.id) (owned by: \(.owned_by), ready: \(.ready))"'
        print_success "Successfully retrieved model list"
        echo ""
        echo "Note: model-a and model-b are KServe InferenceServices."
        echo "The /v1/models endpoint discovers all InferenceServices across namespaces."
    else
        print_error "Failed to retrieve models"
        echo "$response" | jq '.' 2>/dev/null || echo "$response"
    fi

    wait_for_enter
}

# ==============================================================================
# Demo 3: Rate Limiting
# ==============================================================================
demo_rate_limiting() {
    print_header "Demo 3: Rate Limiting"

    echo "This demo shows:"
    echo "  • Request-based rate limits (5 req/2min for Free tier)"
    echo "  • Token-based rate limits (100 tokens/min for Free tier)"
    echo "  • 429 Too Many Requests responses"
    echo ""
    wait_for_enter

    # Setup
    print_step "Setting up Free tier user..."
    kubectl create sa free-user -n maas-api 2>/dev/null || true
    kubectl create clusterrole llm-model-access --verb=get,list,create --resource=llminferenceservices 2>/dev/null || true
    kubectl create clusterrolebinding free-user-llm-access --clusterrole=llm-model-access --serviceaccount=maas-api:free-user 2>/dev/null || true
    FREE_TOKEN=$(kubectl create token free-user -n maas-api --duration=1h --audience=maas-default-gateway-sa)
    print_success "Free tier user ready"
    echo ""
    wait_for_enter

    # Test 1: Request rate limiting
    print_step "Test 1: Request-based rate limiting (Free tier: 5 req/2min)"
    echo "Sending 7 requests rapidly..."
    echo ""

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
            echo -e "  Request $i: ${GREEN}✓ Success (200)${NC}"
        elif [ "$http_code" == "429" ]; then
            ((rate_limited_count++))
            echo -e "  Request $i: ${RED}✗ Rate Limited (429)${NC}"
        else
            echo -e "  Request $i: ${YELLOW}? Unexpected ($http_code)${NC}"
        fi
        sleep 0.2
    done

    echo ""
    echo "Summary:"
    echo "  Successful: $success_count"
    echo "  Rate Limited: $rate_limited_count"

    if [ $rate_limited_count -gt 0 ]; then
        print_success "Rate limiting is working! (limit: 5 req/2min)"
    else
        print_warning "No rate limits hit"
    fi
    wait_for_enter

    # Test 2: Token-based rate limiting
    print_step "Test 2: Token-based rate limiting (Free tier: 100 tokens/min)"
    echo "Requesting 150 tokens (should exceed limit)..."
    echo ""

    http_code=$(curl -s -o /dev/null -w "%{http_code}" \
        -H "Authorization: Bearer $FREE_TOKEN" \
        -H "Content-Type: application/json" \
        -d '{"model":"model-a","messages":[{"role":"user","content":"Write a long response"}],"max_tokens":150}' \
        ${BASE_URL}/llm/model-a/v1/chat/completions)

    if [ "$http_code" == "429" ]; then
        print_success "Token limit enforced (429)"
    else
        print_warning "Unexpected response: $http_code"
    fi

    wait_for_enter
}

# ==============================================================================
# Demo 4: Full Demo
# ==============================================================================
demo_full() {
    print_header "Full Demo - All Features"

    echo "This comprehensive demo includes:"
    echo "  1. Basic connectivity"
    echo "  2. Authentication & Authorization"
    echo "  3. Rate Limiting"
    echo "  4. LLM Inference"
    echo ""
    echo "Press Enter to start..."
    read

    demo_quick_start
    demo_auth
    demo_rate_limiting

    print_header "Demo Complete!"
    echo "You've seen:"
    echo "  ✓ Gateway connectivity via localhost:80"
    echo "  ✓ Kubernetes service account authentication"
    echo "  ✓ RBAC-based authorization"
    echo "  ✓ Request and token-based rate limiting"
    echo "  ✓ LLM inference with model-a model"
    echo ""
    wait_for_enter
}

# ==============================================================================
# Main Loop
# ==============================================================================
while true; do
    show_menu
    read choice

    case $choice in
        1) demo_quick_start ;;
        2) demo_auth ;;
        3) demo_rate_limiting ;;
        4) demo_full ;;
        5) echo "Goodbye!"; exit 0 ;;
        *) echo "Invalid choice. Please select 1-5." ;;
    esac
done
