#!/usr/bin/env bash
# Install OpenDataHub (ODH) operator and apply DataScienceCluster (KServe).
# MaaS is deployed separately via deploy.sh --deployment-mode kustomize.
#
# Prerequisites: cert-manager and LWS operators (run install-cert-manager-and-lws.sh first).
#
# Environment variables:
#   OPERATOR_CATALOG - Custom catalog image (optional). When unset, uses community-operators.
#                      Set to e.g. quay.io/opendatahub/opendatahub-operator-catalog:latest for custom builds.
#   OPERATOR_CHANNEL   - Subscription channel (default: fast-3)
#   OPERATOR_STARTING_CSV - Pin Subscription startingCSV (default: opendatahub-operator.v3.4.0-ea.1). Set to "-" to omit.
#   OPERATOR_INSTALL_PLAN_APPROVAL - Manual (default) or Automatic; use "-" to omit.
#     Manual: blocks auto-upgrades; this script auto-approves only the first InstallPlan so install does not stall.
#   OPERATOR_IMAGE   - Custom operator image to patch into CSV (optional)
#   OPERATOR_OPERANDS_MAP - Path to operands-map.yaml for RELATED_IMAGE env var injection (optional)
#                           Used with OPERATOR_IMAGE to ensure component images match the operator.
#
# Usage: ./install-odh.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
DATA_DIR="${REPO_ROOT}/scripts/data"

NAMESPACE="${OPERATOR_NAMESPACE:-opendatahub}"
OPERATOR_CATALOG="${OPERATOR_CATALOG:-}"
OPERATOR_CHANNEL="${OPERATOR_CHANNEL:-}"
OPERATOR_STARTING_CSV="${OPERATOR_STARTING_CSV:-}"
OPERATOR_INSTALL_PLAN_APPROVAL="${OPERATOR_INSTALL_PLAN_APPROVAL:-}"
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

  # When using a custom operator image, the community CSV may lack RELATED_IMAGE env vars
  # that the operator needs to deploy the correct component versions.
  # If OPERATOR_OPERANDS_MAP points to a local operands-map.yaml, inject its env vars into the CSV.
  if [[ -n "${OPERATOR_OPERANDS_MAP:-}" && -f "$OPERATOR_OPERANDS_MAP" ]]; then
    log_info "Injecting RELATED_IMAGE env vars from $OPERATOR_OPERANDS_MAP into CSV"
    local env_patches="["
    local first=true
    while IFS= read -r line; do
      local name value
      name=$(echo "$line" | sed -n 's/.*name: \(RELATED_IMAGE_[^ ]*\)/\1/p')
      if [[ -n "$name" ]]; then
        read -r value_line
        value=$(echo "$value_line" | sed -n 's/.*value: \(.*\)/\1/p')
        if [[ -n "$value" ]]; then
          $first || env_patches+=","
          first=false
          env_patches+="{\"name\":\"$name\",\"value\":\"$value\"}"
        fi
      fi
    done < "$OPERATOR_OPERANDS_MAP"

    if [[ "$env_patches" != "[" ]]; then
      env_patches+="]"
      local container_path="/spec/install/spec/deployments/0/spec/template/spec/containers/0"
      local existing_env
      existing_env=$(kubectl get csv "$csv_name" -n "$namespace" -o jsonpath="{${container_path}.env}" 2>/dev/null || echo "[]")

      local merged_env
      merged_env=$(python3 -c "
import json, sys
existing = json.loads('${existing_env}')
new_envs = json.loads(sys.stdin.read())
existing_names = {e['name'] for e in existing}
for e in new_envs:
    if e['name'] not in existing_names:
        existing.append(e)
print(json.dumps(existing))
" <<< "$env_patches")

      kubectl patch csv "$csv_name" -n "$namespace" --type='json' \
        -p="[{\"op\": \"replace\", \"path\": \"${container_path}/env\", \"value\": ${merged_env}}]"
      log_info "CSV env vars patched with RELATED_IMAGE entries"
    fi
  fi
}

echo "=== Installing OpenDataHub operator ==="
echo ""

# 1. Catalog setup: community-operators by default, or custom catalog when OPERATOR_CATALOG is set
echo "1. Setting up ODH catalog..."
if [[ -n "$OPERATOR_CATALOG" ]]; then
  echo "   Using custom catalog: $OPERATOR_CATALOG"
  create_custom_catalogsource "odh-custom-catalog" "openshift-marketplace" "$OPERATOR_CATALOG"
  catalog_source="odh-custom-catalog"
  channel="${OPERATOR_CHANNEL:-fast-3}"
else
  echo "   Using community-operators"
  catalog_source="community-operators"
  channel="${OPERATOR_CHANNEL:-fast-3}"
fi

# Pin to ODH 3.4 EA1 unless overridden (omit with OPERATOR_STARTING_CSV=- to follow channel head)
starting_csv="${OPERATOR_STARTING_CSV:-opendatahub-operator.v3.4.0-ea.1}"
[[ "$starting_csv" == "-" ]] && starting_csv=""

# Manual = no auto-upgrades; install_olm_operator still approves the first InstallPlan programmatically
plan_approval="${OPERATOR_INSTALL_PLAN_APPROVAL:-Manual}"
[[ "$plan_approval" == "-" ]] && plan_approval=""

# 2. Install ODH operator via OLM
echo "2. Installing ODH operator..."
if ! install_olm_operator \
  "opendatahub-operator" \
  "$NAMESPACE" \
  "$catalog_source" \
  "$channel" \
  "$starting_csv" \
  "AllNamespaces" \
  "openshift-marketplace" \
  "$plan_approval"; then
  log_error "ODH operator installation failed"
  exit 1
fi

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
wait_for_resource "deployment" "opendatahub-operator-controller-manager" "$NAMESPACE" 120 || {
  log_warn "Webhook deployment not found after 120s, proceeding anyway..."
}
if kubectl get deployment opendatahub-operator-controller-manager -n "$NAMESPACE" &>/dev/null; then
  kubectl wait --for=condition=Available --timeout=120s \
    deployment/opendatahub-operator-controller-manager -n "$NAMESPACE" 2>/dev/null || {
    log_warn "Webhook deployment not fully ready, proceeding anyway..."
  }
fi

# 6. Apply DSCInitialization (with retries)
echo "6. Applying DSCInitialization..."
if kubectl get dscinitializations default-dsci &>/dev/null; then
  echo "   DSCInitialization already exists, skipping"
else
  dsci_applied=false
  for attempt in $(seq 1 5); do
    if kubectl apply -f - <<EOF
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
    then
      dsci_applied=true
      break
    fi
    echo "   Attempt $attempt/5 failed (webhook may not be ready), retrying in 15s..."
    sleep 15
  done
  if [[ "$dsci_applied" != "true" ]]; then
    log_error "Failed to apply DSCInitialization after 5 attempts"
    exit 1
  fi
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

# 9. Wait for odh-model-controller webhook to be ready
# The odh-model-controller registers a ConfigMap validating webhook. If we proceed before
# its pods are ready, any ConfigMap create/update fails with "no endpoints available".
echo "9. Waiting for odh-model-controller webhook..."
wait_for_validating_webhooks "$NAMESPACE" 180 || {
  log_warn "Validating webhooks in $NAMESPACE not ready after 180s, proceeding anyway..."
}

echo ""
echo "=== ODH installation complete ==="
echo ""
echo "Verify:"
echo "  kubectl get datasciencecluster -A"
echo "  kubectl get pods -n opendatahub"
echo "  kubectl get pods -n kserve"
