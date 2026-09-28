#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/deployment-helpers.sh"

HELM_VERSION="${HELM_VERSION:-v3.17.3}"

ensure_helm() {
  if command -v helm &>/dev/null; then
    log_debug "helm found: $(helm version --short 2>/dev/null)"
    return 0
  fi

  log_info "helm not found, installing v${HELM_VERSION#v}..."
  local os arch
  os="$(uname -s | tr '[:upper:]' '[:lower:]')"
  arch="$(uname -m)"
  case "$arch" in
    x86_64) arch="amd64" ;;
    aarch64|arm64) arch="arm64" ;;
  esac

  local tmp_dir
  tmp_dir="$(mktemp -d)"
  local url="https://get.helm.sh/helm-${HELM_VERSION}-${os}-${arch}.tar.gz"
  if ! curl -fsSL "$url" | tar xz -C "$tmp_dir" 2>/dev/null; then
    log_error "Failed to download helm from $url"
    return 1
  fi
  install -m 0755 "$tmp_dir/${os}-${arch}/helm" /usr/local/bin/helm 2>/dev/null \
    || install -m 0755 "$tmp_dir/${os}-${arch}/helm" "${SCRIPT_DIR}/../bin/helm"
  rm -rf "$tmp_dir"

  export PATH="${SCRIPT_DIR}/../bin:${PATH}"
  log_info "helm installed: $(helm version --short 2>/dev/null)"
}

ODH_GITOPS_REPO="${ODH_GITOPS_REPO:-https://github.com/opendatahub-io/odh-gitops.git}"
ODH_GITOPS_BRANCH="${ODH_GITOPS_BRANCH:-main}"
# Pin the default deployment to the reviewed odh-gitops main commit. Set this
# explicitly (or leave it empty) when testing a different repository/branch.
ODH_GITOPS_COMMIT="${ODH_GITOPS_COMMIT:-fadfb31e89ae898b04a9a75bfa8bb91588d7e625}"
ODH_GITOPS_CHART_PATH="${ODH_GITOPS_CHART_PATH:-}"
HELM_RELEASE_NAME="${HELM_RELEASE_NAME:-rhoai-deps}"
HELM_NAMESPACE="${HELM_NAMESPACE:-rhoai-deps}"

OPERATOR_TYPE="${OPERATOR_TYPE:-odh}"
DEPLOY_MODE="${DEPLOY_MODE:-operator}"
# Current names are loadbalancer and ocproute. Keep the legacy route and
# clusterip aliases for callers that have not migrated yet.
INGRESS_MODE="${INGRESS_MODE:-route}"
POLICY_ENGINE="${POLICY_ENGINE:-}"
MAAS_CONTROLLER_IMAGE="${MAAS_CONTROLLER_IMAGE:-}"
MAAS_API_IMAGE="${MAAS_API_IMAGE:-}"
AI_GATEWAY_OPERATOR_IMAGE="${AI_GATEWAY_OPERATOR_IMAGE:-}"
PAYLOAD_PROCESSING_IMAGE="${PAYLOAD_PROCESSING_IMAGE:-}"
OPERATOR_CATALOG="${OPERATOR_CATALOG:-}"
OPERATOR_IMAGE="${OPERATOR_IMAGE:-}"
OPERATOR_CHANNEL="${OPERATOR_CHANNEL:-}"
OPERATOR_STARTING_CSV="${OPERATOR_STARTING_CSV:-}"
OPERATOR_INSTALL_PLAN_APPROVAL="${OPERATOR_INSTALL_PLAN_APPROVAL:-}"
RHCL_STARTING_CSV="${RHCL_STARTING_CSV:-}"
RHCL_NAMESPACE="${RHCL_NAMESPACE:-kuadrant-system}"

CRD_WAIT_TIMEOUT="${CRD_WAIT_TIMEOUT:-600}"

_CLONE_TMP_DIR=""
trap '[[ -n "$_CLONE_TMP_DIR" ]] && rm -rf "$_CLONE_TMP_DIR"' EXIT

