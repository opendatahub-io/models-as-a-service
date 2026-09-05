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

ODH_GITOPS_REPO="${ODH_GITOPS_REPO:-https://github.com/ishitasequeira/odh-gitops.git}"
ODH_GITOPS_BRANCH="${ODH_GITOPS_BRANCH:-feat/maas-profile}"
ODH_GITOPS_CHART_PATH="${ODH_GITOPS_CHART_PATH:-}"
HELM_RELEASE_NAME="${HELM_RELEASE_NAME:-rhoai-deps}"
HELM_NAMESPACE="${HELM_NAMESPACE:-rhoai-deps}"

OPERATOR_TYPE="${OPERATOR_TYPE:-odh}"
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

  log_info "Cloning odh-gitops chart (branch: $ODH_GITOPS_BRANCH)..."
  git clone --depth 1 --branch "$ODH_GITOPS_BRANCH" "$ODH_GITOPS_REPO" "$_CLONE_TMP_DIR/odh-gitops" 2>&1 | tail -1
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
  if [[ -n "$MAAS_CONTROLLER_IMAGE" ]]; then
    HELM_SETS+=(
      --set "operator.${OPERATOR_TYPE}.olm.config.env[${env_idx}].name=RELATED_IMAGE_ODH_MAAS_CONTROLLER_IMAGE"
      --set "operator.${OPERATOR_TYPE}.olm.config.env[${env_idx}].value=${MAAS_CONTROLLER_IMAGE}"
    )
    env_idx=$((env_idx + 1))
  fi

  if [[ -n "$MAAS_API_IMAGE" ]]; then
    HELM_SETS+=(
      --set "operator.${OPERATOR_TYPE}.olm.config.env[${env_idx}].name=RELATED_IMAGE_ODH_MAAS_API_IMAGE"
      --set "operator.${OPERATOR_TYPE}.olm.config.env[${env_idx}].value=${MAAS_API_IMAGE}"
    )
    env_idx=$((env_idx + 1))
  fi

  if [[ -n "$AI_GATEWAY_OPERATOR_IMAGE" ]]; then
    HELM_SETS+=(
      --set "operator.${OPERATOR_TYPE}.olm.config.env[${env_idx}].name=RELATED_IMAGE_ODH_AI_GATEWAY_OPERATOR_IMAGE"
      --set "operator.${OPERATOR_TYPE}.olm.config.env[${env_idx}].value=${AI_GATEWAY_OPERATOR_IMAGE}"
    )
    env_idx=$((env_idx + 1))
  fi

  if [[ -n "$PAYLOAD_PROCESSING_IMAGE" ]]; then
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
  log_info "==================================================="
  log_info "  Setup Shared Dependencies (Helm)"
  log_info "==================================================="
  log_info "  Operator type: $OPERATOR_TYPE"
  log_info "  Policy engine: ${POLICY_ENGINE:-auto}"
  log_info "  Chart source: ${ODH_GITOPS_CHART_PATH:-${ODH_GITOPS_REPO} @ ${ODH_GITOPS_BRANCH}}"

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
  log_info "Applying latest MaaS CRDs from local repo..."
  local project_root
  project_root="$(cd "$SCRIPT_DIR/.." && pwd)"
  install_maas_controller_crds_and_wait "${project_root}/deployment/base/maas-controller/crd"

  log_info ""
  log_info "Phase 2: Applying CRD-dependent resources (DSC, DSCI, Kuadrant CR)..."
  run_helm_install

  post_helm_steps

  log_info ""
  log_info "Shared dependencies installed successfully"
}

main "$@"
