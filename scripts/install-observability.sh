#!/bin/bash

# MaaS Observability Stack Installation Script
# Configures metrics collection (ServiceMonitors, TelemetryPolicy). For dashboards, use install-grafana-dashboards.sh.
#
# This script is idempotent - safe to run multiple times
#
# Usage: ./install-observability.sh [--namespace NAMESPACE]
# For Grafana dashboards, run the helper: ./scripts/install-grafana-dashboards.sh [--grafana-namespace NS] [--grafana-label KEY=VALUE]

set -e
set -o pipefail

# Preflight checks
for cmd in kubectl kustomize jq yq; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "❌ Required command '$cmd' not found. Please install it first."
        exit 1
    fi
done

# Parse arguments
NAMESPACE="${MAAS_API_NAMESPACE:-maas-api}"

show_help() {
    echo "Usage: $0 [--namespace NAMESPACE]"
    echo ""
    echo "Installs monitoring components only (no dashboards):"
    echo "  - Enables user-workload-monitoring"
    echo "  - Deploys TelemetryPolicy and ServiceMonitors"
    echo "  - Configures Istio Gateway and LLM model metrics"
    echo ""
    echo "Options:"
    echo "  -n, --namespace   Target namespace for observability (default: maas-api)"
    echo ""
    echo "To install MaaS Grafana dashboards (separate step), run:"
    echo "  $(dirname "$0")/install-grafana-dashboards.sh [--grafana-namespace NS] [--grafana-label KEY=VALUE]"
    echo ""
    echo "Examples:"
    echo "  $0                    # Install monitoring only"
    echo "  $0 --namespace my-ns"
    echo ""
    exit 0
}

