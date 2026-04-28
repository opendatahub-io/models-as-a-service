#!/bin/bash
# Deploy MaaS platform locally on a Kind cluster (macOS + Linux/WSL2).
#
# One-click deployment of: Kind + Istio + Gateway API + cert-manager +
# Kuadrant (auth/rate-limiting) + PostgreSQL + MaaS controller + MaaS API.
#
# Usage:
#   ./test/e2e/scripts/local-deploy.sh            # Deploy everything
#   ./test/e2e/scripts/local-deploy.sh --teardown  # Delete the Kind cluster
#   ./test/e2e/scripts/local-deploy.sh --status    # Show component status
#
# Environment variables:
#   KIND_CLUSTER_NAME    - Cluster name (default: maas-local)
#   ISTIO_VERSION        - Istio version (default: 1.29.2)
#   KUADRANT_VERSION     - Kuadrant Helm chart version (default: 1.3.1)
#   MAAS_NAMESPACE       - MaaS deployment namespace (default: maas-system)
#   GATEWAY_NAMESPACE    - Gateway namespace (default: istio-system)

set -euo pipefail

# ─── Constants ──────────────────────────────────────────────────────────────

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-maas-local}"
ISTIO_VERSION="${ISTIO_VERSION:-1.29.2}"  # needs Envoy >=1.37 for ext_proc FULL_DUPLEX fix (envoy#41654)
KUADRANT_VERSION="${KUADRANT_VERSION:-1.3.1}"  # matches MaaS install-dependencies.sh
CERTMANAGER_VERSION="${CERTMANAGER_VERSION:-1.17.2}"
GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-1.3.0}"
MAAS_NAMESPACE="${MAAS_NAMESPACE:-maas-system}"
GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-istio-system}"
SUBSCRIPTION_NAMESPACE="models-as-a-service"

MAAS_API_IMAGE="${MAAS_API_IMAGE:-quay.io/opendatahub/maas-api:latest}"
MAAS_CONTROLLER_IMAGE="${MAAS_CONTROLLER_IMAGE:-quay.io/opendatahub/maas-controller:latest}"
BBR_IMAGE="${BBR_IMAGE:-quay.io/opendatahub/odh-ai-gateway-payload-processing:odh-stable}"

# Path to BBR repo (for arm64 image builds)
BBR_REPO="${BBR_REPO:-$(cd "$PROJECT_ROOT/../ai-gateway-payload-processing" 2>/dev/null && pwd || echo "")}"

# KServe (for LLMInferenceService / internal models)
KSERVE_VERSION="${KSERVE_VERSION:-v0.15.2}"
KSERVE_COMMIT="47894470ea49"  # opendatahub fork commit pinned in maas-controller go.mod
KSERVE_ODH_IMAGE="quay.io/opendatahub/kserve-controller:latest"
LLMISVC_IMAGE="quay.io/opendatahub/odh-kserve-llmisvc-controller:odh-stable"

ARCH="$(uname -m)"
# Docker platform string (arm64 stays arm64, x86_64 becomes amd64)
DOCKER_PLATFORM="linux/$(if [[ "$ARCH" == "x86_64" ]]; then echo amd64; else echo "$ARCH"; fi)"

# Source deployment helpers for create_maas_db_config_secret
# shellcheck source=../../../scripts/deployment-helpers.sh
source "$PROJECT_ROOT/scripts/deployment-helpers.sh"

# ─── Colors ─────────────────────────────────────────────────────────────────

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
BLUE='\033[0;34m'
BOLD='\033[1m'
NC='\033[0m'

step_num=0
step() {
  step_num=$((step_num + 1))
  echo ""
  echo -e "${BOLD}${BLUE}[$step_num] $1${NC}"
  echo "────────────────────────────────────────"
}

ok()   { echo -e "  ${GREEN}✓${NC} $1"; }
warn() { echo -e "  ${YELLOW}!${NC} $1"; }
fail() { echo -e "  ${RED}✗${NC} $1" >&2; }

# ─── Argument parsing ───────────────────────────────────────────────────────

ACTION="deploy"
REBUILD_COMPONENT=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --teardown)  ACTION="teardown" ;;
    --status)    ACTION="status" ;;
    --validate)  ACTION="validate" ;;
    --rebuild)
      ACTION="rebuild"
      REBUILD_COMPONENT="${2:-}"
      if [[ -z "$REBUILD_COMPONENT" ]] || [[ "$REBUILD_COMPONENT" == --* ]]; then
        echo "Usage: $0 --rebuild <bbr|maas-api|maas-controller>"
        exit 1
      fi
      shift
      ;;
    --help|-h)
      echo "Usage: $0 [--teardown | --status | --validate | --rebuild <component> | --help]"
      echo ""
      echo "Deploy MaaS platform on a local Kind cluster (macOS + Linux/WSL2)."
      echo ""
      echo "Flags:"
      echo "  --teardown              Delete the Kind cluster and all resources"
      echo "  --status                Show component status"
      echo "  --validate              Run inference tests to verify the deployment works"
      echo "  --rebuild <component>   Rebuild and redeploy a component after code changes"
      echo "                          Components: bbr, maas-api, maas-controller, all"
      echo "  --help                  Show this help message"
      echo ""
      echo "Environment variables:"
      echo "  KIND_CLUSTER_NAME     Cluster name (default: maas-local)"
      echo "  MAAS_API_IMAGE        Custom MaaS API image"
      echo "  MAAS_CONTROLLER_IMAGE Custom MaaS controller image"
      exit 0
      ;;
    *) echo "Unknown flag: $1"; exit 1 ;;
  esac
  shift
done

# ─── Teardown ───────────────────────────────────────────────────────────────

if [[ "$ACTION" == "teardown" ]]; then
  echo -e "${BOLD}Tearing down Kind cluster '${KIND_CLUSTER_NAME}'...${NC}"
  kind delete cluster --name "$KIND_CLUSTER_NAME" 2>/dev/null || true
  echo -e "${GREEN}Done.${NC}"
  exit 0
fi

# ─── Status ─────────────────────────────────────────────────────────────────

if [[ "$ACTION" == "status" ]]; then
  echo -e "${BOLD}MaaS Local Deployment Status${NC}"
  echo ""

  if ! kind get clusters 2>/dev/null | grep -q "^${KIND_CLUSTER_NAME}$"; then
    fail "Kind cluster '${KIND_CLUSTER_NAME}' not found"
    exit 1
  fi
  ok "Kind cluster '${KIND_CLUSTER_NAME}' exists"

  kubectl config use-context "kind-${KIND_CLUSTER_NAME}" &>/dev/null

  echo ""
  echo "Pods by namespace:"
  for ns in istio-system cert-manager kuadrant-system "$MAAS_NAMESPACE"; do
    echo -e "  ${BOLD}$ns:${NC}"
    kubectl get pods -n "$ns" --no-headers 2>/dev/null | sed 's/^/    /' || echo "    (none)"
  done

  echo ""
  echo "Gateway:"
  kubectl get gateway -n "$GATEWAY_NAMESPACE" 2>/dev/null | sed 's/^/  /' || echo "  (none)"

  echo ""
  echo "MaaS CRDs:"
  kubectl get externalmodels -A 2>/dev/null | sed 's/^/  /' || echo "  (none)"
  kubectl get maasmodelrefs -A 2>/dev/null | sed 's/^/  /' || echo "  (none)"
  kubectl get maassubscriptions -A 2>/dev/null | sed 's/^/  /' || echo "  (none)"
  exit 0
fi

# ─── Validate ──────────────────────────────────────────────────────────────

