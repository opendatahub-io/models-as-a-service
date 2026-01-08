#!/usr/bin/env bash

set -euo pipefail

# MaaS Local Development Setup with Kind
# Supports Mac (Intel/ARM) and Linux (x86_64/ARM64)
# Note: Uses fast inference simulators for testing (not real LLM models)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
KIND_CONFIG="$PROJECT_ROOT/deployment/overlays/kind/kind-config.yaml"
CLUSTER_NAME="maas-local"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Logging functions
log_info() {
    echo -e "${BLUE}ℹ ${NC}$1"
}

log_success() {
    echo -e "${GREEN}✅${NC} $1"
}

log_warn() {
    echo -e "${YELLOW}⚠️ ${NC}$1"
}

log_error() {
    echo -e "${RED}❌${NC} $1"
}

# Check if a command exists
command_exists() {
    command -v "$1" >/dev/null 2>&1
}

# Check prerequisites
check_prerequisites() {
    log_info "Checking prerequisites..."

    local missing_tools=()

    # Required tools (Docker or Podman)
    if ! command_exists docker && ! command_exists podman; then
        missing_tools+=("docker or podman")
    fi

    if ! command_exists kubectl; then
        missing_tools+=("kubectl")
    fi

    if ! command_exists kind; then
        missing_tools+=("kind")
    fi

    if ! command_exists istioctl; then
        missing_tools+=("istioctl")
    fi

    if ! command_exists helm; then
        missing_tools+=("helm")
    fi

    if [ ${#missing_tools[@]} -gt 0 ]; then
        log_error "Missing required tools: ${missing_tools[*]}"
        echo ""
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "To install prerequisites, run:"
        echo "  ./install-prerequisites.sh"
        echo ""
        echo "Or install manually:"
        echo ""
        echo "Mac (Homebrew):"
        echo "  brew install kubectl kind istioctl helm"
        echo ""
        echo "Linux:"
        echo "  See README.md for instructions"
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo ""
        exit 1
    fi

    # Check container engine is running
    if command_exists docker && docker ps >/dev/null 2>&1; then
        log_success "Docker is running"
    elif command_exists podman && podman ps >/dev/null 2>&1; then
        log_success "Podman is running"
    else
        log_error "Container engine is not running. Please start Docker Desktop (Mac) or Docker/Podman Engine (Linux)"
        exit 1
    fi

    log_success "All prerequisites satisfied"
}

# Create Kind cluster
create_cluster() {
    log_info "Creating Kind cluster '$CLUSTER_NAME'..."

    # Check if cluster already exists
    if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
        log_warn "Cluster '$CLUSTER_NAME' already exists"
        read -p "Delete and recreate? (y/N): " -n 1 -r
        echo
        if [[ $REPLY =~ ^[Yy]$ ]]; then
            log_info "Deleting existing cluster..."
            kind delete cluster --name "$CLUSTER_NAME"
        else
            log_info "Using existing cluster"
            return 0
        fi
    fi

    # Create cluster
    if ! kind create cluster --config "$KIND_CONFIG" --name "$CLUSTER_NAME"; then
        log_error "Failed to create Kind cluster"
        exit 1
    fi

    # Set kubectl context
    kubectl config use-context "kind-${CLUSTER_NAME}"

    log_success "Kind cluster created successfully"
}

# Install Gateway API CRDs
install_gateway_api() {
    log_info "Installing Gateway API CRDs..."

    local gateway_api_version="v1.2.1"
    # Suppress unrecognized format warnings 
    kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${gateway_api_version}/standard-install.yaml" 2>/dev/null || {
        log_warn "Some warnings occurred during Gateway API installation (this is normal)"
        kubectl apply -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${gateway_api_version}/standard-install.yaml" >/dev/null
    }

    log_success "Gateway API CRDs installed"
}

# Install cert-manager
install_cert_manager() {
    log_info "Installing cert-manager..."

    local cert_manager_version="v1.19.1"
    # Suppress format warnings
    kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${cert_manager_version}/cert-manager.yaml" 2>/dev/null || {
        log_warn "Some warnings occurred during cert-manager installation (this is normal)"
        kubectl apply -f "https://github.com/cert-manager/cert-manager/releases/download/${cert_manager_version}/cert-manager.yaml" >/dev/null
    }

    # Wait for cert-manager to be ready
    log_info "Waiting for cert-manager to be ready..."
    kubectl wait --for=condition=Available deployment/cert-manager -n cert-manager --timeout=300s >/dev/null 2>&1
    kubectl wait --for=condition=Available deployment/cert-manager-webhook -n cert-manager --timeout=300s >/dev/null 2>&1
    kubectl wait --for=condition=Available deployment/cert-manager-cainjector -n cert-manager --timeout=300s >/dev/null 2>&1

    log_success "cert-manager installed"
}

