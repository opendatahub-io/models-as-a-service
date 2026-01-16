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

PROJECT_ROOT=$(git rev-parse --show-toplevel 2>/dev/null || pwd)

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

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

export CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
export AUD="$(kubectl create token default --duration=10m 2>/dev/null | cut -d. -f2 | jq -Rr '@base64d | fromjson | .aud[0]' 2>/dev/null)"
export ENABLE_KEYCLOAK_IDP=${ENABLE_KEYCLOAK_IDP:-false}
# Get the TLS certificate secret name from the default ingress controller
export CERT_NAME=$(kubectl get ingresscontroller default -n openshift-ingress-operator -o jsonpath='{.spec.defaultCertificate.name}' 2>/dev/null)
if [[ -z "$CERT_NAME" ]]; then
  # No custom cert configured - discover the auto-generated secret from the router deployment
  CERT_NAME=$(kubectl get deployment router-default -n openshift-ingress -o jsonpath='{.spec.template.spec.volumes[?(@.name=="default-certificate")].secret.secretName}' 2>/dev/null)
fi

echo "* Cluster domain: ${CLUSTER_DOMAIN}"
echo "* Cluster audience: ${AUD}"
echo "* Keycloak IDP enabled: ${ENABLE_KEYCLOAK_IDP}"
echo "* TLS certificate: ${CERT_NAME}"

if [[ "${ENABLE_KEYCLOAK_IDP}" == "true" ]]; then
  export KEYCLOAK_HOST=${KEYCLOAK_HOST:-"keycloak.${CLUSTER_DOMAIN}"}
  export KEYCLOAK_REALM=${KEYCLOAK_REALM:-"maas"}
  export KEYCLOAK_CLIENT_ID=${KEYCLOAK_CLIENT_ID:-"maas-cli"}
  export KEYCLOAK_CLIENT_SECRET=${KEYCLOAK_CLIENT_SECRET:-"maas-cli-secret"}

  echo "* Keycloak issuer: https://${KEYCLOAK_HOST}/realms/${KEYCLOAK_REALM}"
  echo "* Keycloak client: ${KEYCLOAK_CLIENT_ID}"

  echo "* Deploying Keycloak..."
  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Namespace
metadata:
  name: keycloak-system
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: keycloak
  namespace: keycloak-system
spec:
  replicas: 1
  selector:
    matchLabels:
      app: keycloak
  template:
    metadata:
      labels:
        app: keycloak
    spec:
      containers:
      - name: keycloak
        image: quay.io/keycloak/keycloak:23.0.7
        args: ["start-dev"]
        env:
        - name: KEYCLOAK_ADMIN
          value: "admin"
        - name: KEYCLOAK_ADMIN_PASSWORD
          value: "admin123"
        - name: KC_PROXY
          value: "edge"
        - name: KC_HOSTNAME_STRICT
          value: "false"
        - name: KC_HOSTNAME_STRICT_HTTPS
          value: "false"
        - name: KC_HTTP_ENABLED
          value: "true"
        - name: KC_PROXY_ADDRESS_FORWARDING
          value: "true"
        - name: KC_HOSTNAME
          value: "${KEYCLOAK_HOST}"
        ports:
        - name: http
          containerPort: 8080
        readinessProbe:
          httpGet:
            path: /realms/master
            port: 8080
          initialDelaySeconds: 30
          periodSeconds: 10
        livenessProbe:
          httpGet:
            path: /realms/master
            port: 8080
          initialDelaySeconds: 60
          periodSeconds: 30
        resources:
          requests:
            memory: "512Mi"
            cpu: "50m"
          limits:
            memory: "2Gi"
            cpu: "1000m"
---
apiVersion: v1
kind: Service
metadata:
  name: keycloak
  namespace: keycloak-system
spec:
  selector:
    app: keycloak
  ports:
  - name: http
    port: 8080
    targetPort: 8080
---
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: keycloak
  namespace: keycloak-system
spec:
  host: ${KEYCLOAK_HOST}
  to:
    kind: Service
    name: keycloak
    weight: 100
  port:
    targetPort: http
  tls:
    termination: edge
    insecureEdgeTerminationPolicy: Allow
EOF
  kubectl rollout status deployment/keycloak -n keycloak-system --timeout=180s || true

  echo "* Configuring Keycloak realm from manifests..."
  kubectl apply -f "${PROJECT_ROOT}/keycloak/maas-realm-config.yaml"

  kubectl delete job keycloak-realm-import -n keycloak-system --ignore-not-found
  cat <<EOF | kubectl apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: keycloak-realm-import
  namespace: keycloak-system