if [[ "$ACTION" == "validate" ]]; then
  if ! kind get clusters 2>/dev/null | grep -q "^${KIND_CLUSTER_NAME}$"; then
    fail "Kind cluster '${KIND_CLUSTER_NAME}' not found. Run $0 first."
    exit 1
  fi
  kubectl config use-context "kind-${KIND_CLUSTER_NAME}" &>/dev/null

  echo -e "${BOLD}Validating MaaS Local Deployment${NC}"
  echo ""
  PASS=0; TOTAL=0

  # All _check inputs are hardcoded strings defined below, not user input.
  _check() {
    TOTAL=$((TOTAL + 1))
    if eval "$2" &>/dev/null; then
      ok "$1"; PASS=$((PASS + 1))
    else
      fail "$1"
    fi
  }

  # Pods
  echo -e "${BOLD}Pods${NC}"
  _check "All pods running" '[[ $(kubectl get pods -A --no-headers | grep -v Running | grep -v Completed | wc -l) -eq 0 ]]'
  _check "maas-api ready" 'kubectl get deployment maas-api -n $MAAS_NAMESPACE -o jsonpath="{.status.readyReplicas}" | grep -q 1'
  _check "maas-controller ready" 'kubectl get deployment maas-controller -n $MAAS_NAMESPACE -o jsonpath="{.status.readyReplicas}" | grep -q 1'
  _check "payload-processing (BBR) ready" 'kubectl get deployment payload-processing -n $GATEWAY_NAMESPACE -o jsonpath="{.status.readyReplicas}" | grep -q 1'
  _check "Authorino ready" 'kubectl get deployment authorino -n kuadrant-system -o jsonpath="{.status.readyReplicas}" | grep -q 1'
  _check "Gateway programmed" 'kubectl get gateway maas-default-gateway -n $GATEWAY_NAMESPACE -o jsonpath="{.status.conditions[?(@.type==\"Programmed\")].status}" | grep -q True'

  # CRDs
  echo ""
  echo -e "${BOLD}MaaS Resources${NC}"
  _check "ExternalModel Ready" '[[ "$(kubectl get maasmodelref llm-katan-openai -n llm -o jsonpath="{.status.phase}")" == "Ready" ]]'
  _check "LLMInferenceService exists" 'kubectl get llminferenceservice sim-internal -n llm-internal'
  _check "Subscription Active" '[[ "$(kubectl get maassubscription simulator-subscription -n models-as-a-service -o jsonpath="{.status.phase}")" == "Active" ]]'
  _check "AuthPolicies created" '[[ $(kubectl get authpolicies -A --no-headers | wc -l) -ge 2 ]]'

  # Inference
  echo ""
  echo -e "${BOLD}Inference${NC}"

  # Port-forward (trap ensures cleanup on any exit, including set -e failures)
  pkill -f "port-forward.*19090" 2>/dev/null || true
  sleep 1
  kubectl port-forward -n "$GATEWAY_NAMESPACE" svc/maas-default-gateway-istio 19090:80 > /dev/null 2>&1 &
  _PF_PID=$!
  trap 'kill $_PF_PID 2>/dev/null || true' EXIT
  sleep 3

  # API key
  API_KEY=$(kubectl exec -n "$MAAS_NAMESPACE" deployment/maas-api -- curl -sk \
    "https://localhost:8443/v1/api-keys" \
    -H "X-MaaS-Username: validate-user" \
    -H 'X-MaaS-Group: ["system:authenticated"]' \
    -H "Content-Type: application/json" \
    -d '{"name":"validate"}' 2>/dev/null | jq -r '.key // empty')

  _check "API key creation" '[[ -n "$API_KEY" ]] && [[ "$API_KEY" == sk-oai-* ]]'

  TOTAL=$((TOTAL + 1))
  MODELS=$(curl -s --max-time 5 http://localhost:19090/v1/models -H "Authorization: Bearer $API_KEY" | jq -r '.data[].id' 2>/dev/null | sort | tr '\n' ',' | sed 's/,$//')
  if [[ "$MODELS" == *"llm-katan-openai"* ]]; then
    ok "List models: $MODELS"; PASS=$((PASS + 1))
  else
    fail "List models: ${MODELS:-no response}"
  fi

  TOTAL=$((TOTAL + 1))
  EXT_RESP=$(curl -s --max-time 15 http://localhost:19090/llm/llm-katan-openai/v1/chat/completions \
    -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
    -d '{"model":"llm-katan-echo","messages":[{"role":"user","content":"validate"}],"max_tokens":5}' 2>/dev/null)
  if echo "$EXT_RESP" | jq -e '.choices[0].message.content' &>/dev/null; then
    ok "External model inference (llm-katan): $(echo "$EXT_RESP" | jq -r .model)"; PASS=$((PASS + 1))
  else
    fail "External model inference: ${EXT_RESP:-no response}"
  fi

  TOTAL=$((TOTAL + 1))
  INT_RESP=$(curl -s --max-time 15 http://localhost:19090/llm-internal/sim-internal/v1/chat/completions \
    -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
    -d '{"model":"facebook/opt-125m","messages":[{"role":"user","content":"validate"}],"max_tokens":5}' 2>/dev/null)
  if echo "$INT_RESP" | jq -e '.choices[0].message.content' &>/dev/null; then
    ok "Internal model inference (llm-d sim): $(echo "$INT_RESP" | jq -r .model)"; PASS=$((PASS + 1))
  else
    fail "Internal model inference: ${INT_RESP:-no response}"
  fi

  # Auth
  echo ""
  echo -e "${BOLD}Auth${NC}"
  TOTAL=$((TOTAL + 1))
  HTTP_NO_KEY=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://localhost:19090/v1/models)
  if [[ "$HTTP_NO_KEY" == "401" ]]; then
    ok "No API key → 401 Unauthorized"; PASS=$((PASS + 1))
  else
    fail "No API key → HTTP $HTTP_NO_KEY (expected 401)"
  fi

  TOTAL=$((TOTAL + 1))
  HTTP_BAD_KEY=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://localhost:19090/v1/models -H "Authorization: Bearer sk-oai-INVALID")
  if [[ "$HTTP_BAD_KEY" == "403" ]]; then
    ok "Invalid API key → 403 Forbidden"; PASS=$((PASS + 1))
  else
    fail "Invalid API key → HTTP $HTTP_BAD_KEY (expected 403)"
  fi

  # TLS
  echo ""
  echo -e "${BOLD}TLS${NC}"
  _check "maas-api cert (cert-manager CA)" 'kubectl get secret maas-api-serving-cert -n $MAAS_NAMESPACE'
  TOTAL=$((TOTAL + 1))
  DR_TLS=$(kubectl get dr llm-katan-openai -n llm -o jsonpath='{.spec.trafficPolicy.tls}' 2>/dev/null)
  if echo "$DR_TLS" | grep -q '"mode":"SIMPLE"' && ! echo "$DR_TLS" | grep -q 'insecureSkipVerify'; then
    ok "External model TLS: SIMPLE, no insecureSkipVerify"; PASS=$((PASS + 1))
  else
    fail "External model TLS: $DR_TLS"
  fi

  pkill -f "port-forward.*19090" 2>/dev/null || true

  # Summary
  echo ""
  if [[ $PASS -eq $TOTAL ]]; then
    echo -e "${BOLD}${GREEN}All $TOTAL checks passed.${NC}"
  else
    echo -e "${BOLD}${YELLOW}$PASS/$TOTAL checks passed.${NC}"
  fi
  exit $(( TOTAL - PASS ))
fi

# ─── Rebuild ───────────────────────────────────────────────────────────────

if [[ "$ACTION" == "rebuild" ]]; then
  if ! kind get clusters 2>/dev/null | grep -q "^${KIND_CLUSTER_NAME}$"; then
    fail "Kind cluster '${KIND_CLUSTER_NAME}' not found. Run $0 first."
    exit 1
  fi
  kubectl config use-context "kind-${KIND_CLUSTER_NAME}" &>/dev/null

  # Ensure BBR repo is available
  if [[ -z "$BBR_REPO" || ! -d "$BBR_REPO" ]]; then
    BBR_REPO="$PROJECT_ROOT/../ai-gateway-payload-processing"
    if [[ ! -d "$BBR_REPO" ]]; then
      echo "  Cloning ai-gateway-payload-processing..."
      gh repo clone opendatahub-io/ai-gateway-payload-processing "$BBR_REPO" -- --depth 1 2>&1 | tail -1
    fi
  fi

  case "$REBUILD_COMPONENT" in
    bbr|payload-processing)
      echo -e "${BOLD}Rebuilding BBR (payload-processing)${NC}"
      (cd "$BBR_REPO" && \
        docker buildx build --platform "$DOCKER_PLATFORM" --load \
          -t "$BBR_IMAGE" . 2>&1 | tail -3)
      kind load docker-image "$BBR_IMAGE" --name "$KIND_CLUSTER_NAME"
      kubectl rollout restart deployment/payload-processing -n "$GATEWAY_NAMESPACE"
      kubectl rollout status deployment/payload-processing -n "$GATEWAY_NAMESPACE" --timeout=60s
      ok "BBR rebuilt and redeployed"
      ;;
    maas-api|api)
      echo -e "${BOLD}Rebuilding maas-api${NC}"
      (cd "$PROJECT_ROOT/maas-api" && \
        docker buildx build --platform "$DOCKER_PLATFORM" --load \
          -t "$MAAS_API_IMAGE" . 2>&1 | tail -3)
      kind load docker-image "$MAAS_API_IMAGE" --name "$KIND_CLUSTER_NAME"
      kubectl rollout restart deployment/maas-api -n "$MAAS_NAMESPACE"
      kubectl rollout status deployment/maas-api -n "$MAAS_NAMESPACE" --timeout=60s
      ok "maas-api rebuilt and redeployed"
      ;;
    maas-controller|controller)
      echo -e "${BOLD}Rebuilding maas-controller${NC}"
      (cd "$PROJECT_ROOT" && \
        docker buildx build --platform "$DOCKER_PLATFORM" --load \
          -f maas-controller/Dockerfile \
          -t "$MAAS_CONTROLLER_IMAGE" . 2>&1 | tail -3)
      kind load docker-image "$MAAS_CONTROLLER_IMAGE" --name "$KIND_CLUSTER_NAME"
      kubectl rollout restart deployment/maas-controller -n "$MAAS_NAMESPACE"
      kubectl rollout status deployment/maas-controller -n "$MAAS_NAMESPACE" --timeout=60s
      ok "maas-controller rebuilt and redeployed"
      ;;
    all)
      echo -e "${BOLD}Rebuilding all components${NC}"
      echo ""
      echo "  Building maas-api..."
      (cd "$PROJECT_ROOT/maas-api" && \
        docker buildx build --platform "$DOCKER_PLATFORM" --load \
          -t "$MAAS_API_IMAGE" . 2>&1 | tail -3)
      kind load docker-image "$MAAS_API_IMAGE" --name "$KIND_CLUSTER_NAME"

      echo "  Building maas-controller..."
      (cd "$PROJECT_ROOT" && \
        docker buildx build --platform "$DOCKER_PLATFORM" --load \
          -f maas-controller/Dockerfile \
          -t "$MAAS_CONTROLLER_IMAGE" . 2>&1 | tail -3)
      kind load docker-image "$MAAS_CONTROLLER_IMAGE" --name "$KIND_CLUSTER_NAME"

      echo "  Building BBR..."
      (cd "$BBR_REPO" && \
        docker buildx build --platform "$DOCKER_PLATFORM" --load \
          -t "$BBR_IMAGE" . 2>&1 | tail -3)
      kind load docker-image "$BBR_IMAGE" --name "$KIND_CLUSTER_NAME"

      kubectl rollout restart deployment/maas-api -n "$MAAS_NAMESPACE"
      kubectl rollout restart deployment/maas-controller -n "$MAAS_NAMESPACE"
      kubectl rollout restart deployment/payload-processing -n "$GATEWAY_NAMESPACE"
      kubectl rollout status deployment/maas-api -n "$MAAS_NAMESPACE" --timeout=60s
      kubectl rollout status deployment/maas-controller -n "$MAAS_NAMESPACE" --timeout=60s
      kubectl rollout status deployment/payload-processing -n "$GATEWAY_NAMESPACE" --timeout=60s
      ok "All components rebuilt and redeployed"
      ;;
    *)
      fail "Unknown component: $REBUILD_COMPONENT"
      echo "  Valid components: bbr, maas-api, maas-controller, all"
      exit 1
      ;;
  esac
  exit 0
