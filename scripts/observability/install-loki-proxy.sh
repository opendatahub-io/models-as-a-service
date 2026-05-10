#!/bin/bash

# MaaS Loki Query Proxy Installation
# Deploys a query-rewriting reverse proxy for tenant isolation on Loki.
#
# The proxy enforces per-user log isolation by validating the bearer token
# via the Kubernetes TokenReview API and appending `| user_id="<username>"`
# to every LogQL query. Cluster admins (system:cluster-admins, system:masters)
# bypass filtering and see all data.
#
# Uses stock golang:1.18-ubi9 image from the internal registry.
# Go source is compiled at runtime via `go run` from a ConfigMap.
#
# This script is idempotent - safe to run multiple times.
#
# Usage: ./install-loki-proxy.sh [--namespace NAMESPACE]

set -euo pipefail

if ! command -v oc &>/dev/null; then
    echo "Required command 'oc' not found."
    exit 1
fi

NAMESPACE="${NAMESPACE:-kuadrant-system}"

show_help() {
    echo "Usage: $0 [--namespace NAMESPACE]"
    echo ""
    echo "Deploys the Loki Query Proxy for per-user tenant isolation."
    echo ""
    echo "Options:"
    echo "  --namespace  Namespace to deploy into (default: kuadrant-system)"
    echo ""
    exit 0
}

while [[ $# -gt 0 ]]; do
    case $1 in
        --namespace)
            if [[ $# -lt 2 || -z "${2:-}" || "${2:-}" == -* ]]; then
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

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
LOKI_PROXY_DIR="$PROJECT_ROOT/deployment/components/observability/loki-proxy"

# ==========================================
# Preflight checks
# ==========================================
echo "Loki Query Proxy Installation"
echo ""

if ! oc get namespace "$NAMESPACE" &>/dev/null; then
    echo "Namespace ${NAMESPACE} does not exist."
    exit 1
fi
echo "Namespace ${NAMESPACE} exists"

if ! oc get is golang -n openshift &>/dev/null; then
    echo "golang ImageStream not found in openshift namespace."
    echo "This cluster does not have the golang image stream configured."
    exit 1
fi
echo "golang ImageStream available"

if ! oc get svc maas-loki-gateway-http -n openshift-logging &>/dev/null; then
    echo "LokiStack gateway service not found in openshift-logging."
    echo "Install LokiStack first."
    exit 1
fi
echo "LokiStack gateway service found"

# ==========================================
# Step 1: Deploy RBAC (ServiceAccount + ClusterRoleBinding)
# ==========================================
echo ""
echo "Deploying RBAC..."
oc apply -f "$LOKI_PROXY_DIR/rbac.yaml" -n "$NAMESPACE"
echo "  RBAC deployed (SA + logging-view)"

# ==========================================
# Step 2: Deploy Go source ConfigMap
# ==========================================
echo ""
echo "Deploying proxy source ConfigMap..."
oc apply -f "$LOKI_PROXY_DIR/proxy-source-configmap.yaml" -n "$NAMESPACE"
echo "  ConfigMap deployed"

# ==========================================
# Step 3: Deploy Services
# ==========================================
echo ""
echo "Deploying services..."
oc apply -f "$LOKI_PROXY_DIR/service.yaml" -n "$NAMESPACE"
echo "  Services deployed"

# ==========================================
# Step 4: Deploy Proxy Pod
# ==========================================
echo ""
echo "Deploying proxy pod..."
oc apply -f "$LOKI_PROXY_DIR/deployment-user.yaml" -n "$NAMESPACE"
echo "  Deployment applied"

# ==========================================
# Step 5: Wait for pod to be ready
# ==========================================
echo ""
echo "Waiting for proxy pod to start (go run compile takes ~5-10s)..."

for deploy in loki-query-proxy-user; do
    echo "  Waiting for $deploy..."
    if oc rollout status deployment/"$deploy" -n "$NAMESPACE" --timeout=120s 2>&1; then
        echo "  $deploy is ready"
    else
        echo "  $deploy failed to start within 120s"
        echo ""
        echo "Pod logs:"
        oc logs deployment/"$deploy" -n "$NAMESPACE" --tail=50 2>&1 || true
        echo ""
        echo "Pod status:"
        oc get pods -l app=loki-query-proxy -n "$NAMESPACE" -o wide 2>&1 || true
        exit 1
    fi
done

# ==========================================
# Step 6: Deploy NetworkPolicy (optional, in openshift-logging)
# ==========================================
echo ""
echo "Deploying NetworkPolicy (restricts direct Loki access)..."
if oc apply -f "$LOKI_PROXY_DIR/networkpolicy.yaml" 2>&1; then
    echo "  NetworkPolicy deployed in openshift-logging"
else
    echo "  NetworkPolicy deployment failed (non-blocking)"
    echo "  You may need cluster-admin to apply NetworkPolicies in openshift-logging"
fi

# ==========================================
# Summary
# ==========================================
echo ""
echo "========================================="
echo "Loki Query Proxy installed"
echo "========================================="
echo ""
echo "Deployments:"
oc get deployment -l app=loki-query-proxy -n "$NAMESPACE" --no-headers 2>&1
echo ""
echo "Services:"
oc get svc -l app=loki-query-proxy -n "$NAMESPACE" --no-headers 2>&1
echo ""
echo "Endpoint:"
echo "  User proxy:  http://loki-query-proxy-user.${NAMESPACE}.svc:8080"
echo ""
echo "Next step: Update Perses Loki datasource to point through the proxy."
echo ""

exit 0
