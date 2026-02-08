#!/bin/bash

# MaaS Observability Stack Installation Script
# Configures metrics collection (ServiceMonitors, TelemetryPolicy) and optionally installs dashboards
#
# This script is idempotent - safe to run multiple times
#
# Usage: ./install-observability.sh [--namespace NAMESPACE]

set -e

# Preflight checks
for cmd in kubectl kustomize jq; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "❌ Required command '$cmd' not found. Please install it first."
        exit 1
    fi
done

# Parse arguments
# Default namespace matches deploy-rhoai-stable.sh behavior:
# - ODH deploys to 'opendatahub'
# - RHOAI deploys to 'redhat-ods-applications'
# Auto-detect based on which namespace exists, or use MAAS_API_NAMESPACE if set
if [ -n "${MAAS_API_NAMESPACE:-}" ]; then
    NAMESPACE="$MAAS_API_NAMESPACE"
elif kubectl get namespace redhat-ods-applications &>/dev/null; then
    NAMESPACE="redhat-ods-applications"
elif kubectl get namespace opendatahub &>/dev/null; then
    NAMESPACE="opendatahub"
else
    NAMESPACE="opendatahub"  # Default fallback
fi

show_help() {
    echo "Usage: $0 [--namespace NAMESPACE]"
    echo ""
    echo "Options:"
    echo "  -n, --namespace    Target namespace for observability (default: auto-detect opendatahub/redhat-ods-applications)"
    echo ""
    echo "This script installs monitoring components:"
    echo "  - Enables user-workload-monitoring"
    echo "  - Deploys TelemetryPolicy and ServiceMonitors"
    echo "  - Configures Istio Gateway and LLM model metrics"
    echo ""
    echo "Examples:"
    echo "  $0                              # Install monitoring (auto-detect namespace)"
    echo "  $0 --namespace opendatahub      # Install for ODH deployment"
    echo "  $0 --namespace redhat-ods-applications  # Install for RHOAI deployment"
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

echo "========================================="
echo "📊 MaaS Observability Stack Installation"
echo "========================================="
echo ""
echo "Target namespace: $NAMESPACE"
echo ""

# Helper function
wait_for_crd() {
    local crd="$1"
    local timeout="${2:-120}"
    echo "⏳ Waiting for CRD $crd (timeout: ${timeout}s)..."
    local end_time=$((SECONDS + timeout))
    while [ $SECONDS -lt $end_time ]; do
        if kubectl get crd "$crd" &>/dev/null; then
            # Pass remaining time, not full timeout
            local remaining_time=$((end_time - SECONDS))
            [ $remaining_time -lt 1 ] && remaining_time=1
            if kubectl wait --for=condition=Established --timeout="${remaining_time}s" "crd/$crd" 2>/dev/null; then
                return 0
            else
                echo "❌ CRD $crd failed to become Established"
                return 1
            fi
        fi
        sleep 2
    done
    echo "❌ Timed out waiting for CRD $crd"
    return 1
}

# ==========================================
# Step 1: Enable user-workload-monitoring
# ==========================================
echo "1️⃣ Enabling user-workload-monitoring..."

if kubectl get configmap cluster-monitoring-config -n openshift-monitoring &>/dev/null; then
    CURRENT_CONFIG=$(kubectl get configmap cluster-monitoring-config -n openshift-monitoring -o jsonpath='{.data.config\.yaml}' 2>/dev/null || echo "")
    # Use grep -v to exclude comment lines, then check for the actual setting
    if echo "$CURRENT_CONFIG" | grep -v '^\s*#' | grep -q "enableUserWorkload: true"; then
        echo "   ✅ user-workload-monitoring already enabled"
    else
        echo "   Patching cluster-monitoring-config to enable user-workload-monitoring..."
        # Use patch to merge the setting, preserving any existing configuration
        if [ -z "$CURRENT_CONFIG" ]; then
            # ConfigMap exists but has no config.yaml data
            kubectl patch configmap cluster-monitoring-config -n openshift-monitoring \
                --type merge -p '{"data":{"config.yaml":"enableUserWorkload: true\n"}}'
        elif echo "$CURRENT_CONFIG" | grep -v '^\s*#' | grep -q "enableUserWorkload:"; then
            # ConfigMap has enableUserWorkload set to something other than true (e.g., false)
            # Replace the existing value to avoid duplicate YAML keys
            NEW_CONFIG=$(echo "$CURRENT_CONFIG" | sed 's/enableUserWorkload:.*/enableUserWorkload: true/')
            kubectl patch configmap cluster-monitoring-config -n openshift-monitoring \
                --type merge -p "{\"data\":{\"config.yaml\":$(echo "$NEW_CONFIG" | jq -Rs .)}}"
        else
            # ConfigMap exists with config but no enableUserWorkload setting - append it
            NEW_CONFIG=$(printf '%s\nenableUserWorkload: true\n' "$CURRENT_CONFIG")
            kubectl patch configmap cluster-monitoring-config -n openshift-monitoring \
                --type merge -p "{\"data\":{\"config.yaml\":$(echo "$NEW_CONFIG" | jq -Rs .)}}"
        fi
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
#
# Namespaces to configure:
#   - kuadrant-system: where Limitador runs (rate limiting metrics)
#   - $NAMESPACE: where MaaS API runs (opendatahub or redhat-ods-applications)
#   - llm: where models run (vLLM metrics)
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

# Deploy base observability resources (TelemetryPolicy + ServiceMonitors)
# TelemetryPolicy is CRITICAL - it extracts user/tier/model labels for Limitador metrics
BASE_OBSERVABILITY_DIR="$PROJECT_ROOT/deployment/base/observability"
if [ -d "$BASE_OBSERVABILITY_DIR" ]; then
    kustomize build "$BASE_OBSERVABILITY_DIR" | kubectl apply -f -
    echo "   ✅ TelemetryPolicy and base ServiceMonitors deployed"
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
echo "✅ Observability Stack Installed!"
echo "========================================="
echo ""

echo "📝 Metrics collection configured:"
echo "   Limitador: authorized_hits, authorized_calls, limited_calls, limitador_up"
echo "   Authorino: authorino_authorization_response_duration_seconds"
echo "   Istio:     istio_requests_total, istio_request_duration_milliseconds"
echo "   vLLM:      vllm:num_requests_running, vllm:num_requests_waiting, vllm:gpu_cache_usage_perc"
echo ""