fi

# ─── Prerequisites ──────────────────────────────────────────────────────────

step "Checking prerequisites"

# Supported platforms: macOS and Linux (including WSL2)
OS="$(uname -s)"
if [[ "$OS" != "Darwin" && "$OS" != "Linux" ]]; then
  fail "This script supports macOS and Linux. Detected: $OS"
  exit 1
fi
ok "$OS detected ($ARCH)"

# Docker
if ! docker info &>/dev/null; then
  fail "Docker is not running. Start Docker (Desktop or daemon) and try again."
  exit 1
fi

# Check Docker resources
DOCKER_CPUS=$(docker info --format '{{.NCPU}}' 2>/dev/null || echo "0")
DOCKER_MEM_BYTES=$(docker info --format '{{.MemTotal}}' 2>/dev/null || echo "0")
DOCKER_MEM_GB=$(( DOCKER_MEM_BYTES / 1073741824 ))

if [[ "$DOCKER_CPUS" -lt 4 ]]; then
  warn "Docker has ${DOCKER_CPUS} CPUs allocated. Recommend 6+ for smooth operation."
else
  ok "Docker: ${DOCKER_CPUS} CPUs"
fi
if [[ "$DOCKER_MEM_GB" -lt 6 ]]; then
  warn "Docker has ${DOCKER_MEM_GB}GB RAM allocated. Recommend 8+ GB."
else
  ok "Docker: ${DOCKER_MEM_GB}GB RAM"
fi

# Check disk space
if [[ "$OS" == "Darwin" ]]; then
  DISK_FREE_GB=$(df -g / | tail -1 | awk '{print $4}')
else
  DISK_FREE_GB=$(df --block-size=1G / | tail -1 | awk '{print $4}')
fi
if [[ "$DISK_FREE_GB" -lt 10 ]]; then
  warn "Only ${DISK_FREE_GB}GB free disk space. Recommend 10+ GB."
else
  ok "Disk: ${DISK_FREE_GB}GB free"
fi

# Required tools
MISSING=()
for tool in kind kubectl kustomize helm jq python3 gh; do
  if ! command -v "$tool" &>/dev/null; then
    MISSING+=("$tool")
  fi
done

