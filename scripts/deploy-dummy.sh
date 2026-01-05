#!/bin/bash

# OpenShift MaaS Platform Deployment Script
# This script automates the complete deployment of the MaaS platform on OpenShift

set -e

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
wait_for_csv() {
  local csv_name="$1"
  local namespace="${2:-kuadrant-system}"
  local timeout="${3:-180}"  # timeout in seconds
  local interval=5
  local elapsed=0
  local last_status_print=0

  echo "⏳ Waiting for CSV ${csv_name} to succeed (timeout: ${timeout}s)..."
  while [ $elapsed -lt $timeout ]; do
    local phase=$(kubectl get csv -n "$namespace" "$csv_name" -o jsonpath='{.status.phase}' 2>/dev/null || echo "NotFound")

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

  echo "❌ Timed out after ${timeout}s waiting for CSV ${csv_name}" >&2
  return 1
}

# Helper function to wait for pods in a namespace to be ready
wait_for_pods() {
  local namespace="$1"
  local timeout="${2:-120}"
  
  kubectl get namespace "$namespace" &>/dev/null || return 0
  
  echo "⏳ Waiting for pods in $namespace to be ready..."
  local end=$((SECONDS + timeout))
  while [ $SECONDS -lt $end ]; do
    local not_ready=$(kubectl get pods -n "$namespace" --no-headers 2>/dev/null | grep -v -E 'Running|Completed|Succeeded' | wc -l)
    [ "$not_ready" -eq 0 ] && return 0
    sleep 5
  done
  echo "⚠️  Timeout waiting for pods in $namespace" >&2
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

echo ""
echo "1️⃣2️⃣ Patching AuthPolicy with correct audience..."
echo "   Attempting to detect audience..."
TOKEN=$(kubectl create token default --duration=10m 2>/dev/null || echo "")
if [ -z "$TOKEN" ]; then
    echo "   ⚠️  Could not create token, skipping audience detection"
    AUD=""
else
    echo "   Token created successfully"
    JWT_PAYLOAD=$(echo "$TOKEN" | cut -d. -f2 2>/dev/null || echo "")
    if [ -z "$JWT_PAYLOAD" ]; then
        echo "   ⚠️  Could not extract JWT payload, skipping audience detection"
        AUD=""
    else
        echo "   JWT payload extracted"
        DECODED_PAYLOAD=$(echo "$JWT_PAYLOAD" | jq -Rr '@base64d | fromjson' || echo "")
        if [ -z "$DECODED_PAYLOAD" ]; then
            echo "   ⚠️  Could not decode base64 payload, skipping audience detection"
            AUD=""
        else
            echo "   Payload decoded successfully"
            AUD=$(echo "$DECODED_PAYLOAD" | jq -r '.aud[0]' 2>/dev/null || echo "")
        fi
    fi
fi
if [ -n "$AUD" ] && [ "$AUD" != "null" ]; then
    echo "   Detected audience: $AUD"
    PATCH_JSON="[{\"op\":\"replace\",\"path\":\"/spec/rules/authentication/openshift-identities/kubernetesTokenReview/audiences/0\",\"value\":\"$AUD\"}]"
    kubectl patch authpolicy maas-api-auth-policy -n "$MAAS_API_NAMESPACE"  \
      --type='json' \
      -p "$PATCH_JSON" 2>/dev/null && echo "   ✅ AuthPolicy patched" || echo "   ⚠️  Failed to patch AuthPolicy (may need manual configuration)"
else
    echo "   ⚠️  Could not detect audience, skipping AuthPolicy patch"
    echo "      You may need to manually configure the audience later"
fi
