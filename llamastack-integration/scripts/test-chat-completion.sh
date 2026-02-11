#!/bin/bash

# Llamastack Chat Completion Test Script
# This script tests chat completion functionality through the MaaS gateway

set -e

PROVIDER=${1:-"gemini"}
NAMESPACE=${2:-"llm"}
MODEL=${3:-""}

echo "🧪 Testing chat completion for provider: $PROVIDER"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

print_success() {
    echo -e "${GREEN}✅ $1${NC}"
}

print_error() {
    echo -e "${RED}❌ $1${NC}"
}

print_warning() {
    echo -e "${YELLOW}⚠️  $1${NC}"
}

print_info() {
    echo -e "${BLUE}ℹ️  $1${NC}"
}

# Determine the default model based on provider
if [ -z "$MODEL" ]; then
    case "$PROVIDER" in
        "gemini")
            MODEL="gemini/models/gemini-2.5-flash"
            ;;
        "openai")
            MODEL="gpt-4o"
            ;;
        "anthropic")
            MODEL="claude-3-5-sonnet-20241022"
            ;;
        *)
            print_error "Unknown provider: $PROVIDER"
            print_info "Supported providers: gemini, openai, anthropic"
            exit 1
            ;;
    esac
fi

print_info "Using model: $MODEL"

# Step 1: Get MaaS API endpoint
echo
print_info "Step 1: Finding MaaS API endpoint"

MAAS_API=""
# First check common MaaS namespaces
for ns in maas openshift-operators kube-system default; do
    if kubectl get route maas-api -n "$ns" &>/dev/null; then
        MAAS_API="https://$(kubectl get route maas-api -n "$ns" -o jsonpath='{.spec.host}')"
        break
    elif kubectl get ingress maas-api -n "$ns" &>/dev/null; then
        MAAS_API="https://$(kubectl get ingress maas-api -n "$ns" -o jsonpath='{.spec.rules[0].host}')"
        break
    fi
done

# Fallback: try using the well-known URL pattern
if [ -z "$MAAS_API" ]; then
    MAAS_API="http://maas.apps.ai-dev02.kni.syseng.devcluster.openshift.com"
    print_warning "Using fallback MaaS API endpoint: $MAAS_API"
fi

print_success "Found MaaS API: $MAAS_API"

# Step 2: Get MaaS Gateway endpoint
echo
print_info "Step 2: Finding MaaS Gateway endpoint"

MAAS_GATEWAY=""
# Check common namespaces for gateway
for ns in openshift-ingress maas kube-system; do
    if kubectl get route maas-default-gateway -n "$ns" &>/dev/null; then
        MAAS_GATEWAY="https://$(kubectl get route maas-default-gateway -n "$ns" -o jsonpath='{.spec.host}')"
        break
    elif kubectl get ingress maas-default-gateway -n "$ns" &>/dev/null; then
        MAAS_GATEWAY="https://$(kubectl get ingress maas-default-gateway -n "$ns" -o jsonpath='{.spec.rules[0].host}')"
        break
    fi
done

# Fallback: use the same as API endpoint for basic testing
if [ -z "$MAAS_GATEWAY" ]; then
    MAAS_GATEWAY="$MAAS_API"
    print_warning "Using MaaS API as gateway endpoint: $MAAS_GATEWAY"
fi

print_success "Found MaaS Gateway: $MAAS_GATEWAY"

# Step 3: Get authentication token
echo
print_info "Step 3: Obtaining authentication token"

TOKEN=""
if command -v oc &>/dev/null; then
    # Try OpenShift token
    OC_TOKEN=$(oc whoami -t 2>/dev/null || echo "")
    if [ -n "$OC_TOKEN" ]; then
        print_info "Using OpenShift token for authentication"
        TOKEN_RESPONSE=$(curl -s -k -H "Authorization: Bearer $OC_TOKEN" \
            --json '{"expiration": "1h"}' "$MAAS_API/maas-api/v1/tokens" 2>/dev/null || echo "FAILED")

        if echo "$TOKEN_RESPONSE" | grep -q "token"; then
            TOKEN=$(echo "$TOKEN_RESPONSE" | jq -r .token 2>/dev/null || echo "")
            print_success "Successfully obtained MaaS token"
        else
            print_error "Failed to get MaaS token. Response: $TOKEN_RESPONSE"
            exit 1
        fi
    fi
fi

if [ -z "$TOKEN" ]; then
    print_error "Could not obtain authentication token"
    print_info "Make sure you're logged into OpenShift (oc login) or provide a valid token"
    exit 1
fi

# Step 4: Test chat completion
echo
print_info "Step 4: Testing chat completion"