resolve_chart_path() {
  if [[ -n "$ODH_GITOPS_CHART_PATH" ]]; then
    CHART_PATH="$ODH_GITOPS_CHART_PATH"
    log_info "Using chart from ODH_GITOPS_CHART_PATH: $CHART_PATH"
    return
  fi

  _CLONE_TMP_DIR="$(mktemp -d)"

  log_info "Cloning odh-gitops chart (branch: $ODH_GITOPS_BRANCH, commit: $ODH_GITOPS_COMMIT)..."
  git clone --depth 1 --branch "$ODH_GITOPS_BRANCH" "$ODH_GITOPS_REPO" "$_CLONE_TMP_DIR/odh-gitops" 2>&1 | tail -1
  if [[ -n "$ODH_GITOPS_COMMIT" ]]; then
    # The shallow branch clone may not contain a pinned commit after main has
    # advanced, so fetch that object explicitly before checking it out.
    git -C "$_CLONE_TMP_DIR/odh-gitops" fetch --depth 1 origin "$ODH_GITOPS_COMMIT"
    git -C "$_CLONE_TMP_DIR/odh-gitops" checkout --quiet --detach "$ODH_GITOPS_COMMIT"
  fi
  CHART_PATH="$_CLONE_TMP_DIR/odh-gitops/charts/rhai-on-openshift-chart"

  if [[ ! -f "$CHART_PATH/Chart.yaml" ]]; then
    log_error "Chart.yaml not found at $CHART_PATH"
    return 1
  fi
  log_info "Chart resolved at: $CHART_PATH"
}

