#!/bin/bash
# =============================================================================
# MaaS Platform Deployment
# =============================================================================
# Installs cert-manager, ODH operator, and deploys MaaS via deploy.sh.
# Can be sourced by prow_run_smoke_test.sh or run standalone.
# =============================================================================

set -euo pipefail

# Bootstrap: find PROJECT_ROOT and source helpers if not already loaded.
if [[ -z "${PROJECT_ROOT:-}" ]]; then
    _dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    PROJECT_ROOT="$(cd "$_dir/../../.." && pwd)"
fi
[[ "$(type -t find_project_root 2>/dev/null)" == "function" ]] || source "$PROJECT_ROOT/scripts/deployment-helpers.sh"
[[ "$(type -t apply_default_oidc_for_keycloak 2>/dev/null)" == "function" ]] || source "$PROJECT_ROOT/test/e2e/scripts/auth_utils.sh"

# Env defaults (no-op if already set by orchestrator)
DEPLOY_MODE="${DEPLOY_MODE:-kustomize}"
INSECURE_HTTP="${INSECURE_HTTP:-false}"
EXTERNAL_OIDC="${EXTERNAL_OIDC:-false}"
SKIP_AUTH_CHECK="${SKIP_AUTH_CHECK:-true}"
export POLICY_ENGINE="${POLICY_ENGINE:-rhcl}"
export INGRESS_MODE="${INGRESS_MODE:-clusterip}"
AUTHORINO_NAMESPACE="${AUTHORINO_NAMESPACE:-$(resolve_authorino_namespace "${POLICY_ENGINE}")}"
export AUTHORINO_NAMESPACE

