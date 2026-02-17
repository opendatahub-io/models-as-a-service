#!/bin/bash

# MaaS Perses Dashboard Installation (helper)
# Enables the Perses UIPlugin and deploys MaaS dashboard definitions (PersesDashboard CRs).
# Does not install the Cluster Observability Operator; assumes it is already present.
# Never fails for missing operator: warnings only (same pattern as install-grafana-dashboards.sh).
#
# This script is idempotent - safe to run multiple times
#
# Usage: ./install-perses-dashboards.sh

set -e
set -o pipefail

if ! command -v kubectl &>/dev/null; then
    echo "❌ Required command 'kubectl' not found. Please install it first."
    exit 1
fi

show_help() {
    echo "Usage: $0"
    echo ""
    echo "Enables the Perses UIPlugin and deploys MaaS PersesDashboard definitions into openshift-operators."
    echo "Requires the Cluster Observability Operator to be installed first (provides Perses CRDs)."
    echo ""
    echo "Perses dashboards are accessible via OpenShift Console → Observe → Dashboards → Perses tab."
    echo ""
    echo "Examples:"
    echo "  $0    # Deploy Perses dashboards"
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


# ==========================================
# Preflight: Cluster Observability Operator & Perses CRDs
# ==========================================
echo "📊 MaaS Perses Dashboard Installation"
echo ""

if ! kubectl get crd persesdashboards.perses.dev &>/dev/null; then
    echo "⚠️  Perses CRDs not found. Install the Cluster Observability Operator first."
    echo "   Run:  ./scripts/installers/install-perses.sh"
    echo "   See:  https://docs.redhat.com/en/documentation/red_hat_openshift_cluster_observability_operator/1-latest/html/about_red_hat_openshift_cluster_observability_operator/index"
    exit 0
fi
echo "✅ Perses CRDs available"

# ==========================================
# Step 1: Enable UIPlugin (shows Perses in OpenShift Console)
# ==========================================
echo ""
echo "🔌 Enabling Perses UIPlugin..."
kubectl apply -f "$OBSERVABILITY_DIR/perses/perses-uiplugin.yaml"
echo "   ✅ UIPlugin enabled"

# ==========================================
# Step 2: Wait for Perses pod (created by UIPlugin)
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
# Step 3: Deploy dashboards and datasource
# ==========================================
echo ""
echo "📊 Deploying Perses dashboards..."
kubectl apply -f "$OBSERVABILITY_DIR/perses/dashboards/dashboard-ai-engineer.yaml" -n openshift-operators
kubectl apply -f "$OBSERVABILITY_DIR/perses/dashboards/dashboard-platform-admin.yaml" -n openshift-operators
echo "   ✅ Dashboards deployed (Platform Admin, AI Engineer)"

echo ""
echo "🔗 Configuring Prometheus datasource..."
if kubectl apply -f "$OBSERVABILITY_DIR/perses/perses-datasource.yaml" -n openshift-operators 2>/dev/null; then
    echo "   ✅ Datasource configured"
else
    echo "   ⚠️  Datasource may already be configured"
fi

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