build_helm_sets() {
  HELM_SETS=(
    --set profile=maas
    --set "operator.type=${OPERATOR_TYPE}"
    --set components.kserve.dsc.rawDeploymentServiceConfig=Headed
    --set-json 'components.aigateway.modelsAsAService.gateway.spec.listeners=[{"name":"https","port":443,"protocol":"HTTPS","allowedRoutes":{"namespaces":{"from":"All"}}}]'
    --set-json 'components.kserve.gateway.spec.listeners=[{"name":"https","port":443,"protocol":"HTTPS","allowedRoutes":{"namespaces":{"from":"All"}}}]'
  )

  if [[ "$DEPLOY_MODE" == "kustomize" ]]; then
    HELM_SETS+=(
      --set components.aigateway.dsc.managementState=Removed
      --set components.aigateway.dsc.modelsAsAService.managementState=Removed
      --set components.aigateway.modelsAsAService.gatewayClass.create=true
      --set components.aigateway.modelsAsAService.gateway.create=true
      --set dependencies.rhcl.enabled=true
    )
  fi

  case "$INGRESS_MODE" in
    # "clusterip" was the old name for this mode. Keep accepting it while
    # using the current "ocproute" name from the e2e deployment scripts.
    ocproute|clusterip)
      local cluster_domain="${CLUSTER_DOMAIN:-}"
      if [[ -z "$cluster_domain" ]]; then
        cluster_domain=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null || true)
      fi
      if [[ -z "$cluster_domain" ]]; then
        log_error "Could not determine the OpenShift ingress domain for ${INGRESS_MODE} mode"
        return 1
      fi
      HELM_SETS+=(
        --set components.aigateway.modelsAsAService.gatewayClass.create=true
        --set components.aigateway.modelsAsAService.gatewayClass.name=openshift-default
        --set components.aigateway.modelsAsAService.gateway.spec.gatewayClassName=openshift-default
        --set components.aigateway.modelsAsAService.gateway.openshiftRoute.enabled=true
        --set "components.aigateway.modelsAsAService.gateway.openshiftRoute.host=maas.${cluster_domain}"
      )
      ;;
    route|loadbalancer)
      # The chart defaults to the external Gateway/load-balancer topology.
      ;;
    *)
      log_error "Invalid ingress mode: $INGRESS_MODE (expected loadbalancer or ocproute; legacy aliases route and clusterip are also accepted)"
      return 1
      ;;
  esac

  if [[ -n "$OPERATOR_CHANNEL" ]]; then
    HELM_SETS+=(--set "operator.${OPERATOR_TYPE}.olm.channel=${OPERATOR_CHANNEL}")
  fi

  if [[ -n "$OPERATOR_STARTING_CSV" ]]; then
    HELM_SETS+=(--set "operator.${OPERATOR_TYPE}.olm.version=${OPERATOR_STARTING_CSV}")
  fi

  if [[ -n "$OPERATOR_INSTALL_PLAN_APPROVAL" ]]; then
    HELM_SETS+=(--set "operator.${OPERATOR_TYPE}.olm.installPlanApproval=${OPERATOR_INSTALL_PLAN_APPROVAL}")
  fi

  local env_idx=0
  if [[ "$DEPLOY_MODE" == "operator" && -n "$MAAS_CONTROLLER_IMAGE" ]]; then
    HELM_SETS+=(
      --set "operator.${OPERATOR_TYPE}.olm.config.env[${env_idx}].name=RELATED_IMAGE_ODH_MAAS_CONTROLLER_IMAGE"
      --set "operator.${OPERATOR_TYPE}.olm.config.env[${env_idx}].value=${MAAS_CONTROLLER_IMAGE}"
    )
    env_idx=$((env_idx + 1))
  fi

  if [[ "$DEPLOY_MODE" == "operator" && -n "$MAAS_API_IMAGE" ]]; then
    HELM_SETS+=(
      --set "operator.${OPERATOR_TYPE}.olm.config.env[${env_idx}].name=RELATED_IMAGE_ODH_MAAS_API_IMAGE"
      --set "operator.${OPERATOR_TYPE}.olm.config.env[${env_idx}].value=${MAAS_API_IMAGE}"
    )
    env_idx=$((env_idx + 1))
  fi

  if [[ "$DEPLOY_MODE" == "operator" && -n "$AI_GATEWAY_OPERATOR_IMAGE" ]]; then
    HELM_SETS+=(
      --set "operator.${OPERATOR_TYPE}.olm.config.env[${env_idx}].name=RELATED_IMAGE_ODH_AI_GATEWAY_OPERATOR_IMAGE"
      --set "operator.${OPERATOR_TYPE}.olm.config.env[${env_idx}].value=${AI_GATEWAY_OPERATOR_IMAGE}"
    )
    env_idx=$((env_idx + 1))
  fi

  if [[ "$DEPLOY_MODE" == "operator" && -n "$PAYLOAD_PROCESSING_IMAGE" ]]; then
    HELM_SETS+=(
      --set "operator.${OPERATOR_TYPE}.olm.config.env[${env_idx}].name=RELATED_IMAGE_ODH_AI_GATEWAY_PAYLOAD_PROCESSING_IMAGE"
      --set "operator.${OPERATOR_TYPE}.olm.config.env[${env_idx}].value=${PAYLOAD_PROCESSING_IMAGE}"
    )
    env_idx=$((env_idx + 1))
  fi

  if [[ -n "$OPERATOR_CATALOG" ]]; then
    local catalog_name="odh-custom-catalog"
    local catalog_ns="openshift-marketplace"
    log_info "Creating custom CatalogSource: $catalog_name"
    create_custom_catalogsource "$catalog_name" "$catalog_ns" "$OPERATOR_CATALOG"
    HELM_SETS+=(--set "operator.${OPERATOR_TYPE}.olm.source=${catalog_name}")
  fi

  setup_policy_engine_helm_sets

  HELM_SETS+=(
    --set-json 'dependencies.rhcl.olm.config.env=[
      {"name":"ISTIO_GATEWAY_CONTROLLER_NAMES","value":"istio.io/gateway-controller,openshift.io/gateway-controller/v1"},
      {"name":"RATELIMIT_CHECK_SERVICE_FAILURE_MODE","value":"deny"},
      {"name":"RATELIMIT_REPORT_SERVICE_FAILURE_MODE","value":"deny"},
      {"name":"AUTH_SERVICE_TIMEOUT","value":"2s"}
    ]'
  )

  if [[ -n "$RHCL_STARTING_CSV" ]]; then
    HELM_SETS+=(--set "dependencies.rhcl.olm.version=${RHCL_STARTING_CSV}")
  fi
}

setup_policy_engine_helm_sets() {
  case "${POLICY_ENGINE:-}" in
    kuadrant)
      local kuadrant_catalog="kuadrant-operator-catalog"
      local kuadrant_ns="kuadrant-system"

      log_info "Creating upstream Kuadrant v1.4.2 CatalogSource..."
      kubectl create namespace "$kuadrant_ns" 2>/dev/null || true

      cat <<EOF | kubectl apply -f -
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: $kuadrant_catalog
  namespace: $kuadrant_ns
spec:
  sourceType: grpc
  image: quay.io/kuadrant/kuadrant-operator-catalog:v1.4.2
  displayName: Kuadrant Operator Catalog
  publisher: Kuadrant
  updateStrategy:
    registryPoll:
      interval: 45m
EOF

      cat <<EOF | kubectl apply -f -
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: kuadrant-operator-group
  namespace: $kuadrant_ns
