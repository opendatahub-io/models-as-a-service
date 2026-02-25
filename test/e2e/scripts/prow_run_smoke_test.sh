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
# CI/CD PIPELINE USAGE:
#   # Test with pipeline-built images
#   OPERATOR_CATALOG=quay.io/opendatahub/opendatahub-operator-catalog:pr-123 \
#   MAAS_API_IMAGE=quay.io/opendatahub/maas-api:pr-456 \
#   MAAS_CONTROLLER_IMAGE=quay.io/maas/maas-controller:pr-42 \
#   ./test/e2e/scripts/prow_run_smoke_test.sh
#
# ENVIRONMENT VARIABLES:
#   OPERATOR_TYPE   - Operator to deploy: "odh" or "rhoai" (default: odh)
#                     odh   → uses Kuadrant (upstream), Authorino in kuadrant-system
#                     rhoai → uses RHCL (downstream), Authorino in rh-connectivity-link
#   SKIP_VALIDATION - Skip deployment validation (default: false)
#   SKIP_SMOKE      - Skip smoke tests (default: false)
#   SKIP_TOKEN_VERIFICATION - Skip token metadata verification (default: false)
#   MAAS_API_IMAGE - Custom MaaS API image (default: uses operator default)
#                    Example: quay.io/opendatahub/maas-api:pr-232
#   MAAS_CONTROLLER_IMAGE - Custom MaaS controller image (default: quay.io/maas/maas-controller:latest)
#                           Example: quay.io/opendatahub/maas-controller:pr-430
#   OPERATOR_CATALOG - Custom operator catalog image (default: latest from main)
#                      Example: quay.io/opendatahub/opendatahub-operator-catalog:pr-456
#   OPERATOR_IMAGE - Custom operator image (default: uses catalog default)
#                    Example: quay.io/opendatahub/opendatahub-operator:pr-456
#   INSECURE_HTTP  - Deploy without TLS and use HTTP for tests (default: false)
#                    Affects both deploy.sh (via --disable-tls-backend) and smoke.sh
# =============================================================================

set -euo pipefail

# Find project root before sourcing helpers (helpers need to be sourced from correct path)
_find_project_root_bootstrap() {
  local start_dir="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
  local dir="$start_dir"
  while [[ "$dir" != "/" && ! -e "$dir/.git" ]]; do
    dir="$(dirname "$dir")"
  done
  [[ -e "$dir/.git" ]] && printf '%s\n' "$dir" || return 1
}

# Configuration
PROJECT_ROOT="$(_find_project_root_bootstrap)"

# Source helper functions (includes find_project_root and other utilities)
source "$PROJECT_ROOT/scripts/deployment-helpers.sh"

# Options (can be set as environment variables)
SKIP_VALIDATION=${SKIP_VALIDATION:-false}
SKIP_SMOKE=${SKIP_SMOKE:-false}
SKIP_TOKEN_VERIFICATION=${SKIP_TOKEN_VERIFICATION:-false}
SKIP_SUBSCRIPTION_TESTS=${SKIP_SUBSCRIPTION_TESTS:-false}
SKIP_AUTH_CHECK=${SKIP_AUTH_CHECK:-true}  # TODO: Set to false once operator TLS fix lands
INSECURE_HTTP=${INSECURE_HTTP:-false}

# Operator configuration
# OPERATOR_TYPE determines which operator and policy engine to use:
#   odh   → Kuadrant (upstream) → kuadrant-system
#   rhoai → RHCL (downstream)   → rh-connectivity-link
OPERATOR_TYPE=${OPERATOR_TYPE:-odh}

# Image configuration (for CI/CD pipelines)
# OPERATOR_CATALOG: For ODH, defaults to snapshot catalog (required for v2 API / MaaS support)
#                   For RHOAI, no default (uses redhat-operators from OCP marketplace)
if [[ -z "${OPERATOR_CATALOG:-}" ]]; then
    if [[ "$OPERATOR_TYPE" == "odh" ]]; then
        # ODH requires v3+ for DataScienceCluster v2 API (MaaS support)
        # community-operators only has v2.x which doesn't have v2 API
        OPERATOR_CATALOG="quay.io/opendatahub/opendatahub-operator-catalog:latest"
    fi
    # RHOAI: intentionally no default - uses redhat-operators from OCP marketplace
fi
export MAAS_API_IMAGE=${MAAS_API_IMAGE:-}           # Optional: uses operator default if not set
export MAAS_CONTROLLER_IMAGE=${MAAS_CONTROLLER_IMAGE:-}  # Optional: default quay.io/maas/maas-controller:latest
export OPERATOR_IMAGE=${OPERATOR_IMAGE:-}            # Optional: uses catalog default if not set

# Compute namespaces based on operator type (matches deploy-rhoai-stable.sh behavior)
# MaaS API is always deployed to the fixed application namespace, NOT the CI namespace
case "${OPERATOR_TYPE}" in
    rhoai)
        AUTHORINO_NAMESPACE="rh-connectivity-link"
        MAAS_NAMESPACE="redhat-ods-applications"
        ;;
    *)
        AUTHORINO_NAMESPACE="kuadrant-system"
        MAAS_NAMESPACE="opendatahub"
        ;;