# Install Istio
install_istio() {
    log_info "Installing Istio..."

    # Install with minimal profile (Gateway API will auto-provision gateways)
    # Suppress the Istio banner and verbose output
    istioctl install --set profile=minimal -y >/dev/null 2>&1

    # Wait for Istio control plane to be ready
    log_info "Waiting for Istio to be ready..."
    kubectl wait --for=condition=Available deployment/istiod -n istio-system --timeout=300s >/dev/null 2>&1

    log_success "Istio installed"
}

# Install Knative Serving (required for KServe)
install_knative_serving() {
    log_info "Installing Knative Serving..."

    local knative_version="knative-v1.10.1"
    local knative_istio_version="knative-v1.10.0"

    # Install Knative Serving CRDs (suppress format warnings)
    kubectl apply -f "https://github.com/knative/serving/releases/download/${knative_version}/serving-crds.yaml" 2>/dev/null || {
        log_warn "Some warnings occurred during Knative CRDs installation (this is normal)"
        kubectl apply -f "https://github.com/knative/serving/releases/download/${knative_version}/serving-crds.yaml" >/dev/null
    }

    # Install Knative Serving Core
    log_info "Installing Knative core components..."
    kubectl apply -f "https://github.com/knative/serving/releases/download/${knative_version}/serving-core.yaml" >/dev/null 2>&1

    # Install Knative Istio integration
    log_info "Installing Knative-Istio integration..."
    kubectl apply -f "https://github.com/knative/net-istio/releases/download/${knative_istio_version}/release.yaml" >/dev/null 2>&1

    # Configure domain for local development
    kubectl patch cm config-domain \
      --patch '{"data":{"example.com":""}}' \
      -n knative-serving >/dev/null 2>&1 || log_warn "Failed to patch domain config"

    # Wait for Knative Serving to be ready
    log_info "Waiting for Knative Serving to be ready..."
    kubectl wait --for=condition=Ready pod --all -n knative-serving --timeout=300s >/dev/null 2>&1 || log_warn "Some Knative pods may not be ready yet"

    log_success "Knative Serving installed"
}

# Install Kuadrant via Helm
install_kuadrant() {
    log_info "Installing Kuadrant..."

    # Create namespace
    kubectl create namespace kuadrant-system --dry-run=client -o yaml | kubectl apply -f - >/dev/null 2>&1

    # Add Helm repo
    log_info "Setting up Helm repository..."
    helm repo add kuadrant https://kuadrant.io/helm-charts/ >/dev/null 2>&1 || true
    helm repo update >/dev/null 2>&1

    # Install Kuadrant operator
    log_info "Installing Kuadrant operator (this may take a few minutes)..."
    helm upgrade --install kuadrant-operator kuadrant/kuadrant-operator \
        -n kuadrant-system \
        --create-namespace \
        --wait \
        --timeout 5m >/dev/null 2>&1 || {
        log_warn "Kuadrant installation may have warnings, checking status..."
        helm upgrade --install kuadrant-operator kuadrant/kuadrant-operator \
            -n kuadrant-system \
            --create-namespace \
            --wait \
            --timeout 5m 2>&1 | grep -E "(Error|Failed)" || true
    }

    log_success "Kuadrant installed"
}

