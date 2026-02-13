#!/usr/bin/env bash
# Install the MaaS control plane (CRDs + RBAC + controller) into the cluster.
# Default namespace: opendatahub (must exist; created by Open Data Hub operator).
#
# Prerequisites: kubectl/oc, cluster with Gateway API and Kuadrant installed.
# Before using MaaSAuthPolicy you must disable the shared gateway-auth-policy
# in openshift-ingress (see README).
#
# Usage: ./scripts/install-maas-controller.sh [namespace]

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
CONTROLLER_NAMESPACE="${1:-opendatahub}"

echo "=== Installing MaaS controller (namespace: $CONTROLLER_NAMESPACE) ==="

# Validate target namespace exists (do not create it)
if ! kubectl get namespace "$CONTROLLER_NAMESPACE" &>/dev/null; then
  echo "Error: namespace $CONTROLLER_NAMESPACE does not exist. Create it first (e.g. via Open Data Hub operator) or choose another namespace."
  exit 1
fi

# If installing to a different namespace, we need to patch the config
if [[ "$CONTROLLER_NAMESPACE" != "opendatahub" ]]; then
  echo "Patching manifests to use namespace $CONTROLLER_NAMESPACE"
  (cd "$REPO_ROOT" && kubectl kustomize config/default | \
    sed "s/namespace: opendatahub/namespace: $CONTROLLER_NAMESPACE/g") | kubectl apply -f -
else
  echo "Applying config/default (opendatahub)"
  kubectl apply -k "$REPO_ROOT/config/default"
fi

echo "Done. Check controller: kubectl get pods -n $CONTROLLER_NAMESPACE -l app=maas-controller"