esac

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
    echo "Using operator type: ${OPERATOR_TYPE}"
    echo "Using operator catalog: ${OPERATOR_CATALOG:-"(default)"}"
    if [[ -n "${MAAS_API_IMAGE:-}" ]]; then
        echo "Using custom MaaS API image: ${MAAS_API_IMAGE}"
    fi
    if [[ -n "${MAAS_CONTROLLER_IMAGE:-}" ]]; then
        echo "Using custom MaaS controller image: ${MAAS_CONTROLLER_IMAGE}"
    fi
    if [[ -n "${OPERATOR_IMAGE:-}" ]]; then
        echo "Using custom operator image: ${OPERATOR_IMAGE}"
    fi

    # Build deploy.sh command with optional parameters
    # NOTE: Do NOT hardcode --channel here! deploy.sh sets per-operator defaults:
    #   - ODH: fast-3 (for v3+ with MaaS support)
    #   - RHOAI: fast-3.x (for v3.x with MaaS support)
    # Hardcoding --channel fast breaks RHOAI (installs 2.x without modelsAsService)
    local deploy_cmd=(
        "$PROJECT_ROOT/scripts/deploy.sh"
        --operator-type "${OPERATOR_TYPE}"
    )

    # Add optional operator catalog if specified (otherwise uses default catalog)
    if [[ -n "${OPERATOR_CATALOG:-}" ]]; then
        deploy_cmd+=(--operator-catalog "${OPERATOR_CATALOG}")
    fi

    # Add optional operator image if specified
    if [[ -n "${OPERATOR_IMAGE:-}" ]]; then
        deploy_cmd+=(--operator-image "${OPERATOR_IMAGE}")
    fi

    if ! "${deploy_cmd[@]}"; then
        echo "❌ ERROR: MaaS platform deployment failed"
        exit 1
    fi
    # Wait for DataScienceCluster's KServe and ModelsAsService to be ready
    # Using 300s timeout to fit within Prow's 15m job limit
    if ! wait_datasciencecluster_ready "default-dsc" 300; then
        echo "❌ ERROR: DataScienceCluster components did not become ready"
        exit 1
    fi
    
    # Wait for Authorino to be ready and auth service cluster to be healthy
    # TODO(https://issues.redhat.com/browse/RHOAIENG-48760): Remove SKIP_AUTH_CHECK
    # once the operator creates the gateway→Authorino TLS EnvoyFilter at Gateway/AuthPolicy creation
    # time, not at first LLMInferenceService creation. Currently there's a chicken-egg problem where
    # auth checks fail before any model is deployed because the TLS config doesn't exist yet.
    if [[ "${SKIP_AUTH_CHECK:-true}" == "true" ]]; then
        echo "⚠️  WARNING: Skipping Authorino readiness check (SKIP_AUTH_CHECK=true)"
        echo "   This is a temporary workaround for the gateway→Authorino TLS chicken-egg problem"
    else
        # Using 300s timeout to fit within Prow's 15m job limit
        echo "Waiting for Authorino and auth service to be ready (namespace: ${AUTHORINO_NAMESPACE})..."
        if ! wait_authorino_ready "$AUTHORINO_NAMESPACE" 300; then
            echo "⚠️  WARNING: Authorino readiness check had issues, continuing anyway"
        fi
    fi
    
    echo "✅ MaaS platform deployment completed"
}

deploy_models() {
    echo "Deploying simulator models (regular + premium)"
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
    echo "✅ Regular simulator deployed"

    if ! (cd "$PROJECT_ROOT" && kustomize build docs/samples/models/simulator-premium/ | kubectl apply -f -); then
        echo "❌ ERROR: Failed to deploy premium simulator model"
        exit 1
    fi
    echo "✅ Premium simulator deployed"

    echo "Waiting for models to be ready..."
    if ! oc wait llminferenceservice/facebook-opt-125m-simulated -n llm --for=condition=Ready --timeout=300s; then
        echo "❌ ERROR: Timed out waiting for regular simulator to be ready"
        oc get llminferenceservice/facebook-opt-125m-simulated -n llm -o yaml || true
        oc get events -n llm --sort-by='.lastTimestamp' || true
        exit 1
    fi
    if ! oc wait llminferenceservice/premium-simulated-simulated-premium -n llm --for=condition=Ready --timeout=300s; then
        echo "❌ ERROR: Timed out waiting for premium simulator to be ready"
        oc get llminferenceservice/premium-simulated-simulated-premium -n llm -o yaml || true
        oc get events -n llm --sort-by='.lastTimestamp' || true
        exit 1
    fi
    echo "✅ Simulator models deployed"
}