if [[ ${#MISSING[@]} -gt 0 ]]; then
  fail "Missing required tools: ${MISSING[*]}"
  if [[ "$OS" == "Darwin" ]]; then
    echo "  Install with: brew install ${MISSING[*]}"
  else
    echo "  Install with your package manager (e.g., apt install ${MISSING[*]})"
  fi
  exit 1
fi
ok "Tools: kind, kubectl, kustomize, helm, jq"

# istioctl — install inline if missing
if ! command -v istioctl &>/dev/null; then
  warn "istioctl not found, installing ${ISTIO_VERSION}..."
  curl -sL https://istio.io/downloadIstio | ISTIO_VERSION="$ISTIO_VERSION" sh -
  export PATH="$PWD/istio-${ISTIO_VERSION}/bin:$PATH"
fi
ok "istioctl: $(istioctl version --remote=false 2>/dev/null || echo "$ISTIO_VERSION")"

# ─── Step 1: Kind cluster ──────────────────────────────────────────────────

step "Creating Kind cluster"

if kind get clusters 2>/dev/null | grep -q "^${KIND_CLUSTER_NAME}$"; then
  if kubectl cluster-info --context "kind-${KIND_CLUSTER_NAME}" &>/dev/null; then
    ok "Cluster '${KIND_CLUSTER_NAME}' already exists and is reachable"
  else
    warn "Cluster exists but unreachable, recreating..."
    kind delete cluster --name "$KIND_CLUSTER_NAME"
  fi
fi

if ! kind get clusters 2>/dev/null | grep -q "^${KIND_CLUSTER_NAME}$"; then
  kind create cluster --name "$KIND_CLUSTER_NAME" --wait 120s --config - <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
EOF
  ok "Cluster created"
fi

kubectl config use-context "kind-${KIND_CLUSTER_NAME}"

# ─── Step 2: Gateway API CRDs ──────────────────────────────────────────────

step "Installing Gateway API CRDs"

if kubectl get crd gatewayclasses.gateway.networking.k8s.io &>/dev/null; then
  ok "Gateway API CRDs already installed"
else
  kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/v${GATEWAY_API_VERSION}/standard-install.yaml"
  ok "Gateway API CRDs v${GATEWAY_API_VERSION} installed"
fi

# ─── Step 3: Istio ──────────────────────────────────────────────────────────

step "Installing Istio (minimal profile)"

if kubectl get deployment istiod -n istio-system &>/dev/null; then
  ok "Istio already installed"
else
  istioctl install --set profile=minimal \
    --set values.pilot.env.SUPPORT_GATEWAY_API_INFERENCE_EXTENSION=true \
    --set values.pilot.env.ENABLE_GATEWAY_API_INFERENCE_EXTENSION=true \
    -y
  kubectl rollout status deployment/istiod -n istio-system --timeout=120s
  ok "Istio ${ISTIO_VERSION} installed"
fi

# ─── Step 3b: MetalLB (LoadBalancer for Kind) ───────────────────────────────

step "Installing MetalLB (LoadBalancer for Kind)"

if kubectl get ipaddresspool kind-pool -n metallb-system &>/dev/null; then
  ok "MetalLB already installed"
else
  if ! kubectl get deployment controller -n metallb-system &>/dev/null; then
    kubectl apply -f https://raw.githubusercontent.com/metallb/metallb/v0.14.9/config/manifests/metallb-native.yaml 2>&1 | tail -3
  fi
  kubectl wait --for=condition=Available deployment/controller -n metallb-system --timeout=120s

  # Wait for webhook pod to be ready before applying config (avoids race condition)
  kubectl wait --for=condition=Ready pod -l component=controller -n metallb-system --timeout=120s 2>/dev/null || true

  # Configure IP pool from Kind's docker network (extract IPv4 subnet)
  KIND_SUBNET=$(docker network inspect kind -f '{{range .IPAM.Config}}{{.Subnet}} {{end}}' | tr ' ' '\n' | grep '\.')
  LB_BASE=$(echo "$KIND_SUBNET" | cut -d'.' -f1-3)
  _metallb_retries=0
  while [[ $_metallb_retries -lt 6 ]]; do
    if kubectl apply -f - <<EOF 2>/dev/null
apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: kind-pool
  namespace: metallb-system
spec:
  addresses:
  - ${LB_BASE}.200-${LB_BASE}.250
---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: kind-l2
  namespace: metallb-system
EOF
    then
      break
    fi
    echo "  MetalLB webhook not ready, retrying in 10s..."
    sleep 10
    _metallb_retries=$((_metallb_retries + 1))
  done
  ok "MetalLB installed (LB range: ${LB_BASE}.200-250)"
fi

# ─── Step 4: cert-manager ──────────────────────────────────────────────────

step "Installing cert-manager"

if kubectl get deployment cert-manager -n cert-manager &>/dev/null; then
  ok "cert-manager already installed"
else
  kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/v${CERTMANAGER_VERSION}/cert-manager.yaml"
  kubectl wait --for=condition=Available deployment/cert-manager -n cert-manager --timeout=120s
  kubectl wait --for=condition=Available deployment/cert-manager-webhook -n cert-manager --timeout=120s
  # Wait for webhook to actually serve (deployment Available != TLS endpoint ready)
  echo "  Waiting for cert-manager webhook to serve..."
  _cm_retries=0
  while [[ $_cm_retries -lt 12 ]]; do
    if kubectl apply --dry-run=server -f - <<CMEOF 2>/dev/null
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: cert-manager-webhook-test
spec:
  selfSigned: {}
CMEOF
    then
      break
    fi
    sleep 5
    _cm_retries=$((_cm_retries + 1))
  done
  ok "cert-manager v${CERTMANAGER_VERSION} installed"
fi

# Create TLS certificate for maas-api (replaces OpenShift's service-ca auto-cert).
# On OpenShift, the service annotation auto-creates the cert via the service-ca controller,
# and the CA is in the system trust store so Authorino can verify it.
# On Kind, we use cert-manager with a CA chain (root CA → CA issuer → leaf cert).
# The root CA cert is then injected into Authorino so it trusts maas-api's cert.
kubectl create namespace "$MAAS_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
if kubectl get secret maas-api-serving-cert -n "$MAAS_NAMESPACE" &>/dev/null; then
  ok "TLS certificate for maas-api already exists"
else
  kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: maas-selfsigned-issuer
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: maas-root-ca
  namespace: ${MAAS_NAMESPACE}
spec:
  isCA: true
  commonName: maas-ca
  secretName: maas-root-ca
  issuerRef:
    name: maas-selfsigned-issuer
    kind: ClusterIssuer
  duration: 87600h
---
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: maas-ca-issuer
  namespace: ${MAAS_NAMESPACE}
spec:
  ca:
    secretName: maas-root-ca
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: maas-api-serving-cert
  namespace: ${MAAS_NAMESPACE}
spec:
  secretName: maas-api-serving-cert
  issuerRef:
    name: maas-ca-issuer
    kind: Issuer
  dnsNames:
  - maas-api
  - maas-api.${MAAS_NAMESPACE}
  - maas-api.${MAAS_NAMESPACE}.svc
  - maas-api.${MAAS_NAMESPACE}.svc.cluster.local
  duration: 8760h
  renewBefore: 720h
EOF
  kubectl wait --for=condition=Ready certificate/maas-api-serving-cert \
    -n "$MAAS_NAMESPACE" --timeout=60s
  ok "TLS certificate created for maas-api (CA chain)"
fi

# ─── Step 5: Kuadrant ──────────────────────────────────────────────────────

step "Installing Kuadrant (auth + rate limiting)"

if helm list -n kuadrant-system 2>/dev/null | grep -q kuadrant-operator; then
  ok "Kuadrant already installed via Helm"
else
  helm repo add kuadrant https://kuadrant.io/helm-charts/ 2>/dev/null || true
  helm repo update kuadrant

  kubectl create namespace kuadrant-system --dry-run=client -o yaml | kubectl apply -f -

  helm upgrade --install kuadrant-operator kuadrant/kuadrant-operator \
    --namespace kuadrant-system \
    --version "$KUADRANT_VERSION" \
    --set manager.env[0].name=ISTIO_GATEWAY_CONTROLLER_NAMES \
    --set manager.env[0].value=istio.io/gateway-controller \
    --wait --timeout 180s

  ok "Kuadrant operator v${KUADRANT_VERSION} installed"

  # Create Kuadrant instance
  kubectl apply -f - <<EOF
apiVersion: kuadrant.io/v1beta1
kind: Kuadrant
metadata:
  name: kuadrant
  namespace: kuadrant-system
spec: {}
EOF

  # Wait for Authorino and Limitador to come up
  echo "  Waiting for Kuadrant components..."
  kubectl wait --for=condition=Available deployment -l app=authorino -n kuadrant-system --timeout=120s 2>/dev/null || \
    warn "Authorino not ready yet (may take a moment)"
  ok "Kuadrant instance created"
fi

# Inject maas-api CA cert into Authorino so it can verify maas-api's TLS cert.
# On OpenShift, the service-ca root CA is in the system trust store. On Kind,
# we mount the cert-manager root CA and point Go's SSL_CERT_FILE to a combined bundle.
if ! kubectl get configmap maas-ca-bundle -n kuadrant-system &>/dev/null; then
  # Wait for Authorino deployment (created asynchronously by Kuadrant operator)
  echo "  Waiting for Authorino deployment..."
  for _i in $(seq 1 24); do
    kubectl get deployment authorino -n kuadrant-system &>/dev/null && break
    sleep 5
  done
  kubectl wait --for=condition=Available deployment/authorino -n kuadrant-system --timeout=120s

  # Extract the root CA cert
  kubectl get secret maas-root-ca -n "$MAAS_NAMESPACE" -o jsonpath='{.data.ca\.crt}' | base64 -d > /tmp/maas-ca.crt

  kubectl create configmap maas-ca-bundle -n kuadrant-system \
    --from-file=maas-ca.crt=/tmp/maas-ca.crt

  # Patch Authorino: add init container to build combined CA bundle, mount it
  kubectl patch deployment authorino -n kuadrant-system --type=strategic -p '{
    "spec":{"template":{"spec":{
      "initContainers":[{
        "name":"setup-ca",
        "image":"registry.access.redhat.com/ubi9/ubi-minimal:latest",
        "command":["/bin/sh","-c"],
        "args":["cat /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem /maas-ca/maas-ca.crt > /certs/ca-bundle.crt"],
        "volumeMounts":[
          {"name":"maas-ca","mountPath":"/maas-ca","readOnly":true},
          {"name":"combined-certs","mountPath":"/certs"}
        ]
      }],
      "containers":[{
        "name":"authorino",
        "env":[{"name":"SSL_CERT_FILE","value":"/certs/ca-bundle.crt"}],
        "volumeMounts":[{"name":"combined-certs","mountPath":"/certs","readOnly":true}]
      }],
      "volumes":[
        {"name":"maas-ca","configMap":{"name":"maas-ca-bundle"}},
        {"name":"combined-certs","emptyDir":{}}
      ]
    }}}
  }'

  kubectl rollout status deployment/authorino -n kuadrant-system --timeout=120s
  rm -f /tmp/maas-ca.crt
  ok "Authorino CA trust configured for maas-api"
fi

# ─── Step 6: PostgreSQL ────────────────────────────────────────────────────

step "Deploying PostgreSQL"

kubectl create namespace "$MAAS_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

if kubectl get deployment postgres -n "$MAAS_NAMESPACE" &>/dev/null; then
  ok "PostgreSQL already deployed"
else
  POSTGRES_USER="maas"
  POSTGRES_DB="maas"
  POSTGRES_PASSWORD="$(openssl rand -base64 32 | tr -d '/+=' | cut -c1-32)"

  kubectl apply -n "$MAAS_NAMESPACE" -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: postgres-creds
  labels:
    app: postgres
stringData:
  POSTGRES_USER: "${POSTGRES_USER}"
  POSTGRES_PASSWORD: "${POSTGRES_PASSWORD}"
  POSTGRES_DB: "${POSTGRES_DB}"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
  labels:
    app: postgres
spec:
  replicas: 1
  selector:
    matchLabels:
      app: postgres
  template:
    metadata:
      labels:
        app: postgres
    spec:
      containers:
      - name: postgres
        image: postgres:16-alpine
        env:
        - name: POSTGRES_USER
          valueFrom:
            secretKeyRef:
              name: postgres-creds
              key: POSTGRES_USER
        - name: POSTGRES_PASSWORD
          valueFrom:
            secretKeyRef:
              name: postgres-creds
              key: POSTGRES_PASSWORD
        - name: POSTGRES_DB
          valueFrom:
            secretKeyRef:
              name: postgres-creds
              key: POSTGRES_DB
        ports:
        - containerPort: 5432
        volumeMounts:
        - name: data
          mountPath: /var/lib/postgresql/data
        resources:
          requests:
            memory: "256Mi"
            cpu: "100m"
          limits:
            memory: "512Mi"
            cpu: "500m"
        readinessProbe:
          exec:
            command: ["pg_isready", "-U", "maas"]
          initialDelaySeconds: 5
          periodSeconds: 5
      volumes:
      - name: data
        emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: postgres
  labels:
    app: postgres
spec:
  selector:
    app: postgres
  ports:
  - port: 5432
    targetPort: 5432
EOF

  # Create maas-db-config secret
  ENCODED_PASSWORD=$(printf '%s' "$POSTGRES_PASSWORD" | od -An -tx1 | tr -d ' \n' | sed 's/../%&/g')
  DB_CONNECTION_URL="postgresql://${POSTGRES_USER}:${ENCODED_PASSWORD}@postgres:5432/${POSTGRES_DB}?sslmode=disable"
  create_maas_db_config_secret "$MAAS_NAMESPACE" "$DB_CONNECTION_URL"

  kubectl wait -n "$MAAS_NAMESPACE" --for=condition=available deployment/postgres --timeout=120s
  ok "PostgreSQL deployed"
fi

# ─── Step 7: MaaS CRDs ─────────────────────────────────────────────────────

step "Installing MaaS + KServe CRDs"

# Stub OpenShift CRDs — controllers watch these and crash-loop without them on Kind.
# maas-controller: Authentication.config.openshift.io (Tenant reconciler)
# kserve-controller: Route.route.openshift.io (InferenceGraph)
for stub_crd in \
  "authentications.config.openshift.io:Authentication:AuthenticationList" \
  "routes.route.openshift.io:Route:RouteList"; do
  crd_name="${stub_crd%%:*}"
  rest="${stub_crd#*:}"
  kind_name="${rest%%:*}"
  list_name="${rest#*:}"
  group="${crd_name#*.}"
  if ! kubectl get crd "$crd_name" &>/dev/null; then
    kubectl apply -f - <<EOF
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: ${crd_name}
spec:
  group: ${group}
  names:
    kind: ${kind_name}
    listKind: ${list_name}
    plural: ${crd_name%%.*}
    singular: $(echo "${kind_name}" | tr '[:upper:]' '[:lower:]')
  scope: Namespaced
  versions:
  - name: v1
    served: true
    storage: true
    schema:
      openAPIV3Schema:
        type: object
        x-kubernetes-preserve-unknown-fields: true
EOF
  fi
done

if kubectl get crd externalmodels.maas.opendatahub.io &>/dev/null; then
  ok "MaaS CRDs already installed"
else
  kubectl apply -f "$PROJECT_ROOT/deployment/base/maas-controller/crd/bases/"
  for crd in externalmodels.maas.opendatahub.io maasmodelrefs.maas.opendatahub.io \
             maassubscriptions.maas.opendatahub.io maasauthpolicies.maas.opendatahub.io; do
    kubectl wait --for=condition=Established "crd/$crd" --timeout=60s
  done
  ok "MaaS CRDs installed"
fi

# KServe LLMInferenceService CRD — required by maas-controller at startup
if kubectl get crd llminferenceservices.serving.kserve.io &>/dev/null; then
  ok "KServe CRDs already installed"
else
  kubectl apply --server-side -f \
    "https://raw.githubusercontent.com/opendatahub-io/kserve/${KSERVE_COMMIT}/config/crd/full/serving.kserve.io_llminferenceservices.yaml"
  kubectl apply --server-side -f \
    "https://raw.githubusercontent.com/opendatahub-io/kserve/${KSERVE_COMMIT}/config/crd/full/serving.kserve.io_llminferenceserviceconfigs.yaml"
  kubectl wait --for=condition=Established crd/llminferenceservices.serving.kserve.io --timeout=60s
  ok "KServe CRDs installed"
fi

# ─── Step 7b: KServe controller (for LLMInferenceService / internal models) ─

step "Installing KServe (model serving)"

if kubectl get deployment kserve-controller-manager -n kserve &>/dev/null; then
  ok "KServe already installed"
else
  # Install vanilla KServe (provides base controller, CRDs, webhooks)
  "$PROJECT_ROOT/scripts/installers/install-kserve.sh"

  # Scale down kserve-controller-manager — the opendatahub fork image watches
  # Route.route.openshift.io for InferenceGraph, which crash-loops on non-OpenShift
  # clusters. We only need LLMInferenceService, handled by llmisvc-controller-manager.
  kubectl scale deployment/kserve-controller-manager --replicas=0 -n kserve
  ok "KServe installed (main controller scaled to 0 — llmisvc-controller handles LLMInferenceService)"
fi

# LLMInferenceService controller — separate binary in the opendatahub kserve fork
if kubectl get deployment llmisvc-controller-manager -n kserve &>/dev/null; then
  ok "LLMIS controller already installed"
else
  # Clone opendatahub kserve fork to get the deployment manifests
  KSERVE_CLONE="/tmp/opendatahub-kserve"
  if [[ ! -d "$KSERVE_CLONE" ]]; then
    echo "  Cloning opendatahub-io/kserve..."
    gh repo clone opendatahub-io/kserve "$KSERVE_CLONE" -- --depth 1 2>&1 | tail -1
  fi

  # Build arm64 image if needed
  if [[ "$ARCH" == "arm64" ]]; then
    echo "  Building llmisvc-controller for arm64..."
    (cd "$KSERVE_CLONE" && docker buildx build --platform "$DOCKER_PLATFORM" --load \
      -f llmisvc-controller.Dockerfile \
      -t "$LLMISVC_IMAGE" . 2>&1 | tail -3)
    kind load docker-image "$LLMISVC_IMAGE" --name "$KIND_CLUSTER_NAME"
  fi

  # Deploy the llmisvc controller via kustomize (this also installs CRDs for LLMInferenceServiceConfig)
  kustomize build "$KSERVE_CLONE/config/llmisvc/" | kubectl apply --server-side -f - 2>&1 | tail -5
  kubectl rollout status deployment/llmisvc-controller-manager -n kserve --timeout=120s

  # Apply LLMInferenceServiceConfig templates AFTER the CRD and webhook are ready.
  # These templates tell the LLMIS controller how to create pods, routes, etc.
  kubectl wait --for=condition=Established crd/llminferenceserviceconfigs.serving.kserve.io --timeout=30s
  echo "  Waiting for LLMIS webhook to be ready..."
  _retries=0
  while [[ $_retries -lt 12 ]]; do
    if kubectl apply -f "$KSERVE_CLONE/config/llmisvcconfig/config-llm-template.yaml" -n kserve 2>/dev/null; then
      break
    fi
    sleep 5
    _retries=$((_retries + 1))
  done
  for f in "$KSERVE_CLONE"/config/llmisvcconfig/config-*.yaml; do
    kubectl apply -f "$f" -n kserve
  done
  ok "LLMIS controller installed"
fi

# ─── Step 8: Gateway ───────────────────────────────────────────────────────

step "Creating Gateway"

if kubectl get gateway maas-default-gateway -n "$GATEWAY_NAMESPACE" &>/dev/null; then
  ok "Gateway already exists"
else
  kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: maas-default-gateway
  namespace: ${GATEWAY_NAMESPACE}
spec:
  gatewayClassName: istio
  listeners:
  - name: http
    port: 80
    protocol: HTTP
    allowedRoutes:
      namespaces:
        from: All
EOF

  echo "  Waiting for Gateway to be programmed..."
  kubectl wait --for=condition=Programmed "gateway/maas-default-gateway" \
    -n "$GATEWAY_NAMESPACE" --timeout=120s || \
    warn "Gateway not programmed after 120s (continuing...)"
  ok "Gateway created in ${GATEWAY_NAMESPACE}"
fi

# Configure Istio networking for Kind:
# 1. Permissive mTLS — maas-api pods don't have sidecars
# 2. NetworkPolicy — allow gateway pods (istio-system) to reach maas-api
#    (default policy only allows Authorino from kuadrant-system/openshift-operators)
# TLS origination (gateway → maas-api:8443) is handled by the DestinationRule
# from the TLS kustomize overlay — no manual DR needed here.

# Clean up old HTTP-mode DestinationRule if present (from previous deployments)
kubectl delete destinationrule maas-api-no-mtls -n "$MAAS_NAMESPACE" 2>/dev/null || true

if ! kubectl get peerauthentication maas-permissive -n "$MAAS_NAMESPACE" &>/dev/null; then
  kubectl apply -f - <<EOF
apiVersion: security.istio.io/v1
kind: PeerAuthentication
metadata:
  name: maas-permissive
  namespace: ${MAAS_NAMESPACE}
spec:
  mtls:
    mode: PERMISSIVE
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: maas-gateway-allow
  namespace: ${MAAS_NAMESPACE}
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: maas-api
  policyTypes:
  - Ingress
  ingress:
  - from:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: ${GATEWAY_NAMESPACE}
EOF
  ok "Istio networking configured"
fi

# ─── Step 9: MaaS controller + API ─────────────────────────────────────────

step "Deploying MaaS controller + API"

# Handle arm64: check if images need local build
if [[ "$ARCH" == "arm64" ]]; then
  echo "  Detected Apple Silicon — checking if images need local build..."

  # Check if the image is already loaded in Kind (from a previous run)
  _needs_build=false
  if ! docker exec "${KIND_CLUSTER_NAME}-control-plane" crictl images 2>/dev/null | grep -q "maas-api"; then
    _needs_build=true
  fi

  if [[ "$_needs_build" == "true" ]]; then
    warn "quay.io images are x86-only. Building arm64 images locally..."

    echo "  Building maas-api..."
    (cd "$PROJECT_ROOT/maas-api" && \
      docker buildx build --platform "$DOCKER_PLATFORM" --load \
        -t "$MAAS_API_IMAGE" . 2>&1 | tail -3)
    kind load docker-image "$MAAS_API_IMAGE" --name "$KIND_CLUSTER_NAME"

    echo "  Building maas-controller..."
    (cd "$PROJECT_ROOT" && \
      docker buildx build --platform "$DOCKER_PLATFORM" --load \
        -f maas-controller/Dockerfile \
        -t "$MAAS_CONTROLLER_IMAGE" . 2>&1 | tail -3)
    kind load docker-image "$MAAS_CONTROLLER_IMAGE" --name "$KIND_CLUSTER_NAME"

    # Auto-clone BBR repo if not present
    if [[ -z "$BBR_REPO" || ! -d "$BBR_REPO" ]]; then
      BBR_REPO="$PROJECT_ROOT/../ai-gateway-payload-processing"
      if [[ ! -d "$BBR_REPO" ]]; then
        echo "  Cloning ai-gateway-payload-processing..."
        gh repo clone opendatahub-io/ai-gateway-payload-processing "$BBR_REPO" -- --depth 1 2>&1 | tail -1
      fi
    fi

    echo "  Building payload-processing (BBR)..."
    (cd "$BBR_REPO" && \
      docker buildx build --platform "$DOCKER_PLATFORM" --load \
        -t "$BBR_IMAGE" . 2>&1 | tail -3)
    kind load docker-image "$BBR_IMAGE" --name "$KIND_CLUSTER_NAME"

    ok "arm64 images built and loaded into Kind"
  else
    ok "Images already loaded in Kind"
  fi
fi

# Create subscription namespace
kubectl create namespace "$SUBSCRIPTION_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

# Build kustomize manifests with TLS backend (matching OpenShift's TLS deployment).
# We create a temp overlay directory with symlinks to the real deployment dirs,
# because kustomize does not allow absolute paths in resources/components.
TEMP_DIR=$(mktemp -d)
trap 'rm -rf "$TEMP_DIR"' EXIT

# Symlink deployment dirs so kustomize can reference them with relative paths
ln -s "$PROJECT_ROOT/deployment" "$TEMP_DIR/deployment"

# Create Kind-specific params.env
cat > "$TEMP_DIR/params.env" <<EOF
maas-api-image=${MAAS_API_IMAGE}
maas-controller-image=${MAAS_CONTROLLER_IMAGE}
payload-processing-image=quay.io/opendatahub/odh-ai-gateway-payload-processing:odh-stable
maas-api-key-cleanup-image=docker.io/curlimages/curl:latest
payload-processing-replicas=1
gateway-namespace=${GATEWAY_NAMESPACE}
gateway-name=maas-default-gateway
app-namespace=${MAAS_NAMESPACE}
api-key-max-expiration-days=90
metadata-cache-ttl=60
authz-cache-ttl=60
cluster-audience=https://kubernetes.default.svc
EOF

# Create wrapping kustomization that uses TLS backend with Kind-specific params
cat > "$TEMP_DIR/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

namespace: ${MAAS_NAMESPACE}

configMapGenerator:
- envs:
  - params.env
  name: maas-parameters
generatorOptions:
  disableNameSuffixHash: true

resources:
  - deployment/base/maas-api/overlays/tls
  - deployment/base/maas-controller/default
  - deployment/base/payload-processing/default

components:
  - deployment/components/shared-patches

patches:
  - target:
      kind: NetworkPolicy
      name: maas-authorino-allow
    patch: |
      - op: replace
        path: /spec/ingress/0/from/0/podSelector/matchLabels
        value:
          authorino-resource: authorino
  # Fix EnvoyFilter subFilter name for Kind (istio-system instead of openshift-ingress)
  - target:
      kind: EnvoyFilter
      name: payload-processing
    patch: |
      - op: replace
        path: /spec/configPatches/0/match/listener/filterChain/filter/subFilter/name
        value: "extensions.istio.io/wasmplugin/${GATEWAY_NAMESPACE}.kuadrant-maas-default-gateway"

# Payload-processing resources must be in the gateway namespace (same as ODH overlay)
replacements:
- source:
    kind: ConfigMap
    version: v1
    name: maas-parameters
    fieldPath: data.payload-processing-image
  targets:
  - select:
      kind: Deployment
      name: payload-processing
    fieldPaths:
    - spec.template.spec.containers.[name=payload-processing].image
- source:
    kind: ConfigMap
    version: v1
    name: maas-parameters
    fieldPath: data.payload-processing-replicas
  targets:
  - select:
      kind: Deployment
      name: payload-processing
    fieldPaths:
    - spec.replicas
- source:
    kind: ConfigMap
    version: v1
    name: maas-parameters
    fieldPath: data.gateway-namespace
  targets:
  - select:
      kind: Deployment
      name: payload-processing
    fieldPaths:
    - metadata.namespace
  - select:
      kind: Service
      name: payload-processing
    fieldPaths:
    - metadata.namespace
  - select:
      kind: ServiceAccount
      name: payload-processing
    fieldPaths:
    - metadata.namespace
  - select:
      kind: EnvoyFilter
      name: payload-processing
    fieldPaths:
    - metadata.namespace
  - select:
      kind: DestinationRule
      name: payload-processing
    fieldPaths:
    - metadata.namespace
  - select:
      kind: ConfigMap
      name: payload-processing-plugins
    fieldPaths:
    - metadata.namespace
  - select:
      kind: ClusterRoleBinding
      name: payload-processing-reader
    fieldPaths:
    - subjects.0.namespace
- source:
    kind: ConfigMap
    version: v1
    name: maas-parameters
    fieldPath: data.gateway-name
  targets:
  - select:
      kind: EnvoyFilter
      name: payload-processing
    fieldPaths:
    - spec.targetRefs.0.name
- source:
    kind: ConfigMap
    version: v1
    name: maas-parameters
    fieldPath: data.gateway-namespace
  targets:
  - select:
      kind: DestinationRule
      name: payload-processing
    fieldPaths:
    - spec.host
    options:
      delimiter: "."
      index: 1
  - select:
      kind: EnvoyFilter
      name: payload-processing
    fieldPaths:
    - spec.configPatches.0.patch.value.typed_config.grpc_service.envoy_grpc.cluster_name
    options:
      delimiter: "."
      index: 1
# Replace gateway namespace in maas-api-backend-tls DestinationRule
# (host replacement is handled by shared-patches component)
- source:
    kind: ConfigMap
    version: v1
    name: maas-parameters
    fieldPath: data.gateway-namespace
  targets:
  - select:
      kind: DestinationRule
      name: maas-api-backend-tls
    fieldPaths:
    - metadata.namespace
EOF

echo "  Building kustomize manifests..."
# Filter out PodMonitor and ServiceMonitor resources (require Prometheus Operator CRDs, not needed locally)
MANIFESTS_FILE="$TEMP_DIR/manifests.yaml"
kustomize build --load-restrictor LoadRestrictionsNone "$TEMP_DIR" > "$MANIFESTS_FILE"

# Remove monitoring resources that need Prometheus Operator
python3 -c "
import sys
with open('$MANIFESTS_FILE') as f:
    content = f.read()
docs = content.split('\n---\n')
filtered = []
for doc in docs:
    if not doc.strip():
        continue
    if 'kind: PodMonitor' in doc or 'kind: ServiceMonitor' in doc:
        continue
    filtered.append(doc)
print('\n---\n'.join(filtered))
" > "$TEMP_DIR/filtered.yaml"

if ! kubectl apply --server-side=true --force-conflicts -f "$TEMP_DIR/filtered.yaml" 2>&1 | tail -15; then
  fail "Failed to apply MaaS manifests"
  exit 1
fi

echo "  Waiting for MaaS API..."
kubectl rollout status deployment/maas-api -n "$MAAS_NAMESPACE" --timeout=180s 2>/dev/null || \
  warn "maas-api not ready yet"

# Disable sidecar injection on payload-processing (BBR uses self-signed TLS for ext-proc)
kubectl patch deployment payload-processing -n "$GATEWAY_NAMESPACE" --type=merge \
  -p='{"spec":{"template":{"metadata":{"annotations":{"sidecar.istio.io/inject":"false"}}}}}' 2>/dev/null || true

# Fix controller's GATEWAY_NAMESPACE — base manifest hardcodes openshift-ingress
CURRENT_GW_NS=$(kubectl get deployment maas-controller -n "$MAAS_NAMESPACE" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="GATEWAY_NAMESPACE")].value}' 2>/dev/null)
if [[ "$CURRENT_GW_NS" != "$GATEWAY_NAMESPACE" ]]; then
  kubectl set env deployment/maas-controller -n "$MAAS_NAMESPACE" GATEWAY_NAMESPACE="$GATEWAY_NAMESPACE"
