#!/bin/bash

# =============================================================================
# MaaS Platform End-to-End Testing Script
# =============================================================================
#
# This script automates the complete deployment and validation of the MaaS 
# platform on OpenShift with multi-user testing capabilities.
#
# WHAT IT DOES:
#   1. Deploy MaaS platform on OpenShift
#   2. Deploy simulator model for testing
#   3. Validate deployment functionality
#   4. Create test users with different permission levels:
#      - Admin user (cluster-admin role)
#      - Edit user (edit role) 
#      - View user (view role)
#   5. Run token metadata verification (as admin user)
#   6. Run smoke tests for each user
# 
# USAGE:
#   ./test/e2e/scripts/prow_run_smoke_test.sh
#
# ENVIRONMENT VARIABLES:
#   SKIP_DEPLOY     - Skip platform deployment (default: false)
#   SKIP_VALIDATION - Skip deployment validation (default: false)
#   SKIP_SMOKE      - Skip smoke tests (default: false)
#   SKIP_TOKEN_VERIFICATION - Skip token metadata verification (default: false)
#   SKIP_AUTH_CHECK - Skip Authorino auth readiness check (default: true, temporary workaround)
#   SKIP_OBSERVABILITY - Skip observability tests (default: false)
#   MAAS_API_IMAGE - Custom image for MaaS API (e.g., quay.io/opendatahub/maas-api:pr-232)
#   INSECURE_HTTP  - Deploy without TLS and use HTTP for tests (default: false)
#                    Affects both deploy-openshift.sh and smoke.sh
# =============================================================================

set -euo pipefail

find_project_root() {
  local start_dir="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
  local marker="${2:-.git}"
  local dir="$start_dir"

  while [[ "$dir" != "/" && ! -e "$dir/$marker" ]]; do
    dir="$(dirname "$dir")"
  done

  if [[ -e "$dir/$marker" ]]; then
    printf '%s\n' "$dir"
  else
    echo "Error: couldn't find '$marker' in any parent of '$start_dir'" >&2
    return 1
  fi
}

# Configuration
PROJECT_ROOT="$(find_project_root)"

# Source helper functions
source "$PROJECT_ROOT/scripts/deployment-helpers.sh"

# Options (can be set as environment variables)
SKIP_DEPLOY=${SKIP_DEPLOY:-false}
SKIP_VALIDATION=${SKIP_VALIDATION:-false}
SKIP_SMOKE=${SKIP_SMOKE:-false}
SKIP_TOKEN_VERIFICATION=${SKIP_TOKEN_VERIFICATION:-false}
SKIP_AUTH_CHECK=${SKIP_AUTH_CHECK:-true}  # TODO: Set to false once operator TLS fix lands
SKIP_OBSERVABILITY=${SKIP_OBSERVABILITY:-false}
INSECURE_HTTP=${INSECURE_HTTP:-false}

# Track test failures - script continues but exits with error at end
TESTS_FAILED=false

print_header() {
    echo ""
    echo "----------------------------------------"
    echo "$1"
    echo "----------------------------------------"
    echo ""
}

check_prerequisites() {
    echo "Checking prerequisites..."
    
    # Get current user (also checks if logged in)
    local current_user
    if ! current_user=$(oc whoami 2>/dev/null); then
        echo "❌ ERROR: Not logged into OpenShift. Please run 'oc login' first"
        exit 1
    fi
    
    # Combined check: admin privileges + OpenShift cluster
    if ! oc auth can-i '*' '*' --all-namespaces >/dev/null 2>&1; then
        echo "❌ ERROR: User '$current_user' does not have admin privileges"
        echo "   This script requires cluster-admin privileges to deploy and manage resources"
        echo "   Please login as an admin user with 'oc login' or contact your cluster administrator"
        exit 1
    elif ! kubectl get --raw /apis/config.openshift.io/v1/clusterversions >/dev/null 2>&1; then
        echo "❌ ERROR: This script is designed for OpenShift clusters only"
        exit 1
    fi
    
    echo "✅ Prerequisites met - logged in as: $current_user on OpenShift"
}