# Install KServe
install_kserve() {
    log_info "Installing KServe..."

    local kserve_version="v0.16.0"
    # Use server-side apply to handle large CRD annotations (>256KB limit)
    log_info "Downloading and applying KServe components (this may take a few minutes)..."
    
    # Apply KServe with retries to handle webhook timing issues
    local max_apply_retries=3
    local apply_retry_count=0
    
    while [ $apply_retry_count -lt $max_apply_retries ]; do
        # Temporarily disable exit-on-error for kubectl apply
        set +e
        kubectl apply --server-side -f "https://github.com/kserve/kserve/releases/download/${kserve_version}/kserve.yaml" >/dev/null 2>&1
        local apply_result=$?
        set -e
        
        if [ $apply_result -eq 0 ]; then
            log_success "KServe components applied successfully"
            break
        else
            apply_retry_count=$((apply_retry_count + 1))
            if [ $apply_retry_count -lt $max_apply_retries ]; then
                log_warn "KServe installation attempt $apply_retry_count failed (webhook timing), retrying in 30 seconds..."
                sleep 30
            else
                log_warn "Some webhook errors during KServe installation (this is normal)"
                # Final attempt - this should succeed even with webhook warnings
                set +e
                kubectl apply --server-side -f "https://github.com/kserve/kserve/releases/download/${kserve_version}/kserve.yaml" >/dev/null 2>&1
                set -e
                log_success "KServe components applied (webhook warnings ignored)"
                break
            fi
        fi
    done

    # Wait for CRDs to be established first
    log_info "Waiting for KServe CRDs to be established..."
    kubectl wait --for condition=established --timeout=300s crd/inferenceservices.serving.kserve.io >/dev/null 2>&1 || log_warn "InferenceService CRD not ready"
    kubectl wait --for condition=established --timeout=300s crd/llminferenceservices.serving.kserve.io >/dev/null 2>&1 || log_warn "LLMInferenceService CRD not ready"
    kubectl wait --for condition=established --timeout=300s crd/llminferenceserviceconfigs.serving.kserve.io >/dev/null 2>&1 || log_warn "LLMInferenceServiceConfig CRD not ready"

    # Wait for KServe controllers to be ready
    log_info "Waiting for KServe controllers to be ready..."
    kubectl wait --for=condition=Available deployment/kserve-controller-manager -n kserve --timeout=300s >/dev/null 2>&1 || log_warn "KServe controller may not be fully ready"
    kubectl wait --for=condition=Available deployment/llmisvc-controller-manager -n kserve --timeout=300s >/dev/null 2>&1 || log_warn "LLMInferenceService controller may not be fully ready"

    # Wait for webhook services to be ready
    log_info "Waiting for KServe webhook services to be ready..."
    kubectl wait --for=condition=Ready pod -l control-plane=kserve-controller-manager -n kserve --timeout=300s >/dev/null 2>&1 || log_warn "KServe controller pods may not be ready"
    kubectl wait --for=condition=Ready pod -l control-plane=llmisvc-controller-manager -n kserve --timeout=300s >/dev/null 2>&1 || log_warn "LLMInferenceService controller pods may not be ready"

    # Give webhooks a moment to register and become ready
    log_info "Allowing webhook services to initialize..."
    sleep 10

    # Configure KServe for serverless mode (using Knative)
    log_info "Configuring KServe for serverless mode..."
    kubectl patch configmap/inferenceservice-config \
      -n kserve \
      --type=merge \
      -p '{"data":{"deploy":"{\"defaultDeploymentMode\":\"Serverless\"}"}}' >/dev/null 2>&1 || log_warn "Failed to configure serverless mode"

    log_success "KServe installed"
}

