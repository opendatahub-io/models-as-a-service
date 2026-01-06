#!/bin/bash

# OpenShift MaaS Platform Deployment Script
# This script automates the complete deployment of the MaaS platform on OpenShift

set -e

ENABLE_TLS_BACKEND=1

# Respect INSECURE_HTTP env var (used by test scripts)
# This provides consistency with prow_run_smoke_test.sh and smoke.sh
if [[ "${INSECURE_HTTP:-}" == "true" ]]; then
  ENABLE_TLS_BACKEND=0
fi

while [[ $# -gt 0 ]]; do
  case "$1" in
    --insecure)
      ENABLE_TLS_BACKEND=0
      shift
      ;;
    *)
      echo "Unknown argument: $1" >&2
      exit 1
      ;;
  esac
done

# Load helper functions
source "$(dirname "$0")/deployment-helpers.sh"

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

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

# Set custom MaaS API image if MAAS_API_IMAGE env var is provided
set_maas_api_image

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

# Determine MaaS API namespace: use MAAS_API_NAMESPACE env var if set, otherwise default to maas-api
MAAS_API_NAMESPACE=${MAAS_API_NAMESPACE:-maas-api}
export MAAS_API_NAMESPACE
echo "   MaaS API namespace: $MAAS_API_NAMESPACE (set MAAS_API_NAMESPACE env var to override)"

for ns in opendatahub kserve kuadrant-system llm "$MAAS_API_NAMESPACE"; do
    kubectl create namespace $ns 2>/dev/null || echo "   Namespace $ns already exists"
done

echo ""
echo "3️⃣ Installing dependencies..."

# Only clean up leftover CRDs if Kuadrant operators are NOT already installed
echo "   Checking for existing Kuadrant installation..."
EXISTING_KUADRANT_CSV=$(find_csv_with_min_version "kuadrant-operator" "$KUADRANT_MIN_VERSION" "kuadrant-system" || echo "")
if [ -z "$EXISTING_KUADRANT_CSV" ]; then
    echo "   No existing installation found, checking for leftover CRDs..."
    LEFTOVER_CRDS=$(kubectl get crd 2>/dev/null | grep -E "kuadrant|authorino|limitador" | awk '{print $1}')
    if [ -n "$LEFTOVER_CRDS" ]; then
        echo "   Found leftover CRDs, cleaning up before installation..."
        echo "$LEFTOVER_CRDS" | xargs -r kubectl delete crd --timeout=30s 2>/dev/null || true
        sleep 5  # Brief wait for cleanup to complete
    fi
else
    echo "   ✅ Kuadrant operator already installed ($EXISTING_KUADRANT_CSV), skipping CRD cleanup"
fi

echo "   Installing Kuadrant..."
"$SCRIPT_DIR/install-dependencies.sh" --kuadrant

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
    kubectl annotate deployment/odh-model-controller opendatahub.io/managed=false -n opendatahub
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
    echo "   ⚠️  odh-model-controller deployment not found in opendatahub namespace, skipping patch"
    echo "      (The deployment may be created later by the ODH operator)"
fi

# Patch GatewayConfig to use LoadBalancer instead of OcpRoute (default mode)
echo ""
echo "   Patching GatewayConfig to use LoadBalancer ingress mode..."
if kubectl get gatewayconfig.services.platform.opendatahub.io default-gateway &>/dev/null; then
    kubectl patch gatewayconfig.services.platform.opendatahub.io default-gateway \
      --type='merge' \
      -p '{"spec":{"ingressMode":"LoadBalancer"}}' && \
      echo "   ✅ GatewayConfig patched to use LoadBalancer mode" || \
      echo "   ⚠️  Failed to patch GatewayConfig"
else
    echo "   ⚠️  GatewayConfig default-gateway not found, skipping patch"
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

echo "   Deploying Gateway and GatewayClass..."
cd "$PROJECT_ROOT"
kubectl apply --server-side=true --force-conflicts -f deployment/base/networking/odh/odh-gateway-api.yaml

# Detect which TLS certificate secret exists for the MaaS gateway
CERT_CANDIDATES=("default-gateway-cert" "data-science-gatewayconfig-tls" "data-science-gateway-service-tls")
CERT_NAME=""
for cert in "${CERT_CANDIDATES[@]}"; do
    if kubectl get secret -n openshift-ingress "$cert" &>/dev/null; then
        CERT_NAME="$cert"
        echo "   Found TLS certificate secret: $cert"
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
    kubectl apply --server-side=true --force-conflicts -f <(envsubst '$CLUSTER_DOMAIN' < deployment/base/networking/maas/maas-gateway-api.yaml | yq eval 'del(.spec.listeners[] | select(.name == "https"))' -)
fi


echo ""
echo "6️⃣ Waiting for Kuadrant operators to be installed by OLM..."
# Wait for CSVs to reach Succeeded state (this ensures CRDs are created and deployments are ready)
wait_for_csv_with_min_version "kuadrant-operator" "$KUADRANT_MIN_VERSION" "kuadrant-system" 300 || \
    echo "   ⚠️  Kuadrant operator CSV did not succeed, continuing anyway..."

wait_for_csv_with_min_version "authorino-operator" "$AUTHORINO_MIN_VERSION" "kuadrant-system" 60 || \
    echo "   ⚠️  Authorino operator CSV did not succeed"

wait_for_csv_with_min_version "limitador-operator" "$LIMITADOR_MIN_VERSION" "kuadrant-system" 60 || \
    echo "   ⚠️  Limitador operator CSV did not succeed"

