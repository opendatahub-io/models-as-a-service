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
#   4. Deploy MaaS system (free + premium + unconfigured: LLMIS + MaaSModelRef + MaaSAuthPolicy + MaaSSubscription)
#   5. Setup test tokens (admin + regular user) for comprehensive testing
#   6. Run E2E tests (API keys + subscription tests)
#   7. Run deployment validation + token metadata verification
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
#   OPERATOR_CATALOG - ODH catalog image (optional). Unset = community-operators ODH 3.3.
#                      Set for custom builds, e.g. quay.io/opendatahub/opendatahub-operator-catalog:latest
#   OPERATOR_IMAGE   - Custom ODH operator image for CSV patch (optional)
#   SKIP_DEPLOYMENT - Skip platform and model deployment (default: false)
#                     Use for running tests against an existing cluster
#   SKIP_VALIDATION - Skip deployment validation (default: false)
#   MAAS_API_IMAGE - Custom MaaS API image (default: uses operator default)
#                    Example: quay.io/opendatahub/maas-api:pr-232
#   MAAS_CONTROLLER_IMAGE - Custom MaaS controller image (default: quay.io/opendatahub/maas-controller:latest)
#                           Example: quay.io/opendatahub/maas-controller:pr-430
#   INSECURE_HTTP  - Deploy without TLS and use HTTP for tests (default: false)
#                    Affects deploy.sh (via --disable-tls-backend) and test env
#   DEPLOYMENT_NAMESPACE - Namespace of MaaS API and controller (default: opendatahub)
#   MAAS_SUBSCRIPTION_NAMESPACE - Namespace of MaaS CRs (default: models-as-a-service)
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
SKIP_DEPLOYMENT=${SKIP_DEPLOYMENT:-false}  # Skip platform and model deployment (for existing clusters)
SKIP_VALIDATION=${SKIP_VALIDATION:-false}
SKIP_AUTH_CHECK=${SKIP_AUTH_CHECK:-true}  # TODO: Set to false once operator TLS fix lands
INSECURE_HTTP=${INSECURE_HTTP:-false}

# ODH operator deployment
export MAAS_API_IMAGE=${MAAS_API_IMAGE:-}
export MAAS_CONTROLLER_IMAGE=${MAAS_CONTROLLER_IMAGE:-}
export OPERATOR_CATALOG=${OPERATOR_CATALOG:-}
export OPERATOR_IMAGE=${OPERATOR_IMAGE:-}
AUTHORINO_NAMESPACE="kuadrant-system"
DEPLOYMENT_NAMESPACE="${DEPLOYMENT_NAMESPACE:-opendatahub}"
MAAS_SUBSCRIPTION_NAMESPACE="${MAAS_SUBSCRIPTION_NAMESPACE:-models-as-a-service}"

# Artifact collection: OpenShift CI provides ARTIFACT_DIR (docs.ci.openshift.org/docs/architecture/step-registry).
# Files written here are collected to artifacts/<job>/<step>/ in Prow. Fallbacks: ARTIFACTS, LOG_DIR, or local reports.
ARTIFACTS_DIR="${ARTIFACT_DIR:-${ARTIFACTS:-${LOG_DIR:-$PROJECT_ROOT/test/e2e/reports}}}"

