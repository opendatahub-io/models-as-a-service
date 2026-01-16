#!/bin/bash

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
MAGENTA='\033[0;35m'
NC='\033[0m' # No Color

if ! command -v oc &> /dev/null; then
    echo -e "${RED}Error: 'oc' command not found!${NC}"
    echo "This script requires OpenShift CLI to obtain identity tokens."
    exit 1
fi

if ! command -v yq &> /dev/null; then
    echo -e "${RED}Error: 'yq' command not found!${NC}"
    echo "This script requires yq to parse YAML. Install it from: https://github.com/mikefarah/yq"
    exit 1
fi

if [ -z "${GATEWAY_URL:-}" ]; then
    echo -e "${BLUE}Looking up gateway configuration...${NC}"
    
    GATEWAY_YAML=$(kubectl get gateway -l app.kubernetes.io/instance=maas-default-gateway -n openshift-ingress -o yaml 2>/dev/null)
    
    if [ -z "$GATEWAY_YAML" ]; then
        echo -e "${RED}Failed to find gateway with label app.kubernetes.io/instance=maas-default-gateway${NC}" >&2
        exit 1
    fi
    
    LISTENERS=$(echo "$GATEWAY_YAML" | yq eval '.items[0].spec.listeners[]' - 2>/dev/null)
    
    HTTPS_HOSTNAME=$(echo "$LISTENERS" | yq eval 'select(.protocol == "HTTPS") | .hostname' - 2>/dev/null | head -1)
    
    if [ -n "$HTTPS_HOSTNAME" ] && [ "$HTTPS_HOSTNAME" != "null" ]; then
        GATEWAY_URL="https://${HTTPS_HOSTNAME}"
        echo -e "${GREEN}✓ Found HTTPS listener with hostname: ${HTTPS_HOSTNAME}${NC}"
    else
        HTTP_HOSTNAME=$(echo "$LISTENERS" | yq eval 'select(.protocol == "HTTP") | .hostname' - 2>/dev/null | head -1)
        
        if [ -n "$HTTP_HOSTNAME" ] && [ "$HTTP_HOSTNAME" != "null" ]; then
            GATEWAY_URL="http://${HTTP_HOSTNAME}"
            echo -e "${GREEN}✓ Found HTTP listener with hostname: ${HTTP_HOSTNAME}${NC}"
        else
            # Fall back to gateway status address
            STATUS_ADDRESS=$(echo "$GATEWAY_YAML" | yq eval '.items[0].status.addresses[0].value' - 2>/dev/null)
            
            if [ -z "$STATUS_ADDRESS" ] || [ "$STATUS_ADDRESS" = "null" ]; then
                echo -e "${RED}Failed to resolve gateway hostname or address${NC}" >&2
                echo -e "${RED}Please set GATEWAY_URL explicitly${NC}" >&2
                exit 1
            fi
            
            GATEWAY_URL="http://${STATUS_ADDRESS}"
            echo -e "${YELLOW}⚠ No listener hostname found, using status address: ${STATUS_ADDRESS}${NC}"
        fi
    fi
fi

API_BASE="${GATEWAY_URL%/}"

echo -e "${CYAN}======================================${NC}"
echo -e "${CYAN}   Model Inference & Rate Limit Test  ${NC}"
echo -e "${CYAN}======================================${NC}"
echo ""
echo -e "${BLUE}Gateway URL:${NC} ${GATEWAY_URL}"
echo ""

echo -e "${BLUE}Obtaining token from MaaS API...${NC}"

OC_TOKEN=$(oc whoami -t 2>/dev/null)
if [ -z "$OC_TOKEN" ]; then
    echo -e "${RED}Failed to obtain OpenShift identity token!${NC}"
    echo "Please ensure you are logged in: oc login"
    exit 1
fi

TOKEN_RESPONSE=$(curl -sSk \
    -H "Authorization: Bearer $OC_TOKEN" \
    -H "Content-Type: application/json" \
    -X POST \
    -d '{"expiration": "1h"}' \
    -w "\nHTTP_STATUS:%{http_code}\n" \
    "${API_BASE}/maas-api/v1/tokens" 2>&1)

http_status=$(echo "$TOKEN_RESPONSE" | grep "HTTP_STATUS:" | cut -d':' -f2)
response_body=$(echo "$TOKEN_RESPONSE" | sed '/HTTP_STATUS:/d')

if [ "$http_status" != "201" ]; then
    echo -e "${RED}Failed to obtain token from MaaS API!${NC}"
    echo -e "${RED}HTTP Status: $http_status${NC}"
    echo -e "${RED}Response: $response_body${NC}"
    exit 1
fi

TOKEN=$(echo "$response_body" | jq -r '.token' 2>/dev/null)
if [ -z "$TOKEN" ] || [ "$TOKEN" = "null" ]; then
    echo -e "${RED}Failed to parse token from response!${NC}"
    exit 1
fi