validate_deployment() {
    echo "Deployment Validation"
    echo "Using namespace: $MAAS_NAMESPACE"
    
    if [ "$SKIP_VALIDATION" = false ]; then
        if ! "$PROJECT_ROOT/scripts/validate-deployment.sh" --namespace "$MAAS_NAMESPACE"; then
            echo "⚠️  First validation attempt failed, waiting 30 seconds and retrying..."
            sleep 30
            if ! "$PROJECT_ROOT/scripts/validate-deployment.sh" --namespace "$MAAS_NAMESPACE"; then
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
    # This aligns with deploy.sh which also respects TLS configuration
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
            exit 1
        else
            echo "✅ Smoke tests completed successfully"
        fi
    else
        echo "⏭️  Skipping smoke tests"
    fi
}

deploy_subscription_crs() {
    echo "Deploying MaaS subscription example CRs..."
    if ! kubectl apply -k "$PROJECT_ROOT/maas-controller/examples/"; then
        echo "❌ ERROR: Failed to deploy subscription CRs"
        exit 1
    fi

    echo "Waiting for MaaSModels to be Ready..."
    local retries=0
    while [[ $retries -lt 30 ]]; do
        local all_ready=true
        while IFS= read -r phase; do
            if [[ "$phase" != "Ready" ]]; then
                all_ready=false
                break
            fi
        done < <(oc get maasmodels -n "$MAAS_NAMESPACE" -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' 2>/dev/null)
        if $all_ready && [[ -n "$(oc get maasmodels -n "$MAAS_NAMESPACE" -o name 2>/dev/null)" ]]; then
            break
        fi
        retries=$((retries + 1))
        sleep 5
    done
    if [[ $retries -ge 30 ]]; then
        echo "⚠️  WARNING: MaaSModels did not all become Ready within timeout"
    fi

    echo "✅ Subscription CRs deployed"
    oc get maasmodels,maasauthpolicies,maassubscriptions -n "$MAAS_NAMESPACE" 2>/dev/null || true
}

run_subscription_tests() {
    echo "-- Subscription Controller Tests --"

    export GATEWAY_HOST="${HOST}"
    export MAAS_NAMESPACE

    local test_dir="$PROJECT_ROOT/test/e2e"
    local reports_dir="$test_dir/reports"
    mkdir -p "$reports_dir"

    if [[ -d "$test_dir/.venv" ]]; then
        source "$test_dir/.venv/bin/activate"
    fi

    local user
    user="$(oc whoami 2>/dev/null || echo 'unknown')"
    local html="$reports_dir/subscription-${user}.html"
    local xml="$reports_dir/subscription-${user}.xml"

    if ! PYTHONPATH="$test_dir:${PYTHONPATH:-}" pytest \
        -q --maxfail=3 --disable-warnings \
        --junitxml="$xml" \
        --html="$html" --self-contained-html \
        --capture=tee-sys --show-capture=all --log-level=INFO \
        "$test_dir/tests/test_subscription.py"; then
        echo "❌ ERROR: Subscription tests failed"
        exit 1
    fi

    echo "✅ Subscription tests completed"
    echo " - JUnit XML : ${xml}"
    echo " - HTML      : ${html}"
}

cleanup_subscription_resources() {
    echo "Cleaning up subscription resources so other tests are not affected..."

    oc delete maasauthpolicies --all -n "$MAAS_NAMESPACE" --timeout=60s 2>/dev/null || true
    oc delete maassubscriptions --all -n "$MAAS_NAMESPACE" --timeout=60s 2>/dev/null || true
    oc delete maasmodels --all -n "$MAAS_NAMESPACE" --timeout=60s 2>/dev/null || true

    oc delete authpolicy -n openshift-ingress -l app.kubernetes.io/managed-by=maas-controller --timeout=30s 2>/dev/null || true
    oc delete tokenratelimitpolicy -n openshift-ingress -l app.kubernetes.io/managed-by=maas-controller --timeout=30s 2>/dev/null || true
    oc delete authpolicy --all -n llm --timeout=30s 2>/dev/null || true
    oc delete tokenratelimitpolicy --all -n llm --timeout=30s 2>/dev/null || true

    echo "Waiting for gateway to settle..."
    sleep 15

    echo "✅ Subscription resources cleaned up"
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

# Subscription tests run as cluster admin (not an SA) because premium model
# tests require the user to be in the premium-user OpenShift group.
if [[ "${SKIP_SUBSCRIPTION_TESTS}" != "true" ]]; then
    print_header "Deploying Subscription CRs"
    deploy_subscription_crs

    print_header "Running Subscription Controller Tests (as cluster admin)"
    run_subscription_tests

    print_header "Cleaning up Subscription Resources"
    cleanup_subscription_resources
fi

# TODO: The maas-api /v1/models catalog now discovers models via MaaSModel CRs,
# which are deleted during subscription cleanup. Until the maas-api supports
# model discovery without MaaSModel CRs (or gateway-default-deny is scoped to
# model routes only), smoke tests need MODEL_NAME set explicitly.
export MODEL_NAME="${MODEL_NAME:-facebook/opt-125m}"

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