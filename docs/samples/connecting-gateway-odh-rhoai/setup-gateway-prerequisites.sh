#!/bin/bash
################################################################################
# Setup MaaS Gateway Prerequisites
#
# Installs the prerequisites that the ODH/RHOAI operator does not create
# when modelsAsService: Managed is enabled. This script is idempotent —
# it checks for existing resources before creating them.
#
# What this script does:
#   1. Installs Kuadrant operator (with Gateway controller env patch)
#   2. Installs cert-manager and LeaderWorkerSet operators
#   3. Creates the GatewayClass and maas-default-gateway
#   4. Deploys a POC PostgreSQL database for maas-api
#   5. Applies supplemental RBAC for maas-api and maas-controller
#   6. Configures TLS for Authorino ↔ maas-api
#   7. Applies workarounds for operator bugs:
#      - Removes unsupported --cluster-audience flag from maas-controller
#      - Fixes maas-api selector label conflict with tenant reconciler
#
# Prerequisites:
#   - OpenShift 4.14+ cluster
#   - oc / kubectl with cluster-admin access
#   - ODH or RHOAI operator installed with modelsAsService: Managed
#   - Run from repository root (scripts reference repo files)
#
# Usage:
#   ./docs/samples/connecting-gateway-odh-rhoai/setup-gateway-prerequisites.sh
#   NAMESPACE=redhat-ods-applications ./docs/samples/connecting-gateway-odh-rhoai/setup-gateway-prerequisites.sh
#
# For full documentation see:
#   docs/samples/connecting-gateway-odh-rhoai/odh-gateway-adding.md
################################################################################

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Locate the project root (walk up until we find .git)
PROJECT_ROOT="$SCRIPT_DIR"
while [[ "$PROJECT_ROOT" != "/" && ! -d "$PROJECT_ROOT/.git" ]]; do
  PROJECT_ROOT="$(dirname "$PROJECT_ROOT")"
done
if [[ ! -d "$PROJECT_ROOT/.git" ]]; then
  echo "ERROR: Could not find project root (no .git directory). Run from the repo." >&2
  exit 1
fi

SCRIPTS_DIR="${PROJECT_ROOT}/scripts"

# shellcheck source=scripts/deployment-helpers.sh
source "${SCRIPTS_DIR}/deployment-helpers.sh"

: "${NAMESPACE:=opendatahub}"
: "${KUADRANT_NAMESPACE:=kuadrant-system}"
: "${KUADRANT_CATALOG_IMAGE:=quay.io/kuadrant/kuadrant-operator-catalog:v1.4.2}"

echo ""
echo "┏━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┓"
echo "┃  MaaS Gateway Prerequisites Setup                                  ┃"
echo "┃  Namespace: ${NAMESPACE}"
echo "┗━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━┛"
echo ""

#──────────────────────────────────────────────────────────────
# 1. Kuadrant
#──────────────────────────────────────────────────────────────

