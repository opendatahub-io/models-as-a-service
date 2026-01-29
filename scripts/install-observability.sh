#!/bin/bash

# MaaS Observability Stack Installation Script
# Configures metrics collection (ServiceMonitors, TelemetryPolicy) and optionally installs dashboards
#
# This script is idempotent - safe to run multiple times
#
# Usage: ./install-observability.sh [--namespace NAMESPACE] [--stack grafana|perses|both]

set -e

# Preflight checks
for cmd in kubectl kustomize jq; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "❌ Required command '$cmd' not found. Please install it first."
        exit 1
    fi
done

# Parse arguments
NAMESPACE="${MAAS_API_NAMESPACE:-maas-api}"
OBSERVABILITY_STACK=""  # Empty = prompt if interactive

show_help() {
    echo "Usage: $0 [--namespace NAMESPACE] [--stack STACK]"
    echo ""
    echo "Options:"
    echo "  -n, --namespace    Target namespace for observability (default: maas-api)"
    echo "  -s, --stack        Dashboard stack to install: grafana, perses, or both"
    echo ""
    echo "By default, this script installs only monitoring components:"
    echo "  - Enables user-workload-monitoring"
    echo "  - Deploys TelemetryPolicy and ServiceMonitors"
    echo "  - Configures Istio Gateway and LLM model metrics"
    echo ""
    echo "Examples:"
    echo "  $0                              # Install monitoring only (no dashboards)"
    echo "  $0 --stack grafana              # Install monitoring + Grafana dashboards"
    echo "  $0 --stack perses               # Install monitoring + Perses dashboards"
    echo "  $0 --stack both                 # Install monitoring + both dashboards"
    echo "  $0 --namespace my-ns --stack grafana"
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
        --stack|-s)
            if [[ -z "$2" || "$2" == -* ]]; then
                echo "Error: --stack requires a value (grafana, perses, or both)"
                exit 1
            fi
            case "$2" in
                grafana|perses|both)
                    OBSERVABILITY_STACK="$2"
                    ;;
                *)
                    echo "Error: --stack must be 'grafana', 'perses', or 'both'"
                    exit 1
                    ;;
            esac
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

# ==========================================
# Stack Selection
# ==========================================
# Default: monitoring only (no dashboards)
# Use --stack grafana|perses|both to install dashboards

echo "========================================="
echo "📊 MaaS Observability Stack Installation"
echo "========================================="
echo ""
echo "Target namespace: $NAMESPACE"
if [ -z "$OBSERVABILITY_STACK" ]; then
    echo "Stack: monitoring only (no dashboards)"
else
    echo "Stack: $OBSERVABILITY_STACK"
