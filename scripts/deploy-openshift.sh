#!/bin/bash

# OpenShift MaaS Platform Deployment Script
# This script automates the complete deployment of the MaaS platform on OpenShift
#
# Usage: ./deploy-openshift.sh [OPTIONS]
#
# Options:
#   --with-observability     Install observability stack (Grafana + dashboards)
#   --skip-observability     Skip observability installation (no prompt)
#   --namespace NAMESPACE    MaaS API namespace (default: maas-api)
#   -h, --help               Show this help message

set -e

# Parse command line arguments
INSTALL_OBSERVABILITY=""  # Empty = prompt, set by flags
MAAS_API_NAMESPACE="${MAAS_API_NAMESPACE:-maas-api}"

show_help() {
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Options:"
    echo "  --with-observability     Install observability stack (Grafana + dashboards)"
    echo "  --skip-observability     Skip observability installation (no prompt)"
    echo "  --namespace NAMESPACE    MaaS API namespace (default: maas-api)"
    echo "  -h, --help               Show this help message"
    echo ""
    echo "Examples:"
    echo "  $0                           # Interactive mode, prompts for observability"
    echo "  $0 --with-observability      # Install with observability"
    echo "  $0 --skip-observability      # Install without observability"
    echo "  $0 --namespace my-namespace  # Use custom namespace"
    echo ""
    exit 0
}

while [[ $# -gt 0 ]]; do
    case $1 in
        --with-observability)
            INSTALL_OBSERVABILITY="y"
            shift
            ;;
        --skip-observability)
            INSTALL_OBSERVABILITY="n"
            shift
            ;;
        --namespace|-n)
            if [[ -z "$2" || "$2" == -* ]]; then
                echo "Error: --namespace requires a non-empty value"
                echo "Use --help for usage information"
                exit 1
            fi
            MAAS_API_NAMESPACE="$2"
            shift 2
            ;;
        -h|--help)
            show_help
            ;;
        *)
            echo "Unknown option: $1"
            echo "Use --help for usage information"
            exit 1
            ;;
    esac
done

export MAAS_API_NAMESPACE

# Script directory and project root
SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Helper function to wait for CRD to be established
wait_for_crd() {
  local crd="$1"
  local timeout="${2:-60}"  # timeout in seconds
  local interval=2
  local elapsed=0

  echo "⏳ Waiting for CRD ${crd} to appear (timeout: ${timeout}s)…"
  while [ $elapsed -lt $timeout ]; do
    if kubectl get crd "$crd" &>/dev/null; then
      echo "✅ CRD ${crd} detected, waiting for it to become Established..."
      kubectl wait --for=condition=Established --timeout="${timeout}s" "crd/$crd" 2>/dev/null
      return 0
    fi
    sleep $interval
    elapsed=$((elapsed + interval))
  done

  echo "❌ Timed out after ${timeout}s waiting for CRD $crd to appear." >&2
  return 1
}

# Helper function to wait for CSV to reach Succeeded state
# Supports both exact CSV names (e.g., "kuadrant-operator.v1.3.0") and partial names (e.g., "rhcl-operator")
wait_for_csv() {
  local csv_pattern="$1"
  local namespace="${2:-kuadrant-system}"
  local timeout="${3:-180}"  # timeout in seconds
  local interval=5
  local elapsed=0
  local last_status_print=0

  echo "⏳ Waiting for CSV matching '${csv_pattern}' to succeed (timeout: ${timeout}s)..."
  while [ $elapsed -lt $timeout ]; do
    # Find CSV matching the pattern (supports partial names like "rhcl-operator" or exact like "kuadrant-operator.v1.3.0")
    local csv_name=$(kubectl get csv -n "$namespace" --no-headers 2>/dev/null | grep "$csv_pattern" | awk '{print $1}' | head -1)
    
    if [ -z "$csv_name" ]; then
      if [ $((elapsed - last_status_print)) -ge 30 ]; then
        echo "   CSV matching '${csv_pattern}' not found yet (${elapsed}s elapsed)"
        last_status_print=$elapsed
      fi
      sleep $interval
      elapsed=$((elapsed + interval))
      continue
    fi

    local phase=$(kubectl get csv -n "$namespace" "$csv_name" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")

    case "$phase" in
      "Succeeded")
        echo "✅ CSV ${csv_name} succeeded"
        return 0
        ;;
      "Failed")
        echo "❌ CSV ${csv_name} failed" >&2
        kubectl get csv -n "$namespace" "$csv_name" -o jsonpath='{.status.message}' 2>/dev/null
        return 1
        ;;
      *)
        if [ $((elapsed - last_status_print)) -ge 30 ]; then
          echo "   CSV ${csv_name} status: ${phase} (${elapsed}s elapsed)"
          last_status_print=$elapsed
        fi
        ;;
    esac

    sleep $interval
    elapsed=$((elapsed + interval))
  done

  echo "❌ Timed out after ${timeout}s waiting for CSV matching '${csv_pattern}'" >&2
  return 1
}

# Helper function to wait for pods in a namespace to be ready
wait_for_pods() {
  local namespace="$1"
  local timeout="${2:-120}"
  
  kubectl get namespace "$namespace" &>/dev/null || return 0
  
  echo "⏳ Waiting for pods in $namespace to be ready..."
  local end=$((SECONDS + timeout))
  local pods_found=false
  
  while [ $SECONDS -lt $end ]; do
    local total_pods=$(kubectl get pods -n "$namespace" --no-headers 2>/dev/null | wc -l)
    
    # If no pods exist yet, wait for them to appear
    if [ "$total_pods" -eq 0 ]; then
      if [ "$pods_found" = "false" ]; then
        echo "   Waiting for pods to be created..."
      fi
      sleep 5
      continue
    fi
    
    pods_found=true
    local not_ready=$(kubectl get pods -n "$namespace" --no-headers 2>/dev/null | grep -v -E 'Running|Completed|Succeeded' | wc -l)
    [ "$not_ready" -eq 0 ] && return 0
    sleep 5
  done
  
  if [ "$pods_found" = "false" ]; then
    echo "⚠️  No pods found in $namespace within timeout" >&2
  else
    echo "⚠️  Timeout waiting for pods in $namespace to be ready" >&2
  fi
  return 1
}

# version_compare <version1> <version2>
#   Compares two version strings in semantic version format (e.g., "4.19.9")
#   Returns 0 if version1 >= version2, 1 otherwise
version_compare() {
  local version1="$1"
  local version2="$2"
  
  local v1=$(echo "$version1" | awk -F. '{printf "%d%03d%03d", $1, $2, $3}')
  local v2=$(echo "$version2" | awk -F. '{printf "%d%03d%03d", $1, $2, $3}')
  
  [ "$v1" -ge "$v2" ]
}