install_kuadrant() {
  echo "━━━ Step 1: Kuadrant operator ━━━"

  if kubectl get subscription.operators.coreos.com kuadrant-operator -n "$KUADRANT_NAMESPACE" &>/dev/null; then
    echo "  Kuadrant subscription already exists, skipping install"
  else
    echo "  Installing Kuadrant operator..."
    kubectl create namespace "$KUADRANT_NAMESPACE" 2>/dev/null || true

    kubectl apply -f - <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: kuadrant-operator-catalog
  namespace: ${KUADRANT_NAMESPACE}
spec:
  sourceType: grpc
  image: ${KUADRANT_CATALOG_IMAGE}
  displayName: Kuadrant Operator Catalog
  publisher: Kuadrant
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: kuadrant-operator-group
  namespace: ${KUADRANT_NAMESPACE}
spec: {}
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: kuadrant-operator
  namespace: ${KUADRANT_NAMESPACE}
spec:
  channel: stable
  name: kuadrant-operator
  source: kuadrant-operator-catalog
  sourceNamespace: ${KUADRANT_NAMESPACE}
EOF

    echo "  Waiting for Kuadrant subscription..."
    kubectl wait --for=jsonpath='{.status.state}'=AtLatestKnown \
      subscription/kuadrant-operator -n "$KUADRANT_NAMESPACE" --timeout="${SUBSCRIPTION_TIMEOUT}s"
  fi

  # Wait for deployment regardless (may exist but not yet ready)
  echo "  Waiting for Kuadrant operator deployment..."
  wait_for_resource deployment kuadrant-operator-controller-manager "$KUADRANT_NAMESPACE" 180
  kubectl wait --for=condition=Available --timeout="${ROLLOUT_TIMEOUT}s" \
    deployment/kuadrant-operator-controller-manager -n "$KUADRANT_NAMESPACE"

  # Patch CSV for OpenShift Gateway controller
  patch_kuadrant_csv "$KUADRANT_NAMESPACE" "kuadrant-operator"

  # Create Kuadrant CR if it doesn't exist
  if kubectl get kuadrant kuadrant -n "$KUADRANT_NAMESPACE" &>/dev/null; then
    echo "  Kuadrant CR already exists"
  else
    echo "  Creating Kuadrant CR..."
    kubectl apply -f "${SCRIPTS_DIR}/data/kuadrant.yaml" -n "$KUADRANT_NAMESPACE"
  fi

  echo "  ✓ Kuadrant ready"
  echo ""
}

#──────────────────────────────────────────────────────────────
# 2. cert-manager + LeaderWorkerSet
#──────────────────────────────────────────────────────────────

install_optional_operators() {
  echo "━━━ Step 2: cert-manager and LeaderWorkerSet operators ━━━"

  local data_dir="${SCRIPTS_DIR}/data"

  # cert-manager
  if is_operator_installed "openshift-cert-manager-operator" "cert-manager-operator"; then
    echo "  cert-manager already installed"
  else
    echo "  Installing cert-manager..."
    kubectl apply -f "${data_dir}/cert-manager-subscription.yaml"
    echo "  Waiting for cert-manager subscription..."
    kubectl wait --for=jsonpath='{.status.state}'=AtLatestKnown \
      subscription.operators.coreos.com/openshift-cert-manager-operator \
      -n cert-manager-operator --timeout="${SUBSCRIPTION_TIMEOUT}s"
  fi

  # LWS
  if is_operator_installed "leader-worker-set" "openshift-lws-operator"; then
    echo "  LeaderWorkerSet already installed"
  else
    echo "  Installing LeaderWorkerSet..."
    kubectl apply -f "${data_dir}/lws-subscription.yaml"
    echo "  Waiting for LWS subscription..."
    kubectl wait --for=jsonpath='{.status.state}'=AtLatestKnown \
      subscription.operators.coreos.com/leader-worker-set \
      -n openshift-lws-operator --timeout="${SUBSCRIPTION_TIMEOUT}s"
  fi

  # Activate LWS controller
  if kubectl get leaderworkersetoperator cluster -n openshift-lws-operator &>/dev/null; then
    echo "  LWS controller already activated"
  else
    echo "  Activating LWS controller..."
    kubectl apply -f "${data_dir}/lws-operator-cr.yaml"
  fi

  echo "  Waiting for LWS CRD..."
  wait_for_crd "leaderworkersets.leaderworkerset.x-k8s.io" "$CRD_TIMEOUT"

  echo "  ✓ Optional operators ready"
  echo ""
}

#──────────────────────────────────────────────────────────────
# 3. GatewayClass + maas-default-gateway
#──────────────────────────────────────────────────────────────

