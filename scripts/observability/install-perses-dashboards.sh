#!/bin/bash

# MaaS Perses Dashboard Installation (helper)
# Enables the Perses UIPlugin and deploys MaaS dashboard definitions (PersesDashboard CRs).
# Does not install the Cluster Observability Operator; assumes it is already present.
# Never fails for missing operator: warnings only (same pattern as install-grafana-dashboards.sh).
#
# This script is idempotent - safe to run multiple times
#
# Usage: ./install-perses-dashboards.sh [--tenant-namespace NAMESPACE]

set -euo pipefail

if ! command -v kubectl &>/dev/null; then
    echo "❌ Required command 'kubectl' not found. Please install it first."
    exit 1
fi

TENANT_NAMESPACE="${TENANT_NAMESPACE:-kuadrant-system}"

show_help() {
    echo "Usage: $0 [--tenant-namespace NAMESPACE]"
    echo ""
    echo "Enables the Perses UIPlugin and deploys MaaS PersesDashboard definitions."
    echo "Requires the Cluster Observability Operator to be installed first (provides Perses CRDs)."
    echo ""
    echo "Admin dashboards (Platform Admin, AI Engineer) are deployed to openshift-operators."
    echo "A tenant-scoped Usage Dashboard is deployed to the tenant namespace with a namespace-scoped"
    echo "Thanos datasource (port 9092) and metrics.k8s.io RBAC, allowing restricted users with"
    echo "'view' access on the tenant namespace to see only the Usage Dashboard with metrics from"
    echo "that namespace."
    echo ""
    echo "Options:"
    echo "  --tenant-namespace  Namespace for tenant-scoped Usage Dashboard (default: kuadrant-system)"
    echo ""
    echo "Perses dashboards are accessible via OpenShift Console → Observe → Dashboards → Perses tab."
    echo ""
    echo "Examples:"
    echo "  $0                                        # Deploy with default tenant namespace"
    echo "  $0 --tenant-namespace kuadrant-system      # Explicit tenant namespace"
    echo ""
    exit 0
}

