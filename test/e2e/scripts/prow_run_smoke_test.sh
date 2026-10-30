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
#   AI_GATEWAY_OPERATOR_IMAGE - Custom ai-gateway-operator image (optional)
#   AI_GATEWAY_CONTROLLER_IMAGE - AIGC manager image; unset defaults to
#                                quay.io/opendatahub/odh-ai-gateway-controller:latest (main after AIGC #91).
#                                Set to empty to skip companion install.
#   AIGC_GIT_URL / AIGC_GIT_REF - Optional overrides for deploy-aigc-companion.sh (defaults live in that script)
#   DEPLOY_MODE - MaaS ownership mode (default: kustomize for presubmits)
#   POLICY_ENGINE - Rate-limiting policy engine (default: rhcl)
#   RHCL_STARTING_CSV - Optional RHCL operator startingCSV pin
#   RHCL_NAMESPACE - RHCL/Kuadrant workload namespace (default: kuadrant-system)
#   OLM_BUNDLE_UNPACK_RETRY - Retry failed OLM bundle unpacks during deploy (default: true)
#   INSECURE_HTTP  - Deploy without TLS and use HTTP for tests (default: false)
#   EXTERNAL_OIDC - Enable external OIDC e2e coverage (default: false)
#   OIDC_ISSUER_URL, OIDC_TOKEN_URL, OIDC_CLIENT_ID, OIDC_USERNAME, OIDC_PASSWORD
#   OIDC_AUDIENCE, OIDC_ACCESS_TOKEN_PURPOSE_CLAIM, OIDC_ACCESS_TOKEN_PURPOSE - Optional strict
#                   OIDC contract applied to the default AITenant by deploy.sh
#   E2E_RUN_ONLY_OIDC_TESTS - Skip passes 1–2 and run only the OIDC tests (requires EXTERNAL_OIDC=true)
#   OIDC_READINESS_STRICT - Exit if OIDC gateway readiness fails (default: true)
#   DEPLOYMENT_NAMESPACE - Namespace of maas-controller (default: opendatahub)
#   MAAS_SUBSCRIPTION_NAMESPACE - Namespace of MaaS CRs (default: models-as-a-service)
#   ENABLE_TENANT_NAMESPACE_DISCOVERY - Patch maas-controller before pytest (default: true)
#   AITENANT_NAMESPACE - Namespace for AITenant CRs (default: ai-tenants)
#   GATEWAY_NAMESPACE - Gateway namespace (default: openshift-ingress)
#   MODEL_NAMESPACE - Namespace of models (default: llm)
#   E2E_PARALLEL_WORKERS - pytest-xdist worker count (default: 5)
#   TENANT_EXTERNAL_MODEL_CROSSNS_E2E - Opt in to P1 cross-namespace external
#                         provider E2E (needs AGC cross-namespace provider
#                         support packaged, default: false)
#   TENANT_EXTERNAL_MODEL_LEGACY_IPP_E2E - Opt in to the legacy IPP coexistence
#                         E2E (scenario 5; needs the IPP image pin with the
#                         legacy-controller exclusion guard, default: false)
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
# Default to AIGC main (Konflux :latest after AIGC #91). odh-stable lags that merge.
# Unset → install companion; explicit empty → skip.
if [[ -z "${AI_GATEWAY_CONTROLLER_IMAGE+x}" ]]; then
    export AI_GATEWAY_CONTROLLER_IMAGE="quay.io/opendatahub/odh-ai-gateway-controller:latest"
else
    export AI_GATEWAY_CONTROLLER_IMAGE
fi
export OPERATOR_CATALOG=${OPERATOR_CATALOG:-}
export OPERATOR_IMAGE=${OPERATOR_IMAGE:-}
export DEPLOY_MODE=${DEPLOY_MODE:-kustomize}
export DEPLOY_MODE=${DEPLOY_MODE:-kustomize}
export POLICY_ENGINE="${POLICY_ENGINE:-rhcl}"
export RHCL_NAMESPACE="${RHCL_NAMESPACE:-kuadrant-system}"
export RHCL_STARTING_CSV="${RHCL_STARTING_CSV:-}"
export OLM_BUNDLE_UNPACK_RETRY="${OLM_BUNDLE_UNPACK_RETRY:-true}"

AUTHORINO_NAMESPACE="${AUTHORINO_NAMESPACE:-$(resolve_authorino_namespace "$POLICY_ENGINE")}"
export AUTHORINO_NAMESPACE

