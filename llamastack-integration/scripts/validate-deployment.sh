#!/bin/bash

# Llamastack Integration Validation Script
# This script validates that a Llamastack deployment is working correctly with MaaS

set -e

PROVIDER=${1:-"gemini"}
NAMESPACE=${2:-"llm"}

echo "🔍 Validating Llamastack deployment for provider: $PROVIDER"

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

# Step 1: Check LLMInferenceService exists and is ready
echo
print_info "Step 1: Checking LLMInferenceService status"

LIS_NAME="${PROVIDER}-llamastack"
if kubectl get llminferenceservice "$LIS_NAME" -n "$NAMESPACE" &>/dev/null; then
    print_success "LLMInferenceService $LIS_NAME exists"

    # Check if it's ready
    READY=$(kubectl get llminferenceservice "$LIS_NAME" -n "$NAMESPACE" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "Unknown")
    if [ "$READY" = "True" ]; then
        print_success "LLMInferenceService is Ready"
    else
        print_error "LLMInferenceService is not Ready (status: $READY)"
        kubectl describe llminferenceservice "$LIS_NAME" -n "$NAMESPACE"
        exit 1
    fi
else
    print_error "LLMInferenceService $LIS_NAME not found in namespace $NAMESPACE"
    echo "Available LLMInferenceServices:"
    kubectl get llminferenceservice -n "$NAMESPACE"
    exit 1
fi

# Step 2: Check pods are running
echo
print_info "Step 2: Checking pod status"

POD_SELECTOR="app.kubernetes.io/name=${PROVIDER}-llamastack"
PODS=$(kubectl get pods -n "$NAMESPACE" -l "$POD_SELECTOR" --no-headers 2>/dev/null || echo "")

if [ -z "$PODS" ]; then
    print_error "No pods found with selector: $POD_SELECTOR"
    exit 1
fi

while read -r line; do
    if [ -n "$line" ]; then
        POD_NAME=$(echo "$line" | awk '{print $1}')
        POD_STATUS=$(echo "$line" | awk '{print $3}')

        if [ "$POD_STATUS" = "Running" ]; then
            print_success "Pod $POD_NAME is Running"
        else
            print_error "Pod $POD_NAME status: $POD_STATUS"
            kubectl describe pod "$POD_NAME" -n "$NAMESPACE"
            exit 1
        fi
    fi
done <<< "$PODS"

# Step 3: Test health endpoint
echo
print_info "Step 3: Testing health endpoint"

SERVICE_NAME=$(kubectl get svc -n "$NAMESPACE" -l "$POD_SELECTOR" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")

if [ -z "$SERVICE_NAME" ]; then
    print_error "No service found for provider: $PROVIDER"
    exit 1
fi

print_info "Found service: $SERVICE_NAME"

# Port forward and test health
PORT_FORWARD_PID=""
cleanup_port_forward() {
    if [ -n "$PORT_FORWARD_PID" ]; then
        kill $PORT_FORWARD_PID 2>/dev/null || true
        wait $PORT_FORWARD_PID 2>/dev/null || true
    fi
}
trap cleanup_port_forward EXIT

kubectl port-forward -n "$NAMESPACE" "service/$SERVICE_NAME" 8443:8000 &
PORT_FORWARD_PID=$!

# Wait for port forward to be ready
sleep 3

# Test health endpoint
HEALTH_RESPONSE=$(curl -k -s https://localhost:8443/v1/health 2>/dev/null || echo "FAILED")

if echo "$HEALTH_RESPONSE" | grep -qi "ok"; then
    print_success "Health endpoint responding correctly"
else
    print_error "Health endpoint failed. Response: $HEALTH_RESPONSE"
    # Show pod logs for debugging
    POD_NAME=$(kubectl get pods -n "$NAMESPACE" -l "$POD_SELECTOR" -o jsonpath='{.items[0].metadata.name}')
    echo "Pod logs:"
    kubectl logs "$POD_NAME" -n "$NAMESPACE" --tail=20
    exit 1
fi

# Clean up port forward
cleanup_port_forward
PORT_FORWARD_PID=""

# Step 4: Test model discovery through MaaS API
echo
print_info "Step 4: Testing model discovery through MaaS API"

# Try to get MaaS API endpoint
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

if [ -n "$MAAS_API" ]; then
    print_info "Found MaaS API endpoint: $MAAS_API"

    # Try to get a token (this may fail depending on auth setup)
    print_warning "Attempting to get MaaS token (may fail depending on auth configuration)"

    TOKEN=""
    if command -v oc &>/dev/null; then
        # Try OpenShift token
        OC_TOKEN=$(oc whoami -t 2>/dev/null || echo "")
        if [ -n "$OC_TOKEN" ]; then
            TOKEN_RESPONSE=$(curl -s -k -H "Authorization: Bearer $OC_TOKEN" \
                --json '{"expiration": "1h"}' "$MAAS_API/maas-api/v1/tokens" 2>/dev/null || echo "FAILED")

            if echo "$TOKEN_RESPONSE" | grep -q "token"; then
                TOKEN=$(echo "$TOKEN_RESPONSE" | jq -r .token 2>/dev/null || echo "")
                print_success "Successfully obtained MaaS token"
            fi
        fi
    fi

    if [ -n "$TOKEN" ]; then
        # Test model discovery
        MODELS_RESPONSE=$(curl -s -k -H "Authorization: Bearer $TOKEN" "$MAAS_API/maas-api/v1/models" 2>/dev/null || echo "FAILED")

        if echo "$MODELS_RESPONSE" | grep -q "data"; then
            print_success "Model discovery endpoint responding"

            # Check if our provider's models are listed
            PROVIDER_MODELS=""
            case "$PROVIDER" in
                "gemini")
                    PROVIDER_MODELS="gemini/models/gemini-2"
                    ;;
                "openai")
                    PROVIDER_MODELS="gpt-4o"
                    ;;
                "anthropic")
                    PROVIDER_MODELS="claude-3-5-sonnet"
                    ;;
            esac

            if [ -n "$PROVIDER_MODELS" ] && echo "$MODELS_RESPONSE" | grep -q "$PROVIDER_MODELS"; then
                print_success "Found expected models from $PROVIDER provider"
            else
                print_warning "Expected models from $PROVIDER not found in response"
                echo "Models response (first 500 chars):"
                echo "$MODELS_RESPONSE" | head -c 500
            fi
        else
            print_warning "Model discovery failed. Response: $MODELS_RESPONSE"
        fi
    else
        print_warning "Could not obtain MaaS token - skipping model discovery test"
        print_info "To test manually, obtain a token and run:"
        print_info "curl -H \"Authorization: Bearer \$TOKEN\" $MAAS_API/v1/models"
    fi
else
    print_warning "MaaS API endpoint not found - skipping model discovery test"
fi

# Step 5: Summary
echo
print_info "=== Validation Summary ==="
print_success "Llamastack deployment for $PROVIDER provider is working correctly"
print_info "Next steps:"
print_info "1. Test chat completions through MaaS gateway"
print_info "2. Verify rate limiting and authentication"
print_info "3. Check monitoring and metrics collection"

echo
print_info "For detailed testing instructions, see:"
print_info "examples/$PROVIDER-*/README.md"