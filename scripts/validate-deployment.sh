#!/bin/bash

# Source helper functions for JWT decoding
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/deployment-helpers.sh"

# MaaS Platform Deployment Validation Script
# This script validates that the MaaS platform is correctly deployed and functional
#
# Usage: ./validate-deployment.sh [MODEL_NAME]
#   MODEL_NAME: Optional. If provided, the script will validate using this specific model

# Note: We don't use 'set -e' because we want to continue validation even if some checks fail

# Parse command line arguments
REQUESTED_MODEL=""
CUSTOM_REQUEST_PAYLOAD=""
INFERENCE_ENDPOINT="chat/completions"  # Default to chat completions
CUSTOM_MODEL_PATH=""  # Custom path for model endpoint (overrides --endpoint)
RATE_LIMIT_TEST_COUNT=10  # Default number of requests for rate limit testing
MAX_TOKENS=50  # Default max_tokens for requests
MAAS_API_NAMESPACE="${MAAS_API_NAMESPACE:-opendatahub}"  # Default namespace for MaaS API (use --namespace to override)

# Show help if requested
if [ "$1" = "--help" ] || [ "$1" = "-h" ]; then
    echo "MaaS Platform Deployment Validation Script"
    echo ""
    echo "Usage: $0 [OPTIONS] [MODEL_NAME]"
    echo ""
    echo "This script validates that the MaaS platform is correctly deployed and functional."
    echo "It performs checks on components, gateway status, policies, and API endpoints."
    echo ""
    echo "Arguments:"
    echo "  MODEL_NAME    Optional. Name of a specific model to use for validation."
    echo "                If not provided, the first available model will be used."
    echo ""
    echo "Options:"
    echo "  -h, --help                Show this help message and exit"
    echo "  --request-payload JSON    Custom JSON request payload for model inference tests."
    echo "                            Use \${MODEL_NAME} as a placeholder for the model name."
    echo "                            Default (Chat): '{\"model\": \"\${MODEL_NAME}\", \"messages\": [{\"role\": \"user\", \"content\": \"Hello\"}], \"max_tokens\": 50}'"
    echo "  --endpoint ENDPOINT       API endpoint to use: 'chat/completions' or 'completions'"
    echo "                            Default: 'chat/completions'"
    echo "  --model-path PATH         Custom path for model endpoint (e.g., '/v1/responses')"
    echo "                            Overrides --endpoint if provided"
    echo "  --rate-limit-requests N   Number of requests to send for rate limit testing"
    echo "                            Default: 10"
    echo "  --max-tokens N            Maximum number of tokens to generate per request"
    echo "                            Default: 50"
    echo "  -n, --namespace NS        Namespace where MaaS API is deployed"
    echo "                            Default: opendatahub (or MAAS_API_NAMESPACE env var)"
    echo ""
    echo "Examples:"
    echo "  # Basic validation"
    echo "  $0                                              # Validate using first available model (default chat format)"
    echo "  $0 llm-simulator                                # Validate using llm-simulator model"
    echo ""
    echo "  # For base models like granite (use completions endpoint with 'prompt')"
    echo "  $0 granite-8b-code-instruct-maas --endpoint responses --request-payload '{\"model\": \"\${MODEL_NAME}\", \"input\": \"Hello\", \"max_tokens\": 50}'"
    echo ""
    echo "  # For custom model paths (e.g., special inference endpoints)"
    echo "  $0 my-model --model-path /v1/responses --request-payload '{\"input\": \"test\"}'"
    echo ""
    echo "  # For instruction/chat models (default format works)"
    echo "  $0 qwen3-instruct"
    echo ""
    echo "  # Test with custom rate limit request count"
    echo "  $0 llm-simulator --rate-limit-requests 20          # Send 20 requests for rate limit test"
    echo ""
    echo "Exit Codes:"
    echo "  0    All critical checks passed"
    echo "  1    Some checks failed"
    echo ""
    exit 0
fi