wait_for_validating_webhooks() {
    local namespace="$1"
    local timeout="${2:-60}"
    local interval=2
    local end=$((SECONDS+timeout))

    echo "⏳ Waiting for validating webhooks in namespace $namespace (timeout: $timeout sec)..."

    while [ $SECONDS -lt $end ]; do
        local not_ready=0

        local services
        services=$(kubectl get validatingwebhookconfigurations \
          -o jsonpath='{range .items[*].webhooks[*].clientConfig.service}{.namespace}/{.name}{"\n"}{end}' \
          | grep "^$namespace/" | sort -u)

        if [ -z "$services" ]; then
            echo "⚠️  No validating webhooks found in namespace $namespace"
            return 0
        fi

        for svc in $services; do
            local ns name ready
            ns=$(echo "$svc" | cut -d/ -f1)
            name=$(echo "$svc" | cut -d/ -f2)

            ready=$(kubectl get endpoints -n "$ns" "$name" -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null || true)
            if [ -z "$ready" ]; then
                echo "🔴 Webhook service $ns/$name not ready"
                not_ready=1
            else
                echo "✅ Webhook service $ns/$name has ready endpoints"
            fi
        done

        if [ "$not_ready" -eq 0 ]; then
            echo "🎉 All validating webhook services in $namespace are ready"
            return 0
        fi

        sleep $interval
    done

    echo "❌ Timed out waiting for validating webhooks in $namespace"
    return 1
}

echo "========================================="
echo "🚀 MaaS Platform OpenShift Deployment"
echo "========================================="
echo ""

# Check if running on OpenShift
if ! kubectl api-resources | grep -q "route.openshift.io"; then
    echo "❌ This script is for OpenShift clusters only."
    exit 1
fi

# Check prerequisites
echo "📋 Checking prerequisites..."
echo ""
echo "Required tools:"
echo "  - oc: $(oc version --client 2>/dev/null | head -n1 || echo 'not found')"
echo "  - jq: $(jq --version 2>/dev/null || echo 'not found')"
echo "  - yq: $(yq --version 2>/dev/null | head -n1 || echo 'not found')"
echo "  - kustomize: $(kustomize version --short 2>/dev/null || echo 'not found')"
echo "  - git: $(git --version 2>/dev/null || echo 'not found')"
echo ""
echo "ℹ️  Note: OpenShift Service Mesh should be automatically installed when GatewayClass is created."
echo "   If the Gateway gets stuck in 'Waiting for controller', you may need to manually"
echo "   install the Red Hat OpenShift Service Mesh operator from OperatorHub."

echo ""
echo "1️⃣ Checking OpenShift version and Gateway API requirements..."

# Get OpenShift version
OCP_VERSION=$(oc get clusterversion version -o jsonpath='{.status.desired.version}' 2>/dev/null || echo "unknown")
echo "   OpenShift version: $OCP_VERSION"

# Check if version is 4.19.9 or higher
if [[ "$OCP_VERSION" == "unknown" ]]; then
    echo "   ⚠️  Could not determine OpenShift version, applying feature gates to be safe"
    oc patch featuregate/cluster --type='merge' \
      -p '{"spec":{"featureSet":"CustomNoUpgrade","customNoUpgrade":{"enabled":["GatewayAPI","GatewayAPIController"]}}}' || true
    echo "   Waiting for feature gates to reconcile (30 seconds)..."
    sleep 30
elif version_compare "$OCP_VERSION" "4.19.9"; then
    echo "   ✅ OpenShift $OCP_VERSION supports Gateway API via GatewayClass (no feature gates needed)"
else
    echo "   Applying Gateway API feature gates for OpenShift < 4.19.9"
    oc patch featuregate/cluster --type='merge' \
      -p '{"spec":{"featureSet":"CustomNoUpgrade","customNoUpgrade":{"enabled":["GatewayAPI","GatewayAPIController"]}}}' || true
    echo "   Waiting for feature gates to reconcile (30 seconds)..."
    sleep 30
fi

echo ""
echo "2️⃣ Creating namespaces..."
echo "   ℹ️  Note: If ODH/RHOAI is already installed, some namespaces may already exist"

# MAAS_API_NAMESPACE is set at top of script via --namespace flag or env var
echo "   MaaS API namespace: $MAAS_API_NAMESPACE (use --namespace to override)"

for ns in opendatahub kserve kuadrant-system llm "$MAAS_API_NAMESPACE"; do
    kubectl create namespace $ns 2>/dev/null || echo "   Namespace $ns already exists"
done

echo ""
echo "3️⃣ Installing dependencies..."

# Only clean up leftover CRDs if Kuadrant/RHCL operators are NOT already installed
echo "   Checking for existing Kuadrant/RHCL installation..."
KUADRANT_INSTALLED=false

# Check for RHCL (downstream) OR upstream Kuadrant
if kubectl get csv -n kuadrant-system 2>/dev/null | grep -qE "rhcl-operator|kuadrant-operator"; then
    KUADRANT_INSTALLED=true
    echo "   ✅ Kuadrant/RHCL operator already installed, skipping CRD cleanup"
fi

# Also check if CRDs exist (even without CSV - means something is installed)
if kubectl get crd kuadrants.kuadrant.io &>/dev/null 2>&1; then
    KUADRANT_INSTALLED=true
    echo "   ✅ Kuadrant CRDs exist, skipping CRD cleanup"
fi

# Also check if operator pods are running
if kubectl get pods -n kuadrant-system --no-headers 2>/dev/null | grep -qE "kuadrant-operator.*Running"; then
    KUADRANT_INSTALLED=true
    echo "   ✅ Kuadrant operator pods running, skipping CRD cleanup"
fi

if [ "$KUADRANT_INSTALLED" = "false" ]; then
    echo "   No existing Kuadrant/RHCL installation detected"
    # Note: We no longer automatically delete CRDs - this was too dangerous!
    # If there are orphaned CRDs, the operator installation will handle them
fi

echo "   Installing Kuadrant..."
"$SCRIPT_DIR/install-dependencies.sh" --kuadrant

# Install cert-manager if not present (required for model TLS certificates)
echo ""
echo "   Checking for cert-manager..."
if kubectl get crd clusterissuers.cert-manager.io &>/dev/null; then
    echo "   ✅ cert-manager CRDs already present"
elif kubectl get subscription openshift-cert-manager-operator -n cert-manager-operator &>/dev/null; then
    echo "   ✅ cert-manager subscription exists, waiting for CRDs..."
    for i in $(seq 1 60); do
        if kubectl get crd clusterissuers.cert-manager.io &>/dev/null; then
            echo "   ✅ cert-manager CRDs available"
            break
        fi
        [ $i -eq 60 ] && echo "   ⚠️  cert-manager CRDs not yet available"
        sleep 5
    done
