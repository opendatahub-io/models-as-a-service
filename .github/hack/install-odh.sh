#!/usr/bin/env bash
# Install OpenDataHub (ODH) operator and apply DataScienceCluster with ModelsAsService.
#
# Prerequisites: cert-manager and LWS operators (run install-cert-manager-and-lws.sh first).
#
# Environment variables:
#   OPERATOR_CATALOG - Custom catalog image (default: quay.io/opendatahub/opendatahub-operator-catalog:latest)
#   OPERATOR_CHANNEL - Subscription channel (default: fast-3 for released, fast for custom catalog)
#   OPERATOR_IMAGE   - Custom operator image to patch into CSV (optional)
#
# Usage: ./install-odh.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
DATA_DIR="${REPO_ROOT}/scripts/data"

NAMESPACE="${NAMESPACE:-opendatahub}"
OPERATOR_CATALOG="${OPERATOR_CATALOG:-quay.io/opendatahub/opendatahub-operator-catalog:latest}"
OPERATOR_CHANNEL="${OPERATOR_CHANNEL:-}"
OPERATOR_IMAGE="${OPERATOR_IMAGE:-}"

# Source deployment helpers
source "$REPO_ROOT/scripts/deployment-helpers.sh"

patch_operator_csv_if_needed() {
  [[ -z "$OPERATOR_IMAGE" ]] && return 0
  local operator_prefix="$1"
  local namespace="$2"

  log_info "Patching operator CSV with custom image: $OPERATOR_IMAGE"
  local csv_name=""
  local timeout=60
  local elapsed=0
  local interval=5

  while [[ $elapsed -lt $timeout ]]; do
    csv_name=$(kubectl get csv -n "$namespace" --no-headers 2>/dev/null | grep "^${operator_prefix}" | head -n1 | awk '{print $1}')
    [[ -n "$csv_name" ]] && break
    sleep $interval
    elapsed=$((elapsed + interval))
  done

  if [[ -z "$csv_name" ]]; then
    log_warn "Could not find CSV for $operator_prefix after ${timeout}s, skipping image patch"
    return 0
  fi

  kubectl annotate csv "$csv_name" -n "$namespace" opendatahub.io/managed=false --overwrite 2>/dev/null || true
  kubectl patch csv "$csv_name" -n "$namespace" --type='json' -p="[
    {\"op\": \"replace\", \"path\": \"/spec/install/spec/deployments/0/spec/template/spec/containers/0/image\", \"value\": \"$OPERATOR_IMAGE\"}
  ]"
  log_info "CSV $csv_name patched with image $OPERATOR_IMAGE"
}

echo "=== Installing OpenDataHub operator ==="
echo ""

# 1. Create custom CatalogSource (ODH requires v3+ for MaaS; community-operators may have older)
echo "1. Creating ODH CatalogSource..."
create_custom_catalogsource "odh-custom-catalog" "openshift-marketplace" "$OPERATOR_CATALOG"
catalog_source="odh-custom-catalog"
channel="${OPERATOR_CHANNEL:-fast}"

# 2. Install ODH operator via OLM
echo "2. Installing ODH operator..."
install_olm_operator \
  "opendatahub-operator" \
  "$NAMESPACE" \
  "$catalog_source" \
  "$channel" \
  "" \
  "AllNamespaces"

# 3. Patch CSV with custom image if specified
if [[ -n "$OPERATOR_IMAGE" ]]; then
  echo "3. Patching operator image..."
  patch_operator_csv_if_needed "opendatahub-operator" "$NAMESPACE"
else
  echo "3. Skipping operator image patch (OPERATOR_IMAGE not set)"
fi

# 4. Wait for CRDs
echo "4. Waiting for operator CRDs..."
wait_for_crd "datascienceclusters.datasciencecluster.opendatahub.io" 180 || {
  log_error "DataScienceCluster CRD not available - operator may not have installed correctly"
  exit 1
}

# 5. Wait for webhook
echo "5. Waiting for operator webhook..."
wait_for_resource "deployment" "opendatahub-operator-controller-manager" "opendatahub" 120 || {
  log_warn "Webhook deployment not found after 120s, proceeding anyway..."
}
if kubectl get deployment opendatahub-operator-controller-manager -n opendatahub &>/dev/null; then
  kubectl wait --for=condition=Available --timeout=120s \
    deployment/opendatahub-operator-controller-manager -n opendatahub 2>/dev/null || {
    log_warn "Webhook deployment not fully ready, proceeding anyway..."
  }
fi

# 6. Apply DSCInitialization
echo "6. Applying DSCInitialization..."
if kubectl get dscinitializations default-dsci &>/dev/null; then
  echo "   DSCInitialization already exists, skipping"
else
  kubectl apply -f - <<EOF
apiVersion: dscinitialization.opendatahub.io/v1
kind: DSCInitialization
metadata:
  name: default-dsci
spec:
  applicationsNamespace: ${NAMESPACE}
  monitoring:
    managementState: Managed
    namespace: ${NAMESPACE}-monitoring
    metrics: {}
  trustedCABundle:
    managementState: Managed
EOF
fi

# 7. Apply DataScienceCluster (modelsAsService Unmanaged - MaaS deployed separately)
echo "7. Applying DataScienceCluster..."
if kubectl get datasciencecluster -A --no-headers 2>/dev/null | grep -q .; then
  echo "   DataScienceCluster already exists, skipping"
else
  kubectl apply --server-side=true -f "${DATA_DIR}/datasciencecluster-unmanaged.yaml"
fi

# 8. Wait for DataScienceCluster ready (KServe)
echo "8. Waiting for DataScienceCluster (KServe)..."
wait_datasciencecluster_ready "default-dsc" 600 || {
  log_error "DataScienceCluster did not become ready"
  exit 1
}

echo ""
echo "=== ODH installation complete ==="
echo ""
echo "Verify:"
echo "  kubectl get datasciencecluster -A"
echo "  kubectl get pods -n opendatahub"
echo "  kubectl get pods -n kserve"