# Parse arguments
while [ $# -gt 0 ]; do
    case "$1" in
        --request-payload)
            CUSTOM_REQUEST_PAYLOAD="$2"
            shift 2
            ;;
        --endpoint)
            INFERENCE_ENDPOINT="$2"
            shift 2
            ;;
        --model-path)
            CUSTOM_MODEL_PATH="$2"
            shift 2
            ;;
        --rate-limit-requests)
            RATE_LIMIT_TEST_COUNT="$2"
            if ! [[ "$RATE_LIMIT_TEST_COUNT" =~ ^[0-9]+$ ]]; then
                echo "Error: --rate-limit-requests must be a positive integer"
                exit 1
            fi
            shift 2
            ;;
        --max-tokens)
            MAX_TOKENS="$2"
            if ! [[ "$MAX_TOKENS" =~ ^[0-9]+$ ]]; then
                echo "Error: --max-tokens must be a positive integer"
                exit 1
            fi
            shift 2
            ;;
        --namespace|-n)
            if [ -z "$2" ] || [ "${2#-}" != "$2" ]; then
                echo "Error: --namespace requires a value (e.g., --namespace maas-api)" >&2
                exit 1
            fi
            MAAS_API_NAMESPACE="$2"
            shift 2
            ;;
        -*)
            echo "Unknown option: $1"
            echo "Use --help for usage information"
            exit 1
            ;;
        *)
            if [ -z "$REQUESTED_MODEL" ]; then
                REQUESTED_MODEL="$1"
            else
                echo "Error: Multiple model names provided"
                echo "Use --help for usage information"
                exit 1
            fi
            shift
            ;;
    esac
done

# Set default request payload if not provided (OpenAI Chat Completions format)
if [ -z "$CUSTOM_REQUEST_PAYLOAD" ]; then
    DEFAULT_REQUEST_PAYLOAD='{"model": "${MODEL_NAME}", "messages": [{"role": "user", "content": "Hello"}], "max_tokens": '"$MAX_TOKENS"'}'
else
    DEFAULT_REQUEST_PAYLOAD="$CUSTOM_REQUEST_PAYLOAD"
fi

if [ -n "$REQUESTED_MODEL" ]; then
    echo "Requested model for validation: $REQUESTED_MODEL"
fi

if [ -n "$CUSTOM_REQUEST_PAYLOAD" ]; then
    echo "Using custom request payload: $CUSTOM_REQUEST_PAYLOAD"
fi

if [ "$INFERENCE_ENDPOINT" != "chat/completions" ]; then
    echo "Using custom endpoint: /v1/$INFERENCE_ENDPOINT"
fi

if [ "$RATE_LIMIT_TEST_COUNT" != "10" ]; then
    echo "Using custom rate limit test count: $RATE_LIMIT_TEST_COUNT"
fi

if [ "$MAX_TOKENS" != "50" ]; then
    echo "Using custom max_tokens: $MAX_TOKENS"
fi

# Color codes for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Counters
PASSED=0
FAILED=0
WARNINGS=0

# Helper functions
print_header() {
    echo ""
    echo "========================================="
    echo "$1"
    echo "========================================="
    echo ""
}

print_check() {
    echo -e "${BLUE}🔍 Checking: $1${NC}"
}

print_success() {
    echo -e "${GREEN}✅ PASS: $1${NC}"
    ((PASSED++))
}

print_fail() {
    echo -e "${RED}❌ FAIL: $1${NC}"
    if [ -n "$2" ]; then
        echo -e "${RED}   Reason: $2${NC}"
    fi
    if [ -n "$3" ]; then
        echo -e "${YELLOW}   Suggestion: $3${NC}"
    fi
    if [ -n "$4" ]; then
        echo -e "${YELLOW}   Suggestion: $4${NC}"
    fi
    ((FAILED++))
}

print_warning() {
    echo -e "${YELLOW}⚠️  WARNING: $1${NC}"
    if [ -n "$2" ]; then
        echo -e "${YELLOW}   Note: $2${NC}"
    fi
    if [ -n "$3" ]; then
        echo -e "${YELLOW}   $3${NC}"
    fi
    ((WARNINGS++))
}

print_info() {
    echo -e "${BLUE}ℹ️  $1${NC}"
}

# Check if running on OpenShift
if ! kubectl api-resources | grep -q "route.openshift.io"; then
    print_fail "Not running on OpenShift" "This validation script is designed for OpenShift clusters" "Use a different validation approach for vanilla Kubernetes"
    exit 1
fi

print_header "🚀 MaaS Platform Deployment Validation"

if [ -n "$REQUESTED_MODEL" ]; then
    print_info "Validation will use model: $REQUESTED_MODEL"
fi

# ==========================================
# 1. Component Status Checks
# ==========================================
print_header "1️⃣ Component Status Checks"

# Check MaaS API pods
print_check "MaaS API pods"
MAAS_PODS=$(kubectl get pods -n "$MAAS_API_NAMESPACE" -l app.kubernetes.io/name=maas-api --no-headers 2>/dev/null | grep -c "Running" || echo "0")
if [ "$MAAS_PODS" -gt 0 ]; then
    print_success "MaaS API has $MAAS_PODS running pod(s)"
else
    print_fail "No MaaS API pods running" "Pods may be starting or failed" "Check: kubectl get pods -n $MAAS_API_NAMESPACE -l app.kubernetes.io/name=maas-api"