else
    echo "   Installing cert-manager operator..."
    # Create namespace first
    kubectl create namespace cert-manager-operator 2>/dev/null || true
    # Create OperatorGroup
    kubectl apply -f - <<EOF
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: cert-manager-operator-group
  namespace: cert-manager-operator
spec:
  targetNamespaces:
  - cert-manager-operator
EOF
    # Create Subscription
    kubectl apply -f - <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: openshift-cert-manager-operator
  namespace: cert-manager-operator
spec:
  channel: stable-v1
  installPlanApproval: Automatic
  name: openshift-cert-manager-operator
  source: redhat-operators
  sourceNamespace: openshift-marketplace
EOF
    echo "   ⏳ Waiting for cert-manager CRDs to be available..."
    for i in $(seq 1 60); do
        if kubectl get crd clusterissuers.cert-manager.io &>/dev/null; then
            echo "   ✅ cert-manager installed"
            break
        fi
        [ $i -eq 60 ] && echo "   ⚠️  cert-manager CRDs not yet available, ClusterIssuer may need to be created later"
        sleep 5
    done
fi

echo ""
echo "4️⃣ Checking for OpenDataHub/RHOAI KServe..."
if kubectl get crd llminferenceservices.serving.kserve.io &>/dev/null 2>&1; then
    echo "   ✅ KServe CRDs already present (ODH/RHOAI detected)"
else
    echo "   ⚠️  KServe not detected. Deploying ODH KServe components..."
    "$SCRIPT_DIR/install-dependencies.sh" --ocp --odh
fi

# Patch odh-model-controller deployment to set MAAS_NAMESPACE
# This should be done whether ODH was just installed or was already present
echo ""
echo "   Setting MAAS_NAMESPACE for odh-model-controller deployment..."
if kubectl get deployment odh-model-controller -n opendatahub &>/dev/null; then
    # Wait for deployment to be available before patching
    echo "   Waiting for odh-model-controller deployment to be ready..."
    kubectl wait deployment/odh-model-controller -n opendatahub --for=condition=Available=True --timeout=60s 2>/dev/null || \
        echo "   ⚠️  Deployment may still be starting, proceeding with patch..."
    
    # Check if the environment variable already exists
    EXISTING_ENV=$(kubectl get deployment odh-model-controller -n opendatahub -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="MAAS_NAMESPACE")].value}' 2>/dev/null || echo "")
    
    if [ -n "$EXISTING_ENV" ]; then
        if [ "$EXISTING_ENV" = "$MAAS_API_NAMESPACE" ]; then
            echo "   ✅ MAAS_NAMESPACE already set to $MAAS_API_NAMESPACE"
        else
            echo "   Updating MAAS_NAMESPACE from '$EXISTING_ENV' to '$MAAS_API_NAMESPACE'..."
            kubectl set env deployment/odh-model-controller -n opendatahub MAAS_NAMESPACE="$MAAS_API_NAMESPACE"
        fi
    else
        echo "   Adding MAAS_NAMESPACE=$MAAS_API_NAMESPACE..."
        kubectl set env deployment/odh-model-controller -n opendatahub MAAS_NAMESPACE="$MAAS_API_NAMESPACE"
    fi
    
    # Wait for deployment to roll out
    echo "   Waiting for deployment to update..."
    kubectl rollout status deployment/odh-model-controller -n opendatahub --timeout=120s 2>/dev/null || \
        echo "   ⚠️  Deployment update taking longer than expected, continuing..."
    echo "   ✅ odh-model-controller deployment patched"
else
    echo "   ℹ️  odh-model-controller deployment not found in opendatahub namespace"
    echo "      (This is expected if ODH operator hasn't created it yet - it will be patched automatically when created)"
fi

# Patch GatewayConfig to use LoadBalancer instead of OcpRoute (default mode)
# Note: This patch may generate a warning if ingressMode field is not supported in the CRD version
echo ""
echo "   Patching GatewayConfig to use LoadBalancer ingress mode..."
if kubectl get gatewayconfig.services.platform.opendatahub.io default-gateway &>/dev/null; then
    # Suppress stderr warnings about unknown fields (field may not exist in all CRD versions)
    PATCH_OUTPUT=$(kubectl patch gatewayconfig.services.platform.opendatahub.io default-gateway \
      --type='merge' \
      -p '{"spec":{"ingressMode":"LoadBalancer"}}' 2>&1)
    PATCH_EXIT=$?
    
    if [ $PATCH_EXIT -eq 0 ]; then
        # Check if patch resulted in "no change" (already set) or actual change
        if echo "$PATCH_OUTPUT" | grep -q "no change"; then
            echo "   ✅ GatewayConfig already configured for LoadBalancer mode"
        else
            echo "   ✅ GatewayConfig patched to use LoadBalancer mode"
        fi
    else
        # Check if error is about unknown field (non-critical)
        if echo "$PATCH_OUTPUT" | grep -qi "unknown field"; then
            echo "   ℹ️  GatewayConfig ingressMode field not supported in this CRD version (non-critical)"
            echo "      Gateway will use default ingress mode"
        else
            echo "   ⚠️  GatewayConfig patch failed: $(echo "$PATCH_OUTPUT" | head -1)"
        fi
    fi
else
    echo "   ℹ️  GatewayConfig default-gateway not found, skipping patch"
    echo "      (It may be created later by the ODH operator)"
fi

echo ""
echo "5️⃣ Deploying Gateway infrastructure..."
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
if [ -z "$CLUSTER_DOMAIN" ]; then
    echo "❌ Failed to retrieve cluster domain from OpenShift"
    exit 1
fi
export CLUSTER_DOMAIN
echo "   Cluster domain: $CLUSTER_DOMAIN"

# Create TLS certificate for Gateway
echo "   Creating TLS certificate for Gateway..."
if kubectl get secret default-gateway-tls -n openshift-ingress &>/dev/null; then
    echo "   ✅ TLS secret default-gateway-tls already exists"
elif kubectl get secret router-certs-default -n openshift-ingress &>/dev/null; then
    # Copy the existing OpenShift router certificate (works on ROSA and OCP)
    echo "   Copying existing router certificate..."
    # Use kubectl to create a clean copy without server-managed fields
    kubectl get secret router-certs-default -n openshift-ingress -o json | \
      jq 'del(.metadata.resourceVersion, .metadata.uid, .metadata.creationTimestamp, .metadata.ownerReferences, .metadata.managedFields) | .metadata.name = "default-gateway-tls"' | \
      kubectl apply -f - && \
      echo "   ✅ Copied router certificate to default-gateway-tls" || \
      echo "   ⚠️  Failed to copy router certificate, will create self-signed"
fi