wait_for_csv_with_min_version "dns-operator" "$DNS_OPERATOR_MIN_VERSION" "kuadrant-system" 60 || \
    echo "   ⚠️  DNS operator CSV did not succeed"

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
echo "8️⃣ Waiting for Gateway to be ready..."
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

echo ""
echo "9️⃣ Deploying MaaS API and policies..."

# Select overlay based on TLS mode (TLS is default)
OVERLAY="overlays/tls-backend"
if [[ "$ENABLE_TLS_BACKEND" -eq 0 ]]; then
  OVERLAY="overlays/http-backend"
  echo "   ⚠️  TLS disabled, applying HTTP backend overlay..."
else
  echo "   Applying TLS backend overlay (maas-api + policies + Authorino TLS)..."
fi

# Build and apply with correct namespace
# Use sed to replace default namespace while preserving explicit namespaces (openshift-ingress, kuadrant-system)
# This avoids `kustomize edit set namespace` which overwrites ALL namespaces
kustomize build "$PROJECT_ROOT/deployment/$OVERLAY" \
  | sed "s/namespace: maas-api/namespace: ${MAAS_API_NAMESPACE}/g" \
  | sed "s/maas-api\.maas-api\.svc/maas-api.${MAAS_API_NAMESPACE}.svc/g" \
  | kubectl apply --server-side=true --force-conflicts -f -

# Configure Authorino TLS (patches operator-managed resources via kubectl)
if [[ "$ENABLE_TLS_BACKEND" -eq 1 ]]; then
  echo "   Configuring Authorino for TLS..."
  "$PROJECT_ROOT/deployment/overlays/tls-backend/configure-authorino-tls.sh"
  
  echo "   Waiting for Authorino deployment to pick up TLS config..."
  kubectl rollout status deployment/authorino -n kuadrant-system --timeout=120s || \
    echo "   ⚠️  Authorino rollout taking longer than expected, continuing..."
  
  # Restart maas-api to ensure it picks up Authorino TLS config
  echo "   Restarting MaaS API to pick up Authorino TLS configuration..."
  kubectl rollout restart deployment/maas-api -n "$MAAS_API_NAMESPACE"
fi

echo "   Waiting for MaaS API deployment to be ready..."
kubectl rollout status deployment/maas-api -n "$MAAS_API_NAMESPACE" --timeout=180s || \
  echo "   ⚠️  MaaS API rollout is taking longer than expected, continuing..."

echo ""
echo "1️⃣0️⃣ Patching AuthPolicy with correct audience..."
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

echo ""
echo "1️⃣1️⃣ Updating Limitador image for metrics exposure..."
kubectl -n kuadrant-system patch limitador limitador --type merge \
  -p '{"spec":{"image":"quay.io/kuadrant/limitador:1a28eac1b42c63658a291056a62b5d940596fd4c","version":""}}' 2>/dev/null && \
  echo "   ✅ Limitador image updated" || \
  echo "   ⚠️  Could not update Limitador image (may not be critical)"

echo ""
echo "========================================="
echo "⚠️  TEMPORARY WORKAROUNDS (TO BE REMOVED)"
echo "========================================="
echo ""
echo "Applying temporary workarounds for known issues..."

echo "   🔧 Restarting Kuadrant, Authorino, and Limitador operators to refresh webhook configurations..."
kubectl delete pod -n kuadrant-system -l control-plane=controller-manager 2>/dev/null && \
  echo "   ✅ Kuadrant operator restarted" || \
  echo "   ⚠️  Could not restart Kuadrant operator"

kubectl rollout restart deployment authorino-operator -n kuadrant-system 2>/dev/null && \
  echo "   ✅ Authorino operator restarted" || \
  echo "   ⚠️  Could not restart Authorino operator"

kubectl rollout restart deployment limitador-operator-controller-manager -n kuadrant-system 2>/dev/null && \
  echo "   ✅ Limitador operator restarted" || \
  echo "   ⚠️  Could not restart Limitador operator"

echo "   Waiting for operators to be ready..."
kubectl rollout status deployment kuadrant-operator-controller-manager -n kuadrant-system --timeout=60s 2>/dev/null || \
  echo "   ⚠️  Kuadrant operator taking longer than expected"
kubectl rollout status deployment authorino-operator -n kuadrant-system --timeout=60s 2>/dev/null || \
  echo "   ⚠️  Authorino operator taking longer than expected"
kubectl rollout status deployment limitador-operator-controller-manager -n kuadrant-system --timeout=60s 2>/dev/null || \
  echo "   ⚠️  Limitador operator taking longer than expected"

echo ""
echo "========================================="
# Deploy observability components (ServiceMonitor and TelemetryPolicy)
echo "   Deploying observability components..."
kustomize build deployment/base/observability | kubectl apply -f -
echo "   ✅ Observability components deployed"

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
echo "   TOKEN_RESPONSE=\$(curl -sSk -H \"Authorization: Bearer \$(oc whoami -t)\" -H \"Content-Type: application/json\" -X POST -d '{\"expiration\": \"10m\"}' \"https://\${HOST}/maas-api/v1/tokens\")"
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
echo "   ./scripts/validate-deployment.sh"
echo ""
echo "8. Check metrics generation:"
echo "   kubectl port-forward -n kuadrant-system svc/limitador-limitador 8080:8080 &"
echo "   curl http://localhost:8080/metrics | grep -E '(authorized_hits|authorized_calls|limited_calls)'"
echo ""
echo "9. Access Prometheus to view metrics:"
echo "   kubectl port-forward -n openshift-monitoring svc/prometheus-k8s 9090:9091 &"
echo "   # Open http://localhost:9090 in browser and search for: authorized_hits, authorized_calls, limited_calls"
echo ""
