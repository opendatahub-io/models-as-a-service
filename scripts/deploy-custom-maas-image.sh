#!/usr/bin/env bash

# deploy-custom-maas-image.sh
# Deploys a custom maas-api image to an ODH cluster with all necessary fixes
#
# Usage:
#   ./scripts/deploy-custom-maas-image.sh <custom-image-url>
#
# Example:
#   ./scripts/deploy-custom-maas-image.sh quay.io/myuser/maas-api:my-tag

set -euo pipefail

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
NAMESPACE="${NAMESPACE:-opendatahub}"
TIMEOUT="${TIMEOUT:-300}"  # 5 minutes

# Helper functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $*"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $*"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $*"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $*"
}

check_prerequisites() {
    log_info "Checking prerequisites..."

    local missing=()

    command -v kubectl &>/dev/null || missing+=("kubectl")
    command -v jq &>/dev/null || missing+=("jq")

    if [[ ${#missing[@]} -gt 0 ]]; then
        log_error "Missing required tools: ${missing[*]}"
        exit 1
    fi

    # Check cluster connection
    if ! kubectl cluster-info &>/dev/null; then
        log_error "Cannot connect to Kubernetes cluster"
        log_error "Please ensure you are logged in (oc login or kubectl config)"
        exit 1
    fi

    log_success "Prerequisites check passed"
}

wait_for_deployment() {
    local deployment=$1
    local namespace=$2
    local timeout=${3:-300}

    log_info "Waiting for deployment/${deployment} in ${namespace} to be ready (timeout: ${timeout}s)..."

    if kubectl rollout status deployment/"${deployment}" -n "${namespace}" --timeout="${timeout}s" &>/dev/null; then
        log_success "Deployment ${deployment} is ready"
        return 0
    else
        log_warn "Deployment ${deployment} did not become ready within timeout"
        return 1
    fi
}

check_deployment_exists() {
    local deployment=$1
    local namespace=$2

    if kubectl get deployment "${deployment}" -n "${namespace}" &>/dev/null; then
        return 0
    else
        return 1
    fi
}

fix_rbac_permissions() {
    log_info "Applying RBAC permissions fix..."

    # Check if permissions already exist
    if kubectl get clusterrole maas-api -o json | jq -e '.rules[] | select(.apiGroups[] == "maas.opendatahub.io")' &>/dev/null; then
        log_info "RBAC permissions already include maas.opendatahub.io - skipping"
        return 0
    fi

    kubectl patch clusterrole maas-api --type='json' -p='[
      {
        "op": "add",
        "path": "/rules/-",
        "value": {
          "apiGroups": ["maas.opendatahub.io"],
          "resources": ["maasmodels", "maassubscriptions"],
          "verbs": ["get", "list", "watch"]
        }
      }
    ]' || {
        log_error "Failed to patch ClusterRole"
        return 1
    }

    log_success "RBAC permissions fixed"
}

disable_operator_reconciliation() {
    log_info "Disabling operator reconciliation (scaling to 0)..."

    if ! kubectl get deployment opendatahub-operator-controller-manager -n "${NAMESPACE}" &>/dev/null; then
        log_warn "ODH operator deployment not found - skipping operator scaling"
        return 0
    fi

    kubectl scale deployment opendatahub-operator-controller-manager \
      -n "${NAMESPACE}" --replicas=0 || {
        log_error "Failed to scale operator to 0"
        return 1
    }

    # Wait for operator to scale down
    sleep 5

    log_success "Operator reconciliation disabled"
}

update_custom_image() {
    local custom_image=$1

    log_info "Updating maas-api deployment with custom image: ${custom_image}"

    kubectl patch deployment maas-api -n "${NAMESPACE}" --type='json' -p='[
      {
        "op": "replace",
        "path": "/spec/template/spec/containers/0/image",
        "value": "'"${custom_image}"'"
      }
    ]' || {
        log_error "Failed to update image"
        return 1
    }

    log_success "Custom image configured"
}