fi

echo "  Waiting for MaaS controller..."
kubectl rollout status deployment/maas-controller -n "$MAAS_NAMESPACE" --timeout=180s 2>/dev/null || \
  warn "maas-controller not ready yet"

ok "MaaS controller + API deployed"

# ─── Step 10b: Test fixtures ────────────────────────────────────────────────

step "Deploying test fixtures"

LLM_KATAN_FQDN="${LLM_KATAN_FQDN:-3-13-21-181.sslip.io}"
MODEL_NAMESPACE="llm"
INTERNAL_MODEL_NAMESPACE="llm-internal"

kubectl create namespace "$MODEL_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace "$INTERNAL_MODEL_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

# ── External model (ExternalModel → llm-katan on AWS) ──
if kubectl get externalmodel llm-katan-openai -n "$MODEL_NAMESPACE" &>/dev/null; then
  ok "External model fixtures already deployed"
else
  echo "  Deploying external model (llm-katan)..."
  kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: llm-katan-creds
  namespace: ${MODEL_NAMESPACE}
  labels:
    inference.networking.k8s.io/bbr-managed: "true"
stringData:
  api-key: "llm-katan-openai-key"
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: llm-katan-openai
  namespace: ${MODEL_NAMESPACE}
spec:
  endpoint: "${LLM_KATAN_FQDN}"
  provider: openai
  targetModel: llm-katan-echo
  credentialRef:
    name: llm-katan-creds
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: llm-katan-openai
  namespace: ${MODEL_NAMESPACE}
