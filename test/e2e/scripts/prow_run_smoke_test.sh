#!/bin/bash

# =============================================================================
# MaaS Platform End-to-End Testing Script
# =============================================================================
#
# This script automates the complete deployment and validation of the MaaS 
# platform on OpenShift with multi-user testing capabilities.
#
# WHAT IT DOES:
#   1. Install cert-manager and LeaderWorkerSet (LWS) operators (required for KServe)
#   2. Deploy MaaS platform via kustomize (RHCL, gateway, MaaS API, maas-controller)
#   3. Install OpenDataHub (ODH) operator with DataScienceCluster (KServe)
#   4. Deploy MaaS system (free + premium + unconfigured: LLMIS + MaaSModel + MaaSAuthPolicy + MaaSSubscription)
#   5. Run subscription controller tests (test_subscription.py)
#   6. Create admin test user and run deployment validation + token metadata verification
# 
# USAGE:
#   ./test/e2e/scripts/prow_run_smoke_test.sh
#
# CI/CD PIPELINE USAGE:
#   # Test with pipeline-built images
#   OPERATOR_CATALOG=quay.io/opendatahub/opendatahub-operator-catalog:pr-123 \
#   MAAS_API_IMAGE=quay.io/opendatahub/maas-api:pr-456 \
#   MAAS_CONTROLLER_IMAGE=quay.io/opendatahub/maas-controller:pr-42 \
#   ./test/e2e/scripts/prow_run_smoke_test.sh
#
# ENVIRONMENT VARIABLES:
#   OPERATOR_CATALOG - ODH catalog image (default: quay.io/opendatahub/opendatahub-operator-catalog:latest)
#   OPERATOR_IMAGE   - Custom ODH operator image for CSV patch (optional)
#   SKIP_VALIDATION - Skip deployment validation (default: false)
#   SKIP_TOKEN_VERIFICATION - Skip token metadata verification (default: false)
#   MAAS_API_IMAGE - Custom MaaS API image (default: uses operator default)
#                    Example: quay.io/opendatahub/maas-api:pr-232
#   MAAS_CONTROLLER_IMAGE - Custom MaaS controller image (default: quay.io/opendatahub/maas-controller:latest)
#                           Example: quay.io/opendatahub/maas-controller:pr-430
#   INSECURE_HTTP  - Deploy without TLS and use HTTP for tests (default: false)
#                    Affects deploy.sh (via --disable-tls-backend) and test env
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
SKIP_TOKEN_VERIFICATION=${SKIP_TOKEN_VERIFICATION:-false}
SKIP_SUBSCRIPTION_TESTS=${SKIP_SUBSCRIPTION_TESTS:-false}
SKIP_AUTH_CHECK=${SKIP_AUTH_CHECK:-true}  # TODO: Set to false once operator TLS fix lands
INSECURE_HTTP=${INSECURE_HTTP:-false}

