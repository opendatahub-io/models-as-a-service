#!/bin/bash
#
# Clean up Keycloak identity provider deployment.
#
# This script removes Keycloak instance, namespace, and optionally CRDs.
# It performs cleanup in the correct order to allow the operator to
# clean up resources properly before removing the namespace.
#
# What this script does:
#   1. Deletes Keycloak instance (allows operator to clean up)
#   2. Deletes keycloak-system namespace (removes operator and resources)
#   3. Optionally deletes Keycloak CRDs (cluster-scoped, persist after namespace deletion)
#
# Usage:
#   ./scripts/cleanup-keycloak.sh [--delete-crds]
#
# Options:
#   --delete-crds    Also delete Keycloak CRDs (optional)
#   --force          Skip confirmation prompts
#   --help           Show this help message
#

set -euo pipefail

# Configuration
NAMESPACE="keycloak-system"
KEYCLOAK_NAME="maas-keycloak"
DELETE_CRDS=false
FORCE=false

# Parse arguments
while [[ $# -gt 0 ]]; do
  case $1 in
    --delete-crds)
      DELETE_CRDS=true
      shift
      ;;
    --force)
      FORCE=true
      shift
      ;;
    --help)
      grep '^#' "$0" | grep -v '#!/bin/bash' | sed 's/^# *//'
      exit 0
      ;;
    *)
      echo "Unknown option: $1"
      echo "Use --help for usage information"
      exit 1
      ;;
  esac
done

echo "🧹 Keycloak Cleanup"
echo ""

# Check if namespace exists
if ! kubectl get namespace "$NAMESPACE" &>/dev/null; then
  echo "✓ Namespace '$NAMESPACE' does not exist - nothing to clean up"

  # Check if CRDs exist
  if kubectl get crd keycloaks.k8s.keycloak.org &>/dev/null; then
    echo ""
    echo "⚠️  Keycloak CRDs still exist (cluster-scoped resources)"

    if [[ "$DELETE_CRDS" == true ]]; then
      echo "Deleting Keycloak CRDs..."
      kubectl delete crd keycloaks.k8s.keycloak.org --ignore-not-found=true
      kubectl delete crd keycloakrealmimports.k8s.keycloak.org --ignore-not-found=true
      echo "✓ CRDs deleted"
    else
      echo ""
      echo "To remove CRDs, run:"
      echo "  $0 --delete-crds"
      echo ""
      echo "Note: Only delete CRDs if no other Keycloak instances exist in other namespaces"
    fi
  fi

  exit 0
fi

# Confirm deletion unless --force
if [[ "$FORCE" != true ]]; then
  echo "This will delete:"
  echo "  - Keycloak instance: $KEYCLOAK_NAME"
  echo "  - Namespace: $NAMESPACE"
  echo "  - All resources in the namespace (operator, secrets, routes)"

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

#──────────────────────────────────────────────────────────────────────────────
# Delete Keycloak Instance
#──────────────────────────────────────────────────────────────────────────────

echo "🗑️  Deleting Keycloak instance..."

if kubectl get keycloak "$KEYCLOAK_NAME" -n "$NAMESPACE" &>/dev/null; then
  kubectl delete keycloak "$KEYCLOAK_NAME" -n "$NAMESPACE"
  echo "  Waiting for operator to clean up resources..."
  sleep 5
  echo "✓ Keycloak instance deleted"
else
  echo "  Keycloak instance '$KEYCLOAK_NAME' not found - skipping"
fi

#──────────────────────────────────────────────────────────────────────────────
# Delete Namespace
#──────────────────────────────────────────────────────────────────────────────

echo ""
echo "🗑️  Deleting namespace..."

kubectl delete namespace "$NAMESPACE" --wait=false

echo "  Namespace '$NAMESPACE' is being deleted (background)"
echo "  This may take a few moments..."

# Wait for namespace to be gone (with timeout)
TIMEOUT=60
ELAPSED=0
while kubectl get namespace "$NAMESPACE" &>/dev/null; do
  if [ $ELAPSED -ge $TIMEOUT ]; then
    echo ""
    echo "⚠️  Namespace deletion is taking longer than expected"
    echo "  Check status: kubectl get namespace $NAMESPACE"
    echo "  Check remaining resources: kubectl get all -n $NAMESPACE"
    break
  fi
  sleep 2
  ELAPSED=$((ELAPSED + 2))
  echo -n "."
done

if ! kubectl get namespace "$NAMESPACE" &>/dev/null; then
  echo ""
  echo "✓ Namespace deleted"
fi

#──────────────────────────────────────────────────────────────────────────────
# Delete CRDs (Optional)
#──────────────────────────────────────────────────────────────────────────────

if kubectl get crd keycloaks.k8s.keycloak.org &>/dev/null; then
  echo ""

  if [[ "$DELETE_CRDS" == true ]]; then
    echo "🗑️  Deleting Keycloak CRDs..."

    kubectl delete crd keycloaks.k8s.keycloak.org --ignore-not-found=true
    kubectl delete crd keycloakrealmimports.k8s.keycloak.org --ignore-not-found=true

    echo "✓ CRDs deleted"
  else
    echo "⚠️  Keycloak CRDs still exist (cluster-scoped resources)"
    echo ""
    echo "CRDs were not deleted because they are cluster-scoped and may be"
    echo "used by Keycloak instances in other namespaces."
    echo ""
    echo "To also delete CRDs, run:"
    echo "  $0 --delete-crds"
    echo ""
    echo "⚠️  Only delete CRDs if you are sure no other Keycloak instances"
    echo "   exist anywhere in the cluster"
  fi
fi

#──────────────────────────────────────────────────────────────────────────────
# Summary
#──────────────────────────────────────────────────────────────────────────────

echo ""
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo "✅ Cleanup Complete"
echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
echo ""

CLEANED_UP="  - Keycloak instance ($KEYCLOAK_NAME)"
CLEANED_UP="$CLEANED_UP
  - Namespace ($NAMESPACE)"

if [[ "$DELETE_CRDS" == true ]]; then
  CLEANED_UP="$CLEANED_UP
  - Keycloak CRDs"
fi

echo "Cleaned up:"
echo "$CLEANED_UP"
echo ""

if kubectl get crd keycloaks.k8s.keycloak.org &>/dev/null; then
  echo "⚠️  Keycloak CRDs still present (not deleted)"
  echo ""
fi

echo "To redeploy Keycloak, run:"
echo "  ./scripts/setup-keycloak.sh"
echo ""
