#!/bin/bash

# MaaS Perses Dashboard Installation (helper)
# Installs the Cluster Observability Operator (if needed), enables the Perses UIPlugin,
# and deploys MaaS dashboard definitions (PersesDashboard CRs).
#
# This script is idempotent - safe to run multiple times
#
# Usage: ./install-perses-dashboards.sh

set -e
set -o pipefail

for cmd in kubectl; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "❌ Required command '$cmd' not found. Please install it first."
        exit 1
    fi
done

show_help() {
    echo "Usage: $0"
    echo ""
    echo "Installs the Cluster Observability Operator (if needed), enables the Perses UIPlugin,"
    echo "and deploys MaaS PersesDashboard definitions into openshift-operators."
    echo ""
    echo "Perses dashboards are accessible via OpenShift Console → Observe → Dashboards → Perses tab."
    echo ""
    echo "Examples:"
    echo "  $0    # Install Perses + deploy dashboards"
    echo ""
    exit 0
}

while [[ $# -gt 0 ]]; do
    case $1 in
        --help|-h)
            show_help
            ;;
        *)
            echo "Unknown option: $1"
            echo "Use --help for usage information"
            exit 1
            ;;
    esac
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
OBSERVABILITY_DIR="$PROJECT_ROOT/deployment/components/observability"

# Import shared helper functions (wait_for_crd, etc.)
source "$SCRIPT_DIR/deployment-helpers.sh"

# ==========================================
# Step 1: Install Cluster Observability Operator
# ==========================================
echo "📊 MaaS Perses Dashboard Installation"
echo ""

if kubectl get csv -n openshift-operators 2>/dev/null | grep -q "cluster-observability-operator.*Succeeded"; then
    echo "✅ Cluster Observability Operator already installed"
else
    echo "🔧 Installing Cluster Observability Operator..."
    "$SCRIPT_DIR/installers/install-perses.sh"
fi

# ==========================================
# Step 2: Wait for Perses CRDs
# ==========================================
echo ""
echo "⏳ Waiting for Perses CRDs..."
for crd in perses.perses.dev persesdashboards.perses.dev persesdatasources.perses.dev; do
    wait_for_crd "$crd" 120 || {
        echo "❌ CRD $crd not available. Please check Cluster Observability Operator installation."
        exit 1
    }
done
echo "   ✅ All Perses CRDs established"

# ==========================================
# Step 3: Enable UIPlugin (shows Perses in OpenShift Console)
# ==========================================
echo ""
echo "🔌 Enabling Perses UIPlugin..."
kubectl apply -f "$OBSERVABILITY_DIR/perses/uiplugin.yaml"
echo "   ✅ UIPlugin enabled"

# ==========================================
# Step 4: Wait for Perses pod (created by UIPlugin)
# ==========================================
echo ""
echo "⏳ Waiting for Perses instance..."
for i in $(seq 1 30); do
    PERSES_PODS=$(kubectl get pods -n openshift-operators -l app.kubernetes.io/name=perses --no-headers 2>/dev/null | grep -c Running || echo "0")
    if [ "$PERSES_PODS" -ge 1 ] 2>/dev/null; then
        echo "   ✅ Perses pod is running in openshift-operators"
        break
    fi
    if [ "$i" -eq 30 ]; then
        echo "   ❌ Perses pod failed to start after 150s"
        exit 1
    fi
    echo "   Waiting for Perses pod... (attempt $i/30)"
    sleep 5
done

# ==========================================
# Step 5: Deploy dashboards and datasource
# ==========================================
echo ""
echo "📊 Deploying Perses dashboards..."
kubectl apply -f "$OBSERVABILITY_DIR/perses/dashboards/dashboard-ai-engineer.yaml" -n openshift-operators
kubectl apply -f "$OBSERVABILITY_DIR/perses/dashboards/dashboard-platform-admin.yaml" -n openshift-operators
echo "   ✅ Dashboards deployed (Platform Admin, AI Engineer)"

echo ""
echo "🔗 Configuring Prometheus datasource..."
kubectl apply -f "$OBSERVABILITY_DIR/perses/perses-datasource.yaml" -n openshift-operators 2>/dev/null || \
    echo "   ⚠️  Datasource may already be configured"
echo "   ✅ Datasource configured"

# ==========================================
# Summary
# ==========================================
echo ""
echo "========================================="
echo "✅ Perses dashboards installed"
echo "========================================="
echo ""
echo "🌐 Access Perses dashboards via OpenShift Console:"
echo "   Observe → Dashboards → Perses tab"
echo ""
echo "📈 Available Dashboards:"
echo "   - Platform Admin Dashboard"
echo "   - AI Engineer Dashboard"
echo ""

exit 0
