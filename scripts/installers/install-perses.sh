#!/bin/bash
set -euo pipefail

# Perses Installation Script for MaaS Deployment
# Installs Perses via the Red Hat Cluster Observability Operator (COO)
# Similar to install-grafana.sh - uses OLM subscription

OCP=true

usage() {
  cat <<EOF
Usage: $0 [--kubernetes]

Options:
  --kubernetes    Use vanilla Kubernetes installation (Helm) instead of OpenShift operator

Examples:
  $0                # Install via Red Hat Cluster Observability Operator (default)
  $0 --kubernetes   # Install via Helm on vanilla Kubernetes
EOF
  exit 0
}

# Parse flags
while [[ $# -gt 0 ]]; do
  case "$1" in
    --kubernetes)  OCP=false ; shift ;;
    -h|--help) usage ;;
    *) echo "❌ Unknown option: $1"; echo "Use --help for usage information"; exit 1 ;;
  esac
done

echo "📊 Setting up Perses for MaaS observability"

if [[ "$OCP" == true ]]; then
  echo "Using Red Hat Cluster Observability Operator"
  
  # Check if operator is already installed
  if kubectl get csv -n openshift-operators 2>/dev/null | grep -q "cluster-observability-operator.*Succeeded"; then
    echo "✅ Cluster Observability Operator already installed"
    kubectl get csv -n openshift-operators | grep cluster-observability-operator
    # Still need to wait for Perses CRDs to be established before exiting,
    # since callers (e.g., install-perses-dashboards.sh) depend on them.
    echo "   Waiting for Perses CRDs..."
    CRD_FAILURES=0
    for crd in "perses.perses.dev" "persesdashboards.perses.dev" "persesdatasources.perses.dev"; do
      if ! kubectl wait --for=condition=Established "crd/$crd" --timeout=60s 2>/dev/null; then
        echo "   ⚠️  CRD $crd not yet established"
        CRD_FAILURES=$((CRD_FAILURES + 1))
      fi
    done
    if [ "$CRD_FAILURES" -gt 0 ]; then
      echo "❌ $CRD_FAILURES Perses CRD(s) failed to become established"
      exit 1
    fi
    exit 0
  fi
  
  echo "🔧 Installing Cluster Observability Operator subscription..."
  kubectl apply -f - <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: cluster-observability-operator
  namespace: openshift-operators
spec:
  channel: stable
  installPlanApproval: Automatic
  name: cluster-observability-operator
  source: redhat-operators
  sourceNamespace: openshift-marketplace
EOF

  echo "⏳ Waiting for Cluster Observability Operator to be ready..."
  
  # Wait for CSV to succeed
  echo "   Waiting for operator CSV to succeed..."
  for i in $(seq 1 60); do
    # Check if install plan needs approval
    INSTALL_PLAN=$(kubectl get subscription cluster-observability-operator -n openshift-operators -o jsonpath='{.status.installPlanRef.name}' 2>/dev/null || true)
    if [ -n "$INSTALL_PLAN" ]; then
      APPROVED=$(kubectl get installplan "$INSTALL_PLAN" -n openshift-operators -o jsonpath='{.spec.approved}' 2>/dev/null || true)
      if [ "$APPROVED" = "false" ]; then
        echo "   ⚠️  Install plan $INSTALL_PLAN requires approval, auto-approving..."
        kubectl patch installplan "$INSTALL_PLAN" -n openshift-operators --type merge -p '{"spec":{"approved":true}}' || true
      fi
    fi
    
    CSV_NAME=$(kubectl get csv -n openshift-operators --no-headers 2>/dev/null | grep -i "cluster-observability-operator" | awk '{print $1}' | head -1 || true)
    if [ -n "$CSV_NAME" ]; then
      PHASE=$(kubectl get csv -n openshift-operators "$CSV_NAME" -o jsonpath='{.status.phase}' 2>/dev/null || true)
      if [ "$PHASE" = "Succeeded" ]; then
        echo "   ✅ CSV $CSV_NAME succeeded"
        break
      fi
      echo "   CSV $CSV_NAME phase: $PHASE (attempt $i/60)"
    else
      echo "   Waiting for Cluster Observability Operator CSV to appear... (attempt $i/60)"
    fi
    sleep 5
  done

  # Verify CSV succeeded
  CSV_NAME=$(kubectl get csv -n openshift-operators --no-headers 2>/dev/null | grep -i "cluster-observability-operator" | awk '{print $1}' | head -1 || true)
  if [ -z "$CSV_NAME" ]; then
    echo "❌ Cluster Observability Operator CSV not found after waiting"
    exit 1
  fi
  PHASE=$(kubectl get csv -n openshift-operators "$CSV_NAME" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  if [ "$PHASE" != "Succeeded" ]; then
    echo "❌ Cluster Observability Operator CSV phase is '$PHASE', expected 'Succeeded'"
    exit 1
  fi

  # Wait for CRDs to be established
  echo "   Waiting for Perses CRDs..."
  CRD_FAILURES=0
  for crd in "perses.perses.dev" "persesdashboards.perses.dev" "persesdatasources.perses.dev"; do
    if ! kubectl wait --for=condition=Established "crd/$crd" --timeout=60s 2>/dev/null; then
      echo "   ⚠️  CRD $crd not yet established"
      CRD_FAILURES=$((CRD_FAILURES + 1))
    fi
  done

  if [ "$CRD_FAILURES" -gt 0 ]; then
    echo "❌ Cluster Observability Operator installed but $CRD_FAILURES Perses CRD(s) failed to become established"
    exit 1
  fi
  echo "✅ Cluster Observability Operator is installed and running"

else
  echo "Installing Perses via Helm for vanilla Kubernetes..."
  
  # Check if Perses is already installed
  if kubectl get deployment perses -n perses 2>/dev/null | grep -q perses; then
    echo "✅ Perses already installed"
    echo "📊 Perses installation completed!"
    exit 0
  fi
  
  # Add Perses Helm repo
  helm repo add perses https://perses.github.io/helm-charts 2>/dev/null || true
  helm repo update
  
  # Install Perses
  helm upgrade --install perses perses/perses \
    --namespace perses \
    --create-namespace \
    --wait \
    --timeout 5m
  
  echo "✅ Perses installed via Helm"
fi