spec:
  modelRef:
    kind: ExternalModel
    name: llm-katan-openai
EOF
  ok "External model deployed"
fi

# ── Internal model (LLMInferenceService → llm-d simulator in-cluster) ──
if kubectl get llminferenceservice sim-internal -n "$INTERNAL_MODEL_NAMESPACE" &>/dev/null; then
  ok "Internal model fixtures already deployed"
else
  echo "  Deploying internal model (llm-d simulator)..."
  kubectl apply -f - <<EOF
apiVersion: serving.kserve.io/v1alpha1
kind: LLMInferenceService
metadata:
  name: sim-internal
  namespace: ${INTERNAL_MODEL_NAMESPACE}
spec:
  model:
    uri: hf://sshleifer/tiny-gpt2
    name: facebook/opt-125m
  replicas: 1
  router:
    route: {}
    gateway:
      refs:
        - name: maas-default-gateway
          namespace: ${GATEWAY_NAMESPACE}
  template:
    containers:
      - name: main
        image: "ghcr.io/llm-d/llm-d-inference-sim:v0.7.1"
        command: ["/app/llm-d-inference-sim"]
        args:
        - --port
        - "8000"
        - --model
        - facebook/opt-125m
        - --mode
        - random
        env:
          - name: POD_NAME
            valueFrom:
              fieldRef:
                apiVersion: v1
                fieldPath: metadata.name
          - name: POD_NAMESPACE
            valueFrom:
              fieldRef:
                apiVersion: v1
                fieldPath: metadata.namespace
        ports:
          - name: http
            containerPort: 8000
            protocol: TCP
        resources:
          requests:
            cpu: 100m
            memory: 256Mi
          limits:
            cpu: 500m
            memory: 512Mi
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: sim-internal
  namespace: ${INTERNAL_MODEL_NAMESPACE}
