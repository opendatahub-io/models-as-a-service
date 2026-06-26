#!/usr/bin/env bash
#
# setup-shared-deps.sh — Install shared MaaS dependencies via odh-gitops Helm chart
#
# Replaces the imperative operator installation logic in deploy.sh
# (install_optional_operators, install_policy_engine, install_primary_operator,
# apply_custom_resources) with a declarative Helm install that matches
# the odh-gitops Makefile helm-install-verify pattern.
#
# Phase 1: Install operators (Subscriptions, OperatorGroups, Namespaces)
# Phase 2: Wait for CRDs + cert-manager + operator webhook readiness
# Phase 3: Re-run Helm to render CRD-dependent resources
#          (DSCInitialization, DataScienceCluster, Kuadrant CR, etc.)
# Phase 4: Apply MaaS CRDs and RBAC from local checkout
#
# Usage:
#   ./setup-shared-deps.sh                           # defaults: odh
#   ./setup-shared-deps.sh --operator-type rhoai     # RHOAI operator
#   CHART_PATH=/path/to/chart ./setup-shared-deps.sh # custom chart path
#
# Environment variables:
#   CHART_PATH              Path to rhai-on-openshift-chart (default: auto-detect or clone)
#   ODH_GITOPS_REPO         Git URL for odh-gitops (default: https://github.com/opendatahub-io/odh-gitops.git)
#   ODH_GITOPS_BRANCH       Branch to clone from odh-gitops (default: main)
#   OPERATOR_TYPE           odh or rhoai (default: odh)
#   HELM_NAMESPACE          Helm release namespace (default: opendatahub-gitops)
#   HELM_EXTRA_ARGS         Additional helm flags
#   CRD_TIMEOUT             Seconds to wait for CRDs (default: 300)
#
# Operator image overrides (match deploy.sh / Prow interface):
#   OPERATOR_CATALOG        Custom CatalogSource image (e.g., quay.io/.../catalog:pr-123)
#   OPERATOR_CHANNEL        Subscription channel (default: fast-3 for odh, stable-3.x for rhoai)
#   OPERATOR_STARTING_CSV   Pin to specific CSV version (e.g., v3.5.0)
#   OPERATOR_INSTALL_PLAN_APPROVAL  InstallPlan approval (default: Automatic)
#
# MaaS image overrides (injected into operator Subscription via RELATED_IMAGE env vars):
#   MAAS_CONTROLLER_IMAGE   Custom maas-controller image
#   MAAS_API_IMAGE          Custom maas-api image

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

if ! type log_info &>/dev/null 2>&1; then
  log_info()  { echo "[INFO]  $*"; }
  log_warn()  { echo "[WARN]  $*" >&2; }
  log_error() { echo "[ERROR] $*" >&2; }
fi

# ---------------------------------------------------------------------------
# Defaults
# ---------------------------------------------------------------------------

OPERATOR_TYPE="${OPERATOR_TYPE:-odh}"
HELM_NAMESPACE="${HELM_NAMESPACE:-opendatahub-gitops}"
HELM_EXTRA_ARGS="${HELM_EXTRA_ARGS:-}"
CRD_TIMEOUT="${CRD_TIMEOUT:-300}"
OPERATOR_CATALOG="${OPERATOR_CATALOG:-}"
OPERATOR_CHANNEL="${OPERATOR_CHANNEL:-}"
OPERATOR_STARTING_CSV="${OPERATOR_STARTING_CSV:-}"
OPERATOR_INSTALL_PLAN_APPROVAL="${OPERATOR_INSTALL_PLAN_APPROVAL:-}"
ODH_GITOPS_REPO="${ODH_GITOPS_REPO:-https://github.com/ishitasequeira/odh-gitops.git}"
ODH_GITOPS_BRANCH="${ODH_GITOPS_BRANCH:-feat/maas-shared-deps}"
MAAS_CONTROLLER_IMAGE="${MAAS_CONTROLLER_IMAGE:-}"
MAAS_API_IMAGE="${MAAS_API_IMAGE:-}"