setup_gateway() {
  echo "━━━ Step 3: GatewayClass and maas-default-gateway ━━━"

  local data_dir="${SCRIPTS_DIR}/data"

  # GatewayClass
  if kubectl get gatewayclass openshift-default &>/dev/null; then
    echo "  GatewayClass openshift-default already exists"
  else
    echo "  Creating GatewayClass openshift-default..."
    kubectl apply -f "${data_dir}/gatewayclass.yaml"
  fi

  # Gateway
  if kubectl get gateway maas-default-gateway -n openshift-ingress &>/dev/null; then
    echo "  maas-default-gateway already exists"
  else
    echo "  Detecting cluster domain..."
    local cluster_domain
    cluster_domain=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
    echo "  Cluster domain: ${cluster_domain}"

    # Detect TLS certificate
    local cert_name=""
    cert_name=$(kubectl get ingresscontroller default -n openshift-ingress-operator \
      -o jsonpath='{.spec.defaultCertificate.name}' 2>/dev/null || echo "")

    if [[ -z "$cert_name" ]] || ! kubectl get secret -n openshift-ingress "$cert_name" &>/dev/null; then
      cert_name=""
      for candidate in router-certs-default default-gateway-cert; do
        if kubectl get secret -n openshift-ingress "$candidate" &>/dev/null; then
          cert_name="$candidate"
          break
        fi
      done
    fi

    if [[ -z "$cert_name" ]]; then
      echo "  No TLS certificate found, creating self-signed..."
      if create_tls_secret "maas-gateway-tls" "openshift-ingress" "maas.${cluster_domain}"; then
        cert_name="maas-gateway-tls"
      else
        echo "  ERROR: Failed to create TLS certificate" >&2
        return 1
      fi
    fi
    echo "  TLS certificate: ${cert_name}"

    echo "  Creating maas-default-gateway..."
    export CLUSTER_DOMAIN="$cluster_domain"
    export CERT_NAME="$cert_name"

    local maas_networking_dir="${PROJECT_ROOT}/deployment/base/networking/maas"
    if [[ -d "$maas_networking_dir" ]]; then
      kustomize build "$maas_networking_dir" | envsubst '$CLUSTER_DOMAIN $CERT_NAME' | \
        kubectl apply --server-side=true -f -
    else
      kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: maas-default-gateway
  namespace: openshift-ingress
  annotations:
    opendatahub.io/managed: "false"
    security.opendatahub.io/authorino-tls-bootstrap: "true"
  labels:
    app.kubernetes.io/name: maas
    app.kubernetes.io/instance: maas-default-gateway
    app.kubernetes.io/component: gateway
spec:
  gatewayClassName: openshift-default
  listeners:
  - name: http
    hostname: "maas.${cluster_domain}"
    port: 80
    protocol: HTTP
    allowedRoutes:
      namespaces:
        from: All
  - name: https
    hostname: "maas.${cluster_domain}"
    port: 443
    protocol: HTTPS
    allowedRoutes:
      namespaces:
        from: All
    tls:
      certificateRefs:
      - group: ""
        kind: Secret
        name: "${cert_name}"
      mode: Terminate
EOF
    fi
  fi

  echo "  Waiting for gateway to become Programmed..."
  kubectl wait --for=condition=Programmed gateway/maas-default-gateway \
    -n openshift-ingress --timeout="${CUSTOM_CHECK_TIMEOUT}s"

  echo "  ✓ Gateway ready"
  echo ""
}

#──────────────────────────────────────────────────────────────
# 4. PostgreSQL
#──────────────────────────────────────────────────────────────

setup_database() {
  echo "━━━ Step 4: PostgreSQL database ━━━"

  if kubectl get secret maas-db-config -n "$NAMESPACE" &>/dev/null; then
    echo "  maas-db-config secret already exists, skipping database setup"
  else
    NAMESPACE="$NAMESPACE" "${SCRIPTS_DIR}/setup-database.sh"
  fi

  echo "  ✓ Database ready"
  echo ""
}

#──────────────────────────────────────────────────────────────
# 5. Supplemental RBAC
#──────────────────────────────────────────────────────────────