spec: {}
EOF

      HELM_SETS+=(
        --set dependencies.rhcl.olm.name=kuadrant-operator
        --set "dependencies.rhcl.olm.source=${kuadrant_catalog}"
        --set "dependencies.rhcl.olm.sourceNamespace=${kuadrant_ns}"
        --set dependencies.rhcl.olm.channel=stable
      )
      ;;
    rhcl|"")
      ;;
  esac
}

wait_for_operator_ready() {
  local operator_ns
  case "$OPERATOR_TYPE" in
    odh) operator_ns="opendatahub-operator-system" ;;
    rhoai) operator_ns="redhat-ods-operator" ;;
  esac

  local timeout=300
  local elapsed=0
  local interval=10

  while [[ $elapsed -lt $timeout ]]; do
    local phase
    phase=$(kubectl get csv -n "$operator_ns" --no-headers 2>/dev/null \
      | grep -E "^(opendatahub|rhods)-operator" | head -1 | awk '{print $NF}')
    if [[ "$phase" == "Succeeded" ]]; then
      log_info "Operator CSV is Succeeded in $operator_ns"
      kubectl wait deployment -n "$operator_ns" -l app.kubernetes.io/part-of=opendatahub-operator \
        --for=condition=Available --timeout=120s 2>/dev/null || true
      return 0
    fi
    log_info "  Operator CSV phase: ${phase:-pending} (${elapsed}s / ${timeout}s)"
    sleep $interval
    elapsed=$((elapsed + interval))
  done
  log_warn "Operator CSV not Succeeded after ${timeout}s — proceeding anyway"
}

run_helm_install() {
  log_info "Running: helm upgrade --install $HELM_RELEASE_NAME"
  helm upgrade --install "$HELM_RELEASE_NAME" "$CHART_PATH" \
    --namespace "$HELM_NAMESPACE" \
    --create-namespace \
    "${HELM_SETS[@]}" \
    --wait=false \
    --timeout 10m
}

post_helm_steps() {
  if [[ -n "$OPERATOR_IMAGE" ]]; then
    local operator_prefix
    local operator_ns
    case "$OPERATOR_TYPE" in
      odh)
        operator_prefix="opendatahub-operator"
        operator_ns="openshift-operators"
        ;;
      rhoai)
        operator_prefix="rhods-operator"
        operator_ns="redhat-ods-operator"
        ;;
    esac

    log_info "Patching operator CSV with custom image: $OPERATOR_IMAGE"

    local csv_name=""
    local timeout=120
    local elapsed=0
    local interval=5

    log_info "Waiting for CSV to be created (timeout: ${timeout}s)..."
    while [[ $elapsed -lt $timeout ]]; do
      csv_name=$(kubectl get csv -n "$operator_ns" --no-headers 2>/dev/null | grep "^${operator_prefix}" | head -n1 | awk '{print $1}')
      if [[ -n "$csv_name" ]]; then
        log_info "Found CSV: $csv_name after ${elapsed}s"
        break
      fi
      sleep $interval
      elapsed=$((elapsed + interval))
    done

    if [[ -z "$csv_name" ]]; then
      log_warn "Could not find CSV for $operator_prefix after ${timeout}s, skipping image patch"
      return 0
    fi

    kubectl annotate csv "$csv_name" -n "$operator_ns" opendatahub.io/managed=false --overwrite
    kubectl patch csv "$csv_name" -n "$operator_ns" --type='json' -p="[
      {\"op\": \"replace\", \"path\": \"/spec/install/spec/deployments/0/spec/template/spec/containers/0/image\", \"value\": \"$OPERATOR_IMAGE\"}
    ]"
    log_info "CSV $csv_name patched with image $OPERATOR_IMAGE"
  fi
}