while [[ $# -gt 0 ]]; do
    case $1 in
        --namespace|-n)
            if [[ -z "$2" || "$2" == -* ]]; then
                echo "Error: --namespace requires a non-empty value"
                exit 1
            fi
            NAMESPACE="$2"
            shift 2
            ;;
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
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
OBSERVABILITY_DIR="$PROJECT_ROOT/deployment/components/observability"

# Import shared helper functions (wait_for_crd, etc.)
source "$SCRIPT_DIR/deployment-helpers.sh"

# ==========================================
# Local Helper Functions
# ==========================================

# kuadrant_already_scrapes endpoint namespace
#   Checks if any Kuadrant-provided ServiceMonitor or PodMonitor already scrapes
#   the given endpoint path. Used to avoid deploying duplicate monitors.
#   Excludes MaaS-owned monitors (labeled app.kubernetes.io/part-of: maas-observability)
#   so re-runs of this script don't falsely detect our own monitors.
#
# Arguments:
#   endpoint  - The metrics path to check for (e.g. "/server-metrics", "/metrics")
#   namespace - The namespace to search in (default: kuadrant-system)
#
# Returns:
#   0 if a Kuadrant-provided monitor already scrapes this endpoint
#   1 if no existing monitor scrapes it (safe to deploy ours)
kuadrant_already_scrapes() {
    local endpoint="$1"
    local namespace="${2:-kuadrant-system}"

    # Get all monitors, exclude MaaS-owned ones, check for the endpoint path
    kubectl get servicemonitor,podmonitor -n "$namespace" \
        -l 'app.kubernetes.io/part-of!=maas-observability' \
        -o json 2>/dev/null | grep -q "\"${endpoint}\""
}

# ==========================================
# Stack Selection
# ==========================================
echo "========================================="
echo "📊 MaaS Observability Stack Installation"
echo "========================================="
echo ""
echo "Target namespace: $NAMESPACE"
echo ""

# ==========================================
# Step 1: Enable user-workload-monitoring
# ==========================================
echo "1️⃣ Enabling user-workload-monitoring..."

if kubectl get configmap cluster-monitoring-config -n openshift-monitoring &>/dev/null; then
    CURRENT_CONFIG=$(kubectl get configmap cluster-monitoring-config -n openshift-monitoring -o jsonpath='{.data.config\.yaml}' 2>/dev/null || echo "")
    CURRENT_VALUE=$(echo "$CURRENT_CONFIG" | yq '.enableUserWorkload // false' 2>/dev/null || echo "false")
    if [ "$CURRENT_VALUE" = "true" ]; then
        echo "   ✅ user-workload-monitoring already enabled"
    else
        echo "   Patching cluster-monitoring-config to enable user-workload-monitoring..."
        NEW_CONFIG=$(echo "$CURRENT_CONFIG" | yq '.enableUserWorkload = true')
        kubectl patch configmap cluster-monitoring-config -n openshift-monitoring \
            --type merge -p "{\"data\":{\"config.yaml\":$(echo "$NEW_CONFIG" | jq -Rs .)}}"
        echo "   ✅ user-workload-monitoring enabled (existing config preserved)"
    fi
else
    echo "   Creating cluster-monitoring-config..."
    kubectl apply -f "$PROJECT_ROOT/docs/samples/observability/cluster-monitoring-config.yaml"
    echo "   ✅ user-workload-monitoring enabled"
fi

# Wait for user-workload-monitoring pods
echo "   Waiting for user-workload-monitoring pods..."
sleep 5
kubectl wait --for=condition=Ready pods -l app.kubernetes.io/name=prometheus \
    -n openshift-user-workload-monitoring --timeout=120s 2>/dev/null || \
    echo "   ⚠️  Pods still starting, continuing..."

# ==========================================
# Step 2: Ensure namespaces do NOT have cluster-monitoring label
# ==========================================
echo ""
echo "2️⃣ Configuring namespaces for user-workload-monitoring..."

# IMPORTANT: Do NOT add openshift.io/cluster-monitoring=true label!
# That label is for cluster-monitoring (infrastructure) and BLOCKS user-workload-monitoring.
# User-workload-monitoring (which we need) scrapes namespaces that DON'T have this label.
for ns in kuadrant-system "$NAMESPACE" llm; do
    if kubectl get namespace "$ns" &>/dev/null; then
        # Remove the cluster-monitoring label if present (it blocks user-workload-monitoring)
        kubectl label namespace "$ns" openshift.io/cluster-monitoring- 2>/dev/null || true
        echo "   ✅ Configured namespace: $ns (user-workload-monitoring enabled)"
    fi
done

# ==========================================
# Step 3: Deploy TelemetryPolicy and Base ServiceMonitors
# ==========================================
echo ""
echo "3️⃣ Deploying TelemetryPolicy and ServiceMonitors..."

# Deploy base observability resources (TelemetryPolicy + Istio Telemetry)
# TelemetryPolicy is CRITICAL - it extracts user/tier/model labels for Limitador metrics
BASE_OBSERVABILITY_DIR="$PROJECT_ROOT/deployment/base/observability"
if [ -d "$BASE_OBSERVABILITY_DIR" ]; then
    kustomize build "$BASE_OBSERVABILITY_DIR" | kubectl apply -f -
    echo "   ✅ TelemetryPolicy and Istio Telemetry deployed"

    # Deploy Limitador ServiceMonitor only if Kuadrant doesn't already scrape /metrics from Limitador.
    # When Kuadrant CR has spec.observability.enable=true, it creates kuadrant-limitador-monitor
    # which scrapes the same Limitador pod. Deploying both causes duplicate metrics.
    if kuadrant_already_scrapes "/metrics" kuadrant-system \
       || kubectl get podmonitor kuadrant-limitador-monitor -n kuadrant-system &>/dev/null; then
        echo "   ℹ️  Kuadrant already scrapes Limitador /metrics - skipping MaaS ServiceMonitor (no duplicates)"
    else
        kubectl apply -f "$BASE_OBSERVABILITY_DIR/servicemonitor.yaml"
        echo "   ✅ Limitador ServiceMonitor deployed (Kuadrant PodMonitor not found)"
    fi

    # Deploy Authorino server-metrics ServiceMonitor.
    # The Kuadrant operator's authorino-operator-monitor only scrapes /metrics (controller-runtime).
    # This additional ServiceMonitor scrapes /server-metrics for auth evaluation metrics
    # (auth_server_authconfig_duration_seconds, auth_server_authconfig_response_status, etc.)
    if ! kubectl get service -n kuadrant-system -l authorino-resource=authorino,control-plane=controller-manager &>/dev/null 2>&1; then
        echo "   ⚠️  Authorino service not found - skipping Authorino server-metrics"
    elif kuadrant_already_scrapes "/server-metrics"; then
        echo "   ℹ️  Kuadrant already scrapes Authorino /server-metrics - skipping MaaS ServiceMonitor (no duplicates)"
    else
        kubectl apply -f "$BASE_OBSERVABILITY_DIR/authorino-server-metrics-servicemonitor.yaml"
        echo "   ✅ Authorino /server-metrics ServiceMonitor deployed"
    fi
else
    echo "   ⚠️  Base observability directory not found - TelemetryPolicy may be missing!"
fi

# Deploy Istio Gateway metrics (if gateway exists)
if kubectl get deploy -n openshift-ingress maas-default-gateway-openshift-default &>/dev/null; then
    kubectl apply -f "$OBSERVABILITY_DIR/monitors/istio-gateway-service.yaml"
    kubectl apply -f "$OBSERVABILITY_DIR/monitors/istio-gateway-servicemonitor.yaml"
    echo "   ✅ Istio Gateway metrics configured"
else
    echo "   ⚠️  Istio Gateway not found - skipping Istio metrics"
fi

# Deploy LLM models ServiceMonitor (for vLLM metrics)
# NOTE: This ServiceMonitor is in docs/samples/ as it's optional/user-configurable
if kubectl get ns llm &>/dev/null; then
    kubectl apply -f "$PROJECT_ROOT/docs/samples/observability/kserve-llm-models-servicemonitor.yaml"
    echo "   ✅ LLM models metrics configured"
else
    echo "   ⚠️  llm namespace not found - skipping LLM metrics"
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
echo "   Authorino: auth_server_authconfig_duration_seconds, auth_server_authconfig_response_status"
echo "   Istio:     istio_requests_total, istio_request_duration_milliseconds"
echo "   vLLM:      vllm:num_requests_running, vllm:num_requests_waiting, vllm:kv_cache_usage_perc"
echo ""

echo "💡 To install MaaS Grafana dashboards (discovers Grafana cluster-wide, warn-only):"
echo "   $(dirname "$0")/install-grafana-dashboards.sh [--grafana-namespace NS] [--grafana-label KEY=VALUE]"
echo ""
