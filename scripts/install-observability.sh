#!/bin/bash

# MaaS Observability Stack Installation Script
# Installs Grafana, dashboards, and configures Prometheus integration
#
# This script is idempotent - safe to run multiple times
#
# Usage: ./install-observability.sh [--namespace NAMESPACE]

set -e

# Parse arguments
NAMESPACE="${MAAS_API_NAMESPACE:-maas-api}"

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
            echo "Usage: $0 [--namespace NAMESPACE]"
            echo ""
            echo "Options:"
            echo "  -n, --namespace    Target namespace for Grafana (default: maas-api)"
            echo ""
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
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
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        if kubectl get crd "$crd" &>/dev/null; then
            kubectl wait --for=condition=Established --timeout="${timeout}s" "crd/$crd" 2>/dev/null
            return 0
        fi
        sleep 2
        elapsed=$((elapsed + 2))
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
    if echo "$CURRENT_CONFIG" | grep -q "enableUserWorkload: true"; then
        echo "   ✅ user-workload-monitoring already enabled"
    else
        echo "   Updating cluster-monitoring-config..."
        kubectl apply -f "$OBSERVABILITY_DIR/cluster-monitoring-config.yaml"
        echo "   ✅ user-workload-monitoring enabled"
    fi
else
    echo "   Creating cluster-monitoring-config..."
    kubectl apply -f "$OBSERVABILITY_DIR/cluster-monitoring-config.yaml"
    echo "   ✅ user-workload-monitoring enabled"
fi

# Wait for user-workload-monitoring pods
echo "   Waiting for user-workload-monitoring pods..."
sleep 5
kubectl wait --for=condition=Ready pods -l app.kubernetes.io/name=prometheus \
    -n openshift-user-workload-monitoring --timeout=120s 2>/dev/null || \
    echo "   ⚠️  Pods still starting, continuing..."

# ==========================================
# Step 2: Label namespaces for monitoring
# ==========================================
echo ""
echo "2️⃣ Labeling namespaces for monitoring..."

for ns in kuadrant-system "$NAMESPACE" llm; do
    if kubectl get namespace "$ns" &>/dev/null; then
        kubectl label namespace "$ns" openshift.io/cluster-monitoring=true --overwrite 2>/dev/null || true
        echo "   ✅ Labeled namespace: $ns"
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
if kubectl get ns llm &>/dev/null; then
    kubectl apply -f "$OBSERVABILITY_DIR/monitors/kserve-llm-models-servicemonitor.yaml"
    echo "   ✅ LLM models metrics configured"
else
    echo "   ⚠️  llm namespace not found - skipping LLM metrics"
fi

# ==========================================
# Step 4: Install Grafana Operator
# ==========================================
echo ""
echo "4️⃣ Installing Grafana Operator..."

if kubectl get csv -n openshift-operators 2>/dev/null | grep -q "grafana-operator"; then
    echo "   ✅ Grafana Operator already installed"
else
    # Use existing installer script
    "$SCRIPT_DIR/installers/install-grafana.sh"
fi

# Wait for CRDs
echo "   Waiting for Grafana CRDs..."
wait_for_crd "grafanas.grafana.integreatly.org" 120 || {
    echo "   ❌ Grafana CRDs not available. Please install Grafana Operator manually."
    exit 1
}

# ==========================================
# Step 5: Deploy Grafana Instance
# ==========================================
echo ""
echo "5️⃣ Deploying Grafana instance to $NAMESPACE..."

# Ensure namespace exists
kubectl create namespace "$NAMESPACE" 2>/dev/null || true

# Deploy Grafana with namespace override
kustomize build "$OBSERVABILITY_DIR/grafana" | \
    sed "s/namespace: maas-api/namespace: $NAMESPACE/g" | \
    kubectl apply -f -

echo "   ✅ Grafana instance deployed"

# Wait for Grafana pod
echo "   Waiting for Grafana pod..."
kubectl wait --for=condition=Ready pods -l app=grafana -n "$NAMESPACE" --timeout=120s 2>/dev/null || \
    echo "   ⚠️  Grafana pod still starting, continuing..."