echo -e "${GREEN}✓ Token obtained successfully from MaaS API${NC}"

# JWT uses base64url encoding; convert to standard base64 before decoding
TOKEN_PAYLOAD=$(echo "$TOKEN" | jq -R 'split(".") | .[1]' 2>/dev/null | tr -d '"' | tr '_-' '/+' | awk '{while(length($0)%4)$0=$0"=";print}' | jq -Rr '@base64d' 2>/dev/null)
if [ -z "$TOKEN_PAYLOAD" ] || [ "$TOKEN_PAYLOAD" = "null" ]; then
    echo -e "${YELLOW}Warning:${NC} Failed to decode MaaS token payload"
    USER_NAME="unknown"
else
    USER_NAME=$(echo "$TOKEN_PAYLOAD" | jq -r '.sub // "unknown"' 2>/dev/null)
fi

echo -e "${BLUE}Discovering available models...${NC}"
MODELS_RESPONSE=$(curl -sSk \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -w "\nHTTP_STATUS:%{http_code}\n" \
    "${API_BASE}/maas-api/v1/models" 2>&1)

http_status=$(echo "$MODELS_RESPONSE" | grep "HTTP_STATUS:" | cut -d':' -f2)
response_body=$(echo "$MODELS_RESPONSE" | sed '/HTTP_STATUS:/d')

if [ "$http_status" != "200" ]; then
    echo -e "${RED}Failed to discover models!${NC}"
    echo -e "${RED}HTTP Status: $http_status${NC}"
    echo -e "${RED}Response: $response_body${NC}"
    exit 1
fi

MODEL_COUNT=$(echo "$response_body" | jq -r '.data | length' 2>/dev/null)
if [ -z "$MODEL_COUNT" ] || [ "$MODEL_COUNT" = "0" ]; then
    echo -e "${YELLOW}Warning: No models discovered!${NC}"
    echo "Please ensure models are deployed. See DEV.md for deployment instructions."
    exit 0
fi

echo -e "${GREEN}✓ Discovered $MODEL_COUNT model(s)${NC}"
echo "$response_body" | jq -r '.data[] | "  • \(.id) at \(.url)"'
echo ""

echo -e "${BLUE}Testing discovered models...${NC}"
echo ""

mapfile -t MODEL_IDS < <(echo "$response_body" | jq -r '.data[].id')
mapfile -t MODEL_URLS < <(echo "$response_body" | jq -r '.data[].url')

prompts=(
    "What is 2+2?"
    "Say 'Hello World' in Python"
    "What color is the sky?"
)

successful_models=0
failed_models=0

for idx in "${!MODEL_IDS[@]}"; do
    model_id="${MODEL_IDS[$idx]}"
    model_url="${MODEL_URLS[$idx]}"
    
    echo -e "${MAGENTA}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${MAGENTA}Testing Model: $model_id${NC}"
    echo -e "${MAGENTA}URL: $model_url${NC}"
    echo -e "${MAGENTA}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo ""
    
    model_success=0
    
    echo -e "${BLUE}Testing inference with different prompts:${NC}"
    echo ""
    
    for i in "${!prompts[@]}"; do
        prompt="${prompts[$i]}"
        echo -e "${YELLOW}Request #$((i+1)):${NC}"
        echo -e "${CYAN}Prompt:${NC} \"$prompt\""
        
        REQUEST_BODY=$(cat <<EOF
{
  "model": "$model_id",
  "messages": [
    {"role": "system", "content": "You are a helpful assistant. Keep responses brief."},
    {"role": "user", "content": "$prompt"}
  ],
  "temperature": 0.1,
  "max_tokens": 50
}
EOF
)
        
        response=$(curl -sSk \
            -H "Authorization: Bearer $TOKEN" \
            -H "Content-Type: application/json" \
            -X POST \
            -d "$REQUEST_BODY" \
            -w "\nHTTP_STATUS:%{http_code}\n" \
            "${model_url}/v1/chat/completions" 2>&1)
        
        http_status=$(echo "$response" | grep "HTTP_STATUS:" | cut -d':' -f2)
        response_body=$(echo "$response" | sed '/HTTP_STATUS:/d')
        
        if [ "$http_status" = "200" ]; then
            echo -e "${GREEN}Status: $http_status (Success)${NC}"
            model_success=1
            
            answer=$(echo "$response_body" | jq -r '.choices[0].message.content // "No response"' 2>/dev/null)
            tokens_used=$(echo "$response_body" | jq -r '.usage.total_tokens // 0' 2>/dev/null)
            
            echo -e "${CYAN}Response:${NC} $answer"
            echo -e "${CYAN}Tokens Used:${NC} $tokens_used"
        else
            echo -e "${RED}Status: $http_status (Failed)${NC}"
            echo -e "${RED}Error:${NC} $(echo "$response_body" | head -1)"
        fi
        echo ""
        
        # Small delay between requests
        sleep 1
    done
    
    if [ $model_success -eq 1 ]; then
        ((successful_models++))
    else
        ((failed_models++))
    fi
    echo ""