# Auto-detect chart path: look for odh-gitops checkout next to models-as-a-service
if [[ -z "${CHART_PATH:-}" ]]; then
  candidate="${SCRIPT_DIR}/../../odh-gitops/charts/rhai-on-openshift-chart"
  if [[ -d "$candidate" ]]; then
    CHART_PATH="$(cd "$candidate" && pwd)"
  fi
fi

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------

while [[ $# -gt 0 ]]; do
  case "$1" in
    --operator-type)    OPERATOR_TYPE="$2"; shift 2 ;;
    --chart-path)       CHART_PATH="$2"; shift 2 ;;
    --helm-namespace)   HELM_NAMESPACE="$2"; shift 2 ;;
    --crd-timeout)      CRD_TIMEOUT="$2"; shift 2 ;;
    --odh-gitops-repo)   ODH_GITOPS_REPO="$2"; shift 2 ;;
    --odh-gitops-branch) ODH_GITOPS_BRANCH="$2"; shift 2 ;;
    --operator-catalog) OPERATOR_CATALOG="$2"; shift 2 ;;
    --operator-channel) OPERATOR_CHANNEL="$2"; shift 2 ;;
    --operator-starting-csv) OPERATOR_STARTING_CSV="$2"; shift 2 ;;
    --operator-install-plan-approval) OPERATOR_INSTALL_PLAN_APPROVAL="$2"; shift 2 ;;
    --maas-controller-image) MAAS_CONTROLLER_IMAGE="$2"; shift 2 ;;
    --maas-api-image) MAAS_API_IMAGE="$2"; shift 2 ;;
    --help|-h)
      grep '^#' "$0" | grep -v '^#!/' | sed 's/^# \?//'
      exit 0
      ;;
    *)
      log_error "Unknown argument: $1"
      exit 1
      ;;
  esac
done

# ---------------------------------------------------------------------------
# Validation
# ---------------------------------------------------------------------------

if [[ ! "$OPERATOR_TYPE" =~ ^(rhoai|odh)$ ]]; then
  log_error "Invalid operator type: $OPERATOR_TYPE (must be rhoai or odh)"
  exit 1
fi

# If chart path still not set, clone odh-gitops
if [[ -z "${CHART_PATH:-}" ]]; then
  ODH_GITOPS_DIR="${SCRIPT_DIR}/../../odh-gitops"
  log_info "odh-gitops not found locally, cloning branch '${ODH_GITOPS_BRANCH}'..."
  git clone --depth 1 --branch "${ODH_GITOPS_BRANCH}" \
    "${ODH_GITOPS_REPO}" "${ODH_GITOPS_DIR}"
  CHART_PATH="$(cd "${ODH_GITOPS_DIR}/charts/rhai-on-openshift-chart" && pwd)"
fi

if [[ ! -f "${CHART_PATH}/Chart.yaml" ]]; then
  log_error "Chart not found at ${CHART_PATH}/Chart.yaml"
  exit 1
fi

# ---------------------------------------------------------------------------
# Build Helm args
# ---------------------------------------------------------------------------

HELM_ARGS=(
  --set "operator.type=${OPERATOR_TYPE}"
  --set "profile=maas"
  --set "components.kserve.modelsAsService.gatewayClass.create=true"
  --set "components.kserve.modelsAsService.gateway.create=true"
)

# Operator image/catalog overrides — maps deploy.sh env vars to Helm values
olm_prefix="operator.${OPERATOR_TYPE}.olm"

if [[ -n "$OPERATOR_CATALOG" ]]; then
  CATALOG_NAME="${OPERATOR_TYPE}-custom-catalog"
  log_info "Creating custom CatalogSource: ${CATALOG_NAME} -> ${OPERATOR_CATALOG}"
  kubectl apply -f - <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: ${CATALOG_NAME}
  namespace: openshift-marketplace
spec:
  sourceType: grpc
  image: ${OPERATOR_CATALOG}
  displayName: "${OPERATOR_TYPE} custom catalog"
  publisher: CI
  updateStrategy:
    registryPoll:
      interval: 15m
