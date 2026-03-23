#!/bin/bash
#
# Clean up Keycloak identity provider deployment.
#
# Removes Keycloak instance, namespace, and optionally CRDs.
#
# Usage:
#   ./scripts/cleanup-keycloak.sh [--delete-crds] [--force]
#

set -euo pipefail

NAMESPACE="keycloak-system"
KEYCLOAK_NAME="maas-keycloak"
DELETE_CRDS=false
FORCE=false

while [[ $# -gt 0 ]]; do
  case $1 in
    --delete-crds) DELETE_CRDS=true; shift ;;
    --force) FORCE=true; shift ;;
    --help)
      grep '^#' "$0" | grep -v '#!/bin/bash' | sed 's/^# *//'
      exit 0
      ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
done

echo "Keycloak Cleanup"
echo ""

if ! kubectl get namespace "$NAMESPACE" &>/dev/null; then
  echo "Namespace '$NAMESPACE' does not exist - nothing to clean up"

  if kubectl get crd keycloaks.k8s.keycloak.org &>/dev/null && [[ "$DELETE_CRDS" == true ]]; then
    echo "Deleting Keycloak CRDs..."
    kubectl delete crd keycloaks.k8s.keycloak.org --ignore-not-found=true
    kubectl delete crd keycloakrealmimports.k8s.keycloak.org --ignore-not-found=true
    echo "CRDs deleted"
  fi

  exit 0
fi

if [[ "$FORCE" != true ]]; then
  echo "This will delete:"
  echo "  - Keycloak instance: $KEYCLOAK_NAME"
  echo "  - Namespace: $NAMESPACE"
  if [[ "$DELETE_CRDS" == true ]]; then
    echo "  - Keycloak CRDs (cluster-scoped)"
  fi
  echo ""
  read -p "Continue? (y/N) " -n 1 -r
  echo
  if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Cleanup cancelled"
    exit 0
  fi
  echo ""
fi

# Delete Keycloak instance first (let operator clean up)
echo "Deleting Keycloak instance..."
if kubectl get keycloak "$KEYCLOAK_NAME" -n "$NAMESPACE" &>/dev/null; then
  kubectl delete keycloak "$KEYCLOAK_NAME" -n "$NAMESPACE" --timeout=60s 2>/dev/null || true
  sleep 5
  echo "  Keycloak instance deleted"
else
  echo "  Keycloak instance not found - skipping"
fi

# Delete namespace
echo "Deleting namespace..."
kubectl delete namespace "$NAMESPACE" --wait=false 2>/dev/null || true

TIMEOUT=60
ELAPSED=0
while kubectl get namespace "$NAMESPACE" &>/dev/null; do
  if [ $ELAPSED -ge $TIMEOUT ]; then
    echo "  Namespace deletion taking longer than expected"
    echo "  Check status: kubectl get namespace $NAMESPACE"
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo -n "."
done

if ! kubectl get namespace "$NAMESPACE" &>/dev/null; then
  echo ""
  echo "  Namespace deleted"
fi

# Optionally delete CRDs
if kubectl get crd keycloaks.k8s.keycloak.org &>/dev/null; then
  if [[ "$DELETE_CRDS" == true ]]; then
    echo "Deleting Keycloak CRDs..."
    kubectl delete crd keycloaks.k8s.keycloak.org --ignore-not-found=true
    kubectl delete crd keycloakrealmimports.k8s.keycloak.org --ignore-not-found=true
    echo "  CRDs deleted"
  else
    echo ""
    echo "Keycloak CRDs still exist. To remove: $0 --delete-crds"
  fi
fi

echo ""
echo "Cleanup complete. To redeploy: ./scripts/setup-keycloak.sh"