deploy_maas_platform() {
    echo "Deploying MaaS platform on OpenShift..."
    if ! "$PROJECT_ROOT/scripts/deploy-rhoai-stable.sh" --operator-type odh --operator-catalog quay.io/opendatahub/opendatahub-operator-catalog:latest --channel fast; then
        echo "❌ ERROR: MaaS platform deployment failed"
        exit 1
    fi
    # Wait for DataScienceCluster's KServe and ModelsAsService to be ready
    if ! wait_datasciencecluster_ready "default-dsc" 600; then
        echo "❌ ERROR: DataScienceCluster components did not become ready"
        exit 1
    fi
    
    # Wait for Authorino to be ready and auth service cluster to be healthy
    # TODO(https://issues.redhat.com/browse/RHOAIENG-48760): Remove SKIP_AUTH_CHECK
    # once the operator creates the gateway→Authorino TLS EnvoyFilter at Gateway/AuthPolicy creation
    # time, not at first LLMInferenceService creation. Currently there's a chicken-egg problem where
    # auth checks fail before any model is deployed because the TLS config doesn't exist yet.
    if [[ "${SKIP_AUTH_CHECK:-false}" == "true" ]]; then
        echo "⚠️  WARNING: Skipping Authorino readiness check (SKIP_AUTH_CHECK=true)"
        echo "   This is a temporary workaround for the gateway→Authorino TLS chicken-egg problem"
    else
        echo "Waiting for Authorino and auth service to be ready..."
        if ! wait_authorino_ready 600; then
            echo "⚠️  WARNING: Authorino readiness check had issues, continuing anyway"
        fi
    fi
    
    echo "✅ MaaS platform deployment completed"
}

install_observability() {
    echo "Installing observability components..."
    if ! "$PROJECT_ROOT/scripts/install-observability.sh"; then
        echo "❌ ERROR: Failed to deploy observability components"
        exit 1
    fi
    echo "✅ Observability installation completed"
}

deploy_models() {
    echo "Deploying simulator Model"
    # Create llm namespace if it does not exist
    if ! kubectl get namespace llm >/dev/null 2>&1; then
        echo "Creating 'llm' namespace..."
        if ! kubectl create namespace llm; then
            echo "❌ ERROR: Failed to create 'llm' namespace"
            exit 1
        fi
    else
        echo "'llm' namespace already exists"
    fi
    if ! (cd "$PROJECT_ROOT" && kustomize build docs/samples/models/simulator/ | kubectl apply -f -); then
        echo "❌ ERROR: Failed to deploy simulator model"
        exit 1
    fi
    
    # Add 'free' tier so test users can access the model
    # The sample model has tiers: '[]' by default - we need to enable access for testing
    kubectl patch llminferenceservice facebook-opt-125m-simulated -n llm --type='merge' \
        -p '{"metadata":{"annotations":{"alpha.maas.opendatahub.io/tiers":"[\"free\"]"}}}'
    echo "✅ Simulator model deployed with 'free' tier access"
    
    echo "Waiting for model to be ready..."
    if ! oc wait llminferenceservice/facebook-opt-125m-simulated -n llm --for=condition=Ready --timeout=300s; then
        echo "❌ ERROR: Timed out waiting for model to be ready"
        echo "=== LLMInferenceService YAML dump ==="
        oc get llminferenceservice/facebook-opt-125m-simulated -n llm -o yaml || true
        echo "=== Events in llm namespace ==="
        oc get events -n llm --sort-by='.lastTimestamp' || true
        exit 1
    fi
    echo "✅ Simulator Model deployed"
}

validate_deployment() {
    echo "Deployment Validation"
    if [ "$SKIP_VALIDATION" = false ]; then
        if ! "$PROJECT_ROOT/scripts/validate-deployment.sh"; then
            echo "⚠️  First validation attempt failed, waiting 60 seconds and retrying..."
            sleep 60
            if ! "$PROJECT_ROOT/scripts/validate-deployment.sh"; then
                echo "❌ ERROR: Deployment validation failed after retry"
                exit 1
            fi
        fi
        echo "✅ Deployment validation completed"
    else
        echo "⏭️  Skipping validation"
    fi
}

