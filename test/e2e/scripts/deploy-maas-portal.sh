#!/usr/bin/env bash
# =============================================================================
# Deploy the consumer portal enablement for e2e (MAAS_PORTAL=true).
#
# The consumer portal controller (ai-gateway-controller, RHAI-10608) only
# deploys the portal routes and AuthPolicies when:
#   1. a `maas-portal` Service exists in the applications namespace
#      (normally provided by the RHOAI dashboard, which e2e does not install),
#   2. the default AITenant has an OIDC issuer (EXTERNAL_OIDC=true lanes), and
#   3. the auth proxy credentials Secret exists (the administrator path from
#      RHAI-10607: without spec.oidc.clientSecretRef the controller uses the
#      Secret as-is).
#
# This script provides all three:
#   - optionally the ai-gateway-controller itself (maas e2e lanes run
#     DEPLOY_MODE=kustomize, which sets the DSC aigateway component to Removed,
#     so no controller exists; set AI_GATEWAY_CONTROLLER_IMAGE to fetch the
#     ai-gateway-controller repo at AIGC_COMMIT and deploy it, mirroring how
#     the aigc e2e fetches this repo),
#   - a `maas-portal` Service backed by a minimal placeholder pod (the auth
#     boundary tests never send traffic through the backend; a TLS-capable
#     echo backend would be needed to test the signed-in 200 path), and
#   - the credentials Secret carrying the confidential
#     `maas-consumer-portal` Keycloak client added to the tenant-a test realm.
#
# It must run AFTER deploy.sh has configured the tenant (EXTERNAL_OIDC=true).
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="${PROJECT_ROOT:-$(cd "${SCRIPT_DIR}/../../.." && pwd)}"

# Controller deployment (only when AI_GATEWAY_CONTROLLER_IMAGE is set).
AIGC_REPO="${AIGC_REPO:-https://github.com/opendatahub-io/ai-gateway-controller}"
# Pinned ai-gateway-controller main; bump when the portal work merges. Override
# with AIGC_COMMIT to run a PR branch.
AIGC_COMMIT="${AIGC_COMMIT:-7ee8457d9922394889a4737e6111dbaa2ae706eb}"
AI_GATEWAY_CONTROLLER_IMAGE="${AI_GATEWAY_CONTROLLER_IMAGE:-}"
AI_GATEWAY_CONTROLLER_NAMESPACE="${AI_GATEWAY_CONTROLLER_NAMESPACE:-opendatahub}"
GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-openshift-ingress}"
GATEWAY_NAME="${GATEWAY_NAME:-maas-default-gateway}"

deploy_ai_gateway_controller() {
    if [[ -z "${AI_GATEWAY_CONTROLLER_IMAGE}" ]]; then
        echo "AI_GATEWAY_CONTROLLER_IMAGE not set; assuming ai-gateway-controller is already deployed"
        return 0
    fi
    local work_dir
    work_dir="$(mktemp -d -t aigc-e2e.XXXXXXXXXX)"
    git clone -q "${AIGC_REPO}" "${work_dir}/aigc"
    # GitHub serves fetch-by-sha only for full 40-char SHAs; branch names and
    # tags fetch natively. Anything else (e.g. a short SHA) is resolved from
    # the remote's refs after fetching them.
    if git -C "${work_dir}/aigc" fetch -q --depth 1 origin "${AIGC_COMMIT}" 2>/dev/null; then
        git -C "${work_dir}/aigc" checkout -q FETCH_HEAD
    else
        git -C "${work_dir}/aigc" fetch -q origin '+refs/heads/*:refs/remotes/origin/*' --tags
        if ! git -C "${work_dir}/aigc" cat-file -e "${AIGC_COMMIT}^{commit}" 2>/dev/null; then
            echo "ERROR: AIGC_COMMIT=${AIGC_COMMIT} is neither a ref nor a commit on ${AIGC_REPO}" >&2
            rm -rf "${work_dir}"
            return 1
        fi
        git -C "${work_dir}/aigc" checkout -q "${AIGC_COMMIT}"
    fi

    cp -a "${work_dir}/aigc/config/self" "${work_dir}/self"
    sed -i "s|^ai-gateway-controller-image=.*|ai-gateway-controller-image=${AI_GATEWAY_CONTROLLER_IMAGE}|" \
        "${work_dir}/self/default/params.env"

    echo "Deploying ai-gateway-controller ${AI_GATEWAY_CONTROLLER_IMAGE} (AIGC_COMMIT=${AIGC_COMMIT})"
    if command -v kustomize >/dev/null 2>&1; then
        kustomize build "${work_dir}/self/default"
    else
        kubectl kustomize "${work_dir}/self/default"
    fi | sed \
        -e "s|value: maas-default-gateway|value: ${GATEWAY_NAME}|g" \
        -e "s|value: openshift-ingress|value: ${GATEWAY_NAMESPACE}|g" \
        | kubectl apply -f -
    rm -rf "${work_dir}"

    kubectl -n "${AI_GATEWAY_CONTROLLER_NAMESPACE}" rollout status \
        deployment/ai-gateway-controller --timeout=300s
}