fi

# Check Kuadrant pods
print_check "Kuadrant system pods"
if kubectl get namespace kuadrant-system &>/dev/null; then
    KUADRANT_PODS=$(kubectl get pods -n kuadrant-system --no-headers 2>/dev/null | grep -c "Running" || echo "0")
    if [ "$KUADRANT_PODS" -gt 0 ]; then
        print_success "Kuadrant has $KUADRANT_PODS running pod(s)"
    else
        print_fail "No Kuadrant pods running" "Kuadrant operators may not be installed" "Check: kubectl get pods -n kuadrant-system"
    fi
else
    print_fail "Kuadrant namespace not found" "Kuadrant may not be installed" "Run: ./scripts/install-dependencies.sh --kuadrant"
fi

# Check OpenDataHub/KServe pods
print_check "OpenDataHub/KServe pods"
ODH_FOUND=false
ODH_TOTAL_PODS=0

if kubectl get namespace opendatahub &>/dev/null; then
    ODH_PODS=$(kubectl get pods -n opendatahub --no-headers 2>/dev/null | grep -c "Running" || echo "0")
    ODH_TOTAL_PODS=$((ODH_TOTAL_PODS + ODH_PODS))
    ODH_FOUND=true
    if [ "$ODH_PODS" -gt 0 ]; then
        print_info "  opendatahub namespace: $ODH_PODS running pod(s)"
    fi
fi

if kubectl get namespace redhat-ods-applications &>/dev/null; then
    RHOAI_PODS=$(kubectl get pods -n redhat-ods-applications --no-headers 2>/dev/null | grep -c "Running" || echo "0")
    ODH_TOTAL_PODS=$((ODH_TOTAL_PODS + RHOAI_PODS))
    ODH_FOUND=true
    if [ "$RHOAI_PODS" -gt 0 ]; then
        print_info "  redhat-ods-applications namespace: $RHOAI_PODS running pod(s)"
    fi
fi

if [ "$ODH_FOUND" = true ]; then
    if [ "$ODH_TOTAL_PODS" -gt 0 ]; then
        print_success "OpenDataHub/RHOAI has $ODH_TOTAL_PODS total running pod(s)"
    else
        print_warning "No OpenDataHub/RHOAI pods running" "KServe may not be installed or still starting"
    fi
else
    print_warning "OpenDataHub/RHOAI namespaces not found" "KServe may not be installed yet"
fi

# Check LLM namespace
print_check "LLM namespace and models"
if kubectl get namespace llm &>/dev/null; then
    LLM_PODS=$(kubectl get pods -n llm --no-headers 2>/dev/null | grep -c "Running" || echo "0")
    LLM_SERVICES=$(kubectl get llminferenceservices -n llm --no-headers 2>/dev/null | wc -l || echo "0")
    if [ "$LLM_SERVICES" -gt 0 ]; then
        print_success "Found $LLM_SERVICES LLMInferenceService(s) with $LLM_PODS running pod(s)"
    else
        print_warning "Models endpoint accessible but no models found" "You may need to deploy a model a simulated model can be deployed with the following command:" "kustomize build docs/samples/models/simulator | kubectl apply --server-side=true --force-conflicts -f -"
    fi
else
    print_warning "LLM namespace not found" "No models have been deployed yet"
fi

# ==========================================
# 2. Gateway Status
# ==========================================
print_header "2️⃣ Gateway Status"