# ==========================================
# Step 6: Configure Prometheus Datasource
# ==========================================
echo ""
echo "6️⃣ Configuring Prometheus datasource..."

# Get authentication token from grafana-sa ServiceAccount
# Using grafana-sa ensures the token has cluster-monitoring-view permissions
# Token expires in 30 days. To renew: kubectl create token grafana-sa -n $NAMESPACE --duration=720h
TOKEN=$(kubectl create token grafana-sa -n "$NAMESPACE" --duration=720h 2>/dev/null || echo "")

if [ -z "$TOKEN" ]; then
    echo "   ⚠️  Could not create token for grafana-sa ServiceAccount"
    echo "   Deploying datasource without authentication (Prometheus queries may fail)..."
    echo "   To fix later, run: kubectl create token grafana-sa -n $NAMESPACE --duration=720h"
    # Deploy without auth - user will need to manually configure later
    cat <<EOF | kubectl apply -f -
apiVersion: grafana.integreatly.org/v1beta1
kind: GrafanaDatasource
metadata:
  name: prometheus
  namespace: $NAMESPACE
  labels:
    app: grafana
    component: observability
spec:
  instanceSelector:
    matchLabels:
      app: grafana
  datasource:
    name: Prometheus
    type: prometheus
    access: proxy
    url: https://thanos-querier.openshift-monitoring.svc.cluster.local:9091
    isDefault: true
    jsonData:
      tlsSkipVerify: true
EOF
else
    # Apply datasource with token substitution
    cat <<EOF | kubectl apply -f -
apiVersion: grafana.integreatly.org/v1beta1
kind: GrafanaDatasource
metadata:
  name: prometheus
  namespace: $NAMESPACE
  labels:
    app: grafana
    component: observability
spec:
  instanceSelector:
    matchLabels:
      app: grafana
  datasource:
    name: Prometheus
    type: prometheus
    access: proxy
    url: https://thanos-querier.openshift-monitoring.svc.cluster.local:9091
    isDefault: true
    jsonData:
      tlsSkipVerify: true
      httpHeaderName1: Authorization
    secureJsonData:
      httpHeaderValue1: "Bearer $TOKEN"
EOF
    echo "   ✅ Prometheus datasource configured with authentication"
fi

# ==========================================
# Step 7: Deploy Dashboards
# ==========================================
echo ""
echo "7️⃣ Deploying dashboards..."

kustomize build "$OBSERVABILITY_DIR/dashboards" | \
    sed "s/namespace: maas-api/namespace: $NAMESPACE/g" | \
    kubectl apply -f -

echo "   ✅ Dashboards deployed"

# ==========================================
# Summary
# ==========================================
echo ""
echo "========================================="
echo "✅ Observability Stack Installed!"
echo "========================================="
echo ""

# Get Grafana route
GRAFANA_ROUTE=$(kubectl get route grafana-ingress -n "$NAMESPACE" -o jsonpath='{.spec.host}' 2>/dev/null || echo "")
if [ -n "$GRAFANA_ROUTE" ]; then
    echo "📊 Grafana URL: https://$GRAFANA_ROUTE"
    echo ""
    echo "🔐 Default Credentials (change after first login):"
    echo "   Username: admin"
    echo "   Password: admin"
    echo ""
fi

echo "📈 Available Dashboards:"
echo "   - Platform Admin Dashboard"
echo "   - AI Engineer Dashboard"
echo ""
echo "📝 Metrics available:"
echo "   Limitador: authorized_hits, authorized_calls, limited_calls, limitador_up"
echo "   Istio:     istio_requests_total, istio_request_duration_milliseconds"
echo "   vLLM:      vllm:num_requests_running, vllm:num_requests_waiting, vllm:gpu_cache_usage_perc"
echo "   K8s:       kube_pod_status_phase"
echo "   Alerts:    ALERTS{alertstate=\"firing\"}"
echo ""