while [[ $# -gt 0 ]]; do
    case $1 in
        --tenant-namespace)
            if [[ $# -lt 2 || -z "${2:-}" || "${2:-}" == -* ]]; then
                echo "Error: --tenant-namespace requires a non-empty value"
                exit 1
            fi
            TENANT_NAMESPACE="$2"
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

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
OBSERVABILITY_DIR="$PROJECT_ROOT/deployment/components/observability"

# ==========================================
# Preflight: Cluster Observability Operator & Perses CRDs
# ==========================================
echo "📊 MaaS Perses Dashboard Installation"
echo ""

MISSING_CRDS=()
for crd in persesdashboards.perses.dev persesdatasources.perses.dev; do
    if ! kubectl get crd "$crd" &>/dev/null; then
        MISSING_CRDS+=("$crd")
    fi
done
if [ ${#MISSING_CRDS[@]} -gt 0 ]; then
    echo "⚠️  Required Perses CRDs not found: ${MISSING_CRDS[*]}"
    echo "   Install the Cluster Observability Operator first."
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
    PERSES_PODS=$(kubectl get pods -n openshift-operators -l app.kubernetes.io/name=perses --field-selector=status.phase=Running --no-headers 2>/dev/null | wc -l | tr -d ' ')
    if [ "$PERSES_PODS" -ge 1 ]; then
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
# Step 3: Deploy admin dashboards and datasource (openshift-operators)
# ==========================================
echo ""
echo "📊 Deploying admin dashboards to openshift-operators..."
kubectl apply --server-side=true --force-conflicts -f "$OBSERVABILITY_DIR/perses/dashboards/dashboard-ai-engineer.yaml" -n openshift-operators
kubectl apply --server-side=true --force-conflicts -f "$OBSERVABILITY_DIR/perses/dashboards/dashboard-platform-admin.yaml" -n openshift-operators
echo "   ✅ Admin dashboards deployed (Platform Admin, AI Engineer)"

echo ""
echo "🔗 Configuring admin Prometheus datasource (port 9091, cluster-wide)..."
if DATASOURCE_OUT=$(kubectl apply --server-side=true --force-conflicts -f "$OBSERVABILITY_DIR/perses/perses-datasource.yaml" -n openshift-operators 2>&1); then
    echo "   ✅ Admin datasource configured"
else
    echo "   ❌ Failed to configure admin datasource:"
    echo "   $DATASOURCE_OUT"
    exit 1
fi

# ==========================================
# Step 3b: Deploy Loki datasource and RBAC (for structured logs)
# ==========================================
echo ""
echo "📝 Configuring Loki datasource (structured logs via LokiStack Gateway)..."
kubectl apply --server-side=true --force-conflicts -f "$OBSERVABILITY_DIR/perses/perses-loki-ca.yaml" -n openshift-operators
if LOKI_OUT=$(kubectl apply --server-side=true --force-conflicts -f "$OBSERVABILITY_DIR/perses/perses-loki-datasource.yaml" -n openshift-operators 2>&1); then
    echo "   ✅ Loki datasource configured"
else
    echo "   ⚠️  Loki datasource failed (non-blocking): $LOKI_OUT"
fi
kubectl apply --server-side=true --force-conflicts -f "$OBSERVABILITY_DIR/perses/perses-loki-rbac.yaml"
echo "   ✅ Loki RBAC configured (cluster-logging-application-view)"

# ==========================================
# Step 4: Deploy tenant-scoped Usage Dashboard
# ==========================================
echo ""
echo "🔒 Deploying tenant-scoped Usage Dashboard to ${TENANT_NAMESPACE}..."

if ! kubectl get namespace "$TENANT_NAMESPACE" &>/dev/null; then
    echo "   ⚠️  Namespace ${TENANT_NAMESPACE} does not exist - skipping tenant dashboard"
    echo "   Create the namespace first, then re-run this script."
else
    # TLS CA ConfigMaps (OpenShift service CA operator injects the bundles)
    kubectl apply --server-side=true --force-conflicts -f "$OBSERVABILITY_DIR/perses/perses-thanos-ca.yaml" -n "$TENANT_NAMESPACE"
    kubectl apply --server-side=true --force-conflicts -f "$OBSERVABILITY_DIR/perses/perses-loki-ca.yaml" -n "$TENANT_NAMESPACE"

    # Tenant Prometheus datasource (port 9092, namespace-scoped)
    sed "s/TENANT_NAMESPACE/${TENANT_NAMESPACE}/g" \
        "$OBSERVABILITY_DIR/perses/perses-datasource-scoped.yaml" \
        | kubectl apply --server-side=true --force-conflicts -n "$TENANT_NAMESPACE" -f -

    # Tenant Loki datasource (user-scoped, routes through loki-query-proxy-user)
    # Replace loki-stack-namespace placeholder with the namespace where the proxy runs
    LOKI_STACK_NAMESPACE="${LOKI_STACK_NAMESPACE:-openshift-logging}"
    sed "s/loki-stack-namespace/${LOKI_STACK_NAMESPACE}/g" \
        "$OBSERVABILITY_DIR/perses/perses-loki-datasource-scoped.yaml" \
        | kubectl apply --server-side=true --force-conflicts -n "$TENANT_NAMESPACE" -f -

    # RBAC: adds metrics.k8s.io/pods 'create' verb for authenticated users
    kubectl apply --server-side=true --force-conflicts -f "$OBSERVABILITY_DIR/perses/perses-rbac-scoped.yaml" -n "$TENANT_NAMESPACE"

    # Usage Dashboard only (not platform-admin or ai-engineer)
    kubectl apply --server-side=true --force-conflicts -f "$OBSERVABILITY_DIR/perses/dashboards/dashboard-usage.yaml" -n "$TENANT_NAMESPACE"

    # --- Inline validation of deployed Usage Dashboard ---
    echo ""
    echo "🔎 Validating Usage dashboard on cluster..."
    VALIDATION_FAILED=0
    _check() { if [ "$1" = "ok" ]; then echo "   ✅ $2"; else echo "   ❌ $2"; VALIDATION_FAILED=1; fi; }

    DASH_YAML="$(kubectl get persesdashboard usage-dashboard -n "$TENANT_NAMESPACE" -o yaml 2>/dev/null || true)"
    if [ -z "$DASH_YAML" ]; then
        echo "   ❌ Could not fetch persesdashboard/usage-dashboard from ${TENANT_NAMESPACE}"
        exit 1
    fi

    EXPECTED_REVISION="2026-05-17-table-both-cols-filled"
    ACTUAL_REV="$(printf '%s' "$DASH_YAML" | grep 'maas.observability/visual-revision' | head -1 | sed 's/.*: *//' | tr -d '"')"
    if [ "$ACTUAL_REV" = "$EXPECTED_REVISION" ]; then
        _check ok "visual-revision: $ACTUAL_REV"
    else
        _check fail "visual-revision: expected $EXPECTED_REVISION, got '$ACTUAL_REV'"
    fi

    if printf '%s' "$DASH_YAML" | grep -q 'name: view_by'; then
        _check ok "view_by variable present"
    else
        _check fail "view_by variable missing"
    fi

    if printf '%s' "$DASH_YAML" | grep -q 'tokenConsumptionOverTime'; then
        _check ok "tokenConsumptionOverTime panel present"
    else
        _check fail "tokenConsumptionOverTime panel missing"
    fi

    if printf '%s' "$DASH_YAML" | grep -q 'tokenConsumptionTable'; then
        _check ok "tokenConsumptionTable panel present"
    else
        _check fail "tokenConsumptionTable panel missing"
    fi

    if printf '%s' "$DASH_YAML" | grep -q 'tokenConsumptionByUser'; then
        _check fail "deprecated tokenConsumptionByUser panel still present"
    else
        _check ok "deprecated tokenConsumptionByUser panel removed"
    fi

    if printf '%s' "$DASH_YAML" | grep -q 'sum by (${view_by:raw}) (sum_over_time'; then
        _check ok "chart query uses sum by (\${view_by:raw})"
    else
        _check fail "chart query missing sum by (\${view_by:raw})"
    fi

    TABLE_YAML="$(printf '%s' "$DASH_YAML" | sed -n '/tokenConsumptionTable/,/layouts:/p')"
    if printf '%s' "$TABLE_YAML" | grep -q 'sum by (model, subscription) (sum_over_time'; then
        _check ok "table tokens query uses sum by (model, subscription)"
    else
        _check fail "table tokens query missing sum by (model, subscription)"
    fi
    if printf '%s' "$TABLE_YAML" | grep -q 'sum by (model, subscription) (count_over_time'; then
        _check ok "table requests query uses sum by (model, subscription)"
    else
        _check fail "table requests query missing sum by (model, subscription)"
    fi
    if printf '%s' "$TABLE_YAML" | grep -q 'name: model' && printf '%s' "$TABLE_YAML" | grep -q 'name: subscription'; then
        _check ok "table columnSettings has model and subscription"
    else
        _check fail "table columnSettings missing model/subscription"
    fi

    if [ "$VALIDATION_FAILED" -ne 0 ]; then
        echo "   ❌ Usage dashboard validation failed"
        exit 1
    fi
    echo "   ✅ All Usage dashboard checks passed"

    echo "   ✅ Tenant Usage Dashboard deployed to ${TENANT_NAMESPACE}"
    echo "   ✅ Tenant datasources configured (Prometheus port 9092 + Loki)"
    echo "   ✅ Tenant metrics RBAC configured (metrics.k8s.io/pods create)"
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
echo "📈 Admin Dashboards (openshift-operators, port 9091):"
echo "   - Platform Admin Dashboard"
echo "   - AI Engineer Dashboard"
echo "   - Loki datasource (structured logs, ready for future LogQL panels)"
echo ""
echo "🔒 Tenant Dashboard (${TENANT_NAMESPACE}, port 9092):"
echo "   - Usage Dashboard (namespace-scoped metrics)"
echo "   - metrics.k8s.io RBAC (for Thanos port 9092 POST queries)"
echo "   Users with 'view' on ${TENANT_NAMESPACE} see only this dashboard."
echo ""

exit 0
