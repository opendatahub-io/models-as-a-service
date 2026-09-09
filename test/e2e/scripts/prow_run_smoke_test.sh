#!/bin/bash

# =============================================================================
# MaaS Platform End-to-End Testing — Thin Orchestrator
# =============================================================================
#
# Orchestrates the CI pipeline: prereqs → deploy → models → tokens → validate → tests.
# Each phase is implemented in a standalone script; this file only sequences them.
#
# Scripts called:
#   deploy-platform.sh    Install cert-manager, ODH, deploy MaaS via deploy.sh
#   deploy-models.sh      Deploy e2e fixtures (LLMIS, MaaSModelRef, AuthPolicy, Subscription)
#   setup-test-tokens.sh  Create test users (admin + regular) and extract auth tokens
#   run_e2e_tests.sh      pytest execution: two-pass xdist (parallel + serial)
#
# USAGE:
#   ./test/e2e/scripts/prow_run_smoke_test.sh
#
# CI/CD PIPELINE USAGE:
#   OPERATOR_CATALOG=quay.io/opendatahub/opendatahub-operator-catalog:pr-123 \
#   MAAS_API_IMAGE=quay.io/opendatahub/maas-api:pr-456 \
#   ./test/e2e/scripts/prow_run_smoke_test.sh
#
# ENVIRONMENT VARIABLES:
#   OPERATOR_CATALOG - ODH catalog image (optional)
#   OPERATOR_IMAGE   - Custom ODH operator image for CSV patch (optional)
#   SKIP_DEPLOYMENT - Skip platform and model deployment (default: false)
#   SKIP_VALIDATION - Skip deployment validation (default: false)
#   MAAS_API_IMAGE - Custom MaaS API image (optional)
#   MAAS_CONTROLLER_IMAGE - Custom MaaS controller image (optional)
#   AI_GATEWAY_OPERATOR_IMAGE - Custom ai-gateway-operator image (optional, requires DEPLOY_MODE=operator)
#   DEPLOY_MODE           - kustomize (default) or operator
#   POLICY_ENGINE - Rate-limiting policy engine (default: rhcl)
#   RHCL_STARTING_CSV - Optional RHCL operator startingCSV pin
#   RHCL_NAMESPACE - RHCL/Kuadrant workload namespace (default: kuadrant-system)
#   INSECURE_HTTP  - Deploy without TLS and use HTTP for tests (default: false)
#   EXTERNAL_OIDC - Enable external OIDC e2e coverage (default: false)
#   OIDC_ISSUER_URL, OIDC_TOKEN_URL, OIDC_CLIENT_ID, OIDC_USERNAME, OIDC_PASSWORD
#   OIDC_READINESS_STRICT - Exit if OIDC gateway readiness fails (default: true)
#   DEPLOYMENT_NAMESPACE - Namespace of maas-controller (default: opendatahub)
#   MAAS_SUBSCRIPTION_NAMESPACE - Namespace of MaaS CRs (default: models-as-a-service)
#   ENABLE_TENANT_NAMESPACE_DISCOVERY - Patch maas-controller before pytest (default: true)
#   AITENANT_NAMESPACE - Namespace for AITenant CRs (default: ai-tenants)
#   GATEWAY_NAMESPACE - Gateway namespace (default: openshift-ingress)
#   MODEL_NAMESPACE - Namespace of models (default: llm)
#   E2E_PARALLEL_WORKERS - pytest-xdist worker count (default: 7)
#
# TIMEOUT CONFIGURATION (all in seconds, from deployment-helpers.sh):
#   CUSTOM_RESOURCE_TIMEOUT, LLMIS_TIMEOUT, MAASMODELREF_TIMEOUT,
#   AUTHPOLICY_TIMEOUT, AUTHORINO_TIMEOUT, ROLLOUT_TIMEOUT
# =============================================================================

set -euo pipefail

# ── Bootstrap ────────────────────────────────────────────────────────────
_find_project_root_bootstrap() {
  local dir="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
  while [[ "$dir" != "/" && ! -e "$dir/.git" ]]; do dir="$(dirname "$dir")"; done
  [[ -e "$dir/.git" ]] && printf '%s\n' "$dir" || return 1
}
PROJECT_ROOT="$(_find_project_root_bootstrap)"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

source "$PROJECT_ROOT/scripts/deployment-helpers.sh"
source "$PROJECT_ROOT/test/e2e/scripts/auth_utils.sh"