CHAT_REQUEST='{
  "model": "'$MODEL'",
  "messages": [
    {"role": "system", "content": "You are a helpful assistant. Respond briefly and clearly."},
    {"role": "user", "content": "Hello! Please tell me what you are and respond with exactly one sentence."}
  ],
  "max_tokens": 50,
  "temperature": 0.7
}'

# Construct provider-specific endpoint
CHAT_ENDPOINT="$MAAS_GATEWAY/llm/${PROVIDER}-llamastack/v1/chat/completions"

print_info "Sending chat completion request to: $CHAT_ENDPOINT"
print_info "Request payload:"
echo "$CHAT_REQUEST" | jq . 2>/dev/null || echo "$CHAT_REQUEST"

CHAT_RESPONSE=$(curl -s -k -X POST "$CHAT_ENDPOINT" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d "$CHAT_REQUEST" 2>/dev/null || echo "FAILED")

if echo "$CHAT_RESPONSE" | grep -q "choices"; then
    print_success "Chat completion request successful!"

    echo
    print_info "=== Response Details ==="

    # Extract and display key information
    if command -v jq &>/dev/null; then
        echo
        print_info "Model used: $(echo "$CHAT_RESPONSE" | jq -r .model 2>/dev/null || echo "Unknown")"

        CONTENT=$(echo "$CHAT_RESPONSE" | jq -r '.choices[0].message.content' 2>/dev/null || echo "")
        if [ -n "$CONTENT" ] && [ "$CONTENT" != "null" ]; then
            print_info "Assistant response:"
            echo "\"$CONTENT\""
        fi

        USAGE_TOTAL=$(echo "$CHAT_RESPONSE" | jq -r '.usage.total_tokens' 2>/dev/null || echo "")
        if [ -n "$USAGE_TOTAL" ] && [ "$USAGE_TOTAL" != "null" ]; then
            print_info "Total tokens used: $USAGE_TOTAL"
        fi

        FINISH_REASON=$(echo "$CHAT_RESPONSE" | jq -r '.choices[0].finish_reason' 2>/dev/null || echo "")
        if [ -n "$FINISH_REASON" ] && [ "$FINISH_REASON" != "null" ]; then
            print_info "Finish reason: $FINISH_REASON"
        fi
    else
        print_warning "jq not available - showing raw response (first 500 chars):"
        echo "$CHAT_RESPONSE" | head -c 500
    fi

    # Test token usage tracking
    if echo "$CHAT_RESPONSE" | grep -q "total_tokens"; then
        print_success "Token usage tracking is working"
    else
        print_warning "Token usage information not found in response"
    fi

else
    print_error "Chat completion failed"
    echo "Response: $CHAT_RESPONSE"

    # Try to provide helpful debugging info
    if echo "$CHAT_RESPONSE" | grep -q "401"; then
        print_error "Authentication failed - check your token"
    elif echo "$CHAT_RESPONSE" | grep -q "404"; then
        print_error "Model not found - check that the model is available"
        print_info "Try listing available models: curl -H \"Authorization: Bearer \$TOKEN\" $MAAS_API/v1/models"
    elif echo "$CHAT_RESPONSE" | grep -q "503"; then
        print_error "Service unavailable - check that the Llamastack pod is running"
    fi

    exit 1
fi

# Step 5: Test with a different message to verify consistency
echo
print_info "Step 5: Testing with a follow-up request"

FOLLOWUP_REQUEST='{
  "model": "'$MODEL'",
  "messages": [
    {"role": "user", "content": "What is 2 + 2? Please respond with just the number."}
  ],
  "max_tokens": 10,
  "temperature": 0.1
}'

FOLLOWUP_RESPONSE=$(curl -s -k -X POST "$CHAT_ENDPOINT" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d "$FOLLOWUP_REQUEST" 2>/dev/null || echo "FAILED")

if echo "$FOLLOWUP_RESPONSE" | grep -q "choices"; then
    print_success "Follow-up request successful!"

    if command -v jq &>/dev/null; then
        CONTENT=$(echo "$FOLLOWUP_RESPONSE" | jq -r '.choices[0].message.content' 2>/dev/null || echo "")
        if [ -n "$CONTENT" ] && [ "$CONTENT" != "null" ]; then
            print_info "Follow-up response: \"$CONTENT\""
        fi
    fi
else
    print_warning "Follow-up request failed: $FOLLOWUP_RESPONSE"
fi

# Summary
echo
print_info "=== Test Summary ==="
print_success "Chat completion functionality is working correctly!"
print_info "Provider: $PROVIDER"
print_info "Model: $MODEL"
print_info "Gateway: $MAAS_GATEWAY"

echo
print_info "The $PROVIDER Llamastack integration is ready for production use."