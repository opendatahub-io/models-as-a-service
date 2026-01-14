#!/bin/bash
#
# deploy-rhoai-stable.sh - Deploy Red Hat OpenShift AI v3 or OpenDataHub with Models-as-a-Service capability
#
# DESCRIPTION:
#   This script automates the deployment of Red Hat OpenShift AI (RHOAI) v3 or OpenDataHub (ODH)
#   along with its required prerequisites and the Models-as-a-Service (MaaS) capability.
#
#   The deployment includes:
#   - cert-manager
#   - Leader Worker Set (LWS)
#   - Red Hat Connectivity Link (Kuadrant)
#   - RHOAI v3 or ODH with KServe for model serving
#   - MaaS capability (core components managed by the operator)
#   - MaaS Gateway (not managed by operator)
#   - Rate Limit and Token Limit policies (not managed by operator)
#
# PREREQUISITES:
#   - OpenShift cluster v4.19.9+
#   - Cluster administrator privileges
#   - kubectl CLI tool configured and connected to cluster
#   - kustomize tool available in PATH (for usage policies)
#   - jq tool for JSON processing
#
# ENVIRONMENT VARIABLES:
#   OPERATOR_TYPE   - Which operator to install: "rhoai" (default) or "odh"
#   MAAS_REF        - Git ref for MaaS manifests (default: main)
#   CERT_NAME       - TLS certificate secret name (default: data-science-gateway-service-tls)
#
# USAGE:
#   ./deploy-rhoai-stable.sh                    # Install RHOAI (default)
#   OPERATOR_TYPE=odh ./deploy-rhoai-stable.sh  # Install ODH
#
# NOTES:
#   - The script is idempotent for most operations
#   - Core MaaS components (deployment, auth policy) are managed by the RHOAI/ODH operator
#   - Gateway and usage policies are installed separately by this script

set -e

# Configuration
: "${OPERATOR_TYPE:=rhoai}"
: "${MAAS_REF:=main}"
: "${CERT_NAME:=data-science-gateway-service-tls}"

# Export variables needed by envsubst
export CERT_NAME

# Validate OPERATOR_TYPE
if [[ "$OPERATOR_TYPE" != "rhoai" && "$OPERATOR_TYPE" != "odh" ]]; then
  echo "ERROR: OPERATOR_TYPE must be 'rhoai' or 'odh'. Got: $OPERATOR_TYPE"
  exit 1
fi

echo "========================================="
echo "Deploying with operator: ${OPERATOR_TYPE}"
echo "========================================="

waitsubscriptioninstalled() {
  local ns=${1?namespace is required}; shift
  local name=${1?subscription name is required}; shift

  echo "  * Waiting for Subscription $ns/$name to start setup..."
  kubectl wait subscription --timeout=300s -n $ns $name --for=jsonpath='{.status.currentCSV}'
  local csv=$(kubectl get subscription -n $ns $name -o jsonpath='{.status.currentCSV}')

  # Because, sometimes, the CSV is not there immediately.
  while ! kubectl get -n $ns csv $csv > /dev/null 2>&1; do
    sleep 1
  done

  echo "  * Waiting for Subscription setup to finish setup. CSV = $csv ..."
  if ! kubectl wait -n $ns --for=jsonpath="{.status.phase}"=Succeeded csv $csv --timeout=600s; then
    echo "    * ERROR: Timeout while waiting for Subscription to finish installation."
    exit 1
  fi
}

checksubscriptionexists() {
  local catalog_ns=${1?catalog namespace is required}; shift
  local catalog_name=${1?catalog name is required}; shift
  local operator_name=${1?operator name is required}; shift

  local catalogns_cond=".spec.sourceNamespace == \"${catalog_ns}\""
  local catalog_cond=".spec.source == \"${catalog_name}\""
  local op_cond=".spec.name == \"${operator_name}\""
  local query="${catalogns_cond} and ${catalog_cond} and ${op_cond}"

  echo $(kubectl get subscriptions -A -ojson | jq ".items | map(select(${query})) | length")
}

# Check if a CSV exists by name prefix (e.g., "opendatahub-operator" matches "opendatahub-operator.v3.2.0")
checkcsvexists() {
  local csv_prefix=${1?csv prefix is required}; shift

  # Count CSVs whose name starts with the given prefix
  local count
  count=$(kubectl get csv -A -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -c "^${csv_prefix}" 2>/dev/null) || count=0
  echo "$count"
}