# Source auth utils (patch_authorino_debug, collect_e2e_artifacts)
source "$PROJECT_ROOT/test/e2e/scripts/auth_utils.sh"

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
    )
    if [[ -n "${OPERATOR_CATALOG:-}" ]]; then
        deploy_cmd+=(--operator-catalog "${OPERATOR_CATALOG}")
    fi
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
    echo "Deploying MaaS system (free + premium: LLMIS + MaaSModelRef + MaaSAuthPolicy + MaaSSubscription)"
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

    # Create MaaS CRs namespace if it does not exist
    if ! kubectl get namespace "$MAAS_SUBSCRIPTION_NAMESPACE" >/dev/null 2>&1; then
        echo "Creating '$MAAS_SUBSCRIPTION_NAMESPACE' namespace..."
        if ! kubectl create namespace "$MAAS_SUBSCRIPTION_NAMESPACE"; then
            echo "❌ ERROR: Failed to create '$MAAS_SUBSCRIPTION_NAMESPACE' namespace"
            exit 1
        fi
    else
        echo "'$MAAS_SUBSCRIPTION_NAMESPACE' namespace already exists"
    fi

    # Deploy all at once so dependencies resolve correctly
    # Sample kustomizations hardcode namespace: models-as-a-service; override to $MAAS_SUBSCRIPTION_NAMESPACE
    # so CRs land in the correct namespace.
    if ! (cd "$PROJECT_ROOT" && kustomize build docs/samples/maas-system/ | \
            sed "s/namespace: models-as-a-service/namespace: $MAAS_SUBSCRIPTION_NAMESPACE/g" | \
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

    # TODO: Currently waits for  ever and bounces controller (seems like they are not reconciled even after llmisvc are reported as up)
    echo "Waiting for MaaSModelRefs to be Ready..."
    local retries=0
    local all_ready=false
    while [[ $retries -lt 30 ]]; do
        all_ready=true
        while IFS= read -r phase; do
            if [[ "$phase" != "Ready" ]]; then
                all_ready=false
                break
            fi
        done < <(oc get maasmodelrefs -n "$DEPLOYMENT_NAMESPACE" -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' 2>/dev/null)
        if $all_ready && [[ -n "$(oc get maasmodelrefs -n "$DEPLOYMENT_NAMESPACE" -o name 2>/dev/null)" ]]; then
            break
        fi
        retries=$((retries + 1))
        sleep 5
    done

    if ! $all_ready; then
        # TODO: Remove this workaround once maas-controller reconcile logic is correct.
        # Controller can get stuck in a bad state forever; bouncing may unstick it.
        echo "  MaaSModelRefs not ready after ${retries} retries, bouncing maas-controller..."
        kubectl rollout restart deployment/maas-controller -n "$DEPLOYMENT_NAMESPACE" 2>/dev/null || true
        kubectl rollout status deployment/maas-controller -n "$DEPLOYMENT_NAMESPACE" --timeout=120s 2>/dev/null || true
        echo "  Retrying MaaSModelRefs wait..."
        retries=0
        while [[ $retries -lt 30 ]]; do
            all_ready=true
            while IFS= read -r phase; do
                if [[ "$phase" != "Ready" ]]; then
                    all_ready=false
                    break
                fi
            done < <(oc get maasmodelrefs -n "$DEPLOYMENT_NAMESPACE" -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' 2>/dev/null)
            if $all_ready && [[ -n "$(oc get maasmodelrefs -n "$DEPLOYMENT_NAMESPACE" -o name 2>/dev/null)" ]]; then
                break
            fi
            retries=$((retries + 1))
            sleep 5
        done
    fi

    if $all_ready; then
        echo "✅ MaaSModelRefs ready"
    else
        echo "⚠️  WARNING: MaaSModelRefs still not ready after bounce, continuing anyway"
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
    echo "Using namespace: $DEPLOYMENT_NAMESPACE"
    
    if [ "$SKIP_VALIDATION" = false ]; then
        if ! "$PROJECT_ROOT/scripts/validate-deployment.sh" --namespace "$DEPLOYMENT_NAMESPACE"; then
            echo "⚠️  First validation attempt failed, waiting 30 seconds and retrying..."
            sleep 30
            if ! "$PROJECT_ROOT/scripts/validate-deployment.sh" --namespace "$DEPLOYMENT_NAMESPACE"; then
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
    oc patch maasauthpolicy premium-simulator-access -n "$MAAS_SUBSCRIPTION_NAMESPACE" --type=merge -p="{\"spec\": {\"subjects\": {\"groups\": [{\"name\": \"premium-user\"}], \"users\": [\"$sa_user\"]}}}"

    echo "Patching MaaSSubscription premium-simulator-subscription to include $sa_user..."
    oc patch maassubscription premium-simulator-subscription -n "$MAAS_SUBSCRIPTION_NAMESPACE" --type=merge -p="{\"spec\": {\"owner\": {\"groups\": [{\"name\": \"premium-user\"}], \"users\": [\"$sa_user\"]}}}"

    export E2E_TEST_TOKEN_SA_NAMESPACE="$PREMIUM_USERS_NS"
    export E2E_TEST_TOKEN_SA_NAME="$PREMIUM_SA"
    # TODO: Add brief reconcile wait if controller is slow to pick up patches.
    echo "✅ Premium test token setup complete (E2E_TEST_TOKEN_SA_* exported)"
}

run_e2e_tests() {
    echo "-- E2E Tests (API Keys + Subscription) --"

    # Note: setup_premium_test_token() is called earlier in main execution
    # (Phase 1: Admin Setup) while still logged in as system:admin

    export GATEWAY_HOST="${HOST}"
    export DEPLOYMENT_NAMESPACE
    export MAAS_SUBSCRIPTION_NAMESPACE
    # Skip TLS verification in CI (self-signed certs)
    export E2E_SKIP_TLS_VERIFY=true
    # Set MODEL_NAME explicitly - maas-api /v1/models currently only lists MaaSModelRefs
    # from its own namespace, but models are deployed in 'llm' namespace.
    # TODO: Fix maas-api to list MaaSModelRefs from ALL namespaces (pass "" to ListFromMaaSModelRefLister)
    export MODEL_NAME="facebook-opt-125m-simulated"
    # TOKEN and ADMIN_OC_TOKEN are already exported by setup_test_tokens()

    local test_dir="$PROJECT_ROOT/test/e2e"
    # Use ARTIFACTS_DIR so pytest reports go to Prow artifact collection (ARTIFACT_DIR)
    mkdir -p "$ARTIFACTS_DIR"

    if [[ ! -d "$test_dir/.venv" ]]; then
        echo "Creating Python venv for e2e tests..."
        python3 -m venv "$test_dir/.venv" --upgrade-deps
    fi
    source "$test_dir/.venv/bin/activate"
    python -m pip install --upgrade pip --quiet
    python -m pip install -r "$test_dir/requirements.txt" --quiet

    local user
    user="$(oc whoami 2>/dev/null || echo 'unknown')"
    local html="$ARTIFACTS_DIR/e2e-${user}.html"
    local xml="$ARTIFACTS_DIR/e2e-${user}.xml"

    echo "Running e2e tests with:"
    echo "  - TOKEN: $(echo "${TOKEN:-not set}" | cut -c1-20)..."
    echo "  - ADMIN_OC_TOKEN: $(echo "${ADMIN_OC_TOKEN:-not set}" | cut -c1-20)..."
    echo "  - GATEWAY_HOST: ${GATEWAY_HOST}"

    # Run all e2e tests: API keys and subscription tests
    if ! PYTHONPATH="$test_dir:${PYTHONPATH:-}" pytest \
        -v --maxfail=5 --disable-warnings \
        --junitxml="$xml" \
        --html="$html" --self-contained-html \
        --capture=tee-sys --show-capture=all --log-level=INFO \
        "$test_dir/tests/test_api_keys.py" \
        "$test_dir/tests/test_subscription.py"; then
        echo "❌ ERROR: E2E tests failed"
        exit 1
    fi

    echo "✅ E2E tests completed"
    echo " - JUnit XML : ${xml}"
    echo " - HTML      : ${html}"
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

setup_test_tokens() {
    # ═══════════════════════════════════════════════════════════════════════════
    # Extract test tokens WITHOUT switching the main oc session.
    # 
    # Architecture:
    #   - Main oc session stays as system:admin (for any cluster operations)
    #   - Test tokens are extracted into env vars using a TEMPORARY kubeconfig
    #   - Tests use TOKEN/ADMIN_OC_TOKEN env vars for API authentication
    #
    # Why htpasswd users instead of SA tokens?
    #   - htpasswd users have OpenShift group memberships (system:authenticated)
    #   - SA tokens don't carry group memberships, so they can't see models
    # ═══════════════════════════════════════════════════════════════════════════
    
    echo "Setting up test tokens (admin + regular user)..."
    
    local current_user api_server
    current_user=$(oc whoami)
    api_server=$(oc whoami --show-server)
    echo "Current admin session: $current_user (will be preserved)"
    
    export ADMIN_OC_TOKEN=""
    export TOKEN=""
    
    # Use a temporary kubeconfig for token extraction logins
    # This prevents polluting the main oc session
    local temp_kubeconfig
    temp_kubeconfig=$(mktemp)
    trap "rm -f '$temp_kubeconfig'" RETURN
    
    # 1. Try htpasswd users from idp-htpasswd step (Prow CI)
    if [[ -f "${SHARED_DIR:-}/runtime_env" ]]; then
        # shellcheck source=/dev/null
        source "${SHARED_DIR}/runtime_env"
        if [[ -n "${USERS:-}" ]]; then
            echo "Found htpasswd users from idp-htpasswd step"
            
            # Admin user: testuser-1 (added to odh-admins)
            local admin_creds
            admin_creds=$(echo "$USERS" | tr ',' '\n' | grep "^testuser-1:" | head -1)
            if [[ -n "$admin_creds" ]]; then
                local admin_user admin_pass
                admin_user="${admin_creds%%:*}"
                admin_pass="${admin_creds#*:}"
                
                # Add to odh-admins group (using main session which is system:admin)
                oc adm groups add-users odh-admins "$admin_user" 2>/dev/null || true
                
                # Extract token using temp kubeconfig (doesn't affect main session)
                if KUBECONFIG="$temp_kubeconfig" oc login "$api_server" -u "$admin_user" -p "$admin_pass" --insecure-skip-tls-verify=true &>/dev/null; then
                    ADMIN_OC_TOKEN=$(KUBECONFIG="$temp_kubeconfig" oc whoami -t)
                    echo "✅ Admin token for $admin_user (htpasswd)"
                fi
            fi
            
            # Regular user: testuser-2 (NOT in odh-admins, but has system:authenticated)
            local regular_creds
            regular_creds=$(echo "$USERS" | tr ',' '\n' | grep "^testuser-2:" | head -1)
            if [[ -n "$regular_creds" ]]; then
                local regular_user regular_pass
                regular_user="${regular_creds%%:*}"
                regular_pass="${regular_creds#*:}"
                
                # Extract token using temp kubeconfig (doesn't affect main session)
                if KUBECONFIG="$temp_kubeconfig" oc login "$api_server" -u "$regular_user" -p "$regular_pass" --insecure-skip-tls-verify=true &>/dev/null; then
                    TOKEN=$(KUBECONFIG="$temp_kubeconfig" oc whoami -t)
                    echo "✅ Regular user token for $regular_user (htpasswd)"
                fi
            fi
        fi
    fi
    
    # 2. Fallback for admin: use current user's token (local htpasswd user)
    if [[ -z "$ADMIN_OC_TOKEN" ]]; then
        ADMIN_OC_TOKEN=$(oc whoami -t 2>/dev/null || true)
        if [[ -n "$ADMIN_OC_TOKEN" ]]; then
            oc adm groups add-users odh-admins "$current_user" 2>/dev/null || true
            echo "✅ Admin token for $current_user (added to odh-admins)"
        else
            echo "⚠️  No htpasswd token available - using SA token (admin tests may fail)"
            setup_test_user "tester-admin-user" "cluster-admin"
            ADMIN_OC_TOKEN=$(oc create token tester-admin-user -n default --duration=1h)
        fi
    fi
    
    # 3. Fallback for regular user: use current user's token (same as admin for local testing)
    # Local runs typically use the same htpasswd user for both roles
    if [[ -z "$TOKEN" ]]; then
        TOKEN=$(oc whoami -t 2>/dev/null || true)
        if [[ -n "$TOKEN" ]]; then
            echo "✅ Regular user token for $current_user (same as admin for local testing)"
        else
            echo "⚠️  No htpasswd token - using SA token (model catalog may be empty)"
            setup_test_user "tester-regular-user" "view"
            TOKEN=$(oc create token tester-regular-user -n default --duration=1h)
        fi
    fi
    
    echo "Token setup complete (main session unchanged: $(oc whoami))"
}

# Main execution
# On exit (success or failure): collect artifacts (authorino-debug.log, cluster state, pod logs) and auth report
_run_exit_artifacts() {
    DEPLOYMENT_NAMESPACE="$DEPLOYMENT_NAMESPACE" MAAS_SUBSCRIPTION_NAMESPACE="$MAAS_SUBSCRIPTION_NAMESPACE" AUTHORINO_NAMESPACE="$AUTHORINO_NAMESPACE" ARTIFACTS_DIR="$ARTIFACTS_DIR" \
        collect_e2e_artifacts
    echo ""
    echo "========== Auth Debug Report =========="
    mkdir -p "$ARTIFACTS_DIR"
    DEPLOYMENT_NAMESPACE="$DEPLOYMENT_NAMESPACE" MAAS_SUBSCRIPTION_NAMESPACE="$MAAS_SUBSCRIPTION_NAMESPACE" AUTHORINO_NAMESPACE="$AUTHORINO_NAMESPACE" \
        run_auth_debug_report 2>&1 | tee "$ARTIFACTS_DIR/auth-debug.log" || true
    echo "======================================"
}
trap '_run_exit_artifacts' EXIT

print_header "Deploying Maas on OpenShift"
check_prerequisites

if [[ "$SKIP_DEPLOYMENT" == "true" ]]; then
    echo "  Skipping deployment (SKIP_DEPLOYMENT=true)"
    echo "  Assuming MaaS platform and models are already deployed"
else
    deploy_maas_platform

    print_header "Deploying Models"  
    deploy_models
    patch_authorino_debug  # from auth_utils.sh
fi

print_header "Setting up variables for tests"
setup_vars_for_tests

# ═══════════════════════════════════════════════════════════════════════════════
# Phase 1: Admin Setup (runs as system:admin)
# All cluster operations requiring admin privileges happen here BEFORE
# we extract test tokens. This avoids context-switching issues.
# ═══════════════════════════════════════════════════════════════════════════════
print_header "Admin Setup (Premium Test Resources)"
setup_premium_test_token

# ═══════════════════════════════════════════════════════════════════════════════
# Phase 2: Extract Test Tokens
# Uses a temporary kubeconfig so the main oc session stays as system:admin.
# Tests will use TOKEN/ADMIN_OC_TOKEN env vars for API authentication.
# ═══════════════════════════════════════════════════════════════════════════════
print_header "Setting up test tokens"
setup_test_tokens

# 15m matches Prow step timeout; sleep leaves time for cluster debugging before tests
# echo "Sleeping 15 minutes for cluster debugging (Ctrl+C to skip)..."
# sleep 900

# ═══════════════════════════════════════════════════════════════════════════════
# Phase 3: Run Tests
# Tests use TOKEN/ADMIN_OC_TOKEN env vars for API auth.
# The main oc session is still system:admin for any kubectl/oc commands.
# ═══════════════════════════════════════════════════════════════════════════════
print_header "Running E2E Tests"
run_e2e_tests

print_header "Validating Deployment"
validate_deployment

echo "🎉 Deployment completed successfully!"