DEPLOYMENT_NAMESPACE="${DEPLOYMENT_NAMESPACE:-opendatahub}"
MAAS_SUBSCRIPTION_NAMESPACE="${MAAS_SUBSCRIPTION_NAMESPACE:-models-as-a-service}"
MODEL_NAMESPACE="${MODEL_NAMESPACE:-llm}"
GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-openshift-ingress}"
GATEWAY_NAME="${GATEWAY_NAME:-maas-default-gateway}"
INGRESS_MODE="${INGRESS_MODE:-ocproute}"
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
    # Route mode uses the MaaS GatewayClass and exposes an external load
    # balancer address; clusterip mode uses the OpenShift ingress host.
    gateway_class=$(oc get gateway maas-default-gateway -n openshift-ingress \
        -o jsonpath='{.spec.gatewayClassName}' 2>/dev/null || echo "")
    gateway_listener_host=$(oc get gateway maas-default-gateway -n openshift-ingress \
        -o jsonpath='{.spec.listeners[?(@.protocol=="HTTPS")].hostname}' 2>/dev/null | awk '{print $1}')
    HOST=""
    if [[ -n "${MAAS_GATEWAY_HOST:-}" ]]; then
        HOST="${MAAS_GATEWAY_HOST#*://}"
    elif [[ -n "$gateway_listener_host" ]]; then
        HOST="$gateway_listener_host"
    elif [[ "$gateway_class" != "openshift-default" ]]; then
        gateway_address=$(oc get gateway maas-default-gateway -n openshift-ingress \
            -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || echo "")
        HOST="${gateway_address#*://}"
    fi
    [[ -z "$HOST" ]] && HOST="maas.${CLUSTER_DOMAIN}"
    export HOST
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

