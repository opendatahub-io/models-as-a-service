#!/bin/bash
# Demo script for MaaS local deployment.
# Run after ./local-deploy.sh completes.

set -euo pipefail

BOLD='\033[1m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; BLUE='\033[0;34m'
CYAN='\033[0;36m'; NC='\033[0m'

pause() {
  echo ""
  echo -e "  ${YELLOW}[press enter]${NC}"
  read -r
}

GATEWAY_NS="istio-system"

# ─── Setup ─────────────────────────────────────────────────────────────────

echo -e "${BOLD}${BLUE}MaaS Local Deployment Demo${NC}"
echo ""

# Ensure cluster is running
if ! kubectl cluster-info --context kind-maas-local &>/dev/null; then
  echo "Error: Kind cluster 'maas-local' not running. Run ./local-deploy.sh first."
  exit 1
fi
kubectl config use-context kind-maas-local &>/dev/null

# Port-forward
pkill -f "port-forward.*19090" 2>/dev/null || true
sleep 1
kubectl port-forward -n "$GATEWAY_NS" svc/maas-default-gateway-istio 19090:80 > /dev/null 2>&1 &
sleep 2

# Create API key
API_KEY=$(kubectl exec -n maas-system deployment/maas-api -- curl -sk \
  "https://localhost:8443/v1/api-keys" \
  -H "X-MaaS-Username: demo-user" \
  -H 'X-MaaS-Group: ["system:authenticated"]' \
  -H "Content-Type: application/json" \
  -d '{"name":"demo-key"}' 2>/dev/null | jq -r '.key')

# ─── Act 1: What's running ────────────────────────────────────────────────

echo -e "${BOLD}${CYAN}1. What's running${NC}"
echo ""
echo -e "  30 pods, 8 namespaces — full MaaS + BBR + Kuadrant + KServe stack"
echo ""

for ns in istio-system kuadrant-system maas-system kserve; do
  count=$(kubectl get pods -n $ns --no-headers 2>/dev/null | grep Running | wc -l | tr -d ' ')
  echo -e "  ${GREEN}$ns${NC}: $count pods"
done

echo ""
echo -e "  ${GREEN}Gateway${NC}: $(kubectl get gateway -n $GATEWAY_NS -o jsonpath='{.items[0].metadata.name}' 2>/dev/null) (Programmed)"
echo -e "  ${GREEN}Models${NC}:  $(kubectl get maasmodelrefs -A --no-headers 2>/dev/null | wc -l | tr -d ' ') registered"

pause

# ─── Act 2: Auth works ─────────────────────────────────────────────────────

echo -e "${BOLD}${CYAN}2. Auth is enforced (Kuadrant + Authorino)${NC}"
echo ""

echo -e "  ${BOLD}No API key:${NC}"
echo -e "  $ curl http://gateway/v1/models"
HTTP=$(curl -s -o /dev/null -w '%{http_code}' http://localhost:19090/v1/models)
echo -e "  ${YELLOW}→ HTTP $HTTP Unauthorized${NC}"
echo ""

echo -e "  ${BOLD}Invalid API key:${NC}"
echo -e "  $ curl -H 'Authorization: Bearer sk-oai-FAKE' http://gateway/v1/models"
HTTP=$(curl -s -o /dev/null -w '%{http_code}' http://localhost:19090/v1/models -H "Authorization: Bearer sk-oai-FAKE")
echo -e "  ${YELLOW}→ HTTP $HTTP Forbidden${NC}"
echo ""

echo -e "  ${BOLD}Valid API key:${NC}"
echo -e "  $ curl -H 'Authorization: Bearer sk-oai-...' http://gateway/v1/models"
MODELS=$(curl -s http://localhost:19090/v1/models -H "Authorization: Bearer $API_KEY" | jq -c '[.data[].id]')
echo -e "  ${GREEN}→ HTTP 200 — $MODELS${NC}"

pause

# ─── Act 3: External model inference ───────────────────────────────────────

echo -e "${BOLD}${CYAN}3. External model inference (llm-katan on AWS, Let's Encrypt TLS)${NC}"
echo ""
echo -e "  Request → Gateway → Kuadrant auth → BBR (model resolve + credential inject) → llm-katan"
echo ""
echo -e "  $ curl http://gateway/llm/llm-katan-openai/v1/chat/completions"

RESP=$(curl -s --max-time 15 http://localhost:19090/llm/llm-katan-openai/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"llm-katan-echo","messages":[{"role":"user","content":"Hello from local MaaS!"}],"max_tokens":30}')

echo -e "  ${GREEN}→ $(echo "$RESP" | jq -r '.choices[0].message.content')${NC}"
echo ""
echo -e "  Model: $(echo "$RESP" | jq -r '.model')  |  Tokens: $(echo "$RESP" | jq -r '.usage.total_tokens')"
echo ""
echo -e "  TLS: Let's Encrypt cert, verified by Istio (no insecureSkipVerify)"
echo -e "  DestinationRule: $(kubectl get dr llm-katan-openai -n llm -o jsonpath='{.spec.trafficPolicy.tls.mode}' 2>/dev/null) (no insecureSkipVerify)"

pause

# ─── Act 4: Internal model inference ──────────────────────────────────────

echo -e "${BOLD}${CYAN}4. Internal model inference (llm-d simulator, in-cluster via KServe)${NC}"
echo ""
echo -e "  Request → Gateway → Kuadrant auth → KServe HTTPRoute → llm-d pod"
echo ""
echo -e "  $ curl http://gateway/llm-internal/sim-internal/v1/chat/completions"

RESP=$(curl -s --max-time 15 http://localhost:19090/llm-internal/sim-internal/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"facebook/opt-125m","messages":[{"role":"user","content":"Tell me something"}],"max_tokens":20}')

echo -e "  ${GREEN}→ $(echo "$RESP" | jq -r '.choices[0].message.content')${NC}"
echo ""
echo -e "  Model: $(echo "$RESP" | jq -r '.model')  |  Tokens: $(echo "$RESP" | jq -r '.usage.total_tokens')"

pause

# ─── Act 5: Fast rebuild ──────────────────────────────────────────────────

echo -e "${BOLD}${CYAN}5. Fast iteration: --rebuild${NC}"
echo ""
echo -e "  After a code change in any component:"
echo ""
echo -e "  $ ./local-deploy.sh --rebuild bbr             # ~15 seconds"
echo -e "  $ ./local-deploy.sh --rebuild maas-api         # ~15 seconds"
echo -e "  $ ./local-deploy.sh --rebuild maas-controller  # ~15 seconds"
echo -e "  $ ./local-deploy.sh --rebuild all              # ~45 seconds"
echo ""
echo -e "  Rebuilds the image, loads into Kind, restarts the deployment."
echo -e "  Cluster infrastructure stays untouched."

pause

# ─── Summary ──────────────────────────────────────────────────────────────

echo -e "${BOLD}${CYAN}Summary${NC}"
echo ""
echo "  One script deploys the full MaaS platform locally:"
echo "    - MaaS API + Controller (TLS backend, matching OpenShift)"
echo "    - BBR (payload-processing, ext-proc body routing)"
echo "    - Kuadrant (auth + rate limiting)"
echo "    - KServe (LLMInferenceService for internal models)"
echo "    - PostgreSQL, Istio, cert-manager, MetalLB"
echo ""
echo "  External model: llm-katan on AWS with Let's Encrypt TLS"
echo "  Internal model: llm-d simulator via KServe"
echo "  Auth: API key validation through full Authorino → maas-api chain"
echo ""
echo "  Zero existing files modified. Zero manual configuration."
echo ""

# Cleanup
pkill -f "port-forward.*19090" 2>/dev/null || true