# ── Configuration (env var defaults) ─────────────────────────────────────
SKIP_DEPLOYMENT=${SKIP_DEPLOYMENT:-false}
SKIP_VALIDATION=${SKIP_VALIDATION:-false}
SKIP_AUTH_CHECK=${SKIP_AUTH_CHECK:-true}
INSECURE_HTTP=${INSECURE_HTTP:-false}
EXTERNAL_OIDC=${EXTERNAL_OIDC:-false}

export MAAS_API_IMAGE=${MAAS_API_IMAGE:-}
export MAAS_CONTROLLER_IMAGE=${MAAS_CONTROLLER_IMAGE:-}
export AI_GATEWAY_OPERATOR_IMAGE=${AI_GATEWAY_OPERATOR_IMAGE:-}
export OPERATOR_CATALOG=${OPERATOR_CATALOG:-}
export OPERATOR_IMAGE=${OPERATOR_IMAGE:-}
DEPLOY_MODE=${DEPLOY_MODE:-kustomize}
export POLICY_ENGINE="${POLICY_ENGINE:-rhcl}"
export RHCL_NAMESPACE="${RHCL_NAMESPACE:-kuadrant-system}"
export RHCL_STARTING_CSV="${RHCL_STARTING_CSV:-}"

AUTHORINO_NAMESPACE="${AUTHORINO_NAMESPACE:-$(resolve_authorino_namespace "$POLICY_ENGINE")}"
export AUTHORINO_NAMESPACE

DEPLOYMENT_NAMESPACE="${DEPLOYMENT_NAMESPACE:-opendatahub}"
MAAS_SUBSCRIPTION_NAMESPACE="${MAAS_SUBSCRIPTION_NAMESPACE:-models-as-a-service}"
MODEL_NAMESPACE="${MODEL_NAMESPACE:-llm}"
GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-openshift-ingress}"
GATEWAY_NAME="${GATEWAY_NAME:-maas-default-gateway}"
INGRESS_MODE="${INGRESS_MODE:-clusterip}"
export INGRESS_MODE
GATEWAY_PROGRAMMED_TIMEOUT="${GATEWAY_PROGRAMMED_TIMEOUT:-600}"
OIDC_READINESS_STRICT="${OIDC_READINESS_STRICT:-true}"
ENABLE_TENANT_NAMESPACE_DISCOVERY="${ENABLE_TENANT_NAMESPACE_DISCOVERY:-true}"
AITENANT_NAMESPACE="${AITENANT_NAMESPACE:-ai-tenants}"

ARTIFACTS_DIR="${ARTIFACT_DIR:-${ARTIFACTS:-${LOG_DIR:-$PROJECT_ROOT/test/e2e/reports}}}"
mkdir -p "$ARTIFACTS_DIR"

# ── Helpers ──────────────────────────────────────────────────────────────
# OIDC helpers (apply_default_oidc_for_keycloak, require_external_oidc_config)
# are in auth_utils.sh, sourced above.
print_header() {
    echo ""
    echo "----------------------------------------"
    echo "$1"
    echo "----------------------------------------"
    echo ""
}

phase_mark() {
    local name="${1:?phase_mark requires a phase name}"
    local event="${2:?phase_mark requires start or end}"
    mkdir -p "$ARTIFACTS_DIR"
    echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) ${name} ${event}" | tee -a "${ARTIFACTS_DIR}/phase-timings.txt"
}

check_prerequisites() {
    echo "Checking prerequisites..."
    local current_user
    if ! current_user=$(oc whoami 2>/dev/null); then
        echo "❌ ERROR: Not logged into OpenShift. Please run 'oc login' first"
        exit 1
    fi
    if ! oc auth can-i '*' '*' --all-namespaces >/dev/null 2>&1; then
        echo "❌ ERROR: User '$current_user' does not have admin privileges"
        exit 1
    elif ! kubectl get --raw /apis/config.openshift.io/v1/clusterversions >/dev/null 2>&1; then
        echo "❌ ERROR: This script is designed for OpenShift clusters only"
        exit 1
    fi
    echo "✅ Prerequisites met - logged in as: $current_user on OpenShift"
}