# Create custom LLMInferenceServiceConfig for simplified model deployment
configure_llm_inference_service() {
    log_info "Configuring LLMInferenceService with custom config..."
    
    # Test webhook readiness first
    local max_retries=10
    local retry_count=0
    
    while [ $retry_count -lt $max_retries ]; do
        log_info "Testing webhook readiness (attempt $((retry_count + 1))/$max_retries)..."
        
        # Create a simplified LLMInferenceServiceConfig that works with our test models
        set +e
        cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: serving.kserve.io/v1alpha1
kind: LLMInferenceServiceConfig
metadata:
  name: kserve-config-llm-template
  namespace: kserve
spec:
  template:
    containers:
    - name: kserve-container
      image: ghcr.io/llm-d/llm-d-inference-sim:v0.5.1
      ports:
      - containerPort: 8080
        protocol: TCP
      command: ["/app/llm-d-inference-sim"]
      args: ["--port", "8080", "--mode", "random"]
      resources:
        requests:
          cpu: "100m"
          memory: "128Mi"
        limits:
          cpu: "500m"
          memory: "256Mi"
EOF
        local config_result=$?
        set -e
        
        if [ $config_result -eq 0 ]; then
            log_success "LLMInferenceService configured"
            return 0
        else
            retry_count=$((retry_count + 1))
            if [ $retry_count -lt $max_retries ]; then
                log_warn "Webhook not ready yet, waiting 15 seconds before retry..."
                sleep 15
            else
                log_error "Failed to configure LLMInferenceService after $max_retries attempts"
                return 1
            fi
        fi
    done
}

# Build local MaaS API image with K8s-first code
build_maas_api_image() {
    log_info "Building local maas-api image with K8s-first code..."

    cd "$PROJECT_ROOT/maas-api"
    
    # Determine container engine and build image
    if command_exists docker && docker ps >/dev/null 2>&1; then
        log_info "Building MaaS API image with Docker..."
        make build-image CONTAINER_ENGINE=docker REPO=localhost/maas-api TAG=dev >/dev/null 2>&1 || {
            log_error "Failed to build maas-api image with Docker"
            return 1
        }
    elif command_exists podman && podman ps >/dev/null 2>&1; then
        log_info "Building MaaS API image with Podman..."
        make build-image CONTAINER_ENGINE=podman REPO=localhost/maas-api TAG=dev >/dev/null 2>&1 || {
            log_error "Failed to build maas-api image with Podman"
            return 1
        }
    else
        log_error "No working container engine found"
        return 1
    fi

    log_info "Loading maas-api image into Kind cluster..."
    kind load docker-image localhost/maas-api:dev --name "$CLUSTER_NAME" >/dev/null 2>&1 || {
        log_error "Failed to load image into Kind"
        return 1
    }

    cd "$PROJECT_ROOT"
    log_success "MaaS API image built and loaded"
}

# Deploy MaaS components
deploy_maas() {
    log_info "Deploying MaaS components..."

    # Build local maas-api image first
    build_maas_api_image

    # Apply Kustomize overlay
    log_info "Applying MaaS configuration..."
    kubectl apply -k "$PROJECT_ROOT/deployment/overlays/kind/" >/dev/null 2>&1 || {
        log_warn "Some warnings during MaaS deployment (checking for details...)"
        kubectl apply -k "$PROJECT_ROOT/deployment/overlays/kind/" 2>&1 | grep -E "(Error|Failed)" || true
    }

    # Wait for Gateway to be created and service to be provisioned
    log_info "Waiting for Gateway to be provisioned..."
    sleep 5
    kubectl wait --for=condition=Programmed gateway/maas-gateway -n istio-system --timeout=300s >/dev/null 2>&1 || true

    # Patch the Gateway service to use fixed NodePorts (30080, 30443)
    # This is required for Kind's extraPortMappings to work
    log_info "Configuring Gateway service NodePorts..."
    kubectl patch svc maas-gateway-istio -n istio-system --type='json' -p='[
      {"op":"replace","path":"/spec/ports/1/nodePort","value":30080},
      {"op":"replace","path":"/spec/ports/2/nodePort","value":30443}
    ]' >/dev/null 2>&1 || log_warn "Failed to patch NodePorts (may already be set)"

    # Wait for MaaS API to be ready
    log_info "Waiting for MaaS API to be ready..."
    kubectl wait --for=condition=Available deployment/maas-api -n maas-api --timeout=300s >/dev/null 2>&1 || true

    log_success "MaaS components deployed"
}