# If still no certificate, create a self-signed one
if ! kubectl get secret default-gateway-tls -n openshift-ingress &>/dev/null; then
    echo "   Creating self-signed TLS certificate..."
    openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
      -keyout /tmp/gateway-tls.key \
      -out /tmp/gateway-tls.crt \
      -subj "/CN=maas.${CLUSTER_DOMAIN}" \
      -addext "subjectAltName=DNS:maas.${CLUSTER_DOMAIN}" 2>/dev/null
    
    kubectl create secret tls default-gateway-tls \
      --cert=/tmp/gateway-tls.crt \
      --key=/tmp/gateway-tls.key \
      -n openshift-ingress && \
      echo "   ✅ Created self-signed TLS certificate" || \
      echo "   ⚠️  Failed to create TLS certificate"
    
    rm -f /tmp/gateway-tls.key /tmp/gateway-tls.crt
fi

echo "   Deploying Gateway and GatewayClass..."
cd "$PROJECT_ROOT"
kubectl apply --server-side=true --force-conflicts -f deployment/base/networking/odh/odh-gateway-api.yaml

# Detect which TLS certificate secret exists for the MaaS gateway
# Check default-gateway-tls first (created above), then ODH-managed certificates
CERT_CANDIDATES=("default-gateway-tls" "default-gateway-cert" "data-science-gatewayconfig-tls" "data-science-gateway-service-tls")
CERT_NAME=""
for cert in "${CERT_CANDIDATES[@]}"; do
    if kubectl get secret -n openshift-ingress "$cert" &>/dev/null; then
        CERT_NAME="$cert"
        echo "   ✅ Found TLS certificate secret: $cert"
        break
    fi
done
if [ -z "$CERT_NAME" ]; then
    echo "   ⚠️  No TLS certificate secret found (checked: ${CERT_CANDIDATES[*]})"
    echo "      HTTPS listener will not be configured for MaaS gateway"
fi
export CERT_NAME

if [ -n "$CERT_NAME" ]; then
    kubectl apply --server-side=true --force-conflicts -f <(envsubst '$CLUSTER_DOMAIN $CERT_NAME' < deployment/base/networking/maas/maas-gateway-api.yaml)
else
    # Apply without HTTPS listener if no cert is found
    kubectl apply --server-side=true --force-conflicts -f <(envsubst '$CLUSTER_DOMAIN' < deployment/base/networking/maas/maas-gateway-api.yaml | yq 'del(.spec.listeners[] | select(.name == "https"))' -)
fi

echo ""
echo "6️⃣ Waiting for Kuadrant operators to be installed by OLM..."

# First, check if essential Kuadrant CRDs exist
echo "   Checking for essential Kuadrant CRDs..."
ESSENTIAL_CRDS_MISSING=0
for crd in "kuadrants.kuadrant.io" "authpolicies.kuadrant.io" "ratelimitpolicies.kuadrant.io"; do
    if ! kubectl get crd "$crd" &>/dev/null 2>&1; then
        ESSENTIAL_CRDS_MISSING=1
        break
    fi
done

# Check if operator pods are running
RUNNING_PODS=$(kubectl get pods -n kuadrant-system --no-headers 2>/dev/null | grep -E "kuadrant-operator|authorino-operator|limitador-operator" | grep Running | wc -l)

# Detect broken state: pods running but CRDs missing
if [ "$ESSENTIAL_CRDS_MISSING" -eq 1 ] && [ "$RUNNING_PODS" -ge 1 ]; then
    echo ""
    echo "   ❌ BROKEN STATE DETECTED: Operator pods are running but CRDs are missing!"
    echo "   This typically happens after a failed upgrade or CRD deletion."
    echo ""
    echo "   ⚠️  MANUAL FIX REQUIRED - Run these commands:"
    echo "      oc delete csv --all -n kuadrant-system"
    echo "      oc delete installplan --all -n kuadrant-system"
    echo "      # Wait 60s for OLM to recreate CSVs from existing subscriptions"
    echo "      sleep 60"
    echo "      oc get csv -n kuadrant-system"
    echo ""
    echo "   The script will NOT automatically delete resources to avoid further damage."
    echo "   Please fix manually and re-run this script."
    echo ""
    exit 1
fi

# Check if RHCL (downstream) or upstream Kuadrant is installed
# RHCL uses different CSV names than upstream Kuadrant
echo "   Detecting operator distribution..."
if kubectl get csv -n kuadrant-system 2>/dev/null | grep -q "rhcl-operator"; then
    echo "   ✅ Detected RHCL (Red Hat Connectivity Link) - downstream distribution"
    OPERATOR_TYPE="rhcl"
elif kubectl get csv -n kuadrant-system 2>/dev/null | grep -q "kuadrant-operator"; then
    echo "   ✅ Detected upstream Kuadrant operator"
    OPERATOR_TYPE="upstream"
else
    echo "   ⚠️  No Kuadrant operator CSV found, will check if pods are running"
    OPERATOR_TYPE="unknown"
fi

# Re-check running pods after potential reinstall
RUNNING_PODS=$(kubectl get pods -n kuadrant-system --no-headers 2>/dev/null | grep -E "kuadrant-operator|authorino-operator|limitador-operator" | grep Running | wc -l)

# Also verify CRDs exist now
CRDS_EXIST=$(kubectl get crd kuadrants.kuadrant.io &>/dev/null 2>&1 && echo "yes" || echo "no")

if [ "$RUNNING_PODS" -ge 3 ] && [ "$CRDS_EXIST" = "yes" ]; then
    echo "   ✅ Kuadrant operator pods are running ($RUNNING_PODS pods) and CRDs exist"
    echo "   Skipping CSV wait - operators are healthy"
else
    echo "   Waiting for operator CSVs to succeed..."
    if [ "$OPERATOR_TYPE" = "rhcl" ]; then
        # RHCL CSV names (v1.x versions)
        wait_for_csv "rhcl-operator" "kuadrant-system" 120 || \
            echo "   ⚠️  RHCL operator CSV did not succeed, continuing anyway..."
    elif [ "$OPERATOR_TYPE" = "upstream" ]; then
        # Upstream Kuadrant CSV names
        wait_for_csv "kuadrant-operator" "kuadrant-system" 120 || \
            echo "   ⚠️  Kuadrant operator CSV did not succeed, continuing anyway..."
    else
        # Unknown - wait briefly and continue
        echo "   Waiting 30s for operators to initialize..."
        sleep 30
    fi
fi

# Verify pods are running regardless of CSV status
echo "   Verifying operator pods are running..."
kubectl get pods -n kuadrant-system --no-headers 2>/dev/null | grep Running || \
    echo "   ⚠️  Some operator pods may not be running"