spec:
  template:
    spec:
      restartPolicy: OnFailure
      containers:
      - name: realm-import
        image: curlimages/curl:8.5.0
        command: ["/bin/sh"]
        args:
        - -c
        - |
          set -euo pipefail
          KC_BASE="https://${KEYCLOAK_HOST}"
          REALM="${KEYCLOAK_REALM}"
          echo "Waiting for Keycloak at \$KC_BASE..."
          until curl -ks "\$KC_BASE/realms/master" >/dev/null; do sleep 5; done

          echo "Getting admin token..."
          ADMIN_TOKEN=\$(curl -ks -X POST \
            -H "Content-Type: application/x-www-form-urlencoded" \
            -d "username=admin&password=admin123&grant_type=password&client_id=admin-cli" \
            "\$KC_BASE/realms/master/protocol/openid-connect/token" | \
            grep -o '"access_token":"[^"]*' | cut -d':' -f2- | tr -d '"' )

          if [ -z "\$ADMIN_TOKEN" ]; then
            echo "Failed to get admin token"
            exit 1
          fi

          echo "Got admin token, importing realm..."
          cp /tmp/realm-config/maas-realm.json /tmp/maas-realm.rendered.json

          if curl -ks -f -H "Authorization: Bearer \$ADMIN_TOKEN" \
             "\$KC_BASE/admin/realms/\$REALM" >/dev/null 2>&1; then
            echo "Realm exists, updating..."
            curl -ks -X PUT \
              -H "Authorization: Bearer \$ADMIN_TOKEN" \
              -H "Content-Type: application/json" \
              -d @/tmp/maas-realm.rendered.json \
              "\$KC_BASE/admin/realms/\$REALM"
          else
            echo "Realm does not exist, creating..."
            curl -ks -X POST \
              -H "Authorization: Bearer \$ADMIN_TOKEN" \
              -H "Content-Type: application/json" \
              -d @/tmp/maas-realm.rendered.json \
              "\$KC_BASE/admin/realms"
          fi
          echo "Done!"
        volumeMounts:
        - name: realm-config
          mountPath: /tmp/realm-config
      volumes:
      - name: realm-config
        configMap:
          name: maas-realm-config
EOF
fi

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
kubectl apply --server-side=true --force-conflicts \
  -f <(kustomize build "https://github.com/opendatahub-io/models-as-a-service.git/deployment/overlays/openshift?ref=${MAAS_REF}" | \
       envsubst '$CLUSTER_DOMAIN $CERT_NAME')