setup_vars_for_tests() {
    echo "-- Setting up variables for tests --"
    K8S_CLUSTER_URL=$(oc whoami --show-server)
    export K8S_CLUSTER_URL
    if [ -z "$K8S_CLUSTER_URL" ]; then
        echo "❌ ERROR: Failed to retrieve Kubernetes cluster URL. Please check if you are logged in to the cluster."
        exit 1
    fi
    echo "K8S_CLUSTER_URL: ${K8S_CLUSTER_URL}"

    # Export INSECURE_HTTP for smoke.sh (it handles MAAS_API_BASE_URL detection)
    # HTTPS is the default for MaaS.
    # HTTP is used only when INSECURE_HTTP=true (opt-out mode).
    # This aligns with deploy-openshift.sh which also respects INSECURE_HTTP
    export INSECURE_HTTP
    if [ "$INSECURE_HTTP" = "true" ]; then
        echo "⚠️  INSECURE_HTTP=true - will use HTTP for tests"
    fi
       
    export CLUSTER_DOMAIN="$(oc get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"
    if [ -z "$CLUSTER_DOMAIN" ]; then
        echo "❌ ERROR: Failed to detect cluster ingress domain (ingresses.config.openshift.io/cluster)"
        exit 1
    fi
    export HOST="maas.${CLUSTER_DOMAIN}"

    if [ "$INSECURE_HTTP" = "true" ]; then
        export MAAS_API_BASE_URL="http://${HOST}/maas-api"
    else
        export MAAS_API_BASE_URL="https://${HOST}/maas-api"
    fi

    echo "HOST: ${HOST}"
    echo "MAAS_API_BASE_URL: ${MAAS_API_BASE_URL}"
    echo "CLUSTER_DOMAIN: ${CLUSTER_DOMAIN}"
    echo "✅ Variables for tests setup completed"
}

run_smoke_tests() {
    echo "-- Smoke Testing --"
    
    if [ "$SKIP_SMOKE" = false ]; then
        if ! (cd "$PROJECT_ROOT" && bash test/e2e/smoke.sh); then
            echo "❌ ERROR: Smoke tests failed"
            TESTS_FAILED=true
        else
            echo "✅ Smoke tests completed successfully"
        fi
    else
        echo "⏭️  Skipping smoke tests"
    fi
}

run_observability_tests() {
    echo "-- Observability Testing --"
    
    if [ "$SKIP_OBSERVABILITY" = true ]; then
        echo "⏭️  Skipping observability tests"
        return 0
    fi
    
    # Reuse venv from smoke.sh
    local VENV_DIR="${PROJECT_ROOT}/test/e2e/.venv"
    
    if [[ ! -f "${VENV_DIR}/bin/activate" ]]; then
        echo "[observability] Creating virtual environment at ${VENV_DIR}"
        rm -rf "${VENV_DIR}"
        python3 -m venv "${VENV_DIR}" --upgrade-deps
    fi
    
    source "${VENV_DIR}/bin/activate"
    python -m pip install --upgrade pip --quiet
    python -m pip install -r "${PROJECT_ROOT}/test/e2e/requirements.txt" --quiet
    
    export PYTHONPATH="${PROJECT_ROOT}/test/e2e:${PYTHONPATH:-}"
    
    local REPORTS_DIR="${PROJECT_ROOT}/test/e2e/reports"
    mkdir -p "${REPORTS_DIR}"
    
    local USER
    USER="$(oc whoami 2>/dev/null || echo 'unknown')"
    local HTML="${REPORTS_DIR}/observability-${USER}.html"
    local XML="${REPORTS_DIR}/observability-${USER}.xml"
    
    echo "[observability] Running observability tests..."
    
    if pytest "${PROJECT_ROOT}/test/e2e/tests/test_observability.py" \
        -v \
        --tb=short \
        "--junitxml=${XML}" \
        --html="${HTML}" --self-contained-html \
        2>&1; then
        echo "✅ Observability tests completed successfully"
    else
        echo "❌ ERROR: Observability tests failed"
        echo "[observability] Reports: ${HTML}"
        TESTS_FAILED=true
    fi
    
    deactivate 2>/dev/null || true
}

run_token_verification() {
    echo "-- Token Metadata Verification --"
    
    if [ "$SKIP_TOKEN_VERIFICATION" = false ]; then
        if ! (cd "$PROJECT_ROOT" && bash scripts/verify-tokens-metadata-logic.sh); then
            echo "❌ ERROR: Token metadata verification failed"
            exit 1
        else
            echo "✅ Token metadata verification completed successfully"
        fi
    else
        echo "Skipping token metadata verification..."
    fi
}

setup_test_user() {
    local username="$1"
    local cluster_role="$2"
    
    # Check and create service account
    if ! oc get serviceaccount "$username" -n default >/dev/null 2>&1; then
        echo "Creating service account: $username"
        oc create serviceaccount "$username" -n default
    else
        echo "Service account $username already exists"
    fi
    
    # Check and create cluster role binding
    if ! oc get clusterrolebinding "${username}-binding" >/dev/null 2>&1; then
        echo "Creating cluster role binding for $username"
        oc adm policy add-cluster-role-to-user "$cluster_role" "system:serviceaccount:default:$username"
    else
        echo "Cluster role binding for $username already exists"
    fi
    
    echo "✅ User setup completed: $username"
}

# Main execution
print_header "Deploying Maas on OpenShift"
check_prerequisites