EOF
  HELM_ARGS+=(--set "${olm_prefix}.source=${CATALOG_NAME}")
fi

if [[ -n "$OPERATOR_CHANNEL" ]]; then
  HELM_ARGS+=(--set "${olm_prefix}.channel=${OPERATOR_CHANNEL}")
fi

if [[ -n "$OPERATOR_STARTING_CSV" ]]; then
  HELM_ARGS+=(--set "${olm_prefix}.version=${OPERATOR_STARTING_CSV}")
fi

if [[ -n "$OPERATOR_INSTALL_PLAN_APPROVAL" ]]; then
  HELM_ARGS+=(--set "${olm_prefix}.installPlanApproval=${OPERATOR_INSTALL_PLAN_APPROVAL}")
fi

# MaaS image overrides — inject RELATED_IMAGE env vars into operator Subscription
# The operator reads these and passes them to component deployments (maas-controller, maas-api)
env_index=0

if [[ -n "$MAAS_CONTROLLER_IMAGE" ]]; then
  log_info "Overriding maas-controller image: ${MAAS_CONTROLLER_IMAGE}"
  HELM_ARGS+=(
    --set "${olm_prefix}.config.env[${env_index}].name=RELATED_IMAGE_ODH_MAAS_CONTROLLER_IMAGE"
    --set "${olm_prefix}.config.env[${env_index}].value=${MAAS_CONTROLLER_IMAGE}"
  )
  env_index=$((env_index + 1))
fi

if [[ -n "$MAAS_API_IMAGE" ]]; then
  log_info "Overriding maas-api image: ${MAAS_API_IMAGE}"
  HELM_ARGS+=(
    --set "${olm_prefix}.config.env[${env_index}].name=RELATED_IMAGE_ODH_MAAS_API_IMAGE"
    --set "${olm_prefix}.config.env[${env_index}].value=${MAAS_API_IMAGE}"
  )
  env_index=$((env_index + 1))
fi

# ---------------------------------------------------------------------------
# Wait helpers
# ---------------------------------------------------------------------------

wait_for_crd() {
  local crd_name="$1"
  local timeout="${2:-$CRD_TIMEOUT}"
  local elapsed=0
  local interval=5

  while [[ $elapsed -lt $timeout ]]; do
    if kubectl get crd "$crd_name" &>/dev/null; then
      log_info "CRD $crd_name is available"
      return 0
    fi
    sleep $interval
    elapsed=$((elapsed + interval))
  done

  log_error "CRD $crd_name not found after ${timeout}s"
  return 1
}

wait_for_deployment() {
  local name="$1"
  local namespace="$2"
  local timeout="${3:-$CRD_TIMEOUT}"
  local elapsed=0

  while [[ $elapsed -lt $timeout ]]; do
    if kubectl get deployment "$name" -n "$namespace" &>/dev/null; then
      break
    fi
    sleep 5
    elapsed=$((elapsed + 5))
    if (( elapsed % 30 == 0 )); then
      log_info "  Waiting for ${name} deployment in ${namespace}... (${elapsed}s)"
    fi
  done

  kubectl wait --for=condition=Available --timeout="${timeout}s" \
    deployment/"$name" -n "$namespace" || {
    log_error "Deployment ${namespace}/${name} not ready after ${timeout}s"
    return 1
  }
}

# ---------------------------------------------------------------------------
# Phase 1: Install operators
# ---------------------------------------------------------------------------