spec:
  modelRef:
    kind: LLMInferenceService
    name: sim-internal
EOF
  ok "Internal model deployed"
fi

# ── Shared subscription + auth policy (covers both models) ──
if kubectl get maassubscription simulator-subscription -n "$SUBSCRIPTION_NAMESPACE" &>/dev/null; then
  ok "Subscriptions already deployed"
else
  echo "  Deploying subscriptions and auth policies..."
  kubectl apply -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: simulator-subscription
  namespace: ${SUBSCRIPTION_NAMESPACE}
spec:
  owner:
    groups:
      - name: system:authenticated
    users: []
  modelRefs:
    - name: llm-katan-openai
      namespace: ${MODEL_NAMESPACE}
      tokenRateLimits:
        - limit: 100
          window: 1m
    - name: sim-internal
      namespace: ${INTERNAL_MODEL_NAMESPACE}
      tokenRateLimits:
        - limit: 100
          window: 1m
  priority: 10
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: simulator-access
  namespace: ${SUBSCRIPTION_NAMESPACE}
spec:
  modelRefs:
    - name: llm-katan-openai
      namespace: ${MODEL_NAMESPACE}
    - name: sim-internal
      namespace: ${INTERNAL_MODEL_NAMESPACE}
  subjects:
    groups:
      - name: system:authenticated
    users: []