# Verify CRDs are present
echo "   Verifying Kuadrant CRDs are available..."
wait_for_crd "kuadrants.kuadrant.io" 30 || echo "   ⚠️  kuadrants.kuadrant.io CRD not found"
wait_for_crd "authpolicies.kuadrant.io" 10 || echo "   ⚠️  authpolicies.kuadrant.io CRD not found"
wait_for_crd "ratelimitpolicies.kuadrant.io" 10 || echo "   ⚠️  ratelimitpolicies.kuadrant.io CRD not found"
wait_for_crd "tokenratelimitpolicies.kuadrant.io" 10 || echo "   ⚠️  tokenratelimitpolicies.kuadrant.io CRD not found"

echo ""
echo "7️⃣ Deploying Kuadrant configuration (now that CRDs exist)..."
cd "$PROJECT_ROOT"
kubectl apply -f deployment/base/networking/odh/kuadrant.yaml

echo ""
echo "8️⃣ Deploying MaaS API..."
cd "$PROJECT_ROOT"

# Check if maas-api deployment already exists and is healthy
MAAS_API_EXISTS=false
MAAS_API_HEALTHY=false
if kubectl get deployment maas-api -n "$MAAS_API_NAMESPACE" &>/dev/null; then
    MAAS_API_EXISTS=true
    READY_REPLICAS=$(kubectl get deployment maas-api -n "$MAAS_API_NAMESPACE" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
    if [ "$READY_REPLICAS" -ge 1 ] 2>/dev/null; then
        MAAS_API_HEALTHY=true
    fi
fi

if [ "$MAAS_API_HEALTHY" = "true" ]; then
    echo ""
    echo "   ⚠️  ============================================================"
    echo "   ⚠️  WARNING: MaaS API deployment already exists and is HEALTHY!"
    echo "   ⚠️  SKIPPING deployment to avoid overwriting working configuration."
    echo "   ⚠️  ============================================================"
    echo ""
    echo "   Current image: $(kubectl get deployment maas-api -n "$MAAS_API_NAMESPACE" -o jsonpath='{.spec.template.spec.containers[0].image}')"
    echo "   Ready replicas: $READY_REPLICAS"
    echo ""
    echo "   To force redeploy, first delete the deployment:"
    echo "      kubectl delete deployment maas-api -n $MAAS_API_NAMESPACE"
    echo "      Then re-run this script."
    echo ""
else
    # Detect the cluster's OIDC audience for AuthPolicy
    echo "   Detecting cluster OIDC audience..."
    MAAS_AUTH_AUDIENCE="$(kubectl create token default --duration=10m 2>/dev/null | cut -d. -f2 | base64 -d 2>/dev/null | jq -r '.aud[0]' 2>/dev/null)"
    if [ -z "$MAAS_AUTH_AUDIENCE" ] || [ "$MAAS_AUTH_AUDIENCE" = "null" ]; then
        MAAS_AUTH_AUDIENCE="https://kubernetes.default.svc"
        echo "   ⚠️  Could not detect audience, using default: $MAAS_AUTH_AUDIENCE"
    else
        echo "   ✅ Detected audience: $MAAS_AUTH_AUDIENCE"
    fi
    export MAAS_AUTH_AUDIENCE

    # Process kustomization.yaml to replace hardcoded namespace, then build
    TMP_DIR="$(mktemp -d)"
    trap 'rm -rf "$TMP_DIR"' EXIT

    cp -r "$PROJECT_ROOT/deployment/base/maas-api/." "$TMP_DIR"

    (
      cd "$TMP_DIR"
      kustomize edit set namespace "$MAAS_API_NAMESPACE"
    )

    # Build kustomize output, substitute OIDC audience
    MAAS_API_MANIFEST=$(kustomize build "$TMP_DIR" | envsubst '$MAAS_AUTH_AUDIENCE')

    # Clear trap now that we're done with TMP_DIR
    trap - EXIT
    rm -rf "$TMP_DIR"

    # Handle immutable selector error by deleting deployment first if needed
    kubectl apply -f - <<< "$MAAS_API_MANIFEST" 2>&1 | tee /tmp/maas-api-apply.log
    APPLY_EXIT=${PIPESTATUS[0]}
    if [ $APPLY_EXIT -ne 0 ] || grep -q "field is immutable" /tmp/maas-api-apply.log 2>/dev/null; then
        if grep -q "field is immutable" /tmp/maas-api-apply.log 2>/dev/null; then
            echo "   ⚠️  Deployment selector changed, recreating maas-api deployment..."
            kubectl delete deployment maas-api -n "$MAAS_API_NAMESPACE" --ignore-not-found=true
            echo "$MAAS_API_MANIFEST" | kubectl apply -f -
        fi
    fi
    rm -f /tmp/maas-api-apply.log 2>/dev/null
fi

# Restart Kuadrant operator to pick up the new configuration
echo "   Restarting Kuadrant operator to apply Gateway API provider recognition..."
kubectl rollout restart deployment/kuadrant-operator-controller-manager -n kuadrant-system
echo "   Waiting for Kuadrant operator to be ready..."
kubectl rollout status deployment/kuadrant-operator-controller-manager -n kuadrant-system --timeout=60s || \
  echo "   ⚠️  Kuadrant operator taking longer than expected, continuing..."

echo ""
echo "9️⃣ Waiting for Gateway to be ready..."
echo "   Note: This may take a few minutes if Service Mesh is being automatically installed..."

# Wait for Service Mesh CRDs to be established
if kubectl get crd istios.sailoperator.io &>/dev/null 2>&1; then
    echo "   ✅ Service Mesh operator already detected"
else
    echo "   Waiting for automatic Service Mesh installation..."
    if wait_for_crd "istios.sailoperator.io" 300; then
        echo "   ✅ Service Mesh operator installed"
    else
        echo "   ⚠️  Service Mesh CRD not detected within timeout"
        echo "      Gateway may take longer to become ready or require manual Service Mesh installation"
    fi
fi

echo "   Waiting for Gateway to become ready..."
kubectl wait --for=condition=Programmed gateway maas-default-gateway -n openshift-ingress --timeout=300s || \
  echo "   ⚠️  Gateway is taking longer than expected, continuing..."

# Create OpenShift Route to expose Gateway through the default router
# Note: We rely on Route for external access instead of externalIP to avoid conflicts with router
echo "   Creating OpenShift Route to expose Gateway through the default router..."
GATEWAY_SVC="maas-default-gateway-openshift-default"
kubectl apply -f - <<EOF
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: maas-gateway-route
  namespace: openshift-ingress
spec:
  host: maas.${CLUSTER_DOMAIN}
  to:
    kind: Service
    name: ${GATEWAY_SVC}
    weight: 100
  port:
    targetPort: 443
  tls:
    termination: passthrough
EOF
echo "   ✅ OpenShift Route created for Gateway"

# Check Gateway PROGRAMMED status
# Note: Gateway may show PROGRAMMED: False on bare-metal without externalIP, but Route provides external access
# Policies will still work as long as Gateway is Accepted
echo "   Checking Gateway PROGRAMMED status..."
sleep 5
GATEWAY_PROGRAMMED=$(kubectl get gateway maas-default-gateway -n openshift-ingress -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null || echo "False")
GATEWAY_ACCEPTED=$(kubectl get gateway maas-default-gateway -n openshift-ingress -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' 2>/dev/null || echo "False")
if [ "$GATEWAY_PROGRAMMED" = "True" ]; then
    echo "   ✅ Gateway is Programmed"
elif [ "$GATEWAY_ACCEPTED" = "True" ]; then
    echo "   ⚠️  Gateway is Accepted but not Programmed (common on bare-metal without externalIP)"
    echo "   ✅ Route provides external access, policies will still work"
else
    echo "   ⚠️  Gateway is not yet Accepted (status: $GATEWAY_ACCEPTED)"
fi

echo ""
echo "🔟 Applying Gateway Policies (includes RBAC for tier ServiceAccounts)..."
cd "$PROJECT_ROOT"
# Substitute MAAS_API_NAMESPACE in gateway-auth-policy.yaml (for tier lookup URL)
kustomize build deployment/base/policies | envsubst '$MAAS_API_NAMESPACE' | kubectl apply --server-side=true --force-conflicts -f -

# Verify RBAC was applied
if kubectl get clusterrolebinding maas-tier-llm-access &>/dev/null; then
    echo "   ✅ Gateway Policies and RBAC applied"
else
    echo "   ⚠️  RBAC for tier ServiceAccounts may not have been applied"
fi

echo ""
echo "1️⃣1️⃣ Creating ClusterIssuer for cert-manager (for model TLS certificates)..."
if ! kubectl get clusterissuer selfsigned-issuer &>/dev/null; then
    if kubectl get crd clusterissuers.cert-manager.io &>/dev/null; then
        kubectl apply -f - <<EOF
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: selfsigned-issuer
spec:
  selfSigned: {}
EOF
        echo "   ✅ ClusterIssuer created"
    else
        echo "   ⚠️  cert-manager CRDs not found, skipping ClusterIssuer creation"
    fi
else
    echo "   ✅ ClusterIssuer already exists"
fi

echo ""
echo "1️⃣2️⃣ Verifying AuthPolicy audience and authorization verb..."
# Note: Audience is now dynamically set in step 8 via envsubst
# This step verifies and patches as a fallback if needed
AUD="$(kubectl create token default --duration=10m 2>/dev/null | cut -d. -f2 | base64 -d 2>/dev/null | jq -r '.aud[0]' 2>/dev/null)"
CURRENT_AUD=$(kubectl get authpolicy maas-api-auth-policy -n "$MAAS_API_NAMESPACE" -o jsonpath='{.spec.rules.authentication.openshift-identities.kubernetesTokenReview.audiences[0]}' 2>/dev/null || echo "")
if [ -n "$AUD" ] && [ "$AUD" != "null" ]; then
    if [ "$CURRENT_AUD" = "$AUD" ]; then
        echo "   ✅ AuthPolicy audience is correctly set: $AUD"
    else
        echo "   Detected audience: $AUD (current: $CURRENT_AUD)"
        echo "   Patching AuthPolicy with correct audience..."
        kubectl patch authpolicy maas-api-auth-policy -n "$MAAS_API_NAMESPACE" \
          --type='json' \
          -p "$(jq -nc --arg aud "$AUD" '[{
            op:"replace",
            path:"/spec/rules/authentication/openshift-identities/kubernetesTokenReview/audiences/0",
            value:$aud
          }]')" 2>/dev/null && echo "   ✅ AuthPolicy audience patched" || echo "   ⚠️  Failed to patch AuthPolicy (may need manual configuration)"
    fi
