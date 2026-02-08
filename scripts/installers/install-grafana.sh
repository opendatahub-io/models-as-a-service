#!/bin/bash
set -euo pipefail

# Grafana Operator Installation Script for MaaS Deployment
# Handles both OpenShift operator installation and vanilla Kubernetes deployment
# Required for dashboard visualization and monitoring

OCP=true

usage() {
  cat <<EOF
Usage: $0 [--kubernetes]

Options:
  --kubernetes    Use vanilla Kubernetes Grafana instead of OpenShift Grafana operator

Examples:
  $0                # Install OpenShift Grafana operator (default)
  $0 --kubernetes   # Install vanilla Grafana (not implemented yet)
EOF
  exit 1
}

# Parse flags
while [[ $# -gt 0 ]]; do
  case "$1" in
    --kubernetes)  OCP=false ; shift ;;
    -h|--help) usage ;;
    *) echo "❌ Unknown option: $1"; usage ;;
  esac
done

echo "📊 Setting up Grafana for MaaS observability"

if [[ "$OCP" == true ]]; then
  echo "Using OpenShift Grafana operator"
  
  echo "🔧 Installing Grafana operator subscription..."
  kubectl apply -f - <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: grafana-operator
  namespace: openshift-operators
spec:
  channel: v5
  installPlanApproval: Automatic
  name: grafana-operator
  source: community-operators
  sourceNamespace: openshift-marketplace
EOF

  echo "⏳ Waiting for Grafana operator to be ready..."
  
  # Wait for subscription to report its currentCSV (exact match, not grep)
  echo "   Waiting for subscription to report CSV..."
  CSV_NAME=""
  for i in $(seq 1 30); do
    # Check if install plan needs approval
    INSTALL_PLAN=$(kubectl get subscription grafana-operator -n openshift-operators -o jsonpath='{.status.installPlanRef.name}' 2>/dev/null || true)
    if [ -n "$INSTALL_PLAN" ]; then
      APPROVED=$(kubectl get installplan "$INSTALL_PLAN" -n openshift-operators -o jsonpath='{.spec.approved}' 2>/dev/null || true)
      if [ "$APPROVED" = "false" ]; then
        echo "   ⚠️  Install plan $INSTALL_PLAN requires approval, auto-approving..."
        kubectl patch installplan "$INSTALL_PLAN" -n openshift-operators --type merge -p '{"spec":{"approved":true}}' || true
      fi
    fi
    
    # Get CSV name from subscription (not grep - exact match)
    CSV_NAME=$(kubectl get subscription grafana-operator -n openshift-operators \
        -o jsonpath='{.status.currentCSV}' 2>/dev/null || true)
    if [ -n "$CSV_NAME" ]; then
      break
    fi
    echo "   Waiting for subscription to report CSV... (attempt $i/30)"
    sleep 2
  done

  if [ -z "$CSV_NAME" ]; then
    echo "❌ Subscription never reported a CSV after 30 attempts"
    exit 1
  fi

  # Wait for that specific CSV to succeed
  echo "   Waiting for CSV $CSV_NAME to succeed..."
  for i in $(seq 1 60); do
    PHASE=$(kubectl get csv "$CSV_NAME" -n openshift-operators -o jsonpath='{.status.phase}' 2>/dev/null || true)
    if [ "$PHASE" = "Succeeded" ]; then
      echo "   ✅ CSV $CSV_NAME succeeded"
      break
    fi
    echo "   CSV $CSV_NAME phase: $PHASE (attempt $i/60)"
    sleep 5
  done

  # Verify CSV actually succeeded
  PHASE=$(kubectl get csv "$CSV_NAME" -n openshift-operators -o jsonpath='{.status.phase}' 2>/dev/null || true)
  if [ "$PHASE" != "Succeeded" ]; then
    echo "❌ Grafana CSV $CSV_NAME phase is '$PHASE', expected 'Succeeded'"
    exit 1
  fi

  # Wait for any grafana operator deployment to be available
  echo "⏳ Waiting for Grafana operator deployment..."
  DEPLOY_NAME=$(kubectl get deployment -n openshift-operators --no-headers 2>/dev/null | grep -i grafana | awk '{print $1}' | head -1 || true)
  if [ -n "$DEPLOY_NAME" ]; then
    kubectl wait --for=condition=Available deployment/"$DEPLOY_NAME" -n openshift-operators --timeout=120s || \
      echo "   ⚠️  Deployment still starting, continuing..."
  else
    echo "   ⚠️  No Grafana deployment found yet, continuing..."
  fi

  echo "✅ Grafana operator is installed and running"

else
  echo "❌ Vanilla Kubernetes Grafana installation not implemented yet, skipping"
fi

echo "📊 Grafana operator installation completed!"
echo "Note: To deploy a Grafana instance, apply the Grafana custom resources from infrastructure/kustomize-templates/grafana/"