main() {
  if [[ ! "$DEPLOY_MODE" =~ ^(operator|kustomize)$ ]]; then
    log_error "Invalid deployment mode: $DEPLOY_MODE (expected operator or kustomize)"
    return 1
  fi
  if [[ "$DEPLOY_MODE" == "kustomize" && -n "$AI_GATEWAY_OPERATOR_IMAGE" ]]; then
    log_error "AI_GATEWAY_OPERATOR_IMAGE is only supported in operator mode"
    return 1
  fi

  log_info "==================================================="
  log_info "  Setup Shared Dependencies (Helm)"
  log_info "==================================================="
  log_info "  Operator type: $OPERATOR_TYPE"
  log_info "  Deployment mode: $DEPLOY_MODE"
  log_info "  Ingress mode: $INGRESS_MODE"
  log_info "  Policy engine: ${POLICY_ENGINE:-auto}"
  log_info "  Chart source: ${ODH_GITOPS_CHART_PATH:-${ODH_GITOPS_REPO} @ ${ODH_GITOPS_BRANCH} (${ODH_GITOPS_COMMIT:-latest})}"
  log_info "  MaaS controller image: ${MAAS_CONTROLLER_IMAGE:-chart default}"
  log_info "  MaaS API image: ${MAAS_API_IMAGE:-chart default}"
  log_info "  Payload processing image: ${PAYLOAD_PROCESSING_IMAGE:-chart default}"
  log_info "  AI Gateway operator image: ${AI_GATEWAY_OPERATOR_IMAGE:-catalog default}"

  ensure_helm
  resolve_chart_path
  build_helm_sets

  log_info ""
  log_info "Phase 1: Installing operators via Helm chart..."
  run_helm_install

  log_info ""
  log_info "Waiting for operator CRDs to be established..."
  wait_for_crd "dscinitializations.dscinitialization.opendatahub.io" "$CRD_WAIT_TIMEOUT"
  wait_for_crd "datascienceclusters.datasciencecluster.opendatahub.io" "$CRD_WAIT_TIMEOUT"
  wait_for_crd "kuadrants.kuadrant.io" "$CRD_WAIT_TIMEOUT"

  log_info ""
  log_info "Waiting for ODH operator to be ready (webhook must be serving)..."
  wait_for_operator_ready

  log_info ""
  if [[ "$DEPLOY_MODE" == "kustomize" ]]; then
    log_info "Applying local MaaS CRDs for kustomize mode..."
    local project_root
    project_root="$(cd "$SCRIPT_DIR/.." && pwd)"
    install_maas_controller_crds_and_wait "${project_root}/deployment/base/maas-controller/crd"
  fi

  log_info ""
  log_info "Phase 2: Applying CRD-dependent resources (DSC, DSCI, Kuadrant CR)..."
  # RHCL/Kuadrant CRs are rendered conditionally with Helm's lookup() because
  # their CRDs are installed by the operator subscription. The first Helm
  # pass intentionally installs only operators; after the CRD waits above,
  # force the second pass to render those CRs explicitly.
  HELM_SETS+=(--set skipCrdCheck=true)
  run_helm_install

  if [[ "${POLICY_ENGINE:-rhcl}" =~ ^(rhcl|kuadrant)$ ]]; then
    log_info "Waiting for Kuadrant policy engine to become ready..."
    if ! wait_for_custom_check "Kuadrant ready in ${RHCL_NAMESPACE}" "$CUSTOM_RESOURCE_TIMEOUT" 5 -- \
      bash -c "kubectl get kuadrant kuadrant -n '${RHCL_NAMESPACE}' -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}' 2>/dev/null | grep -q True"; then
      local operator_logs
      operator_logs=$(kubectl logs deployment/kuadrant-operator-controller-manager \
        -n "$RHCL_NAMESPACE" --all-containers --tail=200 2>/dev/null || true)

      if grep -Fq 'cannot find RESTMapping for APIVersion kuadrant.io/v1beta1 Kind Kuadrant' \
        <<<"$operator_logs"; then
        log_warn "Restarting Kuadrant operator after transient REST mapping failure..."
        kubectl delete pod -n "$RHCL_NAMESPACE" \
          -l 'app=kuadrant,control-plane=controller-manager' --wait=true
        kubectl rollout status deployment/kuadrant-operator-controller-manager \
          -n "$RHCL_NAMESPACE" --timeout="${ROLLOUT_TIMEOUT}s"

        if ! wait_for_custom_check "Kuadrant ready in ${RHCL_NAMESPACE} after restart" \
          "$CUSTOM_CHECK_TIMEOUT" 5 -- \
          bash -c "kubectl get kuadrant kuadrant -n '${RHCL_NAMESPACE}' -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}' 2>/dev/null | grep -q True"; then
          log_error "Kuadrant did not become ready after operator restart"
          return 1
        fi

        log_info "Kuadrant recovered after operator restart"
      else
        log_error "Kuadrant did not become ready; Authorino and policy enforcement cannot be used"
        return 1
      fi
    fi
  fi

  post_helm_steps

  log_info ""
  log_info "Shared dependencies installed successfully"
}

main "$@"