log_info "=== Phase 1: Install operators via Helm ==="
log_info "  Chart:    ${CHART_PATH}"
log_info "  Operator: ${OPERATOR_TYPE}"
log_info "  Profile:  maas"
[[ -n "$OPERATOR_CATALOG" ]] && log_info "  Catalog:  ${OPERATOR_CATALOG}"
[[ -n "$OPERATOR_CHANNEL" ]] && log_info "  Channel:  ${OPERATOR_CHANNEL}"
[[ -n "$OPERATOR_STARTING_CSV" ]] && log_info "  CSV:      ${OPERATOR_STARTING_CSV}"
[[ -n "$OPERATOR_INSTALL_PLAN_APPROVAL" ]] && log_info "  Approval: ${OPERATOR_INSTALL_PLAN_APPROVAL}"
[[ -n "$MAAS_CONTROLLER_IMAGE" ]] && log_info "  MaaS Controller: ${MAAS_CONTROLLER_IMAGE}"
[[ -n "$MAAS_API_IMAGE" ]] && log_info "  MaaS API: ${MAAS_API_IMAGE}"

helm upgrade --install odh "${CHART_PATH}" \
  -n "${HELM_NAMESPACE}" --create-namespace \
  "${HELM_ARGS[@]}" \
  ${HELM_EXTRA_ARGS}

# ---------------------------------------------------------------------------
# Phase 2: Wait for CRDs + cert-manager + operator webhook
# ---------------------------------------------------------------------------

log_info ""
log_info "=== Phase 2: Waiting for operator CRDs ==="

wait_for_crd "datascienceclusters.datasciencecluster.opendatahub.io"
wait_for_crd "dscinitializations.dscinitialization.opendatahub.io"
wait_for_crd "kuadrants.kuadrant.io"

# cert-manager must be fully operational before Phase 3 — the ODH operator's
# maas-controller deployment needs cert-manager to issue webhook serving certs.
# Without this wait, maas-controller crashes with "tls.crt: no such file".
log_info ""
log_info "=== Waiting for cert-manager readiness ==="

wait_for_deployment "cert-manager" "cert-manager" "$CRD_TIMEOUT"
log_info "cert-manager controller is ready"

wait_for_deployment "cert-manager-webhook" "cert-manager" "$CRD_TIMEOUT"
log_info "cert-manager webhook is ready"

wait_for_deployment "cert-manager-cainjector" "cert-manager" "$CRD_TIMEOUT"
log_info "cert-manager CA injector is ready"

log_info ""
log_info "=== Waiting for operator webhook deployment ==="

case "$OPERATOR_TYPE" in
  rhoai) WEBHOOK_NS="redhat-ods-operator";  WEBHOOK_DEPLOY="rhods-operator" ;;
  odh)   WEBHOOK_NS="opendatahub-operator-system"; WEBHOOK_DEPLOY="opendatahub-operator-controller-manager" ;;
esac

wait_for_deployment "$WEBHOOK_DEPLOY" "$WEBHOOK_NS" "$CRD_TIMEOUT"
log_info "Operator webhook is ready"

# ---------------------------------------------------------------------------
# Phase 3: Re-run Helm for CRD-dependent resources
# ---------------------------------------------------------------------------

log_info ""
log_info "=== Phase 3: Render CRD-dependent resources ==="

helm upgrade --install odh "${CHART_PATH}" \
  -n "${HELM_NAMESPACE}" \
  "${HELM_ARGS[@]}" \
  ${HELM_EXTRA_ARGS}

# ---------------------------------------------------------------------------
# Phase 4: Apply MaaS CRDs and RBAC from local checkout
# ---------------------------------------------------------------------------

MAAS_CRD_DIR="${PROJECT_ROOT}/deployment/base/maas-controller/crd"
MAAS_RBAC_DIR="${PROJECT_ROOT}/deployment/base/maas-controller/rbac"
MAAS_WEBHOOK_DIR="${PROJECT_ROOT}/deployment/base/maas-controller/webhook"