else
    echo "   ⚠️  Could not detect audience, skipping AuthPolicy verification"
fi

# Fix authorization verb: Kubernetes uses 'get' not 'post'
echo "   Checking AuthPolicy authorization verb..."
if kubectl get authpolicy gateway-auth-policy -n openshift-ingress &>/dev/null; then
    CURRENT_VERB=$(kubectl get authpolicy gateway-auth-policy -n openshift-ingress -o jsonpath='{.spec.rules.authorization.tier-access.kubernetesSubjectAccessReview.resourceAttributes.verb.value}' 2>/dev/null || echo "")
    if [ "$CURRENT_VERB" = "get" ]; then
        echo "   ✅ AuthPolicy authorization verb is already correct (get)"
    elif [ -n "$CURRENT_VERB" ]; then
        echo "   Current verb is '$CURRENT_VERB', patching to 'get'..."
        kubectl patch authpolicy gateway-auth-policy -n openshift-ingress \
          --type='json' \
          -p='[{"op":"replace","path":"/spec/rules/authorization/tier-access/kubernetesSubjectAccessReview/resourceAttributes/verb/value","value":"get"}]' && \
          echo "   ✅ AuthPolicy authorization verb patched" || \
          echo "   ⚠️  Failed to patch authorization verb"
    else
        echo "   ⚠️  Could not read authorization verb, skipping patch"
    fi
else
    echo "   ⚠️  AuthPolicy gateway-auth-policy not found, skipping verb patch"
fi

echo ""
echo "1️⃣3️⃣ Updating Limitador image for metrics exposure..."
kubectl -n kuadrant-system patch limitador limitador --type merge \
  -p '{"spec":{"image":"quay.io/kuadrant/limitador:1a28eac1b42c63658a291056a62b5d940596fd4c","version":""}}' 2>/dev/null && \
  echo "   ✅ Limitador image updated" || \
  echo "   ⚠️  Could not update Limitador image (may not be critical)"

echo ""
echo "1️⃣4️⃣ Deploying base observability components (TelemetryPolicy and ServiceMonitors)..."
cd "$PROJECT_ROOT"

# Label namespaces for Prometheus scraping (REQUIRED for ServiceMonitors to work)
for ns in kuadrant-system "$MAAS_API_NAMESPACE" llm; do
    if kubectl get namespace "$ns" &>/dev/null; then
        kubectl label namespace "$ns" openshift.io/cluster-monitoring=true --overwrite 2>/dev/null || true
        echo "   ✅ Labeled namespace: $ns"
    fi
done

# Deploy TelemetryPolicy and base ServiceMonitors (ALWAYS needed for metrics labels)
kustomize build deployment/base/observability | kubectl apply -f -
echo "   ✅ TelemetryPolicy and base ServiceMonitors deployed"

# Deploy component-specific monitors (Istio, LLM models) if components exist
OBSERVABILITY_DIR="$PROJECT_ROOT/deployment/components/observability"
if kubectl get deploy -n openshift-ingress maas-default-gateway-openshift-default &>/dev/null; then
    kubectl apply -f "$OBSERVABILITY_DIR/monitors/istio-gateway-service.yaml"
    kubectl apply -f "$OBSERVABILITY_DIR/monitors/istio-gateway-servicemonitor.yaml"
    echo "   ✅ Istio Gateway metrics configured"