apply_supplemental_rbac() {
  echo "━━━ Step 5: Supplemental RBAC ━━━"

  local rbac_dir="${PROJECT_ROOT}/deployment/base/maas-api/rbac"
  local crd_dir="${PROJECT_ROOT}/deployment/base/maas-controller/crd/bases"

  # Apply latest CRDs from the repo (operator may ship older versions)
  if [[ -d "$crd_dir" ]]; then
    echo "  Applying MaaS CRDs..."
    kubectl apply --server-side=true --force-conflicts -f "$crd_dir/"
  fi

  # maas-api supplemental RBAC
  echo "  Applying maas-api supplemental RBAC..."
  kubectl apply -f "${rbac_dir}/supplemental-clusterrole.yaml" \
                -f "${rbac_dir}/supplemental-clusterrolebinding.yaml"

  # maas-controller supplemental RBAC (the operator's bundled ClusterRole is incomplete)
  echo "  Applying maas-controller supplemental RBAC..."
  local controller_rbac="${PROJECT_ROOT}/deployment/base/maas-controller/rbac/clusterrole.yaml"

  # Apply the repo's full ClusterRole under a separate name to avoid fighting the operator's version.
  # A simple name substitution is sufficient — the file is a single ClusterRole with only one name field.
  if [[ -f "$controller_rbac" ]]; then
    sed 's/name: maas-controller-role/name: maas-controller-supplemental/' "$controller_rbac" | \
      kubectl apply --server-side=true --force-conflicts -f -

    kubectl apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: maas-controller-supplemental
  labels:
    app.kubernetes.io/name: maas-controller
    app.kubernetes.io/component: rbac
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: maas-controller-supplemental
subjects:
- kind: ServiceAccount
  name: maas-controller
  namespace: ${NAMESPACE}
EOF
  fi

  # Restart maas-api to pick up new RBAC and database secret
  if kubectl get deployment maas-api -n "$NAMESPACE" &>/dev/null; then
    echo "  Restarting maas-api..."
    kubectl rollout restart deployment/maas-api -n "$NAMESPACE"
    kubectl rollout status deployment/maas-api -n "$NAMESPACE" --timeout="${ROLLOUT_TIMEOUT}s"
  fi

  echo "  ✓ RBAC applied"
  echo ""
}

#──────────────────────────────────────────────────────────────
# 6. Authorino TLS
#──────────────────────────────────────────────────────────────

setup_tls() {
  echo "━━━ Step 6: Authorino TLS ━━━"

  # Wait for Authorino to exist (Kuadrant creates it)
  if ! kubectl get authorino -n "$KUADRANT_NAMESPACE" &>/dev/null; then
    echo "  Waiting for Authorino CR..."
    wait_for_resource authorino authorino "$KUADRANT_NAMESPACE" "${AUTHORINO_TIMEOUT}"
  fi

  AUTHORINO_NAMESPACE="$KUADRANT_NAMESPACE" "${SCRIPTS_DIR}/setup-authorino-tls.sh"

  echo "  Restarting Authorino and maas-api..."
  kubectl rollout restart deployment/authorino -n "$KUADRANT_NAMESPACE"
  kubectl rollout status deployment/authorino -n "$KUADRANT_NAMESPACE" --timeout="${ROLLOUT_TIMEOUT}s"

  if kubectl get deployment maas-api -n "$NAMESPACE" &>/dev/null; then
    kubectl rollout restart deployment/maas-api -n "$NAMESPACE"
    kubectl rollout status deployment/maas-api -n "$NAMESPACE" --timeout="${ROLLOUT_TIMEOUT}s"
  fi

  echo "  ✓ TLS configured"
  echo ""
}

#──────────────────────────────────────────────────────────────
# 7. Operator bug workarounds
#──────────────────────────────────────────────────────────────