# Validate deployment
validate_deployment() {
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    log_info "🔍 Validating deployment..."
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

    # Wait for critical pods to be ready
    log_info "Waiting for critical services to be ready..."
    kubectl wait --for=condition=ready pod -l authorino-resource=authorino -n kuadrant-system --timeout=60s >/dev/null 2>&1 || true
    kubectl wait --for=condition=ready pod -l app=limitador -n kuadrant-system --timeout=60s >/dev/null 2>&1 || true
    kubectl wait --for=condition=ready pod -l app.kubernetes.io/name=maas-api -n maas-api --timeout=60s >/dev/null 2>&1 || true

    # Give policies a moment to reconcile
    sleep 5

    echo ""
    log_info "Checking core components..."

    local validation_failed=false

    # Check Kuadrant instance
    if kubectl get kuadrant -n kuadrant-system kuadrant >/dev/null 2>&1; then
        log_success "✓ Kuadrant instance running"
    else
        log_error "✗ Kuadrant instance not found"
        validation_failed=true
    fi

    # Check Gateway
    if kubectl get gateway -n istio-system maas-gateway >/dev/null 2>&1; then
        log_success "✓ Gateway deployed"
    else
        log_error "✗ Gateway not found"
        validation_failed=true
    fi

    # Check core service pods
    log_info "Checking service pods..."
    
    if kubectl get pods -n istio-system -l app=istiod --no-headers 2>/dev/null | grep -q "Running"; then
        log_success "✓ Istio control plane running"
    else
        log_error "✗ Istio control plane not running"
        validation_failed=true
    fi

    if kubectl get pods -n kuadrant-system -l authorino-resource=authorino --no-headers 2>/dev/null | grep -q "Running"; then
        log_success "✓ Authorino running"
    else
        log_error "✗ Authorino not running"
        validation_failed=true
    fi

    if kubectl get pods -n kuadrant-system -l app=limitador --no-headers 2>/dev/null | grep -q "Running"; then
        log_success "✓ Limitador running"
    else
        log_error "✗ Limitador not running"
        validation_failed=true
    fi

    if kubectl get pods -n maas-api -l app.kubernetes.io/name=maas-api --no-headers 2>/dev/null | grep -q "Running"; then
        log_success "✓ MaaS API running"
    else
        log_error "✗ MaaS API not running"
        validation_failed=true
    fi

    # Test connectivity
    log_info "Testing gateway connectivity..."
    http_code=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 3 http://localhost/v1/models 2>/dev/null || echo "000")
    if [ "$http_code" == "200" ] || [ "$http_code" == "401" ]; then
        log_success "✓ Gateway accessible (HTTP $http_code - auth required)"
    else
        log_warn "⚠ Gateway not accessible yet (HTTP $http_code) - may take a few moments"
    fi

    # Check policy enforcement status 
    log_info "Checking policy enforcement..."
    auth_enforced=$(kubectl get authpolicy -n istio-system -o jsonpath='{.items[0].status.conditions[?(@.type=="Enforced")].status}' 2>/dev/null || echo "False")
    if [ "$auth_enforced" == "True" ]; then
        log_success "✓ Authentication policies enforced"
    else
        log_warn "⚠ Authentication policies not enforced yet (may take a few seconds)"
    fi

    echo ""
    if [ "$validation_failed" = true ]; then
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        log_warn "⚠ Some validation checks failed. Run 'kubectl get pods -A' to investigate."
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    else
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        log_success "🎉 All validation checks passed!"
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    fi
}