print_check "Gateway resource"
if kubectl get gateway maas-default-gateway -n openshift-ingress &>/dev/null; then
    GATEWAY_ACCEPTED=$(kubectl get gateway maas-default-gateway -n openshift-ingress -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' 2>/dev/null || echo "Unknown")
    GATEWAY_PROGRAMMED=$(kubectl get gateway maas-default-gateway -n openshift-ingress -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null || echo "Unknown")
    
    if [ "$GATEWAY_ACCEPTED" = "True" ] && [ "$GATEWAY_PROGRAMMED" = "True" ]; then
        print_success "Gateway is Accepted and Programmed"
    elif [ "$GATEWAY_ACCEPTED" = "True" ]; then
        print_warning "Gateway is Accepted but not Programmed yet" "Gateway may still be initializing"
    else
        print_fail "Gateway not ready" "Accepted: $GATEWAY_ACCEPTED, Programmed: $GATEWAY_PROGRAMMED" "Check: kubectl describe gateway maas-default-gateway -n openshift-ingress"
    fi
else
    print_fail "Gateway not found" "Gateway may not be deployed" "Check: kubectl get gateway -A"
fi

print_check "HTTPRoute for maas-api"
if kubectl get httproute maas-api-route -n "$MAAS_API_NAMESPACE" &>/dev/null; then
    # Check if any parent has an Accepted condition with status True
    # HTTPRoutes can have multiple parents (Kuadrant policies + gateway controller)
    HTTPROUTE_ACCEPTED=$(kubectl get httproute maas-api-route -n "$MAAS_API_NAMESPACE" -o jsonpath='{.status.parents[*].conditions[?(@.type=="Accepted")].status}' 2>/dev/null | grep -q "True" && echo "True" || echo "False")
    if [ "$HTTPROUTE_ACCEPTED" = "True" ]; then
        print_success "HTTPRoute maas-api-route is configured and accepted"
    else
        # Be lenient - if the route exists, that's usually good enough
        print_warning "HTTPRoute maas-api-route exists but acceptance status unclear" "This is usually fine if other checks pass"
    fi
else
    print_fail "HTTPRoute maas-api-route not found" "API routing may not be configured" "Check: kubectl get httproute -n $MAAS_API_NAMESPACE"
fi

print_check "Gateway hostname"
# Get cluster domain and construct the MaaS gateway hostname
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null || echo "")
if [ -n "$CLUSTER_DOMAIN" ]; then
    HOST="https://maas.${CLUSTER_DOMAIN}"
    print_success "Gateway hostname: $HOST"
else
    print_fail "Could not determine cluster domain" "Cannot test API endpoints" "Check: kubectl get ingresses.config.openshift.io cluster"
    HOST=""
fi

# ==========================================
# 3. Policy Status
# ==========================================
print_header "3️⃣ Policy Status"