if [[ -d "$MAAS_CRD_DIR" ]]; then
  log_info ""
  log_info "=== Phase 4: Apply MaaS CRDs, RBAC, and webhook from local checkout ==="

  log_info "Applying CRDs from ${MAAS_CRD_DIR}..."
  kubectl apply --server-side --force-conflicts -k "$MAAS_CRD_DIR"

  # Wait for MaaS CRDs specifically (not all cluster CRDs)
  log_info "Waiting for MaaS CRDs to be established..."
  for crd in tenants.maas.opendatahub.io maasmodelrefs.maas.opendatahub.io maasauthpolicies.maas.opendatahub.io maassubscriptions.maas.opendatahub.io externalmodels.maas.opendatahub.io configs.maas.opendatahub.io; do
    kubectl wait --for=condition=Established "crd/${crd}" --timeout=60s 2>/dev/null || true
  done

  if [[ -d "$MAAS_RBAC_DIR" ]]; then
    log_info "Applying RBAC from ${MAAS_RBAC_DIR}..."
    kubectl apply --server-side --force-conflicts -k "$MAAS_RBAC_DIR"
  fi

  # The webhook Service has an OpenShift annotation (service.beta.openshift.io/serving-cert-secret-name)
  # that triggers the service CA operator to generate a TLS secret. Without this, maas-controller
  # crashes with "tls.crt: no such file or directory".
  case "$OPERATOR_TYPE" in
    rhoai) webhook_cert_ns="redhat-ods-applications" ;;
    odh)   webhook_cert_ns="opendatahub" ;;
  esac

  if [[ -d "$MAAS_WEBHOOK_DIR" ]]; then
    log_info "Applying webhook configuration from ${MAAS_WEBHOOK_DIR}..."
    # The kustomization uses namespace: system as a placeholder; override to the operator namespace
    kustomize build "$MAAS_WEBHOOK_DIR" | sed "s/namespace: system/namespace: ${webhook_cert_ns}/g" | \
      kubectl apply --server-side --force-conflicts -f -

    log_info "Waiting for webhook serving cert (namespace: ${webhook_cert_ns})..."
    elapsed=0
    while [[ $elapsed -lt 120 ]]; do
      if kubectl get secret maas-controller-webhook-cert -n "$webhook_cert_ns" &>/dev/null; then
        log_info "Webhook serving cert is available"
        break
      fi
      sleep 5
      elapsed=$((elapsed + 5))
    done
    if [[ $elapsed -ge 120 ]]; then
      log_warn "Webhook cert not generated after 120s — maas-controller may crash until it appears"
    fi

    # The operator-managed maas-controller deployment doesn't include the webhook cert
    # volume mount. Patch it to mount the secret at the path the controller expects.
    log_info "Patching maas-controller to mount webhook cert..."
    kubectl patch deployment maas-controller -n "$webhook_cert_ns" --type='json' -p='[
      {"op": "add", "path": "/spec/template/spec/volumes", "value": [
        {"name": "webhook-cert", "secret": {"secretName": "maas-controller-webhook-cert", "defaultMode": 420}}
      ]},
      {"op": "add", "path": "/spec/template/spec/containers/0/volumeMounts", "value": [
        {"name": "webhook-cert", "mountPath": "/tmp/k8s-webhook-server/serving-certs", "readOnly": true}
      ]}
    ]' 2>/dev/null || true

    log_info "Waiting for maas-controller rollout..."
    kubectl rollout status deployment/maas-controller -n "$webhook_cert_ns" --timeout=120s || {
      log_warn "maas-controller rollout not complete after 120s"
    }
  fi
else
  log_info ""
  log_info "Skipping Phase 4: MaaS CRD/RBAC directory not found (not in models-as-a-service checkout)"
fi

log_info ""
log_info "=== Shared dependencies installed successfully ==="
log_info "  Operators: cert-manager, RHCL/Kuadrant, ODH/RHOAI"
log_info "  Resources: DSCInitialization, DataScienceCluster (MaaS), GatewayClass, Gateway"
[[ -d "$MAAS_CRD_DIR" ]] && log_info "  MaaS CRDs/RBAC: applied from local checkout" || true
[[ -n "$MAAS_CONTROLLER_IMAGE" ]] && log_info "  MaaS Controller: ${MAAS_CONTROLLER_IMAGE}" || true
[[ -n "$MAAS_API_IMAGE" ]] && log_info "  MaaS API: ${MAAS_API_IMAGE}" || true