enable_tenant_namespace_discovery_for_e2e() {
    [[ "${ENABLE_TENANT_NAMESPACE_DISCOVERY}" == "true" ]] || return 0

    echo "Enabling --enable-tenant-namespace-discovery on maas-controller..."
    if ! oc get deployment maas-controller -n "$DEPLOYMENT_NAMESPACE" &>/dev/null; then
        echo "❌ ERROR: maas-controller not found in ${DEPLOYMENT_NAMESPACE}"
        return 1
    fi

    local args_json
    args_json="$(oc get deployment maas-controller -n "$DEPLOYMENT_NAMESPACE" -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null || echo '[]')"
    if echo "$args_json" | grep -q 'enable-tenant-namespace-discovery'; then
        echo "✅ maas-controller already has tenant namespace discovery enabled"
    elif [[ -z "$args_json" || "$args_json" == "<no value>" ]]; then
        oc patch deployment maas-controller -n "$DEPLOYMENT_NAMESPACE" --type=json -p='[
          {"op": "add", "path": "/spec/template/spec/containers/0/args", "value": ["--enable-tenant-namespace-discovery=true"]}
        ]' || { echo "❌ ERROR: failed to initialize args"; return 1; }
    else
        oc patch deployment maas-controller -n "$DEPLOYMENT_NAMESPACE" --type=json -p='[
          {"op": "add", "path": "/spec/template/spec/containers/0/args/-", "value": "--enable-tenant-namespace-discovery=true"}
        ]' || { echo "❌ ERROR: failed to patch args"; return 1; }
    fi

    if ! echo "$args_json" | grep -q 'enable-tenant-namespace-discovery'; then
        oc rollout status deployment/maas-controller -n "$DEPLOYMENT_NAMESPACE" --timeout=180s || { echo "❌ ERROR: rollout failed"; return 1; }
        echo "✅ maas-controller patched with --enable-tenant-namespace-discovery=true"
    fi
}

setup_vars_for_tests() {
    echo "-- Setting up variables for tests --"
    K8S_CLUSTER_URL=$(oc whoami --show-server)
    export K8S_CLUSTER_URL
    [[ -z "$K8S_CLUSTER_URL" ]] && { echo "❌ ERROR: Failed to retrieve cluster URL"; exit 1; }
    echo "K8S_CLUSTER_URL: ${K8S_CLUSTER_URL}"

    export INSECURE_HTTP
    [[ "$INSECURE_HTTP" == "true" ]] && echo "⚠️  INSECURE_HTTP=true - will use HTTP for tests"

    export CLUSTER_DOMAIN="$(oc get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"
    [[ -z "$CLUSTER_DOMAIN" ]] && { echo "❌ ERROR: Failed to detect cluster domain"; exit 1; }
    export HOST="maas.${CLUSTER_DOMAIN}"
    export EXTERNAL_OIDC

    if [[ "${EXTERNAL_OIDC}" == "true" ]]; then
        apply_default_oidc_for_keycloak
        require_external_oidc_config
        export OIDC_ISSUER_URL OIDC_TOKEN_URL OIDC_CLIENT_ID OIDC_USERNAME OIDC_PASSWORD
        echo "OIDC_ISSUER_URL: ${OIDC_ISSUER_URL}"
    fi

    if [[ "$INSECURE_HTTP" == "true" ]]; then
        export MAAS_API_BASE_URL="http://${HOST}/maas-api"
    else
        export MAAS_API_BASE_URL="https://${HOST}/maas-api"
    fi

    echo "HOST: ${HOST}"
    echo "MAAS_API_BASE_URL: ${MAAS_API_BASE_URL}"
    echo "✅ Variables for tests setup completed"
}