verify_deployment() {
    log_info "Verifying deployment..."

    # Wait for rollout
    if ! wait_for_deployment "maas-api" "${NAMESPACE}" "${TIMEOUT}"; then
        log_error "Deployment did not become ready"
        log_info "Checking pod status..."
        kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/name=maas-api
        log_info "Recent logs:"
        kubectl logs -n "${NAMESPACE}" deployment/maas-api --tail=50 || true
        return 1
    fi

    # Check image
    local actual_image
    actual_image=$(kubectl get deployment maas-api -n "${NAMESPACE}" \
      -o jsonpath='{.spec.template.spec.containers[0].image}')
    log_info "Running image: ${actual_image}"

    # Check pod status
    local pod_status
    pod_status=$(kubectl get pods -n "${NAMESPACE}" -l app.kubernetes.io/name=maas-api \
      -o jsonpath='{.items[0].status.phase}' 2>/dev/null || echo "Unknown")
    log_info "Pod status: ${pod_status}"

    if [[ "${pod_status}" == "Running" ]]; then
        log_success "Pod is running"

        # Check logs for success indicators
        log_info "Checking logs for database connection..."
        if kubectl logs -n "${NAMESPACE}" deployment/maas-api --tail=100 | \
           grep -q "Connected to PostgreSQL database"; then
            log_success "PostgreSQL connection established"
        else
            log_warn "Could not verify PostgreSQL connection in logs"
        fi

        if kubectl logs -n "${NAMESPACE}" deployment/maas-api --tail=100 | \
           grep -q "Server starting"; then
            log_success "Server started successfully"
        else
            log_warn "Could not verify server startup in logs"
        fi

        return 0
    else
        log_error "Pod is not running (status: ${pod_status})"
        return 1
    fi
}

show_next_steps() {
    cat <<EOF

${GREEN}================================${NC}
${GREEN}Deployment Successful!${NC}
${GREEN}================================${NC}

The custom maas-api image has been deployed successfully.

${BLUE}Verification Commands:${NC}

  # Check pod status
  kubectl get pods -n ${NAMESPACE} -l app.kubernetes.io/name=maas-api

  # View logs
  kubectl logs -n ${NAMESPACE} deployment/maas-api --tail=100 -f

  # Check image
  kubectl get deployment maas-api -n ${NAMESPACE} \\
    -o jsonpath='{.spec.template.spec.containers[0].image}'

${BLUE}Testing the API:${NC}

  # Get a JWT token from your cluster
  TOKEN=\$(oc whoami -t)

  # Test the new POST /v1/api-keys/search endpoint
  kubectl exec -n ${NAMESPACE} deployment/maas-api -- \\
    curl -k -s https://localhost:8443/v1/api-keys/search \\
    -H "Authorization: Bearer \${TOKEN}" \\
    -H "Content-Type: application/json" \\
    -d '{
      "filters": {"status": ["active"]},
      "sort": {"by": "created_at", "order": "desc"},
      "pagination": {"limit": 10, "offset": 0}
    }'

${YELLOW}Note:${NC} The ODH operator has been scaled to 0 replicas to prevent
reconciliation. To re-enable it:

  kubectl scale deployment opendatahub-operator-controller-manager \\
    -n ${NAMESPACE} --replicas=1

${YELLOW}Warning:${NC} Re-enabling the operator may revert your custom image.

EOF
}

main() {
    if [[ $# -lt 1 ]]; then
        cat <<EOF
${RED}Error: Missing required argument${NC}

Usage: $0 <custom-image-url>

Example:
  $0 quay.io/myuser/maas-api:my-tag

This script automates deployment of a custom maas-api image by:
  1. Fixing RBAC permissions for maasmodels/maassubscriptions
  2. Disabling operator reconciliation
  3. Updating to custom image
  4. Verifying deployment health

For more details, see: docs/CUSTOM_IMAGE_DEPLOYMENT.md
EOF
        exit 1
    fi

    local custom_image=$1

    echo ""
    log_info "========================================="
    log_info "  Custom MaaS API Image Deployment"
    log_info "========================================="
    log_info "Image: ${custom_image}"
    log_info "Namespace: ${NAMESPACE}"
    log_info "========================================="
    echo ""

    # Run checks and fixes
    check_prerequisites

    # Check if maas-api deployment exists
    if ! check_deployment_exists "maas-api" "${NAMESPACE}"; then
        log_error "maas-api deployment not found in namespace ${NAMESPACE}"
        log_error "Please deploy ODH first using: ./scripts/deploy.sh"
        exit 1
    fi

    # Apply fixes in order
    fix_rbac_permissions || exit 1
    disable_operator_reconciliation || exit 1
    update_custom_image "${custom_image}" || exit 1

    # Verify
    if verify_deployment; then
        show_next_steps
        exit 0
    else
        log_error "Deployment verification failed"
        log_error "Please check logs and pod status manually"
        exit 1
    fi
}

main "$@"