deploy_certmanager() {
  local certmanager_exists=$(checksubscriptionexists openshift-marketplace redhat-operators openshift-cert-manager-operator)
  if [[ $certmanager_exists -ne "0" ]]; then
    echo "* The cert-manager operator is present in the cluster. Skipping installation."
    return 0
  fi

  echo
  echo "* Installing cert-manager operator..."

  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Namespace
metadata:
  name: cert-manager-operator
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: cert-manager-operator
  namespace: cert-manager-operator
---
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

  waitsubscriptioninstalled "cert-manager-operator" "openshift-cert-manager-operator"
}

deploy_lws() {
  local lws_exists=$(checksubscriptionexists openshift-marketplace redhat-operators leader-worker-set)
  if [[ $lws_exists -ne "0" ]]; then
    echo "* The LWS operator is present in the cluster. Skipping installation."
    return 0
  fi

  echo
  echo "* Installing LWS operator..."

  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Namespace
metadata:
  name: openshift-lws-operator
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: leader-worker-set
  namespace: openshift-lws-operator
spec:
  targetNamespaces:
  - openshift-lws-operator
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: leader-worker-set
  namespace: openshift-lws-operator
spec:
  channel: stable-v1.0
  installPlanApproval: Automatic
  name: leader-worker-set
  source: redhat-operators
  sourceNamespace: openshift-marketplace
EOF

  waitsubscriptioninstalled "openshift-lws-operator" "leader-worker-set"
  echo "* Setting up LWS instance and letting it deploy asynchronously."

  cat <<EOF | kubectl apply -f -
apiVersion: operator.openshift.io/v1
kind: LeaderWorkerSetOperator
metadata:
  name: cluster
  namespace: openshift-lws-operator
spec:
  managementState: Managed
EOF
}

deploy_rhcl() {
  echo
  echo "* Initializing Gateway API provider..."

  cat <<EOF | kubectl apply -f -
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: openshift-default
spec:
  controllerName: "openshift.io/gateway-controller/v1"
EOF

  echo "  * Waiting for GatewayClass openshift-default to transition to Accepted status..."
  kubectl wait --timeout=300s --for=condition=Accepted=True GatewayClass/openshift-default

  local rhcl_exists=$(checksubscriptionexists openshift-marketplace redhat-operators rhcl-operator)
  if [[ $rhcl_exists -ne "0" ]]; then
    echo "* The RHCL operator is present in the cluster. Skipping installation."
    echo "  WARNING: Creating an instance of RHCL is also skipped."
    return 0
  fi

  echo
  echo "* Installing RHCL operator..."

  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Namespace
metadata:
  name: kuadrant-system
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: kuadrant-operator-group
  namespace: kuadrant-system
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: kuadrant-operator
  namespace: kuadrant-system
spec:
  channel: stable
  installPlanApproval: Automatic
  name: rhcl-operator
  source: redhat-operators
  sourceNamespace: openshift-marketplace
EOF

  waitsubscriptioninstalled "kuadrant-system" "kuadrant-operator"
  echo "* Setting up RHCL instance..."

  cat <<EOF | kubectl apply -f -
apiVersion: kuadrant.io/v1beta1
kind: Kuadrant
metadata:
  name: kuadrant
  namespace: kuadrant-system
EOF
}

deploy_rhoai() {
  # Check if ODH is already installed - can't have both
  local odh_csv_count=$(checkcsvexists "opendatahub-operator")
  if [[ $odh_csv_count -ne "0" ]]; then
    echo "ERROR: OpenDataHub operator is already installed in the cluster."
    echo "       Cannot install RHOAI when ODH is present. Please uninstall ODH first,"
    echo "       or use OPERATOR_TYPE=odh to continue with ODH."
    exit 1
  fi

  # Check if RHOAI is already installed
  local rhoai_csv_count=$(checkcsvexists "rhods-operator")
  if [[ $rhoai_csv_count -ne "0" ]]; then
    echo "* The RHOAI operator is present in the cluster. Skipping installation."
    return 0
  fi

  echo
  echo "* Installing RHOAI v3 operator..."

  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Namespace
metadata:
  name: redhat-ods-operator
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: rhoai3-operatorgroup
  namespace: redhat-ods-operator
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: rhoai3-operator
  namespace: redhat-ods-operator
spec:
  channel: fast-3.x
  installPlanApproval: Automatic
  name: rhods-operator
  source: redhat-operators
  sourceNamespace: openshift-marketplace
EOF

  waitsubscriptioninstalled "redhat-ods-operator" "rhoai3-operator"
}

