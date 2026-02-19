#!/bin/bash

# MaaS Grafana Dashboard Installation (helper)
# Discovers Grafana instance(s) cluster-wide and deploys MaaS dashboard definitions.
# Does not install Grafana; assumes Grafana Operator and at least one Grafana instance exist.
# Never fails: missing Grafana or multiple instances result in warnings only.
#
# Usage: ./install-grafana-dashboards.sh [--grafana-namespace NS] [--grafana-label KEY=VALUE]

set -e
set -o pipefail

for cmd in kubectl kustomize; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "❌ Required command '$cmd' not found. Please install it first."
        exit 1
    fi
done

GRAFANA_NAMESPACE=""
GRAFANA_LABEL=""

show_help() {
    echo "Usage: $0 [--grafana-namespace NS] [--grafana-label KEY=VALUE]"
    echo ""
    echo "Discovers Grafana instance(s) and deploys MaaS dashboard definitions (GrafanaDashboard CRs)."
    echo "Does not install Grafana. Never exits with error for discovery; warnings only."
    echo ""
    echo "Options:"
    echo "  --grafana-namespace   Limit discovery to this namespace (optional)"
    echo "  --grafana-label      Only consider Grafana CRs with this label (e.g. app=grafana)"
    echo ""
    echo "Behavior:"
    echo "  - 0 instances found: warning, no deploy"
    echo "  - 1 instance found: deploy dashboards to that namespace, success message"
    echo "  - 2+ instances found: warning listing them; use --grafana-namespace or --grafana-label to pick one"
    echo ""
    echo "Examples:"
    echo "  $0"
    echo "  $0 --grafana-namespace maas-api"
    echo "  $0 --grafana-label app=grafana"
    echo ""
    exit 0
}

while [[ $# -gt 0 ]]; do
    case $1 in
        --grafana-namespace)
            if [[ -z "$2" || "$2" == -* ]]; then
                echo "Error: --grafana-namespace requires a value"
                exit 1
            fi
            GRAFANA_NAMESPACE="$2"
            shift 2
            ;;
        --grafana-label)
            if [[ -z "$2" || "$2" == -* ]]; then
                echo "Error: --grafana-label requires a value (e.g. app=grafana)"
                exit 1
            fi
            GRAFANA_LABEL="$2"
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
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
OBSERVABILITY_DIR="$PROJECT_ROOT/deployment/components/observability"

# ==========================================
# Preflight: Grafana CRD
# ==========================================
if ! kubectl get crd grafanas.grafana.integreatly.org &>/dev/null; then
    echo "⚠️  Grafana Operator CRD not found. Install the Grafana Operator first."
    echo "   See: https://grafana.github.io/grafana-operator/docs/installation/"
    exit 0
fi

# ==========================================
# Cluster-wide discovery
# ==========================================
echo "🔍 Discovering Grafana instance(s)..."

LIST_OPTS="-A --no-headers"
[ -n "$GRAFANA_NAMESPACE" ] && LIST_OPTS="-n $GRAFANA_NAMESPACE --no-headers"
[ -n "$GRAFANA_LABEL" ]    && LIST_OPTS="$LIST_OPTS -l $GRAFANA_LABEL"

GRAFANA_LIST=$(kubectl get grafanas.grafana.integreatly.org $LIST_OPTS 2>/dev/null || true)
if [ -z "$GRAFANA_LIST" ]; then
    GRAFANA_COUNT=0
else
    GRAFANA_COUNT=$(echo "$GRAFANA_LIST" | grep -c . 2>/dev/null || echo "0")
fi

if [ "$GRAFANA_COUNT" -eq 0 ] 2>/dev/null; then
    echo "⚠️  No Grafana instance found."
    [ -n "$GRAFANA_NAMESPACE" ] && echo "   (Searched in namespace: $GRAFANA_NAMESPACE)"
    [ -n "$GRAFANA_LABEL" ]    && echo "   (With label: $GRAFANA_LABEL)"
    echo "   Install Grafana (e.g. via Grafana Operator), then re-run this script."
    echo "   See: https://grafana.github.io/grafana-operator/docs/installation/"
    exit 0
fi

if [ "$GRAFANA_COUNT" -gt 1 ] 2>/dev/null; then
    echo "⚠️  Multiple Grafana instances found ($GRAFANA_COUNT). Specify which one to use:"
    if [ -n "$GRAFANA_NAMESPACE" ]; then
        echo "$GRAFANA_LIST" | while read -r line; do
            name=$(echo "$line" | awk '{print $1}')
            echo "   - namespace: $GRAFANA_NAMESPACE, name: $name"
        done
    else
        echo "$GRAFANA_LIST" | while read -r line; do
            ns=$(echo "$line" | awk '{print $1}')
            name=$(echo "$line" | awk '{print $2}')
            echo "   - namespace: $ns, name: $name"
        done
    fi
    echo "   Use: $0 --grafana-namespace <namespace>   or   --grafana-label <key=value>"
    exit 0
fi

# Exactly one Grafana: resolve target namespace and resource name
# With -n NS, kubectl output has only one column (name); with -A, output is namespace then name
if [ -n "$GRAFANA_NAMESPACE" ]; then
    TARGET_NS="$GRAFANA_NAMESPACE"
    GRAFANA_NAME=$(echo "$GRAFANA_LIST" | awk '{print $1}')
else
    TARGET_NS=$(echo "$GRAFANA_LIST" | awk '{print $1}')
    GRAFANA_NAME=$(echo "$GRAFANA_LIST" | awk '{print $2}')
fi
echo "   ✅ One Grafana instance found: $GRAFANA_NAME in namespace $TARGET_NS"

# ==========================================
# Deploy dashboards
# ==========================================
echo ""
echo "📊 Deploying MaaS dashboard definitions to namespace $TARGET_NS..."
kustomize build "$OBSERVABILITY_DIR/grafana/dashboards" | \
    sed "s/namespace: maas-api/namespace: $TARGET_NS/g" | \
    kubectl apply -f -

echo "   ✅ Dashboards applied (Platform Admin, AI Engineer)."
echo "   Ensure your Grafana instance has label app=grafana so these dashboards attach."
echo "   Configure Prometheus/Thanos datasource in Grafana if not already done."
echo ""

# Optional: show route if present
GRAFANA_ROUTE=$(kubectl get route -n "$TARGET_NS" -l app=grafana -o jsonpath='{.items[0].spec.host}' 2>/dev/null || kubectl get route grafana-route -n "$TARGET_NS" -o jsonpath='{.spec.host}' 2>/dev/null || echo "")
if [ -n "$GRAFANA_ROUTE" ]; then
    echo "   🌐 Grafana URL: https://$GRAFANA_ROUTE"
fi

exit 0