_request_oidc_token() {
    local username="${1:-${OIDC_USERNAME}}"
    local password="${2:-${OIDC_PASSWORD}}"
    local attempt token_response access_token
    for ((attempt = 1; attempt <= 12; attempt++)); do
        if token_response=$(curl -sk -m 10 \
            -d "grant_type=password&client_id=${OIDC_CLIENT_ID}&username=${username}&password=${password}&scope=openid" \
            "${OIDC_TOKEN_URL}" 2>/dev/null); then
            access_token=$(printf '%s' "$token_response" | python3 -c \
                "import sys,json; print(json.load(sys.stdin).get('access_token',''))" 2>/dev/null || true)
            if [[ -n "$access_token" ]]; then
                printf '%s' "$access_token"
                return 0
            fi
        fi
        sleep 5
    done
    echo "Could not obtain an OIDC token for ${username} from ${OIDC_TOKEN_URL} after 12 attempts" >&2
    return 1
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
        local oidc_readiness_username="${OIDC_READINESS_USERNAME:-${OIDC_USERNAME}}"
        local oidc_readiness_password="${OIDC_READINESS_PASSWORD:-${OIDC_PASSWORD}}"
        local oidc_token
        local oidc_token_error_file="${ARTIFACTS_DIR}/oidc-readiness-token-error.log"
        mkdir -p "$ARTIFACTS_DIR"
        echo "Using OIDC readiness user: ${oidc_readiness_username}"
        if ! oidc_token=$(_request_oidc_token "$oidc_readiness_username" "$oidc_readiness_password" 2>"$oidc_token_error_file"); then
            echo "❌ ERROR: Could not obtain OIDC token from ${OIDC_TOKEN_URL}"
            exit 1
        fi
        rm -f "$oidc_token_error_file"
        if [[ -n "$oidc_token" ]]; then
            if [[ "${OIDC_READINESS_VALIDATE_ORGANIZATIONS:-false}" == "true" ]]; then
                local oidc_claim_summary
                if ! oidc_claim_summary=$(OIDC_TOKEN="$oidc_token" \
                    OIDC_CLIENT_ID="$OIDC_CLIENT_ID" \
                    OIDC_AUDIENCE="${OIDC_AUDIENCE:-}" \
                    OIDC_ACCESS_TOKEN_PURPOSE_CLAIM="${OIDC_ACCESS_TOKEN_PURPOSE_CLAIM:-}" \
                    OIDC_ACCESS_TOKEN_PURPOSE="${OIDC_ACCESS_TOKEN_PURPOSE:-}" \
                    python3 -c '
import base64
import json
import os

parts = os.environ["OIDC_TOKEN"].split(".")
if len(parts) != 3:
    raise SystemExit("OIDC readiness token is not a three-part JWT")
payload = parts[1]
payload += "=" * (-len(payload) % 4)
claims = json.loads(base64.urlsafe_b64decode(payload))

client_id = os.environ.get("OIDC_CLIENT_ID", "")
if client_id and claims.get("azp") != client_id:
    actual_client_id = claims.get("azp")
    raise SystemExit(f"OIDC azp mismatch: expected {client_id}, got {actual_client_id!r}")

audience = os.environ.get("OIDC_AUDIENCE", "")
token_audience = claims.get("aud")
if audience and token_audience != audience and audience not in (token_audience if isinstance(token_audience, list) else []):
    raise SystemExit(f"OIDC audience mismatch: expected {audience}, got {token_audience!r}")

purpose_claim = os.environ.get("OIDC_ACCESS_TOKEN_PURPOSE_CLAIM", "")
purpose = os.environ.get("OIDC_ACCESS_TOKEN_PURPOSE", "")
if purpose_claim and claims.get(purpose_claim) != purpose:
    raise SystemExit(f"OIDC purpose mismatch: {purpose_claim}={claims.get(purpose_claim)!r}, expected {purpose!r}")

organizations = claims.get("https://redhat.com/maas/orgs")
if not isinstance(organizations, list) or not organizations:
    raise SystemExit(f"OIDC organization claim must be a non-empty array, got {organizations!r}")
for organization in organizations:
    if not isinstance(organization, dict) or not organization.get("organization_id") or not organization.get("roles"):
        raise SystemExit(f"Invalid OIDC organization membership: {organization!r}")

authorized_party = claims.get("azp")
print(f"azp={authorized_party}, audience={token_audience!r}, organizations={len(organizations)}")
                    ' 2>&1); then
                    printf '%s\n' "$oidc_claim_summary" > "${ARTIFACTS_DIR}/oidc-readiness-claim-error.log"
                    echo "❌ ERROR: OIDC readiness token contract validation failed"
                    echo "$oidc_claim_summary"
                    exit 1
                fi
                echo "✅ OIDC token contract valid (${oidc_claim_summary})"
            fi
            local oidc_response_file="${ARTIFACTS_DIR}/oidc-readiness-last-response.json"
            local oidc_headers_file="${ARTIFACTS_DIR}/oidc-readiness-last-headers.txt"
            while [[ $SECONDS -lt $oidc_deadline ]]; do
                local oidc_code
                oidc_code=$(curl -sk -D "$oidc_headers_file" -o "$oidc_response_file" -w '%{http_code}' -m 5 \
                    -H "Authorization: Bearer ${oidc_token}" \
                    -H "Content-Type: application/json" \
                    -d "{\"name\": \"e2e-oidc-readiness-$(date +%s)\"}" \
                    "${api_base}/v1/api-keys" 2>/dev/null || true)
                oidc_code="${oidc_code:-000}"
                if [[ "$oidc_code" =~ ^(200|201)$ ]]; then
                    echo "✅ OIDC token authentication working (HTTP $oidc_code)"
                    rm -f "$oidc_response_file" "$oidc_headers_file"
                    break
                fi
                local oidc_reason
                oidc_reason=$(tr -d '\r' < "$oidc_headers_file" 2>/dev/null | awk 'BEGIN{IGNORECASE=1} /^x-ext-auth-reason:/{sub(/^[^:]*: */, ""); print; exit}' || true)
                local oidc_response
                oidc_response=$(tr '\n' ' ' < "$oidc_response_file" 2>/dev/null | cut -c1-500 || true)
                echo "  OIDC auth check returned HTTP $oidc_code${oidc_reason:+ (x-ext-auth-reason=${oidc_reason})}: ${oidc_response}"
                if [[ "$oidc_code" == "401" ]]; then
                    if ! oidc_token=$(_request_oidc_token "$oidc_readiness_username" "$oidc_readiness_password"); then
                        echo "❌ ERROR: Could not refresh OIDC token from ${OIDC_TOKEN_URL}"
                        exit 1
                    fi
                fi
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
    export E2E_PARALLEL_WORKERS="${E2E_PARALLEL_WORKERS:-5}"
    export E2E_RECONCILE_WAIT="${E2E_RECONCILE_WAIT:-4}"
    # Nightly tier (pass 3) only when EXTERNAL_OIDC is deployed; passes 1–2 always skip nightly.
    if [[ "${EXTERNAL_OIDC}" == "true" ]]; then
        export MAAS_PYTEST_INCLUDE_NIGHTLY="${MAAS_PYTEST_INCLUDE_NIGHTLY:-true}"
    fi
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

    if [[ -n "${AI_GATEWAY_CONTROLLER_IMAGE:-}" ]]; then
        print_header "Installing AIGC companion (praxis dataplane)"
        phase_mark deploy_aigc start
        bash "${SCRIPT_DIR}/deploy-aigc-companion.sh"
        phase_mark deploy_aigc end
    fi

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