fi
if kubectl get ns llm &>/dev/null; then
    kubectl apply -f "$OBSERVABILITY_DIR/monitors/kserve-llm-models-servicemonitor.yaml"
    echo "   ✅ LLM models metrics configured"
fi

echo "   ℹ️  ServiceMonitors configured for OpenShift Prometheus (user-workload-monitoring)"

# Verification
echo ""
echo "========================================="
echo "✅ Deployment Complete!"
echo "========================================="
echo ""
echo "📊 Status Check:"
echo ""

# Check component status
echo "Component Status:"
kubectl get pods -n "$MAAS_API_NAMESPACE" --no-headers | grep Running | wc -l | xargs echo "  MaaS API pods running:"
kubectl get pods -n kuadrant-system --no-headers | grep Running | wc -l | xargs echo "  Kuadrant pods running:"
kubectl get pods -n opendatahub --no-headers | grep Running | wc -l | xargs echo "  KServe pods running:"

echo ""
echo "Gateway Status:"
kubectl get gateway -n openshift-ingress maas-default-gateway -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' | xargs echo "  Accepted:"
kubectl get gateway -n openshift-ingress maas-default-gateway -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' | xargs echo "  Programmed:"

echo ""
echo "Policy Status:"
kubectl get authpolicy -n openshift-ingress gateway-auth-policy -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' 2>/dev/null | xargs echo "  AuthPolicy:"
kubectl get tokenratelimitpolicy -n openshift-ingress gateway-token-rate-limits -o jsonpath='{.status.conditions[?(@.type=="Accepted")].status}' 2>/dev/null | xargs echo "  TokenRateLimitPolicy:"

echo ""
echo "Policy Enforcement Status:"
kubectl get authpolicy -n openshift-ingress gateway-auth-policy -o jsonpath='{.status.conditions[?(@.type=="Enforced")].status}' 2>/dev/null | xargs echo "  AuthPolicy Enforced:"
kubectl get ratelimitpolicy -n openshift-ingress gateway-rate-limits -o jsonpath='{.status.conditions[?(@.type=="Enforced")].status}' 2>/dev/null | xargs echo "  RateLimitPolicy Enforced:"
kubectl get tokenratelimitpolicy -n openshift-ingress gateway-token-rate-limits -o jsonpath='{.status.conditions[?(@.type=="Enforced")].status}' 2>/dev/null | xargs echo "  TokenRateLimitPolicy Enforced:"
kubectl get telemetrypolicy -n openshift-ingress user-group -o jsonpath='{.status.conditions[?(@.type=="Enforced")].status}' 2>/dev/null | xargs echo "  TelemetryPolicy Enforced:"

echo ""
echo "========================================="
echo "🔧 Troubleshooting:"
echo "========================================="
echo ""
echo "If policies show 'Not enforced' status:"
echo "1. Check if Gateway API provider is recognized:"
echo "   kubectl describe authpolicy gateway-auth-policy -n openshift-ingress | grep -A 5 'Status:'"
echo ""
echo "2. If Gateway API provider is not installed, restart all Kuadrant operators:"
echo "   kubectl rollout restart deployment/kuadrant-operator-controller-manager -n kuadrant-system"
echo "   kubectl rollout restart deployment/authorino-operator -n kuadrant-system"
echo "   kubectl rollout restart deployment/limitador-operator-controller-manager -n kuadrant-system"
echo ""
echo "3. Check if OpenShift Gateway Controller is available:"
echo "   kubectl get gatewayclass"
echo ""
echo "4. If policies still show 'MissingDependency', ensure environment variable is set:"
echo "   kubectl get deployment kuadrant-operator-controller-manager -n kuadrant-system -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name==\"ISTIO_GATEWAY_CONTROLLER_NAMES\")]}'"
echo ""
echo "5. If environment variable is missing, patch the deployment:"
echo "   kubectl -n kuadrant-system patch deployment kuadrant-operator-controller-manager --type='json' \\"
echo "     -p='[{\"op\": \"add\", \"path\": \"/spec/template/spec/containers/0/env/-\", \"value\": {\"name\": \"ISTIO_GATEWAY_CONTROLLER_NAMES\", \"value\": \"openshift.io/gateway-controller/v1\"}}]'"
echo ""
echo "6. Restart Kuadrant operator after patching:"
echo "   kubectl rollout restart deployment/kuadrant-operator-controller-manager -n kuadrant-system"
echo "   kubectl rollout status deployment/kuadrant-operator-controller-manager -n kuadrant-system --timeout=60s"
echo ""
echo "7. Wait for policies to be enforced (may take 1-2 minutes):"
echo "   kubectl describe authpolicy gateway-auth-policy -n openshift-ingress | grep -A 10 'Status:'"
echo ""
echo "If metrics are not visible in Prometheus:"
echo "1. Check ServiceMonitor:"
echo "   kubectl get servicemonitor limitador-metrics -n kuadrant-system"
echo ""
echo "2. Check Prometheus targets:"
echo "   kubectl port-forward -n openshift-monitoring svc/prometheus-k8s 9090:9091 &"
echo "   # Visit http://localhost:9090/targets and look for limitador targets"
echo ""
echo "If webhook timeout errors occur during model deployment:"
echo "1. Restart ODH model controller:"
echo "   kubectl rollout restart deployment/odh-model-controller -n opendatahub"
echo ""
echo "2. Temporarily bypass webhook:"
echo "   kubectl patch validatingwebhookconfigurations validating.odh-model-controller.opendatahub.io --type='json' -p='[{\"op\": \"replace\", \"path\": \"/webhooks/1/failurePolicy\", \"value\": \"Ignore\"}]'"
echo "   # Deploy your model, then restore:"
echo "   kubectl patch validatingwebhookconfigurations validating.odh-model-controller.opendatahub.io --type='json' -p='[{\"op\": \"replace\", \"path\": \"/webhooks/1/failurePolicy\", \"value\": \"Fail\"}]'"
echo ""
echo "If API calls return 404 errors (Gateway routing issues):"
echo "1. Check HTTPRoute status:"
echo "   kubectl get httproute -A"
echo "   kubectl describe httproute facebook-opt-125m-simulated-kserve-route -n llm"
echo ""
echo "2. Check if model is accessible directly:"
echo "   kubectl get pods -n llm"
echo "   kubectl port-forward -n llm svc/facebook-opt-125m-simulated-kserve-workload-svc 8080:8000 &"
echo "   curl -k https://localhost:8080/health"
echo ""
echo "3. Test model with correct name and HTTPS:"
echo "   curl -k -H \"Content-Type: application/json\" -d '{\"model\": \"facebook/opt-125m\", \"prompt\": \"Hello\", \"max_tokens\": 50}' https://localhost:8080/v1/chat/completions"
echo ""
echo "4. Check Gateway status:"
echo "   kubectl get gateway -A"
echo "   kubectl describe gateway maas-default-gateway -n openshift-ingress"
echo ""
echo "If metrics are not generated despite successful API calls:"
echo "1. Verify policies are enforced:"
echo "   kubectl describe authpolicy gateway-auth-policy -n openshift-ingress | grep -A 5 'Enforced'"
echo "   kubectl describe ratelimitpolicy gateway-rate-limits -n openshift-ingress | grep -A 5 'Enforced'"
echo ""
echo "2. Check Limitador metrics directly:"
echo "   kubectl port-forward -n kuadrant-system svc/limitador-limitador 8080:8080 &"
echo "   curl http://localhost:8080/metrics | grep -E '(authorized_hits|authorized_calls|limited_calls)'"
echo ""
echo "3. Make test API calls to trigger metrics:"
echo "   # Use HTTPS and correct model name: facebook/opt-125m"
echo "   for i in {1..5}; do curl -k -H \"Authorization: Bearer \$TOKEN\" -H \"Content-Type: application/json\" -d '{\"model\": \"facebook/opt-125m\", \"prompt\": \"Hello \$i\", \"max_tokens\": 50}' \"https://\${HOST}/llm/facebook-opt-125m-simulated/v1/chat/completions\"; done"

