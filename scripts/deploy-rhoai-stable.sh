#!/bin/bash
#
# deploy-rhoai-stable.sh - Deploy Red Hat OpenShift AI v3 with Models-as-a-Service standalone capability
#
# DESCRIPTION:
#   This script automates the deployment of Red Hat OpenShift AI (RHOAI) v3 along with
#   its required prerequisites and the Models-as-a-Service (MaaS) capability.
#
#   The deployment includes:
#   - cert-manager
#   - Leader Worker Set (LWS)
#   - Red Hat Connectivity Link
#   - RHOAI v3 with KServe for model serving
#   - MaaS standalone capability (Developer Preview)
#
# PREREQUISITES:
#   - OpenShift cluster v4.19.9+
#   - Cluster administrator privileges
#   - kubectl CLI tool configured and connected to cluster
#   - kustomize tool available in PATH
#   - jq tool for JSON processing
#
# USAGE:
#   ./deploy-rhoai-stable.sh
#
# NOTES:
#   - The script is idempotent for most operations
#   - No arguments are expected

set -e

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
  local rhoai_exists=$(checksubscriptionexists openshift-marketplace redhat-operators rhods-operator)
  if [[ $rhoai_exists -ne "0" ]]; then
    echo "* The RHOAI operator is present in the cluster. Skipping installation."
    return 0
  fi

  echo
  echo "* Installing RHOAI v3 operator..."

  cat <<EOF | kubectl apply -f -
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: openshift-ai-inference
  namespace: openshift-ingress
spec:
  gatewayClassName: openshift-default
  listeners:
  - name: http
    port: 80
    protocol: HTTP
    allowedRoutes:
      namespaces:
        from: All
  infrastructure:
    labels:
      serving.kserve.io/gateway: kserve-ingress-gateway
---
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
  echo "* Setting up RHOAI instance and letting it deploy asynchronously."

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

    # Components recommended for MaaS:
    dashboard:
      managementState: Managed
EOF
}

echo "## Installing prerequisites"

deploy_certmanager
deploy_lws
deploy_rhcl
deploy_rhoai

echo
echo "## Installing Models-as-a-Service"

export CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
export AUD="$(kubectl create token default --duration=10m 2>/dev/null | cut -d. -f2 | jq -Rr '@base64d | fromjson | .aud[0]' 2>/dev/null)"

echo "* Cluster domain: ${CLUSTER_DOMAIN}"
echo "* Cluster audience: ${AUD}"

cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Namespace
metadata:
  name: maas-api
EOF

if kubectl get namespace opendatahub >/dev/null 2>&1; then
  kubectl wait namespace/opendatahub --for=jsonpath='{.status.phase}'=Active --timeout=60s
else
  echo "* Waiting for opendatahub namespace to be created by the operator..."
  for i in {1..30}; do
    if kubectl get namespace opendatahub >/dev/null 2>&1; then
      kubectl wait namespace/opendatahub --for=jsonpath='{.status.phase}'=Active --timeout=60s
      break
    fi
    sleep 5
  done
fi

: "${MAAS_REF:=main}"
kubectl apply --server-side=true \
  -f <(kustomize build "https://github.com/opendatahub-io/models-as-a-service.git/deployment/overlays/openshift?ref=${MAAS_REF}" | \
       envsubst '$CLUSTER_DOMAIN')

if [[ -n "$AUD" && "$AUD" != "https://kubernetes.default.svc"  ]]; then
  echo "* Configuring audience in MaaS AuthPolicy"
  kubectl patch authpolicy maas-api-auth-policy -n maas-api --type=merge --patch-file <(echo "
spec:
  rules:
    authentication:
      openshift-identities:
        kubernetesTokenReview:
          audiences:
            - $AUD
            - maas-default-gateway-sa")
fi

# Patch maas-api Deployment with stable image
: "${MAAS_RHOAI_IMAGE:=v3.0.0}"
kubectl set image -n maas-api deployment/maas-api maas-api=registry.redhat.io/rhoai/odh-maas-api-rhel9:${MAAS_RHOAI_IMAGE}

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
echo "   CLUSTER_DOMAIN=\$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"
echo "   HOST=\"maas.\${CLUSTER_DOMAIN}\""
echo ""
echo "3. Get authentication token:"
echo "   TOKEN_RESPONSE=\$(curl -sSk --oauth2-bearer '\$(oc whoami -t)' --json '{\"expiration\": \"10m\"}' \"\${HOST}/maas-api/v1/tokens\")"
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