apply_operator_workarounds() {
  echo "━━━ Step 7: Operator bug workarounds ━━━"

  # Workaround 1: maas-controller crashes with "--cluster-audience" flag.
  #
  # The operator passes --cluster-audience=$(CLUSTER_AUDIENCE) as a CLI arg to
  # maas-controller, but the binary doesn't accept that flag (it auto-detects
  # the audience from the cluster). The cluster-audience value in params.env is
  # a kustomize parameter for AuthPolicy, not a controller arg.
  # See: deployment/base/maas-controller/manager/manager.yaml (no --cluster-audience)
  if kubectl get deployment maas-controller -n "$NAMESPACE" &>/dev/null; then
    local current_args
    current_args=$(kubectl get deployment maas-controller -n "$NAMESPACE" \
      -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null || echo "")

    if echo "$current_args" | grep -q -- "--cluster-audience"; then
      echo "  Fixing maas-controller args (removing unsupported --cluster-audience flag)..."
      kubectl annotate deployment maas-controller -n "$NAMESPACE" \
        opendatahub.io/managed=false --overwrite

      # Find the 0-based index of the --cluster-audience arg and remove only that element
      local arg_idx
      arg_idx=$(kubectl get deployment maas-controller -n "$NAMESPACE" \
        -o jsonpath='{range .spec.template.spec.containers[0].args[*]}{@}{"\n"}{end}' | \
        grep -n -- "^--cluster-audience" | head -1 | cut -d: -f1)

      if [[ -n "$arg_idx" ]]; then
        arg_idx=$((arg_idx - 1))
        kubectl patch deployment maas-controller -n "$NAMESPACE" --type='json' \
          -p="[{\"op\":\"remove\",\"path\":\"/spec/template/spec/containers/0/args/$arg_idx\"}]"
      fi
      kubectl rollout status deployment/maas-controller -n "$NAMESPACE" --timeout="${ROLLOUT_TIMEOUT}s"
    else
      echo "  maas-controller args OK (no --cluster-audience)"
    fi
  fi

  # Workaround 2: maas-api deployment has extra selector labels.
  #
  # The operator adds app.opendatahub.io/modelsasservice to spec.selector.matchLabels,
  # but maas-controller's tenant reconciler uses different labels (from the repo's
  # kustomize base). Since spec.selector is immutable, the controller can't update
  # the deployment and tenant reconciliation fails.
  # See: maas-controller/pkg/platform/tenantreconcile/kustomize.go
  #   "Merges labels into metadata only (not into Deployment selectors)"
  if kubectl get deployment maas-api -n "$NAMESPACE" &>/dev/null; then
    local api_selector
    api_selector=$(kubectl get deployment maas-api -n "$NAMESPACE" \
      -o jsonpath='{.spec.selector.matchLabels}' 2>/dev/null || echo "")

    if echo "$api_selector" | grep -q "modelsasservice"; then
      echo "  Fixing maas-api selector labels (operator added extra labels that conflict with controller)..."

      # Determine the ODH operator namespace and deployment name
      local odh_ns odh_deploy
      odh_deploy=$(kubectl get deployment -A --no-headers 2>/dev/null | \
        grep "opendatahub-operator-controller-manager\|rhods-operator" | head -1 | awk '{print $1, $2}')
      odh_ns=$(echo "$odh_deploy" | awk '{print $1}')
      odh_deploy=$(echo "$odh_deploy" | awk '{print $2}')

      local odh_replicas=""
      local _odh_scaled=false

      if [[ -n "$odh_ns" && -n "$odh_deploy" ]]; then
        odh_replicas=$(kubectl get deployment/"$odh_deploy" -n "$odh_ns" \
          -o jsonpath='{.spec.replicas}' 2>/dev/null || echo "3")

        # Restore operator replicas on early exit
        # shellcheck disable=SC2317
        _restore_operator() {
          if [[ "$_odh_scaled" == "true" && -n "$odh_ns" && -n "$odh_deploy" ]]; then
            echo "  Restoring operator replicas ($odh_replicas)..."
            kubectl scale deployment/"$odh_deploy" -n "$odh_ns" --replicas="$odh_replicas" 2>/dev/null || true
          fi
        }
        trap _restore_operator EXIT

        echo "  Scaling down operator ($odh_deploy in $odh_ns, was $odh_replicas replicas)..."
        kubectl scale deployment/"$odh_deploy" -n "$odh_ns" --replicas=0
        _odh_scaled=true
        sleep 5
      fi

      kubectl delete deployment maas-api -n "$NAMESPACE" --wait=true

      echo "  Restarting maas-controller to recreate maas-api with correct labels..."
      kubectl rollout restart deployment/maas-controller -n "$NAMESPACE"
      kubectl rollout status deployment/maas-controller -n "$NAMESPACE" --timeout="${ROLLOUT_TIMEOUT}s"

      echo "  Waiting for controller to recreate maas-api..."
      wait_for_resource deployment maas-api "$NAMESPACE" 60
      kubectl rollout status deployment/maas-api -n "$NAMESPACE" --timeout="${ROLLOUT_TIMEOUT}s"

      kubectl annotate deployment maas-api -n "$NAMESPACE" \
        opendatahub.io/managed=false --overwrite

      if [[ "$_odh_scaled" == "true" ]]; then
        echo "  Scaling operator back up ($odh_replicas replicas)..."
        kubectl scale deployment/"$odh_deploy" -n "$odh_ns" --replicas="$odh_replicas"
        _odh_scaled=false
        trap - EXIT
      fi
    else
      echo "  maas-api selector labels OK"
    fi
  fi

  echo "  ✓ Workarounds applied"
  echo ""
}