fi
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
    if echo "$CURRENT_CONFIG" | grep -q "enableUserWorkload: true"; then
        echo "   ✅ user-workload-monitoring already enabled"
    else
        echo "   Patching cluster-monitoring-config to enable user-workload-monitoring..."
        # Use patch to merge the setting, preserving any existing configuration
        if [ -z "$CURRENT_CONFIG" ]; then
            # ConfigMap exists but has no config.yaml data
            kubectl patch configmap cluster-monitoring-config -n openshift-monitoring \
                --type merge -p '{"data":{"config.yaml":"enableUserWorkload: true\n"}}'
        elif echo "$CURRENT_CONFIG" | grep -q "enableUserWorkload:"; then
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
# Install Grafana (if selected)
# ==========================================
install_grafana() {
    echo ""
    echo "4️⃣ Installing Grafana..."

    # Install Grafana Operator
    if kubectl get csv -n openshift-operators 2>/dev/null | grep -q "grafana-operator"; then
        echo "   ✅ Grafana Operator already installed"
    else
        "$SCRIPT_DIR/installers/install-grafana.sh"
    fi

    # Wait for CRDs
    echo "   Waiting for Grafana CRDs..."
    wait_for_crd "grafanas.grafana.integreatly.org" 120 || {
        echo "   ❌ Grafana CRDs not available. Please install Grafana Operator manually."
        return 1
    }

    # Ensure namespace exists
    kubectl create namespace "$NAMESPACE" 2>/dev/null || true

    # Deploy Grafana instance
    echo "   Deploying Grafana instance to $NAMESPACE..."
    kustomize build "$OBSERVABILITY_DIR/grafana" | \
        sed "s/namespace: maas-api/namespace: $NAMESPACE/g" | \
        kubectl apply -f -

    echo "   ✅ Grafana instance deployed"

    # Wait for Grafana pod (and implicitly for grafana-sa ServiceAccount)
    echo "   Waiting for Grafana pod..."
    for i in $(seq 1 24); do
        if kubectl wait --for=condition=Ready pods -l app=grafana -n "$NAMESPACE" --timeout=5s 2>/dev/null; then
            echo "   ✅ Grafana pod is ready"
            break
        fi
        echo "   Waiting for Grafana pod... (attempt $i/24)"
        sleep 5
    done
    # Verify pod is actually ready (fail if loop exhausted without success)
    if ! kubectl get pods -l app=grafana -n "$NAMESPACE" -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null | grep -q "True"; then
        echo "   ❌ Grafana pod failed to become ready after 24 attempts"
        return 1
    fi

    # Configure Prometheus Datasource
    echo "   Configuring Prometheus datasource..."
    
    # Wait for grafana-sa ServiceAccount to exist (created by operator)
    for i in $(seq 1 12); do
        if kubectl get sa grafana-sa -n "$NAMESPACE" &>/dev/null; then
            break
        fi
        echo "   Waiting for grafana-sa ServiceAccount... (attempt $i/12)"
        sleep 5
    done
    # Verify ServiceAccount exists (fail if loop exhausted without success)
    if ! kubectl get sa grafana-sa -n "$NAMESPACE" &>/dev/null; then
        echo "   ❌ grafana-sa ServiceAccount not found in namespace $NAMESPACE after 12 attempts"
        return 1
    fi
    
    # Token valid for 30 days. To rotate: delete the GrafanaDatasource and re-run this script.
    # For production, consider automating rotation via CronJob or external secrets operator.
    TOKEN=$(kubectl create token grafana-sa -n "$NAMESPACE" --duration=720h 2>/dev/null || echo "")

    if [ -z "$TOKEN" ]; then
        echo "   ⚠️  Could not create token for grafana-sa ServiceAccount"
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

    # Deploy Grafana Dashboards
    echo "   Deploying Grafana dashboards..."
    kustomize build "$OBSERVABILITY_DIR/dashboards" | \
        sed "s/namespace: maas-api/namespace: $NAMESPACE/g" | \
        kubectl apply -f -

    # Validate Grafana dashboards are synced
    echo "   Validating Grafana dashboards..."
    VALIDATION_PASSED=false
    for attempt in $(seq 1 12); do
        sleep 3
        DASHBOARD_COUNT=$(kubectl get grafanadashboard -n "$NAMESPACE" --no-headers 2>/dev/null | wc -l | tr -d ' ')
        
        if [ "$DASHBOARD_COUNT" -ge 1 ] 2>/dev/null; then
            # Check sync status - condition type is "DashboardSynchronized"
            SYNC_OUTPUT=$(kubectl get grafanadashboard -n "$NAMESPACE" -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="DashboardSynchronized")].status}{" "}{end}' 2>/dev/null || echo "")
            SYNCED_COUNT=$(echo "$SYNC_OUTPUT" | tr ' ' '\n' | grep -c "True" 2>/dev/null || echo "0")
            
            # Check for errors in messages
            ERROR_OUTPUT=$(kubectl get grafanadashboard -n "$NAMESPACE" -o jsonpath='{range .items[*]}{.metadata.name}:{.status.conditions[?(@.type=="DashboardSynchronized")].message}{"\n"}{end}' 2>/dev/null || echo "")
            ERROR_STATUS=$(echo "$ERROR_OUTPUT" | grep -i "error\|failed" || true)
            
            if [ -n "$ERROR_STATUS" ]; then
                echo "   ❌ Dashboard sync failed:"
                echo "$ERROR_STATUS" | head -3
                VALIDATION_PASSED=false
                break
            fi
            
            if [ "$SYNCED_COUNT" -eq "$DASHBOARD_COUNT" ] 2>/dev/null; then
                echo "   ✅ Grafana dashboards validated ($SYNCED_COUNT/$DASHBOARD_COUNT synced)"
                VALIDATION_PASSED=true
                break
            fi
        fi
        
        echo "   Waiting for dashboards to sync... (attempt $attempt/12)"
    done

    if [ "$VALIDATION_PASSED" = "false" ]; then
        echo "   ⚠️  Some dashboards may have issues. Check: kubectl get grafanadashboard -n $NAMESPACE"
    fi

    # Show Grafana URL
    GRAFANA_HOST=$(kubectl get route grafana-ingress -n "$NAMESPACE" -o jsonpath='{.spec.host}' 2>/dev/null || true)
    if [ -n "$GRAFANA_HOST" ]; then
        echo ""
        echo "   🌐 Grafana UI: https://$GRAFANA_HOST"
    fi
}

