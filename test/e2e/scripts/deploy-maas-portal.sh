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
# This script provides 1 and 3 with test stand-ins:
#   - a `maas-portal` Service backed by a minimal placeholder pod (the auth
#     boundary tests never send traffic through the backend; a TLS-capable
#     echo backend would be needed to test the signed-in 200 path), and
#   - the credentials Secret carrying the confidential
#     `maas-consumer-portal` Keycloak client added to the tenant-a test realm.
#
# It must run AFTER deploy.sh has configured the tenant (EXTERNAL_OIDC=true)
# and the AIGC controller image must carry RHAI-10608; until then the portal
# reconciler simply keeps requeueing and the portal tests skip.
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="${PROJECT_ROOT:-$(cd "${SCRIPT_DIR}/../../.." && pwd)}"

# MAAS_PORTAL defaults to EXTERNAL_OIDC: portal enablement is only meaningful
# in OIDC lanes, and the OIDC CI lane keeps it on.
MAAS_PORTAL="${MAAS_PORTAL:-${EXTERNAL_OIDC:-false}}"
APPLICATIONS_NAMESPACE="${APPLICATIONS_NAMESPACE:-redhat-ods-applications}"
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
COOKIE_SECRET="$(openssl rand -base64 32)"
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
