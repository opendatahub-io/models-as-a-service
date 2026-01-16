#!/bin/bash

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

MAAS_API_NAMESPACE=${MAAS_API_NAMESPACE:-maas-api}
KEYCLOAK_REALM=${KEYCLOAK_REALM:-"maas"}
MAAS_API_IMAGE=${MAAS_API_IMAGE:-"quay.io/opendatahub/maas-api:latest"}

export KEYCLOAK_REALM

echo "   Deploying Keycloak tier mapping ConfigMap..."
for ns in "${MAAS_API_NAMESPACE}" maas-api; do
  if kubectl get namespace "${ns}" >/dev/null 2>&1; then
    kubectl apply --server-side=true --force-conflicts -n "${ns}" \
      -f "${PROJECT_ROOT}/deployment/idp/maas-api/resources/tier-mapping-configmap.yaml"
    kubectl apply --server-side=true --force-conflicts -n "${ns}" \
      -f "${PROJECT_ROOT}/deployment/idp/maas-api/resources/allow-gateway-networkpolicy.yaml"
  fi
done

echo "   Applying AuthPolicy for Keycloak OIDC..."
envsubst '$KEYCLOAK_REALM' < "${PROJECT_ROOT}/deployment/idp/maas-api/policies/auth-policy-oidc.yaml" | \
  kubectl apply --server-side=true --force-conflicts -n "${MAAS_API_NAMESPACE}" -f - 2>/dev/null && \
  echo "   ✅ AuthPolicy applied" || echo "   ⚠️  Failed to apply AuthPolicy (may need manual configuration)"

echo "   Excluding /maas-api paths from Gateway AuthPolicy..."
kubectl patch authpolicy gateway-auth-policy -n openshift-ingress --type=merge --patch='
spec:
  when:
  - predicate: "!request.path.startsWith(\"/maas-api\")"
' 2>/dev/null || echo "   ⚠️  Could not patch gateway-auth-policy (may not exist yet)"

echo "   Setting MaaS API image for Keycloak IDP..."
kubectl set image -n "${MAAS_API_NAMESPACE}" deployment/maas-api maas-api="${MAAS_API_IMAGE}" 2>/dev/null || \
  echo "   ⚠️  Could not update maas-api image (deployment may not be ready yet)"
