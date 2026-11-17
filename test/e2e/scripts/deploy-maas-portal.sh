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
# This script provides all three (the controller itself is installed by
# deploy-aigc-companion.sh, which prow_run_smoke_test.sh runs next; it
# reconciles the portal as soon as it starts):
#   - a `maas-portal` Service backed by a minimal placeholder pod that serves
#     nothing (signed-in requests pass the AuthPolicy and then fail at the
#     empty backend, so the tests can tell "authenticated" from "redirected
#     to sign-in" without a real dashboard), and
#   - the credentials Secret carrying the confidential
#     `maas-consumer-portal` Keycloak client added to the tenant-a test realm.
#
# It must run AFTER deploy.sh has configured the tenant (EXTERNAL_OIDC=true).
# =============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="${PROJECT_ROOT:-$(cd "${SCRIPT_DIR}/../../.." && pwd)}"

GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-openshift-ingress}"
GATEWAY_NAME="${GATEWAY_NAME:-maas-default-gateway}"

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
        -A -o jsonpath='{.items[*].spec.applicationsNamespace}')"
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
    -o jsonpath='{.items[*].status.infraNamespace}')"
if [[ -z "${INFRA_NAMESPACE}" ]]; then
    echo "ERROR: no MaasTenantConfig with status.infraNamespace found in ${MAAS_SUBSCRIPTION_NAMESPACE}; is MaaS deployed?" >&2
    exit 1
fi
echo "Consumer portal e2e enablement: infra namespace ${INFRA_NAMESPACE}, applications namespace ${APPLICATIONS_NAMESPACE}"

# --- 0. Gateway listener hostname + passthrough Route -------------------------
# The portal controller requires a concrete HTTPS listener hostname on the
# Gateway (OIDC callback URL, route hostname). Kustomize-mode deploys the
# Gateway without one — the OpenShift Route carries the external host — so
# patch it from the cluster domain. The Route must also be passthrough:
# reencrypt re-encrypts with the backend Service name as SNI, which matches
# no filter chain once the listener has a concrete hostname.
CLUSTER_DOMAIN="$(oc get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"
PORTAL_HOSTNAME="${PORTAL_HOSTNAME:-maas.${CLUSTER_DOMAIN}}"
GATEWAY_JSON="$(kubectl get gateway "${GATEWAY_NAME}" -n "${GATEWAY_NAMESPACE}" -o json)"
if jq -e 'any(.spec.listeners[]; .hostname == null)' <<<"${GATEWAY_JSON}" >/dev/null; then
    echo "Patching Gateway ${GATEWAY_NAMESPACE}/${GATEWAY_NAME} listener hostname to ${PORTAL_HOSTNAME}"
    # A merge patch replaces the listeners list, so send every listener back
    # and fill in only the missing hostnames.
    kubectl patch gateway "${GATEWAY_NAME}" -n "${GATEWAY_NAMESPACE}" --type=merge -p "$(
        jq -c --arg host "${PORTAL_HOSTNAME}" \
            '{spec: {listeners: [.spec.listeners[] | .hostname //= $host]}}' <<<"${GATEWAY_JSON}"
    )"
fi
ROUTE_TERMINATION="$(kubectl get route maas-gateway-route -n openshift-ingress \
    -o jsonpath='{.spec.tls.termination}')"
if [[ "${ROUTE_TERMINATION}" == "reencrypt" ]]; then
    echo "Switching maas-gateway-route to passthrough so the client SNI reaches the Gateway"
    kubectl patch route maas-gateway-route -n openshift-ingress --type=merge \
        -p '{"spec":{"tls":{"termination":"passthrough","insecureEdgeTerminationPolicy":"Redirect","destCACertificate":null}}}'
fi

# --- 0.5. Trust the ingress CA in the portal auth proxy ------------------------
# Keycloak is served behind the default ingress certificate, which the proxy
# cannot verify against the system roots (OIDC discovery fails with x509 and
# the proxy crash-loops). The controller trusts the platform's
# odh-trusted-ca-bundle, whose odh-ca-bundle.crt key the operator fills from
# the DSCI customCABundle, so add the ingress CA there (keeping existing CAs).
INGRESS_CA="$(oc get configmap default-ingress-cert -n openshift-config-managed \
    -o jsonpath='{.data.ca-bundle\.crt}')"
if [[ -z "${INGRESS_CA}" ]]; then
    echo "ERROR: openshift-config-managed/default-ingress-cert has no ca-bundle.crt" >&2
    exit 1
fi
DSCI_NAME="$(kubectl get dscinitializations.dscinitialization.opendatahub.io \
    -o jsonpath='{.items[0].metadata.name}')"
EXISTING_CA="$(kubectl get dscinitializations.dscinitialization.opendatahub.io "${DSCI_NAME}" \
    -o jsonpath='{.spec.trustedCABundle.customCABundle}')"
if [[ "${EXISTING_CA}" != *"${INGRESS_CA}"* ]]; then
    echo "Adding the ingress CA to DSCI ${DSCI_NAME} spec.trustedCABundle.customCABundle"
    kubectl patch dscinitializations.dscinitialization.opendatahub.io "${DSCI_NAME}" --type=merge -p "$(
        jq -n --arg ca "${EXISTING_CA:+${EXISTING_CA}$'\n'}${INGRESS_CA}" \
            '{spec: {trustedCABundle: {managementState: "Managed", customCABundle: $ca}}}'
    )"
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
kubectl -n "${INFRA_NAMESPACE}" apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${CREDENTIALS_SECRET_NAME}
  labels:
    app.kubernetes.io/part-of: maas-consumer-portal-e2e
    app.kubernetes.io/managed-by: ai-gateway-controller
stringData:
  OAUTH2_PROXY_CLIENT_ID: ${MAAS_PORTAL_CLIENT_ID}
  OAUTH2_PROXY_CLIENT_SECRET: ${MAAS_PORTAL_CLIENT_SECRET}
  OAUTH2_PROXY_COOKIE_SECRET: ${COOKIE_SECRET}
EOF
echo "Created auth proxy credentials Secret ${CREDENTIALS_SECRET_NAME} in ${INFRA_NAMESPACE}"
echo "Consumer portal e2e enablement complete; ai-gateway-controller deploys the portal on its next reconcile"