# MAAS_PORTAL defaults to EXTERNAL_OIDC: portal enablement is only meaningful
# in OIDC lanes, and the OIDC CI lane keeps it on.
MAAS_PORTAL="${MAAS_PORTAL:-${EXTERNAL_OIDC:-false}}"
# The stand-in Service must live where the controller looks for it. The
# controller resolves the dashboard namespace from the DSCInitialization
# spec.applicationsNamespace (falling back to redhat-ods-applications), so
# follow the same source here; APPLICATIONS_NAMESPACE overrides for clusters
# without a DSCI.
if [[ -z "${APPLICATIONS_NAMESPACE:-}" ]]; then
    APPLICATIONS_NAMESPACE="$(kubectl get dscinitializations.dscinitialization.opendatahub.io \
        -A -o jsonpath='{.items[0].spec.applicationsNamespace}' 2>/dev/null || true)"
    APPLICATIONS_NAMESPACE="${APPLICATIONS_NAMESPACE:-redhat-ods-applications}"
fi
MAAS_SUBSCRIPTION_NAMESPACE="${MAAS_SUBSCRIPTION_NAMESPACE:-models-as-a-service}"
MAAS_PORTAL_CLIENT_ID="${MAAS_PORTAL_CLIENT_ID:-maas-consumer-portal}"
# Must match the tenant-a realm's maas-consumer-portal client secret.
MAAS_PORTAL_CLIENT_SECRET="${MAAS_PORTAL_CLIENT_SECRET:-maas-e2e-portal-client-secret}"
CREDENTIALS_SECRET_NAME="maas-consumer-portal-auth-proxy-creds"

if [[ "${MAAS_PORTAL}" != "true" ]]; then
    echo "MAAS_PORTAL is not true; skipping consumer portal enablement"
    exit 0
fi

if [[ "${EXTERNAL_OIDC:-false}" != "true" ]]; then
    echo "ERROR: MAAS_PORTAL=true requires EXTERNAL_OIDC=true (the portal needs an OIDC issuer on the AITenant)" >&2
    exit 1
fi

# The infra namespace is where the controller deploys the proxy and reads the
# credentials Secret; maas-controller publishes it on the MaasTenantConfig.
INFRA_NAMESPACE="$(kubectl get maastenantconfigs.maas.opendatahub.io \
    -n "${MAAS_SUBSCRIPTION_NAMESPACE}" \
    -o jsonpath='{.items[0].status.infraNamespace}' 2>/dev/null || true)"
if [[ -z "${INFRA_NAMESPACE}" ]]; then
    echo "ERROR: no MaasTenantConfig with status.infraNamespace found in ${MAAS_SUBSCRIPTION_NAMESPACE}; is MaaS deployed?" >&2
    exit 1
fi
echo "Consumer portal e2e enablement: infra namespace ${INFRA_NAMESPACE}, applications namespace ${APPLICATIONS_NAMESPACE}"

# --- 0. ai-gateway-controller (kustomize-mode lanes have none) ---
deploy_ai_gateway_controller

# --- 0.5. Gateway listener hostname + passthrough Route -----------------------
# The portal controller requires a concrete HTTPS listener hostname on the
# Gateway (OIDC callback URL, route hostname). Kustomize-mode deploys the
# Gateway without one — the OpenShift Route carries the external host — so
# patch it from the cluster domain. The Route must also be passthrough:
# reencrypt re-encrypts with the backend Service name as SNI, which matches
# no filter chain once the listener has a concrete hostname.
GATEWAY_LISTENER_HOSTNAME="$(kubectl get gateway "${GATEWAY_NAME}" -n "${GATEWAY_NAMESPACE}" \
    -o jsonpath='{.spec.listeners[0].hostname}' 2>/dev/null || true)"
CLUSTER_DOMAIN="$(oc get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"
PORTAL_HOSTNAME="${PORTAL_HOSTNAME:-maas.${CLUSTER_DOMAIN}}"
if [[ -z "${GATEWAY_LISTENER_HOSTNAME}" ]]; then
    LISTENER_JSON="$(kubectl get gateway "${GATEWAY_NAME}" -n "${GATEWAY_NAMESPACE}" -o json)"
    LISTENER_PATCH="$(python3 -c "
import json,sys
gw=json.loads('''${LISTENER_JSON}''')
listeners=[]
for l in gw['spec']['listeners']:
    if not l.get('hostname'):
        l['hostname']='${PORTAL_HOSTNAME}'
    listeners.append(l)