# ODH operator deployment
export MAAS_API_IMAGE=${MAAS_API_IMAGE:-}
export MAAS_CONTROLLER_IMAGE=${MAAS_CONTROLLER_IMAGE:-}
export OPERATOR_CATALOG=${OPERATOR_CATALOG:-quay.io/opendatahub/opendatahub-operator-catalog:latest}
export OPERATOR_IMAGE=${OPERATOR_IMAGE:-}
AUTHORINO_NAMESPACE="kuadrant-system"
MAAS_NAMESPACE="${NAMESPACE:-opendatahub}"

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
    echo "Deploying MaaS platform via ODH operator..."
    if [[ -n "${MAAS_API_IMAGE:-}" ]]; then
        echo "Using custom MaaS API image: ${MAAS_API_IMAGE}"
    fi
    if [[ -n "${MAAS_CONTROLLER_IMAGE:-}" ]]; then
        echo "Using custom MaaS controller image: ${MAAS_CONTROLLER_IMAGE}"
    fi
    if [[ -n "${OPERATOR_CATALOG:-}" ]]; then
        echo "Using ODH catalog: ${OPERATOR_CATALOG}"
    fi
    if [[ -n "${OPERATOR_IMAGE:-}" ]]; then
        echo "Using custom ODH operator image: ${OPERATOR_IMAGE}"
    fi

    # 1. Install cert-manager and LeaderWorkerSet (required for KServe/LLMInferenceService)
    echo "Installing cert-manager and LeaderWorkerSet operators..."
    if ! bash "$PROJECT_ROOT/.github/hack/install-cert-manager-and-lws.sh"; then
        echo "❌ ERROR: cert-manager/LWS installation failed"
        exit 1
    fi

    # 2. Install ODH operator with DataScienceCluster (KServe + ModelsAsService)
    echo "Installing OpenDataHub operator..."
    if ! bash "$PROJECT_ROOT/.github/hack/install-odh.sh"; then
        echo "❌ ERROR: ODH installation failed"
        exit 1
    fi

    # 3. Deploy MaaS via operator (Kuadrant, gateway, maas-api, maas-controller, policies)
    # Note: ODH/catalog already installed by install-odh.sh; deploy.sh will skip duplicate installs
    local deploy_cmd=(
        "$PROJECT_ROOT/scripts/deploy.sh"
        --deployment-mode kustomize
        --operator-catalog "${OPERATOR_CATALOG}"
    )
    if [[ -n "${OPERATOR_IMAGE:-}" ]]; then
        deploy_cmd+=(--operator-image "${OPERATOR_IMAGE}")
    fi
    if [[ "$INSECURE_HTTP" == "true" ]]; then
        deploy_cmd+=(--disable-tls-backend)
    fi

    if ! "${deploy_cmd[@]}"; then
        echo "❌ ERROR: MaaS platform deployment failed"
        exit 1
    fi

    # Wait for DataScienceCluster (install-odh already waited; deploy may have updated)
    if ! wait_datasciencecluster_ready "default-dsc" 300; then
        echo "⚠️  WARNING: DataScienceCluster readiness check had issues, continuing anyway"
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
    echo "Deploying MaaS system (free + premium: LLMIS + MaaSModel + MaaSAuthPolicy + MaaSSubscription)"
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

    # Deploy all at once so dependencies resolve correctly
    # Sample kustomizations hardcode namespace: opendatahub; override to $MAAS_NAMESPACE
    # so CRs land in the correct namespace (CI sets NAMESPACE to a dynamic value).
    if ! (cd "$PROJECT_ROOT" && kustomize build docs/samples/maas-system/ | \
            sed "s/namespace: opendatahub/namespace: $MAAS_NAMESPACE/g" | \
            kubectl apply -f -); then
        echo "❌ ERROR: Failed to deploy MaaS system"
        exit 1
    fi
    echo "✅ MaaS system deployed (free + premium tiers)"

    echo "Waiting for models to be ready..."
    if ! oc wait llminferenceservice/facebook-opt-125m-simulated -n llm --for=condition=Ready --timeout=300s; then
        echo "❌ ERROR: Timed out waiting for free simulator to be ready"
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
    echo "✅ Simulator models ready"

    echo "Waiting for MaaSModels to be Ready..."
    local retries=0
    local all_ready=false
    while [[ $retries -lt 30 ]]; do
        all_ready=true
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

    if ! $all_ready; then
        # TODO: Remove this workaround once maas-controller reconcile logic is correct.
        # Controller can get stuck in a bad state forever; bouncing may unstick it.
        echo "  MaaSModels not ready after ${retries} retries, bouncing maas-controller..."
        kubectl rollout restart deployment/maas-controller -n "$MAAS_NAMESPACE" 2>/dev/null || true
        kubectl rollout status deployment/maas-controller -n "$MAAS_NAMESPACE" --timeout=120s 2>/dev/null || true
        echo "  Retrying MaaSModels wait..."
        retries=0
        while [[ $retries -lt 30 ]]; do
            all_ready=true
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
    fi

    if $all_ready; then
        echo "✅ MaaSModels ready"
    else
        echo "⚠️  WARNING: MaaSModels still not ready after bounce, continuing anyway"
    fi

    wait_for_auth_policies_enforced
}