done

echo -e "${MAGENTA}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo -e "${MAGENTA}Testing Token Rate Limiting${NC}"
echo -e "${MAGENTA}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo ""

if [ ${#MODEL_IDS[@]} -eq 0 ]; then
    echo -e "${YELLOW}Skipping rate limit test - no models available${NC}"
else
    model_id="${MODEL_IDS[0]}"
    model_url="${MODEL_URLS[0]}"
    
    echo -e "${BLUE}Making rapid requests to trigger rate limit...${NC}"
    echo "Using model: $model_id"
    echo ""
    
    REQUEST_BODY_SIMPLE=$(cat <<EOF
{
  "model": "$model_id",
  "messages": [
    {"role": "user", "content": "Count to 5"}
  ],
  "temperature": 0.1,
  "max_tokens": 30
}
EOF
)
    
    total_success=0
    total_tokens=0
    rate_limited=false
    
    echo -n "Request status: "
    for i in {1..25}; do
        response=$(curl -sSk \
            -H "Authorization: Bearer $TOKEN" \
            -H "Content-Type: application/json" \
            -X POST \
            -d "$REQUEST_BODY_SIMPLE" \
            -w "\nHTTP_STATUS:%{http_code}\n" \
            "${model_url}/v1/chat/completions" 2>&1)
        
        http_status=$(echo "$response" | grep "HTTP_STATUS:" | cut -d':' -f2)
        
        if [ "$http_status" = "200" ]; then
            ((total_success++))
            tokens=$(echo "$response" | sed '/HTTP_STATUS:/d' | jq -r '.usage.total_tokens // 0' 2>/dev/null)
            if [ "$tokens" != "0" ]; then
                total_tokens=$((total_tokens + tokens))
            fi
            echo -ne "${GREEN}✓${NC}"
        elif [ "$http_status" = "429" ]; then
            rate_limited=true
            echo -ne "${RED}✗${NC}"
            if [ $i -gt 5 ]; then
                # If we've made enough requests, break on rate limit
                echo ""
                break
            fi
        else
            echo -ne "${YELLOW}?${NC}"
        fi
        
        # Small delay to avoid overwhelming the system
        sleep 0.5
    done
    
    echo ""
    echo ""
    echo -e "${BLUE}Rate Limiting Test Results:${NC}"
    echo -e "  • Successful requests: ${GREEN}$total_success${NC}"
    echo -e "  • Total tokens consumed: ${CYAN}$total_tokens${NC}"
    if [ "$rate_limited" = true ]; then
        echo -e "  • Rate limiting: ${GREEN}✓ Working${NC} (429 responses received)"
    else
        echo -e "  • Rate limiting: ${YELLOW}⚠ Not triggered${NC} (may need more requests or lower limits)"
    fi
fi

echo ""
echo -e "${CYAN}======================================${NC}"
echo -e "${CYAN}           Test Summary                ${NC}"
echo -e "${CYAN}======================================${NC}"
echo ""

echo -e "${BLUE}Authentication:${NC}"
echo -e "  ${GREEN}✓${NC} MaaS API token endpoint is working"
echo -e "  ${GREEN}✓${NC} Token authentication successful"
echo ""

echo -e "${BLUE}Model Discovery:${NC}"
echo -e "  ${GREEN}✓${NC} Discovered ${MODEL_COUNT} model(s)"
echo ""

echo -e "${BLUE}Model Inference:${NC}"
if [ "$successful_models" -gt 0 ]; then
    echo -e "  ${GREEN}✓${NC} ${successful_models} model(s) responding successfully"
    echo -e "  ${GREEN}✓${NC} Inference endpoints are functional"
fi
if [ "$failed_models" -gt 0 ]; then
    echo -e "  ${RED}✗${NC} ${failed_models} model(s) failed to respond"
fi
echo ""

echo -e "${BLUE}Rate Limiting:${NC}"
if [ ${#MODEL_IDS[@]} -gt 0 ]; then
    if [ "$rate_limited" = true ]; then
        echo -e "  ${GREEN}✓${NC} Token rate limiting is enforced"
    else
        echo -e "  ${YELLOW}⚠${NC}  Token rate limiting not triggered (may need adjustment)"
    fi
else
    echo -e "  ${YELLOW}⚠${NC}  Skipped (no models available)"
fi
echo ""

echo -e "${BLUE}Gateway URL:${NC} ${GATEWAY_URL}"
echo -e "${BLUE}User:${NC} $USER_NAME"
echo ""

if [ "$MODEL_COUNT" -gt 0 ]; then
    echo -e "${BLUE}Models tested:${NC}"
    for idx in "${!MODEL_IDS[@]}"; do
        echo "  • ${MODEL_IDS[$idx]} at ${MODEL_URLS[$idx]}"
    done
    echo ""
fi