if [[ "${ENABLE_KEYCLOAK_IDP}" != "true" && -n "$AUD" && "$AUD" != "https://kubernetes.default.svc"  ]]; then
  echo "* Configuring audience in MaaS AuthPolicy"
  kubectl patch authpolicy maas-api-auth-policy -n opendatahub --type=merge --patch-file <(echo "
spec:
  rules:
    authentication:
      openshift-identities:
        kubernetesTokenReview:
          audiences:
            - $AUD
            - maas-default-gateway-sa")
fi

# Wait for gateway deployment to be created and patch resource requirements
echo "* Waiting for gateway deployment to be created..."
for i in {1..30}; do
  if kubectl get deployment maas-default-gateway-openshift-default -n openshift-ingress >/dev/null 2>&1; then
    echo "  * Gateway deployment found, reducing CPU requirements for scheduling..."
    kubectl patch deployment maas-default-gateway-openshift-default -n openshift-ingress --type='strategic' --patch='
spec:
  template:
    spec:
      containers:
      - name: istio-proxy
        resources:
          requests:
            cpu: "25m"
            memory: "64Mi"
          limits:
            cpu: "1000m"
            memory: "512Mi"
' >/dev/null 2>&1
    break
  fi
  echo "  * Waiting for gateway deployment... ($i/30)"
  sleep 2
done

if [[ "${ENABLE_KEYCLOAK_IDP}" == "true" ]]; then
  export MAAS_API_NAMESPACE=opendatahub
  echo "* Configuring Keycloak IDP..."
  "${SCRIPT_DIR}/deploy-keycloak-idp.sh"
fi

echo "* Creating OpenShift Route for Gateway..."
cat <<EOF | kubectl apply -f -
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: maas-gateway-route
  namespace: openshift-ingress
  labels:
    app.kubernetes.io/name: maas
    app.kubernetes.io/component: gateway
spec:
  host: maas.${CLUSTER_DOMAIN}
  to:
    kind: Service
    name: maas-default-gateway-openshift-default
    weight: 100
  port:
    targetPort: 443
  tls:
    termination: passthrough
  wildcardPolicy: None
EOF

if [[ "${ENABLE_KEYCLOAK_IDP}" != "true" ]]; then
  # Patch maas-api Deployment with stable RHOAI image
  : "${MAAS_RHOAI_IMAGE:=v3.0.0}"
  kubectl set image -n opendatahub deployment/maas-api \
    maas-api="registry.redhat.io/rhoai/odh-maas-api-rhel9:${MAAS_RHOAI_IMAGE}"
fi

echo "* Waiting for KServe webhook to be ready (up to 2 minutes)..."
WEBHOOK_READY=false
for i in {1..24}; do
  ENDPOINTS=$(kubectl get endpoints kserve-webhook-server-service -n redhat-ods-applications -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null)
  if [[ -n "$ENDPOINTS" ]]; then
    echo "  * KServe webhook ready"
    WEBHOOK_READY=true
    break
  fi
  sleep 5
done
if [[ "$WEBHOOK_READY" != "true" ]]; then
  echo "  * WARNING: KServe webhook may not be ready yet. LLMInferenceService creation may fail until the webhook is available."
fi

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
if [[ "${ENABLE_KEYCLOAK_IDP}" == "true" ]]; then
  echo "3. Get authentication token:"
  echo "   KEYCLOAK_URL=\"https://keycloak.\${CLUSTER_DOMAIN}\""
  echo "   KEYCLOAK_REALM=\"maas\""
  echo "   KEYCLOAK_CLIENT_ID=\"maas-cli\""
  echo "   KEYCLOAK_CLIENT_SECRET=\"maas-cli-secret\""
  echo "   KEYCLOAK_USERNAME=\"freeuser1\""
  echo "   KEYCLOAK_PASSWORD=\"password123\""
  echo "   KEYCLOAK_ACCESS_TOKEN=\$(curl -sSk -H \"Content-Type: application/x-www-form-urlencoded\" -d \"grant_type=password\" -d \"client_id=\${KEYCLOAK_CLIENT_ID}\" -d \"client_secret=\${KEYCLOAK_CLIENT_SECRET}\" -d \"username=\${KEYCLOAK_USERNAME}\" -d \"password=\${KEYCLOAK_PASSWORD}\" \"\${KEYCLOAK_URL}/realms/\${KEYCLOAK_REALM}/protocol/openid-connect/token\" | jq -r .access_token)"
  echo "   TOKEN=\$(curl -sSk -H \"Authorization: Bearer \${KEYCLOAK_ACCESS_TOKEN}\" -H \"Content-Type: application/json\" -X POST -d '{\"expiration\": \"24h\"}' \"https://\${HOST}/maas-api/v1/tokens\" | jq -r .token)"
  echo ""
  echo "4. List models (Keycloak token) and call inference (MaaS token):"
  echo "   MODELS=\$(curl -sSk \"https://\${HOST}/maas-api/v1/models\" -H \"Content-Type: application/json\" -H \"Authorization: Bearer \${KEYCLOAK_ACCESS_TOKEN}\")"
  echo "   MODEL_NAME=\$(echo \$MODELS | jq -r '.data[0].id')"
  echo "   MODEL_URL=\$(echo \$MODELS | jq -r '.data[0].url')"
  echo "   curl -sSk -H \"Authorization: Bearer \${TOKEN}\" -H \"Content-Type: application/json\" -d \"{\\\"model\\\": \\\"\${MODEL_NAME}\\\", \\\"messages\\\": [{\\\"role\\\": \\\"user\\\", \\\"content\\\": \\\"Hello\\\"}], \\\"max_tokens\\\": 50}\" \"\${MODEL_URL}/v1/chat/completions\""
  echo ""
  echo "5. Test authorization limiting (no token 401 error):"
  echo "   curl -sSk -H \"Content-Type: application/json\" -d \"{\\\"model\\\": \\\"\${MODEL_NAME}\\\", \\\"messages\\\": [{\\\"role\\\": \\\"user\\\", \\\"content\\\": \\\"Hello\\\"}], \\\"max_tokens\\\": 50}\" \"\${MODEL_URL}/v1/chat/completions\" -v"
  echo ""
  echo "6. Test rate limiting (200 OK followed by 429 Rate Limit Exceeded after about 4 requests):"
  echo "   for i in {1..16}; do curl -sSk -o /dev/null -w \"%{http_code}\\n\" -H \"Authorization: Bearer \${TOKEN}\" -H \"Content-Type: application/json\" -d \"{\\\"model\\\": \\\"\${MODEL_NAME}\\\", \\\"messages\\\": [{\\\"role\\\": \\\"user\\\", \\\"content\\\": \\\"Hello\\\"}], \\\"max_tokens\\\": 50}\" \"\${MODEL_URL}/v1/chat/completions\"; done"
else
  echo "3. Get authentication token:"
  echo "   TOKEN_RESPONSE=\$(curl -sSk --oauth2-bearer '\$(oc whoami -t)' --json '{\"expiration\": \"24h\"}' \"\${HOST}/maas-api/v1/tokens\")"
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
fi
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