print_check "AuthPolicy"
AUTHPOLICY_COUNT=$(kubectl get authpolicy -A --no-headers 2>/dev/null | wc -l || echo "0")
if [ "$AUTHPOLICY_COUNT" -gt 0 ]; then
    AUTHPOLICY_STATUS=$(kubectl get authpolicy -n openshift-ingress gateway-auth-policy -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' 2>/dev/null || echo "NotFound")
    if [ "$AUTHPOLICY_STATUS" = "True" ]; then
        print_success "AuthPolicy is configured and accepted"
    else
        print_warning "AuthPolicy found but status: $AUTHPOLICY_STATUS" "Policy may still be reconciling. Try deleting the kuadrant operator pod:" "kubectl delete pod -n kuadrant-system -l control-plane=controller-manager"
    fi
else
    print_fail "No AuthPolicy found" "Authentication may not be enforced" "Check: kubectl get authpolicy -A"
fi

print_check "TokenRateLimitPolicy"
RATELIMIT_COUNT=$(kubectl get tokenratelimitpolicy -A --no-headers 2>/dev/null | wc -l || echo "0")
if [ "$RATELIMIT_COUNT" -gt 0 ]; then
    RATELIMIT_STATUS=$(kubectl get tokenratelimitpolicy -n openshift-ingress gateway-token-rate-limits -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' 2>/dev/null || echo "NotFound")
    if [ "$RATELIMIT_STATUS" = "True" ]; then
        print_success "TokenRateLimitPolicy is configured and accepted"
    else
        print_warning "TokenRateLimitPolicy found but status: $RATELIMIT_STATUS" "Policy may still be reconciling. Try deleting the kuadrant operator pod:" "kubectl delete pod -n kuadrant-system -l control-plane=controller-manager"
    fi
else
    print_fail "No TokenRateLimitPolicy found" "Rate limiting may not be enforced" "Check: kubectl get tokenratelimitpolicy -A"
fi

# ==========================================
# 4. API Endpoint Tests
# ==========================================
print_header "4️⃣ API Endpoint Tests"

if [ -z "$HOST" ]; then
    print_fail "Cannot test API endpoints" "No gateway route found" "Fix gateway route issues first"
else
    print_info "Using gateway endpoint: $HOST"
    
    # Test authentication endpoint
    print_check "Authentication endpoint"
    ENDPOINT="${HOST}/maas-api/v1/tokens"
    print_info "Testing: curl -sSk -X POST $ENDPOINT -H \"Authorization: Bearer \$(oc whoami -t)\" -H \"Content-Type: application/json\" -d '{\"expiration\": \"10m\"}'"
    
    if command -v oc &> /dev/null; then
        OC_TOKEN=$(oc whoami -t 2>/dev/null || echo "")
        if [ -n "$OC_TOKEN" ]; then
            TOKEN_RESPONSE=$(curl -sSk --connect-timeout 10 --max-time 30 -w "\n%{http_code}" \
                -H "Authorization: Bearer ${OC_TOKEN}" \
                -H "Content-Type: application/json" \
                -X POST \
                -d '{"expiration": "10m"}' \
                "${ENDPOINT}" 2>/dev/null || echo "")
            
            HTTP_CODE=$(echo "$TOKEN_RESPONSE" | tail -n1)
            RESPONSE_BODY=$(echo "$TOKEN_RESPONSE" | sed '$d')
            
            # Handle timeout/connection failure
            if [ -z "$HTTP_CODE" ] || [ "$HTTP_CODE" = "000" ]; then
                print_fail "Connection timeout or failed to reach endpoint" \
                    "The endpoint is not reachable. This is likely because:" \
                    "1) The endpoint is behind a VPN or firewall, 2) DNS resolution failed, 3) Gateway/Route not properly configured. Check: kubectl get gateway -n openshift-ingress && kubectl get httproute -n $MAAS_API_NAMESPACE"
                TOKEN=""
            elif [ "$HTTP_CODE" = "200" ] || [ "$HTTP_CODE" = "201" ]; then
                TOKEN=$(echo "$RESPONSE_BODY" | jq -r '.token' 2>/dev/null || echo "")
                if [ -n "$TOKEN" ] && [ "$TOKEN" != "null" ]; then
                    print_success "Authentication successful (HTTP $HTTP_CODE)"
                    
                    # Decode token and extract tier information using helper function
                    print_check "Token information"
                    TOKEN_PAYLOAD=$(decode_jwt_payload "$TOKEN")
                    if [ -n "$TOKEN_PAYLOAD" ]; then
                        SUB=$(echo "$TOKEN_PAYLOAD" | jq -r '.sub // empty' 2>/dev/null)
                        if [ -n "$SUB" ] && [ "$SUB" != "null" ]; then
                            print_info "Token subject: $SUB"
                            
                            # Extract tier from sub: system:serviceaccount:maas-default-gateway-tier-{tier}:{user}
                            # Extract the part between "tier-" and the colon
                            TIER=$(echo "$SUB" | sed -n 's/.*tier-\([^:]*\):.*/\1/p')
                            if [ -n "$TIER" ]; then
                                print_success "User tier: $TIER"
                            else
                                print_warning "Could not extract tier from subject" "Subject format may not match expected pattern"
                            fi
                        else
                            print_warning "Could not extract sub from token payload"
                        fi
                    else
                        print_warning "Could not decode token payload"
                    fi
                else
                    print_fail "Authentication response invalid" "Received HTTP $HTTP_CODE but no token in response" "Check MaaS API logs: kubectl logs -n $MAAS_API_NAMESPACE -l app.kubernetes.io/name=maas-api"
                fi
            elif [ "$HTTP_CODE" = "404" ]; then
                print_fail "Endpoint not found (HTTP 404)" \
                    "Traffic is reaching the Gateway/pods but the path is incorrect" \
                    "Check HTTPRoute configuration: kubectl describe httproute maas-api-route -n $MAAS_API_NAMESPACE"
                TOKEN=""
            elif [ "$HTTP_CODE" = "502" ] || [ "$HTTP_CODE" = "503" ]; then
                print_fail "Gateway/Service error (HTTP $HTTP_CODE)" \
                    "The Gateway is not able to reach the backend service" \
                    "Check: 1) MaaS API pods are running: kubectl get pods -n $MAAS_API_NAMESPACE -l app.kubernetes.io/name=maas-api, 2) Service exists: kubectl get svc maas-api -n $MAAS_API_NAMESPACE, 3) HTTPRoute is configured: kubectl describe httproute maas-api-route -n $MAAS_API_NAMESPACE"
                TOKEN=""
            else
                print_fail "Authentication failed (HTTP $HTTP_CODE)" "Response: $(echo $RESPONSE_BODY | head -c 100)" "Check AuthPolicy and MaaS API service"
                TOKEN=""
            fi
        else
            print_warning "Cannot get OpenShift token" "Not logged into oc CLI" "Run: oc login"
            TOKEN=""
        fi
    else
        print_warning "oc CLI not found" "Cannot test authentication" "Install oc CLI or use kubectl with token"
        TOKEN=""
    fi
    
    # Test models endpoint
    print_check "Models endpoint"
    if [ -n "$TOKEN" ]; then
        ENDPOINT="${HOST}/maas-api/v1/models"
        print_info "Testing: curl -sSk $ENDPOINT -H \"Content-Type: application/json\" -H \"Authorization: Bearer \$TOKEN\""
        
        MODELS_RESPONSE=$(curl -sSk --connect-timeout 10 --max-time 30 -w "\n%{http_code}" \
            -H "Content-Type: application/json" \
            -H "Authorization: Bearer ${TOKEN}" \
            "${ENDPOINT}" 2>/dev/null || echo "")
        HTTP_CODE=$(echo "$MODELS_RESPONSE" | tail -n1)
        RESPONSE_BODY=$(echo "$MODELS_RESPONSE" | sed '$d')
        
        # Handle timeout/connection failure
        if [ -z "$HTTP_CODE" ] || [ "$HTTP_CODE" = "000" ]; then
            print_fail "Connection timeout or failed to reach endpoint" \
                "The endpoint is not reachable (VPN/firewall/DNS issue)" \
                "Check Gateway and HTTPRoute configuration"
            MODEL_NAME=""
            MODEL_CHAT_ENDPOINT=""
        elif [ "$HTTP_CODE" = "200" ]; then
            MODEL_COUNT=$(echo "$RESPONSE_BODY" | jq -r '.data | length' 2>/dev/null || echo "0")
            if [ "$MODEL_COUNT" -gt 0 ]; then
                print_success "Models endpoint accessible, found $MODEL_COUNT model(s)"
                
                # Print list of available models
                print_info "Available models:"
                echo "$RESPONSE_BODY" | jq -r '.data[] | "  • \(.id) - \(.url)"' 2>/dev/null || echo "  Could not parse model list"
                echo ""
                
                # Check if a specific model was requested
                if [ -n "$REQUESTED_MODEL" ]; then
                    # Look for the requested model in the response
                    MODEL_INDEX=$(echo "$RESPONSE_BODY" | jq -r ".data | map(.id) | index(\"$REQUESTED_MODEL\")" 2>/dev/null || echo "null")
                    
                    if [ "$MODEL_INDEX" != "null" ] && [ -n "$MODEL_INDEX" ]; then
                        MODEL_NAME=$(echo "$RESPONSE_BODY" | jq -r ".data[$MODEL_INDEX].id" 2>/dev/null || echo "")
                        MODEL_CHAT=$(echo "$RESPONSE_BODY" | jq -r ".data[$MODEL_INDEX].url" 2>/dev/null || echo "")
                        print_info "Using requested model: $MODEL_NAME for validation"
                    else
                        # Requested model not found
                        print_fail "Requested model '$REQUESTED_MODEL' not found" "See available models above" "Use one of the available models or deploy the requested model"
                        MODEL_NAME=""
                        MODEL_CHAT=""
                        MODEL_CHAT_ENDPOINT=""
                    fi
                else
                    # No specific model requested, use the first one (default behavior)
                    MODEL_NAME=$(echo "$RESPONSE_BODY" | jq -r '.data[0].id' 2>/dev/null || echo "")
                    MODEL_CHAT=$(echo "$RESPONSE_BODY" | jq -r '.data[0].url' 2>/dev/null || echo "")
                    print_info "Using first available model: $MODEL_NAME for validation"
                fi
                
                # Set the inference endpoint if we have a valid model
                if [ -n "$MODEL_CHAT" ] && [ "$MODEL_CHAT" != "null" ]; then
                    # Use custom model path if provided, otherwise use endpoint
                    if [ -n "$CUSTOM_MODEL_PATH" ]; then
                        MODEL_CHAT_ENDPOINT="${MODEL_CHAT}${CUSTOM_MODEL_PATH}"
                    else
                        MODEL_CHAT_ENDPOINT="${MODEL_CHAT}/v1/${INFERENCE_ENDPOINT}"
                    fi
                elif [ -n "$MODEL_NAME" ]; then
                    print_warning "Model endpoint not found" "Model endpoint not found for $MODEL_NAME" "Check model HTTPRoute configuration: kubectl get httproute -n llm"
                    MODEL_NAME=""
                    MODEL_CHAT_ENDPOINT=""
                fi
            else
                print_warning "Models endpoint accessible but no models found" "You may need to deploy a model a simulated model can be deployed with the following command:" "kustomize build docs/samples/models/simulator | kubectl apply --server-side=true --force-conflicts -f -"
                MODEL_NAME=""
                MODEL_CHAT_ENDPOINT=""
            fi
        elif [ "$HTTP_CODE" = "404" ]; then
            print_fail "Endpoint not found (HTTP 404)" \
                "Path is incorrect - traffic reaching pods but wrong path" \
                "Check HTTPRoute: kubectl describe httproute maas-api-route -n $MAAS_API_NAMESPACE"
            MODEL_NAME=""
            MODEL_CHAT_ENDPOINT=""
        elif [ "$HTTP_CODE" = "502" ] || [ "$HTTP_CODE" = "503" ]; then
            print_fail "Gateway/Service error (HTTP $HTTP_CODE)" \
                "Gateway cannot reach backend service" \
                "Check MaaS API pods and service: kubectl get pods,svc -n $MAAS_API_NAMESPACE -l app.kubernetes.io/name=maas-api"
            MODEL_NAME=""
            MODEL_CHAT_ENDPOINT=""
        else
            print_fail "Models endpoint failed (HTTP $HTTP_CODE)" "Response: $(echo $RESPONSE_BODY | head -c 100)" "Check MaaS API service and logs"
            MODEL_NAME=""
            MODEL_CHAT_ENDPOINT=""
        fi
    else
        print_warning "Skipping models endpoint test" "No authentication token available"
        MODEL_NAME=""
        MODEL_CHAT_ENDPOINT=""
    fi
    
    # Test model inference endpoint (if model exists)
    if [ -n "$TOKEN" ] && [ -n "$MODEL_NAME" ] && [ -n "$MODEL_CHAT_ENDPOINT" ]; then
        print_check "Model inference endpoint"
        
        # Substitute MODEL_NAME placeholder in the request payload
        REQUEST_PAYLOAD="${DEFAULT_REQUEST_PAYLOAD//\$\{MODEL_NAME\}/$MODEL_NAME}"
        
        print_info "Testing: curl -sSk -X POST ${MODEL_CHAT_ENDPOINT} -H \"Authorization: Bearer \$TOKEN\" -H \"Content-Type: application/json\" -d '${REQUEST_PAYLOAD}'"
        
        INFERENCE_RESPONSE=$(curl -sSk --connect-timeout 10 --max-time 30 -w "\n%{http_code}" \
            -H "Authorization: Bearer ${TOKEN}" \
            -H "Content-Type: application/json" \
            -d "${REQUEST_PAYLOAD}" \
            "${MODEL_CHAT_ENDPOINT}" 2>/dev/null || echo "")
        
        HTTP_CODE=$(echo "$INFERENCE_RESPONSE" | tail -n1)
        RESPONSE_BODY=$(echo "$INFERENCE_RESPONSE" | sed '$d')
        
        # Handle timeout/connection failure
        if [ -z "$HTTP_CODE" ] || [ "$HTTP_CODE" = "000" ]; then
            print_fail "Connection timeout or failed to reach endpoint" \
                "Model endpoint is not reachable (VPN/firewall/DNS issue)" \
                "Check Gateway and model HTTPRoute: kubectl get httproute -n llm"
        elif [ "$HTTP_CODE" = "200" ]; then
            print_success "Model inference endpoint working"
            print_info "Response: $(echo $RESPONSE_BODY | head -c 200)"
        elif [ "$HTTP_CODE" = "404" ]; then
            print_fail "Model inference endpoint not found (HTTP 404)" \
                "Path is incorrect - traffic reaching but wrong path" \
                "Check model HTTPRoute configuration: kubectl get httproute -n llm && kubectl describe llminferenceservice -n llm"
        elif [ "$HTTP_CODE" = "502" ] || [ "$HTTP_CODE" = "503" ]; then
            print_fail "Gateway/Service error (HTTP $HTTP_CODE)" \
                "Gateway cannot reach model service" \
                "Check: 1) Model pods running: kubectl get pods -n llm, 2) Model service exists, 3) HTTPRoute configured: kubectl get httproute -n llm"
        elif [ "$HTTP_CODE" = "401" ]; then
            print_fail "Authorization failed (HTTP 401)" "Response: $(echo $RESPONSE_BODY | head -c 200)" "Check AuthPolicy and TokenRateLimitPolicy"
        elif [ "$HTTP_CODE" = "429" ]; then
            print_warning "Rate limiting (HTTP 429)" "Response: $(echo $RESPONSE_BODY | head -c 200)" "wait a minute and try again"
        else
            print_fail "Model inference failed (HTTP $HTTP_CODE)" "Response: $(echo $RESPONSE_BODY | head -c 200)" "Check model pod logs and HTTPRoute configuration, this model may also have a different response format"
          
        fi
    fi
    
    # Test rate limiting
    if [ -n "$TOKEN" ] && [ -n "$MODEL_NAME" ] && [ -n "$MODEL_CHAT_ENDPOINT" ]; then
        print_check "Rate limiting"
        print_info "Sending $RATE_LIMIT_TEST_COUNT rapid requests to test rate limiting..."
        
        # Use the same request payload for rate limiting tests
        REQUEST_PAYLOAD="${DEFAULT_REQUEST_PAYLOAD//\$\{MODEL_NAME\}/$MODEL_NAME}"
        
        SUCCESS_COUNT=0
        RATE_LIMITED_COUNT=0
        FAILED_COUNT=0
        
        for i in $(seq 1 $RATE_LIMIT_TEST_COUNT); do
            HTTP_CODE=$(curl -sSk --connect-timeout 5 --max-time 15 -o /dev/null -w "%{http_code}" \
                -H "Authorization: Bearer ${TOKEN}" \
                -H "Content-Type: application/json" \
                -d "${REQUEST_PAYLOAD}" \
                "${MODEL_CHAT_ENDPOINT}" 2>/dev/null || echo "000")
            
            if [ "$HTTP_CODE" = "200" ]; then
                ((SUCCESS_COUNT++))
            elif [ "$HTTP_CODE" = "429" ]; then
                ((RATE_LIMITED_COUNT++))
            else
                ((FAILED_COUNT++))
                # Log first few failed requests for debugging
                if [ "$FAILED_COUNT" -le 3 ]; then
                    print_info "  Request $i failed with HTTP $HTTP_CODE"
                fi
            fi
        done
        
        if [ "$RATE_LIMITED_COUNT" -gt 0 ]; then
            print_success "Rate limiting is working ($SUCCESS_COUNT successful, $RATE_LIMITED_COUNT rate limited)"
        elif [ "$SUCCESS_COUNT" -gt 0 ] && [ "$FAILED_COUNT" -eq 0 ]; then
            print_warning "Rate limiting may not be enforced" "All $SUCCESS_COUNT requests succeeded without rate limiting"
        elif [ "$SUCCESS_COUNT" -gt 0 ]; then
            print_warning "Rate limiting test inconclusive" "$SUCCESS_COUNT succeeded, $FAILED_COUNT failed (auth may still be stabilizing)"
        else
            print_fail "Rate limiting test failed" "All $RATE_LIMIT_TEST_COUNT requests failed (got $FAILED_COUNT errors)" "Check TokenRateLimitPolicy, Limitador, and auth service health"
        fi
    fi
    
    # Test unauthorized access
    print_check "Authorization enforcement (401 without token)"
    if [ -n "$MODEL_NAME" ] && [ -n "$MODEL_CHAT_ENDPOINT" ]; then
        # Use the same request payload for unauthorized test
        REQUEST_PAYLOAD="${DEFAULT_REQUEST_PAYLOAD//\$\{MODEL_NAME\}/$MODEL_NAME}"
        
        UNAUTH_CODE=$(curl -sSk --connect-timeout 5 --max-time 15 -o /dev/null -w "%{http_code}" \
            -H "Content-Type: application/json" \
            -d "${REQUEST_PAYLOAD}" \
            "${MODEL_CHAT_ENDPOINT}" 2>/dev/null || echo "000")
        
        if [ "$UNAUTH_CODE" = "401" ]; then
            print_success "Authorization is enforced (got 401 without token)"
        elif [ "$UNAUTH_CODE" = "403" ]; then
            print_success "Authorization is enforced (got 403 without token)"
        else
            print_warning "Authorization may not be enforced" "Got HTTP $UNAUTH_CODE instead of 401/403 without token"
        fi
    fi
fi

# ==========================================
# Summary
# ==========================================
print_header "📊 Validation Summary"

echo "Results:"
echo -e "  ${GREEN}✅ Passed: $PASSED${NC}"
echo -e "  ${RED}❌ Failed: $FAILED${NC}"
echo -e "  ${YELLOW}⚠️  Warnings: $WARNINGS${NC}"
echo ""

if [ "$FAILED" -eq 0 ]; then
    print_success "All critical checks passed! 🎉"
    echo ""
    echo "Next steps:"
    echo "  1. Deploy a model: kustomize build docs/samples/models/simulator | kubectl apply -f -"
    echo "  2. Access the API at: ${HOST:-https://maas.\${CLUSTER_DOMAIN}}"
    echo "  3. Check documentation: docs/README.md"
    echo "  4. Re-run validation with specific model: ./scripts/validate-deployment.sh MODEL_NAME"
    exit 0
else
    print_fail "Some checks failed. Please review the errors above."
    echo ""
    echo "Common fixes:"
    echo "  - Wait for pods to start: kubectl get pods -A | grep -v Running"
    echo "  - Check operator logs: kubectl logs -n kuadrant-system -l app.kubernetes.io/name=kuadrant-operator"
    echo "  - Re-run deployment: ./scripts/deploy-openshift.sh"
    echo ""
    echo "Usage: ./scripts/validate-deployment.sh [MODEL_NAME]"
    echo "  MODEL_NAME: Optional. Specify a model to validate against"
    echo ""
    exit 1
fi