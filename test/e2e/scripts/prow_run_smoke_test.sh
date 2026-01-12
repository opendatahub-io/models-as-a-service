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
#   SKIP_VALIDATION - Skip deployment validation (default: false)
#   SKIP_SMOKE      - Skip smoke tests (default: false)
#   SKIP_TOKEN_VERIFICATION - Skip token metadata verification (default: false)
#   MAAS_API_IMAGE - Custom image for MaaS API (e.g., quay.io/opendatahub/maas-api:pr-232)
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

# Options (can be set as environment variables)
SKIP_VALIDATION=${SKIP_VALIDATION:-false}
SKIP_SMOKE=${SKIP_SMOKE:-false}
SKIP_TOKEN_VERIFICATION=${SKIP_TOKEN_VERIFICATION:-false}

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
    if ! "$PROJECT_ROOT/scripts/deploy-openshift.sh"; then
        echo "❌ ERROR: MaaS platform deployment failed"
        exit 1
    fi
    echo "✅ MaaS platform deployment completed"
}

deploy_models() {
    echo "Deploying simulator Model"
    if ! (cd "$PROJECT_ROOT" && kustomize build docs/samples/models/simulator/ | kubectl apply -f -); then
        echo "❌ ERROR: Failed to deploy simulator model"
        exit 1
    fi
    echo "✅ Simulator model deployed"
    
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
            echo "❌ ERROR: Deployment validation failed"
            exit 1
        else
            echo "✅ Deployment validation completed"
        fi
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

    export CLUSTER_DOMAIN="$(oc get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"
    export HOST="maas.${CLUSTER_DOMAIN}"
    export MAAS_API_BASE_URL="http://${HOST}/maas-api"
    echo "CLUSTER_DOMAIN: ${CLUSTER_DOMAIN}"
    echo "HOST: ${HOST}"
    echo "MAAS_API_BASE_URL: ${MAAS_API_BASE_URL}"
    echo "✅ Variables for tests setup completed"
}

run_smoke_tests() {
    echo "-- Smoke Testing --"
    
    if [ "$SKIP_SMOKE" = false ]; then
        if ! (cd "$PROJECT_ROOT" && bash test/e2e/smoke.sh); then
            echo "❌ ERROR: Smoke tests failed"
        else
            echo "✅ Smoke tests completed successfully"
        fi
    else
        echo "⏭️  Skipping smoke tests"
    fi
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
deploy_maas_platform

print_header "Deploying Models"  
deploy_models

print_header "Setting up variables for tests"
setup_vars_for_tests

# Setup all users first (while logged in as admin)
print_header "Setting up test users"
setup_test_user "tester-admin-user" "cluster-admin"
setup_test_user "tester-edit-user" "edit"
setup_test_user "tester-view-user" "view"

# Now run tests for each user
print_header "Running tests for all users"

# Test admin user
print_header "Running Maas e2e Tests as admin user"
ADMIN_TOKEN=$(oc create token tester-admin-user -n default)
oc login --token "$ADMIN_TOKEN" --server "$K8S_CLUSTER_URL"

print_header "Validating Deployment and Token Metadata Logic"
validate_deployment
run_token_verification

run_smoke_tests

# Test edit user  
print_header "Running Maas e2e Tests as edit user"
EDIT_TOKEN=$(oc create token tester-edit-user -n default)
oc login --token "$EDIT_TOKEN" --server "$K8S_CLUSTER_URL"
run_smoke_tests

# Test view user
print_header "Running Maas e2e Tests as view user"
VIEW_TOKEN=$(oc create token tester-view-user -n default)
oc login --token "$VIEW_TOKEN" --server "$K8S_CLUSTER_URL"
run_smoke_tests

echo "🎉 Deployment completed successfully!"