# ==========================================
# Install Perses
# ==========================================
install_perses() {
    echo ""
    echo "5️⃣ Installing Perses..."

    # Install Cluster Observability Operator (includes Perses)
    if kubectl get csv -n openshift-operators 2>/dev/null | grep -q "cluster-observability-operator.*Succeeded"; then
        echo "   ✅ Cluster Observability Operator already installed"
    else
        "$SCRIPT_DIR/installers/install-perses.sh"
    fi

    # Wait for Perses CRDs (all 3 are needed for instance, dashboards, and datasource)
    echo "   Waiting for Perses CRDs..."
    for crd in perses.perses.dev persesdashboards.perses.dev persesdatasources.perses.dev; do
        wait_for_crd "$crd" 120 || {
            echo "   ❌ CRD $crd not available. Please check Cluster Observability Operator installation."
            return 1
        }
    done

    # Deploy UIPlugin (enables Perses in OpenShift Console)
    echo "   Enabling Perses UIPlugin..."
    kubectl apply -f "$OBSERVABILITY_DIR/perses/uiplugin.yaml"
    echo "   ✅ UIPlugin enabled"

    # Wait for UIPlugin's Perses instance to be ready
    echo "   Waiting for Perses instance (created by UIPlugin)..."
    for i in $(seq 1 30); do
        PERSES_PODS=$(kubectl get pods -n openshift-operators -l app.kubernetes.io/name=perses --no-headers 2>/dev/null | grep -c Running || echo "0")
        if [ "$PERSES_PODS" -ge 1 ] 2>/dev/null; then
            echo "   ✅ Perses pod is running in openshift-operators"
            break
        fi
        echo "   Waiting for Perses pod... (attempt $i/30)"
        sleep 5
    done

    # Deploy dashboards to openshift-operators (where UIPlugin's Perses lives)
    echo "   Deploying Perses dashboards..."
    kubectl apply -f "$OBSERVABILITY_DIR/perses/dashboards/dashboard-ai-engineer.yaml" -n openshift-operators
    kubectl apply -f "$OBSERVABILITY_DIR/perses/dashboards/dashboard-platform-admin.yaml" -n openshift-operators
    echo "   ✅ Perses dashboards deployed"

    # Deploy datasource to openshift-operators
    echo "   Configuring Prometheus datasource..."
    kubectl apply -f "$OBSERVABILITY_DIR/perses/perses-datasource.yaml" -n openshift-operators 2>/dev/null || \
        echo "   ⚠️  Datasource may already be configured"

    echo "   ✅ Perses installation complete"
    echo ""
    echo "   🌐 Access Perses dashboards via OpenShift Console:"
    echo "      Observe → Dashboards → Perses tab"
}

# ==========================================
# Execute based on stack selection
# ==========================================
if [ -n "$OBSERVABILITY_STACK" ]; then
    case "$OBSERVABILITY_STACK" in
        grafana)
            install_grafana
            ;;
        perses)
            install_perses
            ;;
        both)
            install_grafana
            install_perses
            ;;
    esac
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

# Show Grafana info if installed
if [[ "$OBSERVABILITY_STACK" == "grafana" || "$OBSERVABILITY_STACK" == "both" ]]; then
    GRAFANA_ROUTE=$(kubectl get route grafana-ingress -n "$NAMESPACE" -o jsonpath='{.spec.host}' 2>/dev/null || echo "")
    if [ -n "$GRAFANA_ROUTE" ]; then
        echo "📊 Grafana URL: https://$GRAFANA_ROUTE"
        echo "   🔐 Default Credentials: admin / admin"
        echo ""
    fi
fi

# Show Perses info if installed
if [[ "$OBSERVABILITY_STACK" == "perses" || "$OBSERVABILITY_STACK" == "both" ]]; then
    echo "📊 Perses Dashboards: OpenShift Console → Observe → Dashboards → Perses"
    echo ""
fi

# Show dashboard info if any dashboard was installed
if [ -n "$OBSERVABILITY_STACK" ]; then
    echo "📈 Available Dashboards:"
    echo "   - Platform Admin Dashboard"
    echo "   - AI Engineer Dashboard"
    echo ""
else
    echo "💡 To install dashboards, run:"
    echo "   $0 --stack grafana    # For Grafana dashboards"
    echo "   $0 --stack perses     # For Perses dashboards"
    echo ""
fi