wait_for_auth_policies_enforced() {
    local timeout=180
    echo "Waiting for Kuadrant AuthPolicies to be enforced (timeout: ${timeout}s)..."

    local namespaces
    namespaces=$(oc get llminferenceservices -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' 2>/dev/null | sort -u)
    if [[ -z "$namespaces" ]]; then
        echo "  No LLMInferenceService namespaces found, skipping AuthPolicy wait"
        return 0
    fi

    local deadline=$((SECONDS + timeout))
    while [[ $SECONDS -lt $deadline ]]; do
        local all_enforced=true
        local total=0
        for ns in $namespaces; do
            while IFS= read -r status; do
                total=$((total + 1))
                if [[ "$status" != "True" ]]; then
                    all_enforced=false
                fi
            done < <(oc get authpolicies -n "$ns" -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Enforced")].status}{"\n"}{end}' 2>/dev/null)
        done
        if $all_enforced && [[ $total -gt 0 ]]; then
            echo "✅ All AuthPolicies enforced ($total policies)"
            return 0
        fi
        echo "  Waiting... ($total policies found, not all enforced yet)"
        sleep 10
    done
    echo "⚠️  WARNING: AuthPolicies not all enforced after ${timeout}s, tests may fail"
    oc get authpolicies -A -o wide 2>/dev/null || true
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

# Premium test token: use premium-service-account when oc whoami -t doesn't work (e.g. Prow).
# TODO: Consolidate token strategies — consider always using SA for consistency across local/Prow.
# TODO: Consider moving SA/namespace constants to a shared config or env defaults.
PREMIUM_USERS_NS="premium-users-namespace"
PREMIUM_SA="premium-service-account"

setup_premium_test_token() {
    echo "Setting up premium test token (SA-based, works when oc whoami -t is unavailable)..."
    # Create namespace and SA
    if ! kubectl get namespace "$PREMIUM_USERS_NS" &>/dev/null; then
        echo "Creating namespace: $PREMIUM_USERS_NS"
        kubectl create namespace "$PREMIUM_USERS_NS"
    fi
    if ! kubectl get sa "$PREMIUM_SA" -n "$PREMIUM_USERS_NS" &>/dev/null; then
        echo "Creating service account: $PREMIUM_SA"
        kubectl create sa "$PREMIUM_SA" -n "$PREMIUM_USERS_NS"
    fi

    # Add premium SA as user (not group) so it gets premium access.
    local sa_user="system:serviceaccount:${PREMIUM_USERS_NS}:${PREMIUM_SA}"
    echo "Patching MaaSAuthPolicy premium-simulator-access to include $sa_user..."
    oc patch maasauthpolicy premium-simulator-access -n "$MAAS_NAMESPACE" --type=merge -p="{\"spec\": {\"subjects\": {\"groups\": [{\"name\": \"premium-user\"}], \"users\": [\"$sa_user\"]}}}"

    echo "Patching MaaSSubscription premium-simulator-subscription to include $sa_user..."
    oc patch maassubscription premium-simulator-subscription -n "$MAAS_NAMESPACE" --type=merge -p="{\"spec\": {\"owner\": {\"groups\": [{\"name\": \"premium-user\"}], \"users\": [\"$sa_user\"]}}}"

    export E2E_TEST_TOKEN_SA_NAMESPACE="$PREMIUM_USERS_NS"
    export E2E_TEST_TOKEN_SA_NAME="$PREMIUM_SA"
    # TODO: Add brief reconcile wait if controller is slow to pick up patches.
    echo "✅ Premium test token setup complete (E2E_TEST_TOKEN_SA_* exported)"
}

run_subscription_tests() {
    echo "-- Subscription Controller Tests --"

    setup_premium_test_token

    export GATEWAY_HOST="${HOST}"
    export MAAS_NAMESPACE

    local test_dir="$PROJECT_ROOT/test/e2e"
    local reports_dir="$test_dir/reports"
    mkdir -p "$reports_dir"

    if [[ ! -d "$test_dir/.venv" ]]; then
        echo "Creating Python venv for subscription tests..."
        python3 -m venv "$test_dir/.venv" --upgrade-deps
    fi
    source "$test_dir/.venv/bin/activate"
    python -m pip install --upgrade pip --quiet
    python -m pip install -r "$test_dir/requirements.txt" --quiet

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

# Setup admin user for validation
print_header "Setting up test user"
setup_test_user "tester-admin-user" "cluster-admin"

print_header "Running Maas e2e Tests as admin user"
ADMIN_TOKEN=$(oc create token tester-admin-user -n default)
oc login --token "$ADMIN_TOKEN" --server "$K8S_CLUSTER_URL"

run_subscription_tests

print_header "Validating Deployment and Token Metadata Logic"
validate_deployment
run_token_verification

echo "🎉 Deployment completed successfully!"