print(json.dumps({'spec':{'listeners':listeners}}))" 2>/dev/null || true)"
    if [[ -n "${LISTENER_PATCH}" ]]; then
        echo "Patching Gateway ${GATEWAY_NAMESPACE}/${GATEWAY_NAME} listener hostname to ${PORTAL_HOSTNAME}"
        kubectl patch gateway "${GATEWAY_NAME}" -n "${GATEWAY_NAMESPACE}" --type=merge -p "${LISTENER_PATCH}"
    fi
fi
ROUTE_TERMINATION="$(kubectl get route maas-gateway-route -n openshift-ingress \
    -o jsonpath='{.spec.tls.termination}' 2>/dev/null || true)"
if [[ "${ROUTE_TERMINATION}" == "reencrypt" ]]; then
    echo "Switching maas-gateway-route to passthrough so the client SNI reaches the Gateway"
    kubectl patch route maas-gateway-route -n openshift-ingress --type=merge \
        -p '{"spec":{"tls":{"termination":"passthrough","insecureEdgeTerminationPolicy":"Redirect","destCACertificate":null}}}'
fi

# --- 1. Stand-in maas-portal Service (unblocks the controller's route gate) ---
# The applications namespace is normally created by the RHOAI dashboard,
# which e2e does not install (dashboard: Removed in the DSC).
if ! kubectl get namespace "${APPLICATIONS_NAMESPACE}" &>/dev/null; then
    echo "Creating namespace ${APPLICATIONS_NAMESPACE} (dashboard component is Removed in e2e)"
    kubectl create namespace "${APPLICATIONS_NAMESPACE}"
fi
kubectl -n "${APPLICATIONS_NAMESPACE}" apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: maas-portal-stand-in
  labels:
    app: maas-portal-stand-in
    app.kubernetes.io/part-of: maas-consumer-portal-e2e
spec:
  replicas: 1
  selector:
    matchLabels:
      app: maas-portal-stand-in
  template:
    metadata:
      labels:
        app: maas-portal-stand-in
    spec:
      containers:
      - name: placeholder
        image: registry.access.redhat.com/ubi9/ubi-minimal:latest
        command: ["sleep", "infinity"]
        resources:
          requests: {cpu: 10m, memory: 16Mi}
          limits: {cpu: 100m, memory: 64Mi}
---
apiVersion: v1
kind: Service
metadata:
  name: maas-portal
  labels:
    app: maas-portal-stand-in
    app.kubernetes.io/part-of: maas-consumer-portal-e2e
spec:
  selector:
    app: maas-portal-stand-in
  ports:
  - name: https
    port: 8443
    targetPort: 8080
EOF
echo "Created stand-in maas-portal Service in ${APPLICATIONS_NAMESPACE}"

# --- 2. Auth proxy credentials Secret (administrator path) -------------------
# Without spec.oidc.clientSecretRef on the AITenant, the controller uses this
# Secret verbatim, so the client id here may differ from the AITenant's
# spec.oidc.clientId (which maas-api validates JWTs against).
# oauth2-proxy requires the cookie secret to be exactly 16, 24, or 32 bytes;
# hex output of 16 random bytes is a 32-character string.
COOKIE_SECRET="$(openssl rand -hex 16)"
sed \
    -e "s|CREDENTIALS_SECRET_NAME|${CREDENTIALS_SECRET_NAME}|g" \
    -e "s|CLIENT_ID_PLACEHOLDER|${MAAS_PORTAL_CLIENT_ID}|g" \
    -e "s|CLIENT_SECRET_PLACEHOLDER|${MAAS_PORTAL_CLIENT_SECRET}|g" \
    -e "s|COOKIE_SECRET_PLACEHOLDER|${COOKIE_SECRET}|g" \
    <<'EOF' | kubectl -n "${INFRA_NAMESPACE}" apply -f -
apiVersion: v1
kind: Secret
metadata:
  name: CREDENTIALS_SECRET_NAME
  labels:
    app.kubernetes.io/part-of: maas-consumer-portal-e2e
    app.kubernetes.io/managed-by: ai-gateway-controller
stringData:
  OAUTH2_PROXY_CLIENT_ID: CLIENT_ID_PLACEHOLDER
  OAUTH2_PROXY_CLIENT_SECRET: CLIENT_SECRET_PLACEHOLDER
  OAUTH2_PROXY_COOKIE_SECRET: COOKIE_SECRET_PLACEHOLDER
EOF
echo "Created auth proxy credentials Secret ${CREDENTIALS_SECRET_NAME} in ${INFRA_NAMESPACE}"
echo "Consumer portal e2e enablement complete; the controller will deploy the portal on its next reconcile"
