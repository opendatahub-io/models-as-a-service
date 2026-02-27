#!/usr/bin/env bash
# Install the MaaS control plane (CRDs + RBAC + controller) into the cluster.
# Default deploy namespace: opendatahub (must exist; created by Open Data Hub operator).
# Default watch namespace: models-as-a-service (will create it if it doesn't exist).
#
# Prerequisites: kubectl/oc, cluster with Gateway API and Kuadrant installed.
# Before using MaaSAuthPolicy you must disable the shared gateway-auth-policy
# in openshift-ingress (see README).
#
# Usage: ./scripts/install-maas-controller.sh [OPTIONS]
#   --deploy-namespace <ns>  Namespace where the controller pod runs (default: opendatahub)
#   --watch-namespace <ns>   Namespace to watch for MaaS CRs (default: models-as-a-service)
#   -h, --help               Show this help

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
CONTROLLER_NAMESPACE="opendatahub"
WATCH_NAMESPACE="models-as-a-service"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --deploy-namespace)
      CONTROLLER_NAMESPACE="${2:?Usage: $0 --deploy-namespace <ns> [--watch-namespace <ns>]}"
      shift 2
      ;;
    --watch-namespace)
      WATCH_NAMESPACE="${2:?Usage: $0 [--deploy-namespace <ns>] --watch-namespace <ns>}"
      shift 2
      ;;
    -h|--help)
      grep -E "^# Usage:|^#   " "$0" | sed 's/^#   //' | sed 's/^# //'
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      exit 1
      ;;
  esac
done

if ! command -v kubectl &>/dev/null; then
  echo "Error: kubectl is required but not found in PATH." >&2
  exit 1
fi

echo "=== Installing MaaS controller in $CONTROLLER_NAMESPACE, watching $WATCH_NAMESPACE ==="

# Validate deploy namespace exists (do not create it)
if ! kubectl get namespace "$CONTROLLER_NAMESPACE" &>/dev/null; then
  echo "Error: namespace $CONTROLLER_NAMESPACE does not exist. Create it first (e.g. via Open Data Hub operator) or choose another namespace."
  exit 1
fi
# Ensure watch namespace exists (create if missing)
if ! kubectl get namespace "$WATCH_NAMESPACE" &>/dev/null; then
  echo "Creating namespace $WATCH_NAMESPACE"
  kubectl create namespace "$WATCH_NAMESPACE"
fi

# If namespaces are non-default, we need to patch the config
manifest=$(cd "$REPO_ROOT" && kubectl kustomize config/default)
if [[ "$CONTROLLER_NAMESPACE" != "opendatahub" ]]; then
  echo "Patching manifests to use deploy namespace $CONTROLLER_NAMESPACE"
  manifest=$(echo "$manifest" | sed "s/namespace: opendatahub/namespace: $CONTROLLER_NAMESPACE/g")
fi
if [[ "$WATCH_NAMESPACE" != "models-as-a-service" ]]; then
  echo "Patching manifests to use watch namespace $WATCH_NAMESPACE"
  manifest=$(echo "$manifest" | sed "s/value: models-as-a-service/value: $WATCH_NAMESPACE/g")
fi
echo "$manifest" | kubectl apply -f -

echo "Done. Controller watches MaaS CRs in $WATCH_NAMESPACE. Check: kubectl get pods -n $CONTROLLER_NAMESPACE -l app=maas-controller"