validate_deployment() {
    echo "Deployment Validation"
    echo "Using controller namespace: $DEPLOYMENT_NAMESPACE"
    echo "Using maas-api namespace: $MAAS_API_DEPLOYMENT_NAMESPACE"
    echo "Using AITenant namespace: $AITENANT_NAMESPACE"

    if [[ "$SKIP_VALIDATION" == "false" ]]; then
        if ! "$PROJECT_ROOT/scripts/validate-deployment.sh"; then
            echo "⚠️  First validation attempt failed; polling gateway and core deployments then retrying..."
            wait_for_gateway_programmed "$GATEWAY_NAME" "$GATEWAY_NAMESPACE" 60 || true
            kubectl wait --for=condition=Available --timeout=60s \
                "deployment/maas-controller" -n "$DEPLOYMENT_NAMESPACE" 2>/dev/null || true
            if [[ -n "${MAAS_API_DEPLOYMENT_NAMESPACE:-}" ]]; then
                kubectl wait --for=condition=Available --timeout=60s \
                    "deployment/maas-api" -n "$MAAS_API_DEPLOYMENT_NAMESPACE" 2>/dev/null || true
            fi
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

run_e2e_tests() {
    echo "-- E2E Tests (API Keys + Subscription + Models Endpoint) --"

    export GATEWAY_HOST="${HOST}"
    export DEPLOYMENT_NAMESPACE
    export MAAS_SUBSCRIPTION_NAMESPACE
    export GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-openshift-ingress}"
    export GATEWAY_NAME="${GATEWAY_NAME:-maas-default-gateway}"
    export AITENANT_NAMESPACE
    export ENABLE_TENANT_NAMESPACE_DISCOVERY
    enable_tenant_namespace_discovery_for_e2e || exit 1
    export E2E_SKIP_TLS_VERIFY=true
    export MODEL_NAME="facebook-opt-125m-simulated"
    export E2E_MODEL_NAMESPACE="$MODEL_NAMESPACE"

    echo "Running e2e tests with:"
    echo "  - TOKEN: $(echo "${TOKEN:-not set}" | cut -c1-20)..."
    echo "  - ADMIN_OC_TOKEN: $(echo "${ADMIN_OC_TOKEN:-not set}" | cut -c1-20)..."
    echo "  - GATEWAY_HOST: ${GATEWAY_HOST}"

    # Wait for gateway to be reachable
    local scheme="https"
    [[ "$INSECURE_HTTP" == "true" ]] && scheme="http"
    local gw_url="${scheme}://${GATEWAY_HOST}/maas-api/health"
    local gw_timeout=120
    local gw_deadline=$((SECONDS + gw_timeout))
    echo "Waiting for gateway to be reachable: ${gw_url} (timeout: ${gw_timeout}s)..."
    while [[ $SECONDS -lt $gw_deadline ]]; do
        local http_code
        http_code=$(curl -sk -o /dev/null -w '%{http_code}' -m 5 "$gw_url" 2>/dev/null || echo "000")
        if [[ "$http_code" =~ ^2 ]]; then
            echo "✅ Gateway is reachable (HTTP $http_code)"
            break
        fi
        sleep 1
    done
    [[ $SECONDS -ge $gw_deadline ]] && echo "⚠️  WARNING: Gateway not reachable after ${gw_timeout}s, proceeding anyway"

    # Wait for authenticated requests to work end-to-end
    local api_base="${scheme}://${GATEWAY_HOST}/maas-api"
    local auth_timeout=180
    local auth_deadline=$((SECONDS + auth_timeout))
    echo "Waiting for authenticated gateway access (timeout: ${auth_timeout}s)..."
    while [[ $SECONDS -lt $auth_deadline ]]; do
        local auth_code
        auth_code=$(curl -sk -o /dev/null -w '%{http_code}' -m 5 \
            -H "Authorization: Bearer ${TOKEN}" \
            "${api_base}/v1/subscriptions" 2>/dev/null || echo "000")
        if [[ "$auth_code" == "200" ]]; then
            echo "✅ Authenticated gateway access working (HTTP $auth_code)"
            break
        fi
        echo "  Auth check returned HTTP $auth_code, retrying..."
        sleep 5
    done
    if [[ $SECONDS -ge $auth_deadline ]]; then
        echo "❌ ERROR: Authenticated gateway access not working after ${auth_timeout}s"
        echo "   Check AuthPolicy status: kubectl get authpolicy -A -o wide"
        echo "   Check Authorino logs: kubectl logs -n ${AUTHORINO_NAMESPACE} -l app=authorino --tail=50"
        exit 1
    fi

    # OIDC readiness check (when external OIDC is enabled)
    if [[ "${EXTERNAL_OIDC}" == "true" ]] && [[ -n "${OIDC_TOKEN_URL:-}" ]]; then
        if [[ -n "${OIDC_ISSUER_URL:-}" ]]; then
            echo "Checking gateway AuthPolicy OIDC issuer matches OIDC_ISSUER_URL..."
            if ! verify_gateway_oidc_authpolicy "${GATEWAY_NAMESPACE:-openshift-ingress}"; then
                echo "❌ ERROR: Fix deploy (same OIDC_ISSUER_URL as tests) or see deployment-helpers.sh"
                exit 1
            fi
        fi
        local oidc_timeout=180
        local oidc_deadline=$((SECONDS + oidc_timeout))
        echo "Verifying OIDC token authentication works (timeout: ${oidc_timeout}s)..."
        local oidc_token
        oidc_token=$(curl -sk -m 10 \
            -d "grant_type=password&client_id=${OIDC_CLIENT_ID}&username=${OIDC_USERNAME}&password=${OIDC_PASSWORD}&scope=openid" \
            "${OIDC_TOKEN_URL}" 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('access_token',''))" 2>/dev/null || echo "")
        if [[ -n "$oidc_token" ]]; then
            while [[ $SECONDS -lt $oidc_deadline ]]; do
                local oidc_code
                oidc_code=$(curl -sk -o /dev/null -w '%{http_code}' -m 5 \
                    -H "Authorization: Bearer ${oidc_token}" \
                    -H "Content-Type: application/json" \
                    -d "{\"name\": \"e2e-oidc-readiness-$(date +%s)\"}" \
                    "${api_base}/v1/api-keys" 2>/dev/null || echo "000")
                if [[ "$oidc_code" =~ ^(200|201)$ ]]; then
                    echo "✅ OIDC token authentication working (HTTP $oidc_code)"
                    break
                fi
                echo "  OIDC auth check returned HTTP $oidc_code, retrying..."
                sleep 5
            done
            if [[ $SECONDS -ge $oidc_deadline ]]; then
                echo "⚠️  WARNING: OIDC gateway readiness failed after ${oidc_timeout}s"
                if [[ "${OIDC_READINESS_STRICT}" == "true" ]]; then
                    echo "❌ ERROR: OIDC_READINESS_STRICT=true — exiting before pytest."
                    exit 1
                fi
                echo "   Continuing to pytest — OIDC tests will run and fail naturally."
            fi
        else
            echo "❌ ERROR: Could not obtain OIDC token from ${OIDC_TOKEN_URL}"
            exit 1
        fi
    fi

    # Delegate pytest execution to run_e2e_tests.sh
    export ARTIFACTS_DIR
    export E2E_PARALLEL_WORKERS="${E2E_PARALLEL_WORKERS:-7}"
    export E2E_RECONCILE_WAIT="${E2E_RECONCILE_WAIT:-4}"
    "${SCRIPT_DIR}/run_e2e_tests.sh"
}