deploy_maas_platform() {
    echo "Deploying MaaS platform via ODH operator..."
    echo "Gateway ingress mode for deploy.sh: ${INGRESS_MODE}"
    [[ -n "${MAAS_API_IMAGE:-}" ]] && echo "Using custom MaaS API image: ${MAAS_API_IMAGE}"
    [[ -n "${MAAS_CONTROLLER_IMAGE:-}" ]] && echo "Using custom MaaS controller image: ${MAAS_CONTROLLER_IMAGE}"
    [[ -n "${OPERATOR_CATALOG:-}" ]] && echo "Using ODH catalog: ${OPERATOR_CATALOG}"
    [[ -n "${OPERATOR_IMAGE:-}" ]] && echo "Using custom ODH operator image: ${OPERATOR_IMAGE}"
    [[ -n "${AI_GATEWAY_OPERATOR_IMAGE:-}" ]] && echo "Using custom ai-gateway-operator image: ${AI_GATEWAY_OPERATOR_IMAGE} (requires DEPLOY_MODE=operator)"
    echo "Deployment mode: ${DEPLOY_MODE}"

    echo "Installing cert-manager and LeaderWorkerSet operators..."
    if ! bash "$PROJECT_ROOT/.github/hack/install-cert-manager-and-lws.sh"; then
        echo "❌ ERROR: cert-manager/LWS installation failed"
        exit 1
    fi

    if [[ "${DEPLOY_MODE}" == "kustomize" ]]; then
        echo "Installing OpenDataHub operator..."
        if ! bash "$PROJECT_ROOT/.github/hack/install-odh.sh"; then
            echo "❌ ERROR: ODH installation failed"
            exit 1
        fi
    else
        echo "Skipping standalone ODH install (DEPLOY_MODE=${DEPLOY_MODE}; deploy.sh installs ODH + DSC directly)"
    fi

    if [[ "${EXTERNAL_OIDC}" == "true" ]]; then
        echo "External OIDC enabled (Keycloak via deploy.sh --enable-keycloak, realm tenant-a defaults)..."
        apply_default_oidc_for_keycloak
        require_external_oidc_config
        export OIDC_ISSUER_URL OIDC_TOKEN_URL OIDC_CLIENT_ID OIDC_USERNAME OIDC_PASSWORD
        echo "Using OIDC issuer: ${OIDC_ISSUER_URL}"
    fi

    export DB_SSLMODE="${DB_SSLMODE:-disable}"
    echo "Using policy engine: ${POLICY_ENGINE} (Authorino namespace: ${AUTHORINO_NAMESPACE})"
    export MODEL_NAMESPACE
    local deploy_cmd=(
        "$PROJECT_ROOT/scripts/deploy.sh"
        --deployment-mode "${DEPLOY_MODE}"
        --policy-engine "${POLICY_ENGINE}"
    )
    [[ -n "${OPERATOR_CATALOG:-}" ]] && deploy_cmd+=(--operator-catalog "${OPERATOR_CATALOG}")
    [[ -n "${OPERATOR_IMAGE:-}" ]] && deploy_cmd+=(--operator-image "${OPERATOR_IMAGE}")
    [[ "$INSECURE_HTTP" == "true" ]] && deploy_cmd+=(--disable-tls-backend)
    [[ "${EXTERNAL_OIDC}" == "true" ]] && deploy_cmd+=(--external-oidc --enable-keycloak)

    if ! "${deploy_cmd[@]}"; then
        echo "❌ ERROR: MaaS platform deployment failed"
        exit 1
    fi

    if [[ "${EXTERNAL_OIDC}" == "true" ]]; then
        echo "Applying Keycloak test realms (tenant-a / tenant-b) for OIDC token tests..."
        if ! bash "$PROJECT_ROOT/docs/samples/install/keycloak/test-realms/apply-test-realms.sh"; then
            echo "❌ ERROR: Keycloak test realm import failed (see docs/samples/install/keycloak/test-realms/)"
            exit 1
        fi

        echo "Mounting ingress CA certificate into Authorino for OIDC JWKS discovery..."
        local ingress_cert_name
        ingress_cert_name=$(oc get ingresscontroller default -n openshift-ingress-operator \
            -o jsonpath='{.spec.defaultCertificate.name}' 2>/dev/null)
        if [[ -n "$ingress_cert_name" ]]; then
            local ca_tmp
            ca_tmp=$(mktemp)
            trap "rm -f '$ca_tmp'; trap - RETURN" RETURN
            if oc get secret "$ingress_cert_name" -n openshift-ingress -o jsonpath='{.data.tls\.crt}' | base64 -d > "$ca_tmp" 2>/dev/null && [[ -s "$ca_tmp" ]]; then
                kubectl create configmap authorino-oidc-ca -n "$AUTHORINO_NAMESPACE" \
                    --from-file=ca.crt="$ca_tmp" --dry-run=client -o yaml | kubectl apply -f -
                oc patch deployment authorino -n "$AUTHORINO_NAMESPACE" --type=json -p '[
                  {"op": "add", "path": "/spec/template/spec/volumes/-", "value": {
                    "name": "oidc-ca", "configMap": {"name": "authorino-oidc-ca"}
                  }},
                  {"op": "add", "path": "/spec/template/spec/containers/0/volumeMounts/-", "value": {
                    "name": "oidc-ca", "mountPath": "/etc/ssl/certs/oidc-ca.crt",
                    "subPath": "ca.crt", "readOnly": true
                  }}
                ]' 2>/dev/null || echo "⚠️  Authorino CA volume may already be mounted"
                if ! oc rollout status deployment/authorino -n "$AUTHORINO_NAMESPACE" --timeout=120s; then
                    echo "⚠️  WARNING: Authorino rollout did not complete within 120s, continuing anyway"
                fi
                echo "✅ Ingress CA mounted into Authorino"
            else
                echo "⚠️  WARNING: Could not extract TLS cert from secret $ingress_cert_name"
            fi
        else
            echo "⚠️  WARNING: No defaultCertificate found on IngressController — Authorino may fail OIDC JWKS discovery"
        fi
    fi

    if ! wait_datasciencecluster_ready "default-dsc" "$CUSTOM_RESOURCE_TIMEOUT"; then
        echo "❌ ERROR: DataScienceCluster did not become ready (timeout: ${CUSTOM_RESOURCE_TIMEOUT}s)"
        echo "Last DataScienceCluster conditions:"
        kubectl get datasciencecluster default-dsc -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}): {.message}{"\n"}{end}' 2>/dev/null \
            || kubectl describe datasciencecluster default-dsc || true
        exit 1
    fi

    if [[ "${SKIP_AUTH_CHECK:-true}" == "true" ]]; then
        echo "⚠️  WARNING: Skipping Authorino readiness check (SKIP_AUTH_CHECK=true)"
    else
        echo "Waiting for Authorino and auth service to be ready (namespace: ${AUTHORINO_NAMESPACE})..."
        if ! wait_authorino_ready "$AUTHORINO_NAMESPACE" "$AUTHORINO_TIMEOUT"; then
            echo "⚠️  WARNING: Authorino readiness check had issues (timeout: ${AUTHORINO_TIMEOUT}s), continuing anyway"
        fi
    fi

    echo "✅ MaaS platform deployment completed"
}

deploy_maas_platform