echo ""
echo "========================================="
echo "📝 Next Steps:"
echo "========================================="
echo ""
echo "1. Deploy a sample model:"
echo "   kustomize build docs/samples/models/simulator | kubectl apply -f -"
echo ""
echo "2. Get Gateway endpoint:"
echo "   CLUSTER_DOMAIN=\$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"
echo "   HOST=\"maas.\${CLUSTER_DOMAIN}\""
echo ""
echo "3. Get authentication token:"
echo "   TOKEN_RESPONSE=\$(curl -sSk -H \"Authorization: Bearer \$(oc whoami -t)\" -H \"Content-Type: application/json\" -X POST -d '{\"expiration\": \"10m\"}' \"\${HOST}/maas-api/v1/tokens\")"
echo "   TOKEN=\$(echo \$TOKEN_RESPONSE | jq -r .token)"
echo ""
echo "4. Test model endpoint:"
echo "   MODELS=\$(curl -sSk \${HOST}/maas-api/v1/models -H \"Content-Type: application/json\" -H \"Authorization: Bearer \$TOKEN\" | jq -r .)"
echo "   MODEL_NAME=\$(echo \$MODELS | jq -r '.data[0].id')"
echo "   MODEL_URL=\"\${HOST}/llm/facebook-opt-125m-simulated/v1/chat/completions\" # Note: This may be different for your model"
echo "   curl -sSk -H \"Authorization: Bearer \$TOKEN\" -H \"Content-Type: application/json\" -d \"{\\\"model\\\": \\\"\${MODEL_NAME}\\\", \\\"prompt\\\": \\\"Hello\\\", \\\"max_tokens\\\": 50}\" \"\${MODEL_URL}\""
echo ""
echo "5. Test authorization limiting (no token 401 error):"
echo "   curl -sSk -H \"Content-Type: application/json\" -d \"{\\\"model\\\": \\\"\${MODEL_NAME}\\\", \\\"prompt\\\": \\\"Hello\\\", \\\"max_tokens\\\": 50}\" \"\${MODEL_URL}\" -v"
echo ""
echo "6. Test rate limiting (200 OK followed by 429 Rate Limit Exceeded after about 4 requests):"
echo "   for i in {1..16}; do curl -sSk -o /dev/null -w \"%{http_code}\\n\" -H \"Authorization: Bearer \$TOKEN\" -H \"Content-Type: application/json\" -d \"{\\\"model\\\": \\\"\${MODEL_NAME}\\\", \\\"prompt\\\": \\\"Hello\\\", \\\"max_tokens\\\": 50}\" \"\${MODEL_URL}\"; done"
echo ""
echo "7. Run validation script (Runs all the checks again):"
echo "   ./scripts/validate-deployment.sh --namespace $MAAS_API_NAMESPACE"
echo ""
echo "8. Check metrics generation:"
echo "   kubectl port-forward -n kuadrant-system svc/limitador-limitador 8080:8080 &"
echo "   curl http://localhost:8080/metrics | grep -E '(authorized_hits|authorized_calls|limited_calls)'"
echo ""
echo "9. Access Prometheus to view metrics:"
echo "   kubectl port-forward -n openshift-monitoring svc/prometheus-k8s 9090:9091 &"
echo "   # Open http://localhost:9090 in browser and search for: authorized_hits, authorized_calls, limited_calls"
echo ""

# Observability installation
echo ""
echo "========================================="
echo "📊 Observability Stack"
echo "========================================="
echo ""

# Check if flag was provided
if [ "$INSTALL_OBSERVABILITY" = "y" ]; then
    echo "Installing observability stack..."
    "$SCRIPT_DIR/install-observability.sh" --namespace "$MAAS_API_NAMESPACE"
elif [ "$INSTALL_OBSERVABILITY" = "n" ]; then
    echo "⏭️  Skipping observability installation"
    echo "   To install later, from project root run: ./scripts/install-observability.sh --namespace $MAAS_API_NAMESPACE"
else
    # No flag - prompt if interactive
    echo "Would you like to install the observability stack (Grafana + dashboards)?"
    echo "This includes:"
    echo "  - Grafana instance with Prometheus datasource"
    echo "  - Platform Admin Dashboard"
    echo "  - AI Engineer Dashboard"
    echo ""

    if [ -t 0 ]; then
        read -p "Install observability? [y/N]: " INSTALL_OBS_ANSWER
        if [ "$INSTALL_OBS_ANSWER" = "y" ] || [ "$INSTALL_OBS_ANSWER" = "Y" ] || [ "$INSTALL_OBS_ANSWER" = "yes" ] || [ "$INSTALL_OBS_ANSWER" = "YES" ]; then
            "$SCRIPT_DIR/install-observability.sh" --namespace "$MAAS_API_NAMESPACE"
        else
            echo "⏭️  Skipping observability installation"
            echo "   To install later, from project root run: ./scripts/install-observability.sh --namespace $MAAS_API_NAMESPACE"
        fi
    else
        echo "Non-interactive mode: skipping observability (use --with-observability to install)"
    fi
fi

# Run validation
echo ""
echo "========================================="
echo "🔍 Running Deployment Validation..."
echo "========================================="
echo ""
"$SCRIPT_DIR/validate-deployment.sh" --namespace "$MAAS_API_NAMESPACE"