#──────────────────────────────────────────────────────────────
# Verify
#──────────────────────────────────────────────────────────────

verify() {
  echo "━━━ Verification ━━━"
  echo ""

  local ok=true

  # Gateway
  local gw_status
  gw_status=$(kubectl get gateway maas-default-gateway -n openshift-ingress \
    -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null || echo "")
  if [[ "$gw_status" == "True" ]]; then
    echo "  ✓ Gateway:     Programmed"
  else
    echo "  ✗ Gateway:     NOT Programmed (${gw_status:-not found})"
    ok=false
  fi

  # Kuadrant
  local kq_status
  kq_status=$(kubectl get kuadrant kuadrant -n "$KUADRANT_NAMESPACE" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
  if [[ "$kq_status" == "True" ]]; then
    echo "  ✓ Kuadrant:    Ready"
  else
    echo "  ✗ Kuadrant:    NOT Ready (${kq_status:-not found})"
    ok=false
  fi

  # maas-api
  local api_ready
  api_ready=$(kubectl get deployment maas-api -n "$NAMESPACE" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
  if [[ "$api_ready" -ge 1 ]]; then
    echo "  ✓ maas-api:    Running (${api_ready} replicas)"
  else
    echo "  ✗ maas-api:    NOT Ready"
    ok=false
  fi

  # maas-controller
  local ctrl_ready
  ctrl_ready=$(kubectl get deployment maas-controller -n "$NAMESPACE" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")
  if [[ "$ctrl_ready" -ge 1 ]]; then
    echo "  ✓ maas-ctrl:   Running (${ctrl_ready} replicas)"
  else
    echo "  ✗ maas-ctrl:   NOT Ready"
    ok=false
  fi

  # Tenant
  local tenant_ready
  tenant_ready=$(kubectl get tenant default-tenant -n models-as-a-service \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
  if [[ "$tenant_ready" == "True" ]]; then
    echo "  ✓ Tenant:      Ready"
  else
    echo "  ○ Tenant:      ${tenant_ready:-not found yet} (may reconcile later)"
  fi

  # Database
  if kubectl get secret maas-db-config -n "$NAMESPACE" &>/dev/null; then
    echo "  ✓ Database:    Configured"
  else
    echo "  ✗ Database:    maas-db-config secret missing"
    ok=false
  fi

  echo ""
  if [[ "$ok" == "true" ]]; then
    echo "✅ All prerequisites are in place."
    echo ""
    echo "Verify with:"
    echo "  DOMAIN=\$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"
    echo "  TOKEN=\$(oc whoami -t)"
    echo "  curl -sk -w '%{http_code}\\n' \"https://maas.\${DOMAIN}/maas-api/v1/models\"           # expect 401"
    echo "  curl -sk -w '%{http_code}\\n' -H \"Authorization: Bearer \${TOKEN}\" \"https://maas.\${DOMAIN}/maas-api/v1/models\"  # expect 200"
    echo ""
    echo "To deploy a model (optional):"
    echo "  kustomize build docs/samples/models/facebook-opt-125m-cpu | kubectl apply --server-side -f -"
    echo "  See docs/samples/connecting-gateway-odh-rhoai/odh-gateway-adding.md#deploy-a-model-and-verify-end-to-end"
  else
    echo "⚠️  Some components are not ready. Check the output above for details."
  fi
  echo ""
}

#──────────────────────────────────────────────────────────────
# Main
#──────────────────────────────────────────────────────────────

main() {
  install_kuadrant
  install_optional_operators
  setup_gateway
  setup_database
  apply_supplemental_rbac
  setup_tls
  apply_operator_workarounds
  verify
}

main "$@"
