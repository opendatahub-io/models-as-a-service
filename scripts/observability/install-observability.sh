#!/bin/bash

# MaaS Observability Stack Installation Script
# Configures metrics collection (ServiceMonitors, TelemetryPolicy).
#
# This script is idempotent - safe to run multiple times
#
# Usage: ./install-observability.sh
# Does not apply PersesDashboard manifests. Operator: LifecycleReconciler.ensureUsageDashboard
# when Config.spec.usageMetricsDashboard is true.
# Kustomize: deployment/components/observability/observability/dashboards/. See docs/content/observability/setup.md.

set -euo pipefail

# Preflight checks
for cmd in kubectl kustomize yq; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "❌ Required command '$cmd' not found. Please install it first."
        exit 1
    fi
done

show_help() {
    echo "Usage: $0"
    echo ""
    echo "Installs monitoring components:"
    echo "  - Deploys TelemetryPolicy and ServiceMonitors"
    echo "  - Configures Istio Gateway metrics"
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

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# Import shared helper functions (wait_for_crd, etc.)
source "$PROJECT_ROOT/scripts/deployment-helpers.sh"

# ==========================================
# Stack Selection
# ==========================================
echo "========================================="
echo "📊 MaaS Observability Stack Installation"
echo "========================================="
echo ""

echo "1️⃣ Deploying TelemetryPolicy and ServiceMonitors..."

# Deploy base observability resources (TelemetryPolicy + Istio Telemetry)
# TelemetryPolicy is CRITICAL - it extracts user/subscription/model labels for Limitador metrics
BASE_OBSERVABILITY_DIR="$PROJECT_ROOT/deployment/base/observability"
if [ -d "$BASE_OBSERVABILITY_DIR" ]; then
    kustomize build "$BASE_OBSERVABILITY_DIR" | kubectl apply -f -
    echo "   ✅ TelemetryPolicy and Istio Telemetry deployed"
else
    echo "   ⚠️  Base observability directory not found - TelemetryPolicy may be missing!"
fi

echo ""
echo "2️⃣ Deploying Istio Gateway metrics..."

# Deploy Istio Gateway metrics (if gateway exists)
if kubectl get deploy -n openshift-ingress maas-default-gateway-openshift-default &>/dev/null; then
    kubectl apply -f "$BASE_OBSERVABILITY_DIR/istio-gateway-service.yaml"
    kubectl apply -f "$BASE_OBSERVABILITY_DIR/istio-gateway-servicemonitor.yaml"
    echo "   ✅ Istio Gateway metrics configured"
else
    echo "   ⚠️  Istio Gateway not found - skipping Istio metrics"
fi

echo ""
echo "3️⃣ Configuring Authorino monitoring..."

# The controller uses these same manifests and respects Config.spec.authorinoMonitoring.
# Never bypass the feature gate or create a second scrape target in operator-managed mode.
if kubectl get configs.maas.opendatahub.io default &>/dev/null; then
    echo "   ℹ️  Config/default exists - maas-controller manages Authorino monitoring"
else
    MONITORING_NAMESPACE="$(kubectl get dscinitialization -o jsonpath='{.items[0].spec.monitoring.namespace}' 2>/dev/null || true)"
    if [[ -z "$MONITORING_NAMESPACE" ]]; then
        echo "   ⚠️  DSCI monitoring namespace not configured - skipping Authorino monitoring"
    elif ! kubectl get deploy -n kuadrant-system authorino &>/dev/null && \
         ! kubectl get deploy -n rh-connectivity-link authorino &>/dev/null; then
        echo "   ⚠️  Authorino deployment not found - skipping Authorino monitoring"
    else
        for manifest in authorino-server-metrics-servicemonitor.yaml \
                        authorino-maas-metadata-evaluator-prometheusrule.yaml \
                        authorino-maas-oidc-prometheusrule.yaml; do
            yq eval ".metadata.namespace = \"$MONITORING_NAMESPACE\"" \
                "$BASE_OBSERVABILITY_DIR/$manifest" | kubectl apply -f -
        done
        echo "   ✅ Authorino monitor and monitoring.rhobs rules deployed in $MONITORING_NAMESPACE"
    fi
fi

# ==========================================
# Summary
# ==========================================
echo ""
echo "========================================="
echo "✅ Observability (monitoring) installed"
echo "========================================="
echo ""

echo "📝 Metrics collection configured:"
echo "   Limitador: authorized_hits, authorized_calls, limited_calls, limitador_up"
echo "   Authorino: auth_server_authconfig_duration_seconds, auth_server_authconfig_response_status_total, auth_server_evaluator_* (metadata HTTP)"
echo "   Istio:     istio_requests_total, istio_request_duration_milliseconds"
echo ""

echo "🚨 Alerting configured (if Authorino found):"
echo "   MaaSAuthorinoMetadataEvaluatorHighFailureRate - maas-api metadata call failure rate above configured threshold"
echo "   MaaSAuthorinoAuthenticationHighFailureRate - gateway authentication failure rate above configured threshold"
echo "   MaaSAuthorinoAuthenticationHighLatency - gateway authentication P95 latency above configured threshold"
echo ""

echo "💡 This script does not apply Perses dashboards."
echo "   Operator-managed: LifecycleReconciler.ensureUsageDashboard when Config.spec.usageMetricsDashboard is true."
echo "   Kustomize: deployment/components/observability/observability/dashboards/"
echo "   See docs/content/observability/setup.md and docs/content/observability/operations.md"
echo ""
