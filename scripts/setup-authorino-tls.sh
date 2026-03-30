#!/bin/bash
#
# Configure Authorino for TLS communication with maas-api.
#
# When maas-api serves HTTPS (TLS backend), Authorino must:
# 1. Enable TLS on its listener so it accepts HTTPS auth requests
# 2. Trust the OpenShift service CA when making outbound requests to maas-api
#    (e.g., API key validation at https://maas-api...:8443/internal/v1/api-keys/validate)
#
# This script patches operator-managed Authorino resources that cannot be
# modified via Kustomize. It is run automatically by deploy.sh when
# --enable-tls-backend is set (default).
#
# Prerequisites:
# - Authorino operator installed (Kuadrant or RHCL)
# - OpenShift cluster (uses service-ca for certificate provisioning)
#
# Environment variables:
#   AUTHORINO_NAMESPACE  Authorino namespace (default: kuadrant-system)
#                       Use rh-connectivity-link for RHCL
#
# Usage:
#   ./scripts/setup-authorino-tls.sh
#   AUTHORINO_NAMESPACE=rh-connectivity-link ./scripts/setup-authorino-tls.sh
#

set -euo pipefail

NAMESPACE="${AUTHORINO_NAMESPACE:-kuadrant-system}"

echo "🔐 Configuring Authorino TLS in namespace: $NAMESPACE"

echo "📝 Adding serving-cert annotation to Authorino service..."
kubectl annotate service authorino-authorino-authorization \
  -n "$NAMESPACE" \
  service.beta.openshift.io/serving-cert-secret-name=authorino-server-cert \
  --overwrite

# Authorino listener TLS: In operator mode, the ODH Model Controller creates an EnvoyFilter
# with a TLS transport_socket for the gateway-to-Authorino gRPC connection (triggered by the
# gateway annotation security.opendatahub.io/authorino-tls-bootstrap: "true"). In kustomize
# mode, this controller doesn't run, so we must NOT enable listener TLS — the Kuadrant
# operator's EnvoyFilter connects to Authorino with plain gRPC and cannot be patched
# (operator reconciles and reverts changes, and MERGE EnvoyFilters don't apply to
# ADD-created clusters).
#
# We still configure CA bundles so Authorino can make outbound TLS calls to maas-api
# for API key validation.
if [[ "${SKIP_LISTENER_TLS:-false}" != "true" ]]; then
  echo "🔧 Patching Authorino CR for TLS listener..."
  kubectl patch authorino authorino -n "$NAMESPACE" --type=merge --patch '
  {
    "spec": {
      "listener": {
        "tls": {
          "enabled": true,
          "certSecretRef": {
            "name": "authorino-server-cert"
          }
        }
      }
    }
  }'
else
  echo "⏭️  Skipping Authorino listener TLS (kustomize mode — gRPC stays plain)"
  # Ensure listener TLS is disabled (may have been enabled by a previous operator-mode deploy)
  kubectl patch authorino authorino -n "$NAMESPACE" --type=merge --patch '
  {
    "spec": {
      "listener": {
        "tls": {
          "enabled": false
        }
      }
    }
  }'
fi

# Note: The Authorino CR doesn't support envVars, so we patch the deployment directly
echo "🌍 Adding environment variables to Authorino deployment..."
kubectl -n "$NAMESPACE" set env deployment/authorino \
  SSL_CERT_FILE=/etc/ssl/certs/openshift-service-ca/service-ca-bundle.crt \
  REQUESTS_CA_BUNDLE=/etc/ssl/certs/openshift-service-ca/service-ca-bundle.crt

echo "✅ Authorino TLS configuration complete"
echo ""
echo "  Restart maas-api and authorino deployments to pick up TLS configuration:"
echo "    kubectl rollout restart deployment/maas-api -n <maas-namespace>"
echo "    kubectl rollout restart deployment/authorino -n $NAMESPACE"