EOF
  ok "Subscriptions and auth policies deployed"
fi

# Wait for reconciliation
echo "  Waiting for controller to reconcile fixtures..."
sleep 20
EXTERNAL_PHASE=$(kubectl get maasmodelref llm-katan-openai -n "$MODEL_NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null)
INTERNAL_PHASE=$(kubectl get maasmodelref sim-internal -n "$INTERNAL_MODEL_NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null)
[[ "$EXTERNAL_PHASE" == "Ready" ]] && ok "External model: Ready" || warn "External model: ${EXTERNAL_PHASE:-unknown}"
[[ "$INTERNAL_PHASE" == "Ready" ]] && ok "Internal model: Ready" || warn "Internal model: ${INTERNAL_PHASE:-Pending (simulator pod may still be starting)}"

# Note: llm-katan uses Let's Encrypt certs (via certbot + sslip.io), so Istio's
# tls.mode=SIMPLE cert verification passes without insecureSkipVerify.

# ─── Smoke test ─────────────────────────────────────────────────────────────

step "Running smoke test"

# Port-forward gateway and verify routing works
kubectl port-forward -n "$GATEWAY_NAMESPACE" svc/maas-default-gateway-istio 19090:80 > /dev/null 2>&1 &
PF_PID=$!
sleep 3

# Test direct maas-api health (bypasses gateway)
HEALTH=$(kubectl exec -n "$MAAS_NAMESPACE" deployment/maas-api -- curl -sk https://localhost:8443/health 2>/dev/null || echo "")
if [[ "$HEALTH" == *"healthy"* ]]; then
  ok "MaaS API health check passed"
else
  warn "MaaS API health check returned: ${HEALTH:-no response}"
fi

# Test gateway routing (should get 401 — auth is working)
HTTP_CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://localhost:19090/v1/models 2>/dev/null || echo "000")
if [[ "$HTTP_CODE" == "401" ]]; then
  ok "Gateway routing + auth working (401 Unauthorized — expected without API key)"
elif [[ "$HTTP_CODE" == "200" ]]; then
  ok "Gateway routing working (200 OK)"
else
  warn "Gateway returned HTTP ${HTTP_CODE} (may need a moment to stabilize)"
fi

kill $PF_PID 2>/dev/null
wait $PF_PID 2>/dev/null || true

# ─── Summary ────────────────────────────────────────────────────────────────

echo ""
echo -e "${BOLD}${GREEN}════════════════════════════════════════════════${NC}"
echo -e "${BOLD}${GREEN}  MaaS Local Deployment Complete${NC}"
echo -e "${BOLD}${GREEN}════════════════════════════════════════════════${NC}"
echo ""
echo -e "  ${BOLD}Cluster:${NC}     kind-${KIND_CLUSTER_NAME}"
echo -e "  ${BOLD}Namespace:${NC}   ${MAAS_NAMESPACE}"
echo -e "  ${BOLD}Gateway:${NC}     maas-default-gateway (${GATEWAY_NAMESPACE})"
echo ""
echo -e "  ${BOLD}Test:${NC}"
echo "    # Port-forward the gateway"
echo "    kubectl port-forward -n ${GATEWAY_NAMESPACE} svc/maas-default-gateway-istio 8080:80 &"
echo ""
echo "    # List models (expect 401 — auth is enforced)"
echo "    curl -v http://localhost:8080/v1/models"
echo ""
echo "    # Direct MaaS API health check (bypasses gateway)"
echo "    kubectl port-forward -n ${MAAS_NAMESPACE} svc/maas-api 9090:8443 &"
echo "    curl -k https://localhost:9090/health"
echo ""
echo -e "  ${BOLD}Check status:${NC}"
echo "    $0 --status"
echo ""
echo -e "  ${BOLD}Teardown:${NC}"
echo "    $0 --teardown"
echo ""