# Print access information
print_access_info() {
    local models_deployed="${1:-false}"

    echo ""
    echo "🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉"
    log_success "🚀 MaaS Local Environment is Ready!"
    echo "🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉🎉"
    echo ""
    echo "📡 Access Information:"
    echo "  🌐 Gateway:     http://localhost/"
    echo "  🔌 MaaS API:    http://localhost/maas-api/"
    echo "  💗 Health:      http://localhost/maas-api/health"
    echo ""

    if [ "$models_deployed" = true ]; then
        echo "🤖 Test Models Deployed (Dual CRD Architecture):"
        echo "  ✅ model-a (InferenceService - free tier, all users)"
        echo "  🚀 model-b (LLMInferenceService - premium tier, premium/enterprise only)"
        echo ""
        echo "🧪 Quick Test Commands:"
        echo ""
        echo "1️⃣  Get authentication token:"
        echo "   TOKEN=\$(kubectl create token free-user -n maas-api --audience=maas-default-gateway-sa --duration=1h)"
        echo ""
        echo "2️⃣  List all models (dual CRD discovery):"
        echo "   curl -H \"Authorization: Bearer \$TOKEN\" http://localhost/v1/models | jq ."
        echo ""
        echo "3️⃣  Test model-a (InferenceService):"
        echo "  curl -H \"Authorization: Bearer \$TOKEN\" \\"
        echo "    -H 'Content-Type: application/json' \\"
        echo "    -d '{\"model\":\"model-a\",\"messages\":[{\"role\":\"user\",\"content\":\"Hello\"}],\"max_tokens\":20}' \\"
        echo "    http://localhost/llm/model-a/v1/chat/completions"
        echo ""
        echo "Run Demo:"
        echo "  cd deployment/scripts/kind && ./demo.sh"
        echo ""
    else
        echo "Deploy Test Models:"
        echo "  kubectl apply -k deployment/overlays/kind/test-models/model-a"
        echo "  kubectl apply -k deployment/overlays/kind/test-models/model-b"
        echo ""
    fi

    echo "Get Auth Tokens:"
    echo "  Free:       kubectl create token free-user -n maas-api --audience=maas-default-gateway-sa --duration=1h"
    echo "  Premium:    kubectl create token premium-user -n maas-api --audience=maas-default-gateway-sa --duration=1h"
    echo "  Enterprise: kubectl create token enterprise-user -n maas-api --audience=maas-default-gateway-sa --duration=1h"
    echo ""
    echo "Start Frontend/Backend:"
    echo "  cd apps && ./scripts/start-dev.sh"
    echo ""
    echo "Cleanup:"
    echo "  kind delete cluster --name $CLUSTER_NAME"
    echo ""
}

# Usage information
usage() {
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Options:"
    echo "  --check              Check prerequisites only"
    echo "  --cluster-only       Create cluster only (skip component installation)"
    echo "  --skip-maas          Skip MaaS deployment (infrastructure only)"
    echo "  --without-models     Skip test model deployment (default: models included)"
    echo "  -h, --help           Show this help message"
    echo ""
    echo "Examples:"
    echo "  $0                          # Full setup with test models (default)"
    echo "  $0 --without-models         # Setup without test models"
    echo "  $0 --check                  # Check prerequisites only"
    echo "  $0 --cluster-only           # Create cluster only"
    echo ""
}