# ── Artifact collection (EXIT trap) ─────────────────────────────────────
_run_exit_artifacts() {
    local exit_code=$?
    set +e
    DEPLOYMENT_NAMESPACE="$DEPLOYMENT_NAMESPACE" MAAS_SUBSCRIPTION_NAMESPACE="$MAAS_SUBSCRIPTION_NAMESPACE" AUTHORINO_NAMESPACE="$AUTHORINO_NAMESPACE" ARTIFACTS_DIR="$ARTIFACTS_DIR" \
        collect_e2e_artifacts
    echo ""
    echo "========== Auth Debug Report =========="
    mkdir -p "$ARTIFACTS_DIR"
    DEPLOYMENT_NAMESPACE="$DEPLOYMENT_NAMESPACE" MAAS_SUBSCRIPTION_NAMESPACE="$MAAS_SUBSCRIPTION_NAMESPACE" AUTHORINO_NAMESPACE="$AUTHORINO_NAMESPACE" \
        run_auth_debug_report 2>&1 | tee "$ARTIFACTS_DIR/auth-debug.log"
    echo "======================================"
    exit $exit_code
}
trap '_run_exit_artifacts' EXIT

# ═════════════════════════════════════════════════════════════════════════
# Main Execution: prereqs → deploy → models → tokens → validate → tests
# ═════════════════════════════════════════════════════════════════════════

print_header "Deploying Maas on OpenShift"
check_prerequisites

if [[ "$SKIP_DEPLOYMENT" == "true" ]]; then
    echo "  Skipping deployment (SKIP_DEPLOYMENT=true)"
    echo "  Assuming MaaS platform and models are already deployed"
else
    phase_mark deploy_platform start
    source "${SCRIPT_DIR}/deploy-platform.sh"
    phase_mark deploy_platform end

    print_header "Deploying Models"
    phase_mark deploy_models start
    source "${SCRIPT_DIR}/deploy-models.sh"
    phase_mark deploy_models end
    patch_authorino_debug  # from auth_utils.sh
fi

print_header "Setting up variables for tests"
setup_vars_for_tests

print_header "Admin Setup (Premium Test Resources)"
print_header "Setting up test tokens"
source "${SCRIPT_DIR}/setup-test-tokens.sh"

print_header "Validating Deployment"
phase_mark validate start
validate_deployment
phase_mark validate end

print_header "Running E2E Tests"
run_e2e_tests

echo "🎉 Deployment completed successfully!"