deploy_odh() {
  # Check if RHOAI is already installed - can't have both
  local rhoai_csv_count=$(checkcsvexists "rhods-operator")
  if [[ $rhoai_csv_count -ne "0" ]]; then
    echo "ERROR: RHOAI operator is already installed in the cluster."
    echo "       Cannot install ODH when RHOAI is present. Please uninstall RHOAI first,"
    echo "       or use OPERATOR_TYPE=rhoai to continue with RHOAI."
    exit 1
  fi

  # Check if ODH is already installed
  local odh_csv_count=$(checkcsvexists "opendatahub-operator")
  if [[ $odh_csv_count -ne "0" ]]; then
    echo "* The ODH operator is present in the cluster. Skipping installation."
    return 0
  fi

  echo
  echo "* Installing OpenDataHub operator..."

  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Namespace
metadata:
  name: openshift-operators
  labels:
    openshift.io/cluster-monitoring: "true"
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: opendatahub-operator
  namespace: openshift-operators
spec:
  channel: fast-3
  installPlanApproval: Automatic
  name: opendatahub-operator
  source: community-operators
  sourceNamespace: openshift-marketplace
EOF

  waitsubscriptioninstalled "openshift-operators" "opendatahub-operator"
}

deploy_dscinitialization() {
  # Check if a DSCInitialization already exists, skip creation if so
  if kubectl get dscinitialization -A --no-headers 2>/dev/null | grep -q .; then
    echo "* A DSCInitialization already exists in the cluster. Skipping creation."
    return 0
  fi

  echo "* Setting up DSCInitialization..."

  cat <<EOF | kubectl apply -f -
apiVersion: dscinitialization.opendatahub.io/v2
kind: DSCInitialization
metadata:
  name: default-dsci
  labels:
    app.kubernetes.io/name: dscinitialization
spec:
  applicationsNamespace: opendatahub
  monitoring:
    metrics:
      storage:
        retention: 90d
        size: 5Gi
    namespace: opendatahub
    traces:
      sampleRatio: '0.1'
      storage:
        backend: pv
        retention: 2160h
    managementState: Managed
  trustedCABundle:
    customCABundle: ''
    managementState: Managed
EOF
}

deploy_datasciencecluster() {
  # Check if a DataScienceCluster already exists, skip creation if so
  if kubectl get datasciencecluster -A --no-headers 2>/dev/null | grep -q .; then
    echo "* A DataScienceCluster already exists in the cluster. Skipping creation."
    return 0
  fi
  echo "* Setting up DataScienceCluster with MaaS capability..."

  cat <<EOF | kubectl apply -f -
apiVersion: datasciencecluster.opendatahub.io/v2
kind: DataScienceCluster
metadata:
  name: default-dsc
spec:
  components:
    # Components required for MaaS:
    kserve:
      managementState: Managed
      rawDeploymentServiceConfig: Headed

      # MaaS capability - managed by operator
      modelsAsService:
        managementState: Managed

    # Components recommended for MaaS:
    dashboard:
      managementState: Managed
EOF
}

wait_for_opendatahub_namespace() {
  if kubectl get namespace opendatahub >/dev/null 2>&1; then
    kubectl wait namespace/opendatahub --for=jsonpath='{.status.phase}'=Active --timeout=60s
  else
    echo "* Waiting for opendatahub namespace to be created by the operator..."
    for i in {1..60}; do
      if kubectl get namespace opendatahub >/dev/null 2>&1; then
        kubectl wait namespace/opendatahub --for=jsonpath='{.status.phase}'=Active --timeout=60s
        return 0
      fi
      sleep 5
    done
    echo "  WARNING: opendatahub namespace was not created within timeout."
  fi
}

# ========================================
# Main Deployment Flow
# ========================================

echo "## Installing prerequisites"

deploy_certmanager
deploy_lws
deploy_rhcl

echo
echo "## Installing ${OPERATOR_TYPE^^} operator"

if [[ "$OPERATOR_TYPE" == "rhoai" ]]; then
  deploy_rhoai
else
  deploy_odh
fi

echo
echo "## Configuring DSCInitialization and DataScienceCluster"
deploy_dscinitialization
deploy_datasciencecluster

echo
echo "## Waiting for operator to initialize..."
wait_for_opendatahub_namespace

echo
echo "## Installing MaaS components (not managed by operator)"

export CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
export AUD="$(kubectl create token default --duration=10m 2>/dev/null | cut -d. -f2 | jq -Rr '@base64d | fromjson | .aud[0]' 2>/dev/null)"

echo "* Cluster domain: ${CLUSTER_DOMAIN}"
echo "* Cluster audience: ${AUD}"
echo "* TLS certificate secret: ${CERT_NAME}"