# Deploy test models
deploy_test_models() {
    log_info "Deploying test models (model-a and model-b)..."

    # Deploy model-a (free tier - accessible to all users)
    if [ -d "$PROJECT_ROOT/deployment/overlays/kind/test-models/model-a" ]; then
        log_info "Deploying model-a (InferenceService - free tier)..."
        kubectl apply -k "$PROJECT_ROOT/deployment/overlays/kind/test-models/model-a"

        # Wait for InferenceService to be ready (simulator should be fast)
        log_info "Waiting for model-a (InferenceService) to be ready..."
        if kubectl wait --for=condition=Ready inferenceservice/model-a -n llm --timeout=30s; then
            log_success "model-a (InferenceService) is ready"
        else
            log_warn "model-a is taking longer than expected (check pod status)"
            # Check pod status for debugging
            kubectl get pods -n llm -l serving.kserve.io/inferenceservice=model-a
        fi
    else
        log_warn "model-a directory not found, skipping..."
    fi

    # Deploy model-b (premium tier - accessible to premium/enterprise users only)
    if [ -d "$PROJECT_ROOT/deployment/overlays/kind/test-models/model-b" ]; then
        log_info "Deploying model-b (LLMInferenceService - premium tier)..."
        kubectl apply -k "$PROJECT_ROOT/deployment/overlays/kind/test-models/model-b"

        # Wait for LLMInferenceService to be ready (simulator should be fast)
        log_info "Waiting for model-b (LLMInferenceService) to be ready..."
        if kubectl wait --for=condition=Ready llminferenceservice/model-b -n llm --timeout=30s; then
            log_success "model-b (LLMInferenceService) is ready"
        else
            log_warn "model-b is taking longer than expected (check pod status)"
            # Check pod status for debugging
            kubectl get pods -n llm -l serving.kserve.io/inferenceservice=model-b
            kubectl describe llminferenceservice/model-b -n llm | tail -10
        fi
    else
        log_warn "model-b directory not found, skipping..."
    fi

    # Show final status of both models
    log_info "Final model status:"
    echo "InferenceService status:"
    kubectl get inferenceservice -n llm || echo "No InferenceServices found"
    echo "LLMInferenceService status:"  
    kubectl get llminferenceservice -n llm || echo "No LLMInferenceServices found"
    echo "Pod status:"
    kubectl get pods -n llm

    log_success "Test models deployment completed"
}

# Main execution
main() {
    local check_only=false
    local cluster_only=false
    local skip_maas=false
    local with_test_models=true  # Default: deploy test models

    # Parse arguments
    while [[ $# -gt 0 ]]; do
        case $1 in
            --check)
                check_only=true
                shift
                ;;
            --cluster-only)
                cluster_only=true
                shift
                ;;
            --skip-maas)
                skip_maas=true
                shift
                ;;
            --without-models)
                with_test_models=false
                shift
                ;;
            --with-test-models)
                # Keep for backwards compatibility
                with_test_models=true
                shift
                ;;
            -h|--help)
                usage
                exit 0
                ;;
            *)
                log_error "Unknown option: $1"
                usage
                exit 1
                ;;
        esac
    done

    echo "🚀 MaaS Local Development Setup"
    echo ""

    # Check prerequisites
    check_prerequisites

    if [ "$check_only" = true ]; then
        exit 0
    fi

    # Create cluster
    create_cluster

    if [ "$cluster_only" = true ]; then
        log_success "Cluster created. Use kubectl to interact with it."
        exit 0
    fi

    # Install components
    install_gateway_api
    install_cert_manager
    install_istio
    install_knative_serving
    install_kuadrant
    install_kserve
    configure_llm_inference_service

    if [ "$skip_maas" = false ]; then
        deploy_maas
    fi

    # Deploy test models if requested
    if [ "$with_test_models" = true ]; then
        deploy_test_models
        MODELS_DEPLOYED=true
    else
        MODELS_DEPLOYED=false
    fi

    # Validate
    validate_deployment

    # Print access info
    print_access_info "$MODELS_DEPLOYED"
}

# Run main function
main "$@"