# Save original user context to restore at the end
ORIGINAL_USER=$(oc whoami 2>/dev/null || true)
ORIGINAL_TOKEN=$(oc whoami -t 2>/dev/null || true)
echo "ℹ️  Original user: ${ORIGINAL_USER:-unknown}"

if [ "$SKIP_DEPLOY" = false ]; then
    deploy_maas_platform

    print_header "Installing Observability"
    install_observability

    print_header "Deploying Models"  
    deploy_models
else
    echo "⏭️  Skipping deployment (SKIP_DEPLOY=true)"
fi

print_header "Setting up variables for tests"
setup_vars_for_tests

# Setup all users first (while logged in as admin)
print_header "Setting up test users"
setup_test_user "tester-admin-user" "cluster-admin"
setup_test_user "tester-edit-user" "edit"
setup_test_user "tester-view-user" "view"

# Now run tests for each user
print_header "Running tests for all users"

# Clear any stale TOKEN from environment to ensure fresh token generation
unset TOKEN

# Test admin user
print_header "Running Maas e2e Tests as admin user"
ADMIN_TOKEN=$(oc create token tester-admin-user -n default)
oc login --token "$ADMIN_TOKEN" --server "$K8S_CLUSTER_URL"

print_header "Validating Deployment"
validate_deployment

# Run smoke tests
run_smoke_tests

# Run observability tests (once, before destructive token verification)
run_observability_tests

# Token verification test - this REVOKES ALL TOKENS and recreates Service Account
# Run AFTER observability tests since it breaks model access temporarily
print_header "Token Metadata Verification"
run_token_verification

sleep 120       # Wait for the rate limit to reset
unset TOKEN  # Clear any token set by token verification

# After token verification revokes all tokens, the Service Account is recreated
# Wait for RBAC to propagate and verify model access before proceeding
echo "Waiting for model access to be restored after token revocation..."
MODEL_ACCESS_RESTORED=false
for i in {1..10}; do
    TEMP_TOKEN=$(curl -sSk -X POST "${MAAS_API_BASE_URL}/v1/tokens" \
        -H "Authorization: Bearer $(oc whoami -t)" \
        -H "Content-Type: application/json" \
        -d '{"expiration": "5m"}' | jq -r '.token // empty')
    if [ -n "$TEMP_TOKEN" ]; then
        MODEL_COUNT=$(curl -sSk "${MAAS_API_BASE_URL}/v1/models" \
            -H "Authorization: Bearer $TEMP_TOKEN" | jq -r '.data | length // 0')
        if [ "$MODEL_COUNT" -gt 0 ]; then
            echo "✅ Model access restored (found $MODEL_COUNT model(s))"
            MODEL_ACCESS_RESTORED=true
            break
        fi
    fi
    echo "  Attempt $i/10: waiting for model access..."
    sleep 10
done
unset TEMP_TOKEN

if [ "$MODEL_ACCESS_RESTORED" = false ]; then
    echo "❌ ERROR: Model access could not be verified after 10 attempts (100 seconds)"
    echo "  This may cause smoke tests to fail with confusing errors."
    echo "  Check:"
    echo "    1. Service Account exists: kubectl get sa -n <tier-namespace>"
    echo "    2. RBAC is configured: kubectl get rolebinding -n llm"
    echo "    3. Model is ready: kubectl get llminferenceservice -n llm"
    TESTS_FAILED=true
fi

# Test edit user  
print_header "Running Maas e2e Tests as edit user"
unset TOKEN  # Clear stale token from previous user
EDIT_TOKEN=$(oc create token tester-edit-user -n default)
oc login --token "$EDIT_TOKEN" --server "$K8S_CLUSTER_URL"
run_smoke_tests

# Test view user
print_header "Running Maas e2e Tests as view user"
unset TOKEN  # Clear stale token from previous user
VIEW_TOKEN=$(oc create token tester-view-user -n default)
oc login --token "$VIEW_TOKEN" --server "$K8S_CLUSTER_URL"
run_smoke_tests

# Restore original user context
if [ -n "$ORIGINAL_TOKEN" ]; then
    echo ""
    echo "ℹ️  Restoring original user context: ${ORIGINAL_USER}"
    oc login --token "$ORIGINAL_TOKEN" --server "$K8S_CLUSTER_URL" >/dev/null 2>&1 || true
fi

# Final status - exit with error if any tests failed
if [ "$TESTS_FAILED" = true ]; then
    echo ""
    echo "❌ Some tests failed! See logs above for details."
    exit 1
fi

echo "🎉 All tests passed successfully!"