echo
echo "## Installing MaaS Gateway"
echo "* Deploying maas-default-gateway..."
kubectl apply --server-side=true \
  -f <(kustomize build "https://github.com/opendatahub-io/models-as-a-service.git/deployment/base/networking/maas?ref=${MAAS_REF}" | \
       envsubst '$CLUSTER_DOMAIN $CERT_NAME')

echo
echo "## Applying usage policies (RateLimit and TokenRateLimit)"
echo "* Deploying rate-limit and token-limit policies..."
kubectl apply --server-side=true \
  -f <(kustomize build "https://github.com/opendatahub-io/models-as-a-service.git/deployment/base/policies/usage-policies?ref=${MAAS_REF}")

# Fix audience for ROSA/non-standard clusters
if [[ -n "$AUD" && "$AUD" != "https://kubernetes.default.svc" ]]; then
  echo
  echo "## Configuring audience for non-standard cluster"
  echo "* Detected non-default audience: ${AUD}"
  
  # Wait for maas-api namespace and AuthPolicy to be created by operator
  echo "  * Waiting for maas-api namespace..."
  for i in {1..60}; do
    if kubectl get namespace maas-api >/dev/null 2>&1; then
      break
    fi
    sleep 5
  done
  
  echo "  * Waiting for AuthPolicy to be created by operator..."
  for i in {1..60}; do
    if kubectl get authpolicy maas-api-auth-policy -n maas-api >/dev/null 2>&1; then
      kubectl patch authpolicy maas-api-auth-policy -n maas-api --type=merge --patch-file <(echo "
spec:
  rules:
    authentication:
      openshift-identities:
        kubernetesTokenReview:
          audiences:
            - $AUD
            - maas-default-gateway-sa")
      echo "  * AuthPolicy patched with custom audience."
      break
    fi
    sleep 5
  done
fi

echo
echo "## Fixing NetworkPolicy for Authorino"
echo "* Creating NetworkPolicy to allow Authorino ingress to opendatahub namespace..."
# The opendatahub NetworkPolicy blocks traffic from Authorino pods.
# This fix allows Authorino to communicate for AuthN/AuthZ flows.
cat <<EOF | kubectl apply -f -
kind: NetworkPolicy
apiVersion: networking.k8s.io/v1
metadata:
  name: opendatahub-authorino-allow
  namespace: opendatahub
spec:
  podSelector: {}
  ingress:
    - from:
        - namespaceSelector: {}
          podSelector:
            matchLabels:
              authorino-resource: authorino
  policyTypes:
    - Ingress
EOF

echo
echo "## Observability Setup (Optional)"
echo "* NOTE: Observability (Prometheus/Grafana integration) is not installed by default."
echo "* To enable observability, apply the observability manifests:"
echo "   kubectl apply --server-side=true \\"
echo "     -f <(kustomize build \"https://github.com/opendatahub-io/models-as-a-service.git/deployment/base/observability?ref=${MAAS_REF}\")"

echo ""
echo "========================================="
echo "Deployment is complete."
echo ""
echo "Next Steps:"
echo "1. Deploy a sample model:"
echo "   kubectl create namespace llm"
echo "   kustomize build 'https://github.com/opendatahub-io/models-as-a-service.git/docs/samples/models/simulator?ref=${MAAS_REF}' | kubectl apply -f -"
echo ""
echo "2. Get Gateway endpoint:"
echo "   CLUSTER_DOMAIN=\$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}' -n openshift-ingress)"
echo "   HOST=\"maas.\${CLUSTER_DOMAIN}\""
echo ""
echo "3. Get authentication token:"
echo "   TOKEN_RESPONSE=\$(curl -sSk  -H "Authorization: Bearer $TOKEN" --json '{\"expiration\": \"10m\"}' \"\${HOST}/maas-api/v1/tokens\")"
echo "   TOKEN=\$(echo \$TOKEN_RESPONSE | jq -r .token)"
echo "   echo \$TOKEN"
echo ""
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
echo "   curl https://raw.githubusercontent.com/opendatahub-io/models-as-a-service/refs/heads/${MAAS_REF}/scripts/validate-deployment.sh | sh -v -"
echo ""
echo "8. Check metrics generation:"
echo "   kubectl port-forward -n kuadrant-system svc/limitador-limitador 8080:8080 &"
echo "   curl http://localhost:8080/metrics | grep -E '(authorized_hits|authorized_calls|limited_calls)'"
echo ""
echo "9. Access Prometheus to view metrics:"
echo "   kubectl port-forward -n openshift-monitoring svc/prometheus-k8s 9090:9091 &"
echo "   # Open http://localhost:9090 in browser and search for: authorized_hits, authorized_calls, limited_calls"
echo ""
