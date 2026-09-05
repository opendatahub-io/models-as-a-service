#!/bin/bash
################################################################################
# MaaS Deployment Script
#
# Unified deployment script for Models-as-a-Service (MaaS) platform.
# Supports RHOAI and ODH operators with configurable rate limiting.
#
# USAGE:
#   ./scripts/deploy.sh [OPTIONS]
#
# OPTIONS:
#   --operator-type <odh|rhoai>   Operator to install (default: odh)
#                                 Policy engine is auto-selected:
#                                   odh → kuadrant (community v1.4.2)
#                                   rhoai → rhcl (Red Hat Connectivity Link)
#   --policy-engine <rhcl|kuadrant>
#                                 Override rate-limiting policy engine
#   --enable-tls-backend          Enable TLS for Authorino/MaaS API (default: on)
#   --enable-keycloak             Deploy Keycloak for external OIDC (optional)
#   --namespace <namespace>       Target namespace
#   --verbose                     Enable debug logging
#   --dry-run                     Show what would be done
#   --dev                         Use dev overlay with :latest images
#   --help                        Show full help with all options
#
# ADVANCED OPTIONS (PR Testing):
#   --operator-catalog <image>    Custom operator catalog image
#   --operator-image <image>      Custom operator image (patches CSV)
#   --maas-api-image <image>      Custom MaaS API container image
#   --ai-gateway-operator-image <image> Custom ai-gateway-operator image (operator mode only)
#   --channel <channel>           Operator channel override
#
# ENVIRONMENT VARIABLES:
#   MAAS_API_IMAGE            Custom MaaS API image (passed to Tenant reconciler via RELATED_IMAGE)
#   MAAS_CONTROLLER_IMAGE     Custom MaaS controller container image
#   AI_GATEWAY_OPERATOR_IMAGE Custom ai-gateway-operator image (operator mode only; patches ODH CSV
#                             RELATED_IMAGE_ODH_AI_GATEWAY_OPERATOR_IMAGE and enables the AIGateway
#                             DSC component)
#   OPERATOR_TYPE             Operator type (rhoai/odh)
#   LOG_LEVEL                 Logging verbosity (DEBUG, INFO, WARN, ERROR)
#   FORCE_OVERWRITE           When true, re-apply manifests even if the resource already exists
#
# TIMEOUT CONFIGURATION (all in seconds, see deployment-helpers.sh for defaults):
#   CUSTOM_RESOURCE_TIMEOUT   DataScienceCluster wait (default: 600)
#   NAMESPACE_TIMEOUT         Namespace creation/ready (default: 300)
#   RESOURCE_TIMEOUT          Generic resource wait (default: 300)
#   CRD_TIMEOUT               CRD establishment (default: 180)
#   CSV_TIMEOUT               CSV installation (default: 180)
#   SUBSCRIPTION_TIMEOUT      Subscription install (default: 300)
#   POD_TIMEOUT               Pod ready wait (default: 120)
#   WEBHOOK_TIMEOUT           Webhook ready (default: 60)
#   CUSTOM_CHECK_TIMEOUT      Generic check (default: 120)
#   AUTHORINO_TIMEOUT         Authorino ready (default: 120)
#   ROLLOUT_TIMEOUT           kubectl rollout status (default: 120)
#   CATALOGSOURCE_TIMEOUT     CatalogSource ready (default: 120)
#
# EXAMPLES:
#   # Deploy ODH (default, uses kuadrant policy engine)
#   ./scripts/deploy.sh
#
#   # Deploy RHOAI (uses rhcl policy engine)
#   ./scripts/deploy.sh --operator-type rhoai
#
#   # Deploy with Keycloak for external OIDC support
#   ./scripts/deploy.sh --enable-keycloak
#
#   # Test custom MaaS API image
#   MAAS_API_IMAGE=quay.io/myuser/maas-api:pr-123 ./scripts/deploy.sh
#
#   # Use external PostgreSQL (production)
#   ./scripts/deploy.sh --postgres-connection 'postgresql://user:pass@db.example.com:5432/maas?sslmode=require'
#
# For detailed documentation, see:
# https://opendatahub-io.github.io/models-as-a-service/latest/install/maas-setup/
################################################################################

set -euo pipefail

# Source helpers
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deployment-helpers.sh
source "${SCRIPT_DIR}/deployment-helpers.sh"

# Derive infrastructure namespace from controller namespace (matches Go code logic)
derive_infra_namespace() {
  local controller_ns="$1"
  case "$controller_ns" in
    redhat-ods-applications)
      echo "redhat-ai-gateway-infra"
      ;;
    opendatahub)
      echo "odh-ai-gateway-infra"
      ;;
    *)
      echo "$controller_ns"
      ;;
  esac
}

apply_infra_secret_migration_rbac() {
  local infra_ns="$1"
  local controller_ns="$2"
  local rbac_dir="${SCRIPT_DIR}/../deployment/base/maas-controller/infra-rbac"

  if [ ! -d "$rbac_dir" ]; then
    log_warn "Infra RBAC directory not found at $rbac_dir, skipping"
    return 0
  fi

  log_info "Applying secret migration RBAC to namespace $infra_ns..."
  kustomize build "$rbac_dir" \
    | sed "s|namespace: opendatahub|namespace: $controller_ns|g" \
    | kubectl apply -n "$infra_ns" -f -
}

# Set log level from environment variable if provided
case "${LOG_LEVEL:-}" in
  DEBUG)
    CURRENT_LOG_LEVEL=$LOG_LEVEL_DEBUG
    ;;
  INFO)
    CURRENT_LOG_LEVEL=$LOG_LEVEL_INFO
    ;;
  WARN)
    CURRENT_LOG_LEVEL=$LOG_LEVEL_WARN
    ;;
  ERROR)
    CURRENT_LOG_LEVEL=$LOG_LEVEL_ERROR
    ;;
esac

#──────────────────────────────────────────────────────────────
# DEFAULT CONFIGURATION
#──────────────────────────────────────────────────────────────

OPERATOR_TYPE="${OPERATOR_TYPE:-odh}"
POLICY_ENGINE="${POLICY_ENGINE:-}"  # Auto-determined unless set via env or --policy-engine
RHCL_STARTING_CSV="${RHCL_STARTING_CSV:-}"
RHCL_NAMESPACE="${RHCL_NAMESPACE:-kuadrant-system}"
NAMESPACE="${DEPLOYMENT_NAMESPACE:-}"  # Auto-determined based on operator type
ENABLE_TLS_BACKEND="${ENABLE_TLS_BACKEND:-true}"
ENABLE_KEYCLOAK="${ENABLE_KEYCLOAK:-false}"
VERBOSE="${VERBOSE:-false}"
DRY_RUN="${DRY_RUN:-false}"
DEV_MODE="${DEV_MODE:-false}"
OPERATOR_CATALOG="${OPERATOR_CATALOG:-}"
OPERATOR_IMAGE="${OPERATOR_IMAGE:-}"
OPERATOR_CHANNEL="${OPERATOR_CHANNEL:-}"
OPERATOR_STARTING_CSV="${OPERATOR_STARTING_CSV:-opendatahub-operator.v3.5.0-ea.2}"
OPERATOR_INSTALL_PLAN_APPROVAL="${OPERATOR_INSTALL_PLAN_APPROVAL:-}"
MAAS_API_IMAGE="${MAAS_API_IMAGE:-}"
MAAS_CONTROLLER_IMAGE="${MAAS_CONTROLLER_IMAGE:-}"
AI_GATEWAY_OPERATOR_IMAGE="${AI_GATEWAY_OPERATOR_IMAGE:-}"
PAYLOAD_PROCESSING_IMAGE="${PAYLOAD_PROCESSING_IMAGE:-}"
FORCE_OVERWRITE="${FORCE_OVERWRITE:-false}"
EXTERNAL_OIDC="${EXTERNAL_OIDC:-false}"
POSTGRES_CONNECTION="${POSTGRES_CONNECTION:-}"

#──────────────────────────────────────────────────────────────
# HELP TEXT
#──────────────────────────────────────────────────────────────

show_help() {
  cat <<EOF
Unified deployment script for Models-as-a-Service

USAGE:
  ./scripts/deploy.sh [OPTIONS]

OPTIONS:
  --operator-type <odh|rhoai>
      Which operator to install (default: odh)
      Policy engine is auto-selected based on operator type:
      - rhoai → rhcl (Red Hat Connectivity Link)
      - odh → kuadrant (community v1.4.2 with AuthPolicy v1)

  --policy-engine <rhcl|kuadrant>
      Rate-limiting policy engine (default: auto-selected)
      - rhcl: Red Hat Connectivity Link from redhat-operators (stable channel head)
      - kuadrant: upstream community catalog (v1.4.2)
      Overrides auto-selection.

  --enable-tls-backend
      Enable TLS backend for Authorino and MaaS API (default: enabled)
      Configures HTTPS for Authorino to maas-api communication

  --disable-tls-backend
      Disable TLS backend for Authorino and MaaS API
      Uses HTTP for Authorino to maas-api communication

  --enable-keycloak
      Deploy Keycloak identity provider for external OIDC support (optional)
      Creates keycloak-system namespace and deploys Keycloak operator
      See docs/samples/install/keycloak/ for configuration guide

  --postgres-connection <connection-string>
      Use an external PostgreSQL database instead of deploying a POC instance.
      Format: postgresql://USER:PASSWORD@HOST:PORT/DATABASE?sslmode=require
      When set, skips the built-in PostgreSQL deployment entirely.

  --namespace <namespace>
      Target namespace for deployment
      Default: redhat-ods-applications (RHOAI) or opendatahub (ODH)

  --verbose
      Enable verbose/debug logging

  --dry-run
      Show what would be done without applying changes

  --dev
      Use dev overlay with :latest images (for MaaS developers)
      Default: uses odh overlay with :odh-stable images

  --help
      Display this help message

ADVANCED OPTIONS (PR Testing):
  --operator-catalog <image>
      Custom operator catalog/index image (for testing PRs)
      Example: quay.io/opendatahub/opendatahub-operator-catalog:pr-456

  --operator-image <image>
      Custom operator image (patches CSV after install)
      Example: quay.io/opendatahub/opendatahub-operator:pr-456

  --maas-api-image <image>
      Custom MaaS API container image (PR testing)
      Example: quay.io/opendatahub/maas-api:pr-456

  --maas-controller-image <image>
      Custom MaaS controller container image (PR testing)
      Example: quay.io/opendatahub/maas-controller:pr-406

  --ai-gateway-operator-image <image>
      Custom ai-gateway-operator image (PR/stable testing, operator mode only)
      Patches RELATED_IMAGE_ODH_AI_GATEWAY_OPERATOR_IMAGE on the ODH operator CSV
      and enables spec.components.aigateway.managementState=Managed on the DSC.
      Example: quay.io/opendatahub/odh-ai-gateway-operator:odh-stable

  --channel <channel>
      Operator channel override
      Default: fast-3 (ODH), stable-3.x (RHOAI)

  --external-oidc
      Enable external OIDC on the maas-api AuthPolicy.
      Requires OIDC_ISSUER_URL (and OIDC_CLIENT_ID) to be set.

ENVIRONMENT VARIABLES:
  MAAS_API_IMAGE            Custom MaaS API container image
  MAAS_CONTROLLER_IMAGE     Custom MaaS controller container image
  AI_GATEWAY_OPERATOR_IMAGE Custom ai-gateway-operator image (operator mode only)
  OPERATOR_CATALOG          Custom operator catalog
  OPERATOR_IMAGE            Custom operator image
  OPERATOR_STARTING_CSV     ODH Subscription startingCSV (default: opendatahub-operator.v3.5.0-ea.2; set "-" to follow channel head)
  OPERATOR_INSTALL_PLAN_APPROVAL  ODH Subscription OLM approval (default: Manual — no auto-upgrades; first InstallPlan is auto-approved by the script)
  OPERATOR_TYPE             Operator type (rhoai/odh)
  POLICY_ENGINE             Policy engine override (rhcl|kuadrant)
  RHCL_STARTING_CSV         Pin RHCL operator CSV (default: channel head on redhat-operators)
  RHCL_NAMESPACE            RHCL operator/Kuadrant workload namespace (default: kuadrant-system)
  EXTERNAL_OIDC             Enable external OIDC on maas-api (true/false)
  OIDC_ISSUER_URL           External OIDC issuer URL for maas-api AuthPolicy patching
  OIDC_CLIENT_ID            External OIDC client ID for maas-api AuthPolicy patching (required with EXTERNAL_OIDC)
  LOG_LEVEL                 Logging verbosity (DEBUG, INFO, WARN, ERROR)
  FORCE_OVERWRITE           When true, re-apply manifests even if the resource already exists (default: false)
  POSTGRES_CONNECTION       External PostgreSQL connection string (same as --postgres-connection)

TIMEOUT CONFIGURATION (all values in seconds):
  Customize timeouts for slow clusters or CI/CD environments:
  - CUSTOM_RESOURCE_TIMEOUT=600   DataScienceCluster wait
  - NAMESPACE_TIMEOUT=300         Namespace creation
  - CRD_TIMEOUT=180              CRD establishment
  - CSV_TIMEOUT=180              Operator CSV installation
  - ROLLOUT_TIMEOUT=120          Deployment rollout
  - AUTHORINO_TIMEOUT=120        Authorino ready
  See deployment-helpers.sh for complete list and defaults

EXAMPLES:
  # Deploy ODH (default, uses kuadrant policy engine)
  ./scripts/deploy.sh

  # Deploy RHOAI (uses rhcl policy engine)
  ./scripts/deploy.sh --operator-type rhoai

  # Deploy with Keycloak for external OIDC support
  ./scripts/deploy.sh --enable-keycloak

  # Test MaaS API PR #123
  MAAS_API_IMAGE=quay.io/myuser/maas-api:pr-123 \\
    ./scripts/deploy.sh --operator-type odh

  # Test ODH operator PR #456 with manifests
  ./scripts/deploy.sh \\
    --operator-type odh \\
    --operator-catalog quay.io/opendatahub/opendatahub-operator-catalog:pr-456 \\
    --operator-image quay.io/opendatahub/opendatahub-operator:pr-456

  # Use an external PostgreSQL database
  ./scripts/deploy.sh --postgres-connection 'postgresql://user:pass@rds.example.com:5432/maas?sslmode=require'

For more information, see: https://github.com/opendatahub-io/models-as-a-service
EOF
}

#──────────────────────────────────────────────────────────────
# ARGUMENT PARSING
#──────────────────────────────────────────────────────────────

# Helper function to validate flag has a value
require_flag_value() {
  local flag=$1
  local value=${2:-}

  if [[ -z "$value" || "$value" == --* ]]; then
    log_error "Flag $flag requires a value"
    log_error "Use --help for usage information"
    exit 1
  fi
}

parse_arguments() {
  while [[ $# -gt 0 ]]; do
    case $1 in
      --operator-type)
        require_flag_value "$1" "${2:-}"
        OPERATOR_TYPE="$2"
        shift 2
        ;;
      --policy-engine)
        require_flag_value "$1" "${2:-}"
        POLICY_ENGINE="$2"
        shift 2
        ;;
      --enable-tls-backend)
        ENABLE_TLS_BACKEND="true"
        shift
        ;;
      --disable-tls-backend)
        ENABLE_TLS_BACKEND="false"
        shift
        ;;
      --enable-keycloak)
        ENABLE_KEYCLOAK="true"
        shift
        ;;
      --external-oidc)
        EXTERNAL_OIDC="true"
        shift
        ;;
      --namespace)
        require_flag_value "$1" "${2:-}"
        NAMESPACE="$2"
        shift 2
        ;;
      --verbose)
        VERBOSE="true"
        LOG_LEVEL="DEBUG"
        CURRENT_LOG_LEVEL=$LOG_LEVEL_DEBUG
        shift
        ;;
      --dry-run)
        DRY_RUN="true"
        shift
        ;;
      --operator-catalog)
        require_flag_value "$1" "${2:-}"
        OPERATOR_CATALOG="$2"
        shift 2
        ;;
      --operator-image)
        require_flag_value "$1" "${2:-}"
        OPERATOR_IMAGE="$2"
        shift 2
        ;;
      --maas-api-image)
        require_flag_value "$1" "${2:-}"
        MAAS_API_IMAGE="$2"
        shift 2
        ;;
      --maas-controller-image)
        require_flag_value "$1" "${2:-}"
        MAAS_CONTROLLER_IMAGE="$2"
        shift 2
        ;;
      --ai-gateway-operator-image)
        require_flag_value "$1" "${2:-}"
        AI_GATEWAY_OPERATOR_IMAGE="$2"
        shift 2
        ;;
      --channel)
        require_flag_value "$1" "${2:-}"
        OPERATOR_CHANNEL="$2"
        shift 2
        ;;
      --postgres-connection)
        require_flag_value "$1" "${2:-}"
        POSTGRES_CONNECTION="$2"
        shift 2
        ;;
      --dev)
        DEV_MODE="true"
        shift
        ;;
      --help|-h)
        show_help
        exit 0
        ;;
      *)
        log_error "Unknown option: $1"
        log_error "Use --help for usage information"
        exit 1
        ;;
    esac
  done
}

#──────────────────────────────────────────────────────────────
# PREREQUISITE CHECKS
#──────────────────────────────────────────────────────────────

check_required_tools() {
  local missing=()
  local required_kustomize="5.7.0"

  command -v oc &>/dev/null || missing+=("oc (OpenShift CLI)")
  command -v kubectl &>/dev/null || missing+=("kubectl")
  command -v jq &>/dev/null || missing+=("jq")
  if command -v kustomize &>/dev/null; then
    local kustomize_version
    kustomize_version=$(kustomize version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)
    # Fallback: extract version from Go binary metadata (works for dev builds)
    if [[ -z "$kustomize_version" ]] && command -v go &>/dev/null; then
      kustomize_version=$(go version -m "$(command -v kustomize)" 2>/dev/null | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' | head -1 | tr -d 'v')
    fi
    if [[ -z "$kustomize_version" ]]; then
      log_warn "kustomize is a dev build with unverifiable version. Cannot guarantee compatibility with v$required_kustomize+."
    elif [[ "$(printf '%s\n%s' "$required_kustomize" "$kustomize_version" | sort -V | head -1)" != "$required_kustomize" ]]; then
      missing+=("kustomize (v$required_kustomize+ required, found ${kustomize_version})")
    fi
  else
    missing+=("kustomize (v$required_kustomize+)")
  fi
  if [[ "$(uname -s)" == "Darwin" ]]; then
    command -v gsed &>/dev/null || missing+=("gsed (GNU sed) for MacOS")
  else
    command -v sed &>/dev/null || missing+=("sed (GNU sed)")
  fi

  if [[ ${#missing[@]} -gt 0 ]]; then
    log_error "Missing or incompatible required tools:"
    for tool in "${missing[@]}"; do
      log_error "  - $tool"
    done
    return 1
  fi
}

#──────────────────────────────────────────────────────────────
# CONFIGURATION VALIDATION
#──────────────────────────────────────────────────────────────

validate_configuration() {
  log_info "Validating configuration..."

  # Validate operator type
  if [[ ! "$OPERATOR_TYPE" =~ ^(rhoai|odh)$ ]]; then
    log_error "Invalid operator type: $OPERATOR_TYPE"
    log_error "Must be 'rhoai' or 'odh'"
    exit 1
  fi

  # Auto-determine policy engine based on operator type unless explicitly set.
  if [[ -n "$POLICY_ENGINE" ]]; then
    if [[ ! "$POLICY_ENGINE" =~ ^(rhcl|kuadrant)$ ]]; then
      log_error "Invalid policy engine: $POLICY_ENGINE"
      log_error "Must be 'rhcl' or 'kuadrant'"
      exit 1
    fi
    log_debug "Using explicitly configured policy engine: $POLICY_ENGINE"
  else
    case "$OPERATOR_TYPE" in
      odh)
        POLICY_ENGINE="kuadrant"
        log_debug "Auto-selected policy engine for ODH: kuadrant (community v1.4.2)"
        ;;
      rhoai)
        POLICY_ENGINE="rhcl"
        log_debug "Auto-selected policy engine for RHOAI: rhcl (Red Hat Connectivity Link)"
        ;;
    esac
  fi

  # Determine namespace based on operator type
  case "$OPERATOR_TYPE" in
    rhoai)
      NAMESPACE="redhat-ods-applications"
      ;;
    odh|*)
      NAMESPACE="opendatahub"
      ;;
  esac
  log_debug "Using namespace: $NAMESPACE"

  # Export so subprocesses (subscripts called via bash, not sourced functions) inherit the values.
  export NAMESPACE OPERATOR_TYPE

  log_info "Configuration validated successfully"
}

#──────────────────────────────────────────────────────────────
# DEPLOYMENT ORCHESTRATION
#──────────────────────────────────────────────────────────────

main() {
  log_info "==================================================="
  log_info "  Models-as-a-Service Deployment"
  log_info "==================================================="

  parse_arguments "$@"
  check_required_tools
  validate_configuration

  log_info "Deployment configuration:"
  log_info "  Operator: $OPERATOR_TYPE"
  log_info "  Policy Engine: $POLICY_ENGINE"
  log_info "  Namespace: $NAMESPACE"
  log_info "  TLS Backend: $ENABLE_TLS_BACKEND"
  log_info "  External OIDC: $EXTERNAL_OIDC"
  if [[ "$EXTERNAL_OIDC" == "true" ]]; then
    log_warn "  --external-oidc is ignored in operator mode. Configure external OIDC via"
    log_warn "  the ModelsAsService CR: spec.externalOIDC.issuerUrl / clientId instead."
  fi
  if [[ -n "${MAAS_API_IMAGE:-}" ]]; then
    log_info "  MaaS API image: $MAAS_API_IMAGE"
  fi
  if [[ -n "${MAAS_CONTROLLER_IMAGE:-}" ]]; then
    log_info "  MaaS controller image: $MAAS_CONTROLLER_IMAGE"
  fi
  if [[ -n "${AI_GATEWAY_OPERATOR_IMAGE:-}" ]]; then
    log_info "  ai-gateway-operator image: $AI_GATEWAY_OPERATOR_IMAGE"
  fi

  if [[ "$DRY_RUN" == "true" ]]; then
    log_info "DRY RUN MODE - no changes will be applied"
    log_info "Deployment plan validated. Exiting."
    exit 0
  fi

  deploy_via_operator

  # Install maas-controller.
  # The Tenant reconciler in maas-controller is the sole deployer of maas-api.
  # Skip if the ODH operator already created the deployment (3.4+).
  log_info ""
  log_info "MaaS Controller..."
  local script_dir
  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  local project_root="$script_dir/.."
  local controller_dir="$project_root/maas-controller"
  local config_dir="$project_root/deployment/base/maas-controller/default"

  if [[ ! -d "$controller_dir" ]]; then
    log_error "maas-controller directory not found at $controller_dir — controller is required"
    return 1
  fi

  if ! kubectl get namespace "$NAMESPACE" &>/dev/null; then
    log_error "Namespace $NAMESPACE does not exist."
    return 1
  fi

  local maas_controller_exists=false
  if kubectl get deployment maas-controller -n "$NAMESPACE" &>/dev/null; then
    maas_controller_exists=true
  elif [[ "$FORCE_OVERWRITE" != "true" ]]; then
    # The ODH operator's AIGateway/ModelsAsService module reconciler owns deploying
    # maas-controller. Silently falling back to a direct kustomize install here would
    # mask integration gaps (e.g. RBAC errors, manifest drift, version skew). So: wait
    # briefly for the operator to reconcile, then fail loudly with diagnostics if it doesn't.
    log_info "  Waiting for the ODH operator to create maas-controller (operator-managed)..."
    if wait_for_resource "deployment" "maas-controller" "$NAMESPACE" "$ROLLOUT_TIMEOUT"; then
      maas_controller_exists=true
    else
      log_error "The ODH operator did not create maas-controller within ${ROLLOUT_TIMEOUT}s."
      log_error "This means the operator's AIGateway/ModelsAsService module failed to reconcile it — a real integration gap, not something deploy.sh should paper over in operator mode."
      log_error "Failing DataScienceCluster module conditions:"
      local dsc_name_diag
      dsc_name_diag=$(kubectl get datasciencecluster -A -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
      if [[ -n "$dsc_name_diag" ]]; then
        kubectl get datasciencecluster "$dsc_name_diag" \
          -o jsonpath='{range .status.conditions[?(@.status=="False")]}  {.type}: {.reason} - {.message}{"\n"}{end}' 2>/dev/null \
          | while IFS= read -r line; do log_error "$line"; done
      fi
      log_error "Tip: set FORCE_OVERWRITE=true to bypass this check and install maas-controller directly (only for local debugging; defeats the purpose of operator-mode validation)."
      return 1
    fi
  fi

  if [[ "$maas_controller_exists" == "true" && "$FORCE_OVERWRITE" != "true" ]]; then
    log_info "  maas-controller already exists in $NAMESPACE (e.g. operator-managed), skipping manifest apply"
  else
    # Direct-install path used when maas-controller is absent, or when
    # FORCE_OVERWRITE=true requests a full local re-apply and restart.
    # deployment/base/maas-controller/default now generates its own
    # maas-parameters ConfigMap (see its kustomization.yaml), so image
    # overrides below are injected via a `behavior: merge` configMapGenerator
    # in the temporary overlay (Phase 2) instead of a separate kubectl-create
    # step -- a standalone create here would just be overwritten by Phase 2's
    # apply and silently drop PR-testing image overrides.
    local default_tag="odh-stable"
    [[ "${DEV_MODE:-false}" == "true" ]] && default_tag="latest"
    local cm_maas_api_image="${MAAS_API_IMAGE:-quay.io/opendatahub/maas-api:${default_tag}}"
    local cm_maas_controller_image="${MAAS_CONTROLLER_IMAGE:-quay.io/opendatahub/maas-controller:${default_tag}}"
    local cm_payload_processing_image="${PAYLOAD_PROCESSING_IMAGE:-$(get_odh_overlay_param payload-processing-image 2>/dev/null || echo "quay.io/opendatahub/odh-ai-gateway-payload-processing:odh-stable")}"
    local cm_cleanup_image="registry.redhat.io/ubi9/ubi-minimal:9.7"
    local cm_monitoring_namespace="${MONITORING_NAMESPACE:-opendatahub}"

    log_info "  Phase 1: Applying MaaS CRDs and waiting until Established (controller creates Config after CRD is ready)..."
    if ! install_maas_controller_crds_and_wait "${project_root}/deployment/base/maas-controller/crd"; then
      log_error "MaaS CRD install or Established wait failed"
      return 1
    fi
    log_info "  Phase 2: Applying full controller kustomize (same as operator: deployment/base/maas-controller/default)..."
    local controller_overlay_dir
    controller_overlay_dir="$(mktemp -d "${project_root}/.deploy-controller-overlay.XXXXXX")" || {
      log_error "Failed to create temporary maas-controller overlay directory"
      return 1
    }
    cat > "${controller_overlay_dir}/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: ${NAMESPACE}
resources:
  - ../deployment/base/maas-controller/default
# Overrides deployment/base/maas-controller/default's own maas-parameters
# ConfigMap (merge, not create) with this run's resolved image/namespace
# values, so PR-testing overrides (--maas-api-image, --payload-processing-image,
# MONITORING_NAMESPACE, etc.) reach both the Deployment env vars and any
# tenant-reconciler code that reads this ConfigMap.
configMapGenerator:
  - name: maas-parameters
    behavior: merge
    literals:
      - maas-api-image=${cm_maas_api_image}
      - maas-controller-image=${cm_maas_controller_image}
      - payload-processing-image=${cm_payload_processing_image}
      - maas-api-key-cleanup-image=${cm_cleanup_image}
      - monitoring-namespace=${cm_monitoring_namespace}
      - namespace=${NAMESPACE}
generatorOptions:
  disableNameSuffixHash: true
# Re-run the serverName replacement at the parent level so it picks up the
# merged namespace value (child replacements run before parent merge).
replacements:
  - source:
      kind: ConfigMap
      name: maas-parameters
      fieldPath: data.namespace
    targets:
      - select:
          kind: ServiceMonitor
          name: maas-controller-metrics
        fieldPaths:
          - spec.endpoints.0.tlsConfig.serverName
        options:
          delimiter: "."
          index: 1
EOF
    (
      cd "${controller_overlay_dir}" && \
      kustomize edit set image "quay.io/opendatahub/maas-controller=${cm_maas_controller_image}" && \
      kustomize build .
    ) | kubectl apply -f - || {
      rm -rf "${controller_overlay_dir}"
      log_error "Failed to apply maas-controller manifests"
      return 1
    }
    rm -rf "${controller_overlay_dir}"

    # Force pod recreation so imagePullPolicy=Always can pick up newly published
    # image content even when the maas-controller image tag itself is unchanged.
    log_info "  Restarting maas-controller to pick up manifest and ConfigMap changes"
    kubectl rollout restart deployment/maas-controller -n "$NAMESPACE" || {
      log_error "Failed to restart maas-controller deployment"
      return 1
    }
  fi

  # Patch INFRA_NAMESPACE if set via environment variable
  # Patch INFRA_NAMESPACE if explicitly set (including empty string for ROSA)
  # Use parameter expansion to distinguish: unset vs set-to-empty vs set-to-value
  if [ "${INFRA_NAMESPACE+x}" = "x" ]; then
    log_info "  Patching maas-controller with INFRA_NAMESPACE=${INFRA_NAMESPACE}"
    local infra_ns_value="$INFRA_NAMESPACE"

    # Find the index of INFRA_NAMESPACE in the env array
    local env_index
    env_index=$(kubectl get deployment maas-controller -n "$NAMESPACE" -o json | \
      jq '.spec.template.spec.containers[0].env | map(.name) | index("INFRA_NAMESPACE")')

    if [ "$env_index" != "null" ]; then
      kubectl patch deployment maas-controller -n "$NAMESPACE" --type=json -p="[
        {\"op\": \"replace\", \"path\": \"/spec/template/spec/containers/0/env/${env_index}\",
         \"value\": {\"name\": \"INFRA_NAMESPACE\", \"value\": \"${infra_ns_value}\"}}
      ]" || log_warn "Failed to patch INFRA_NAMESPACE (non-fatal)"
    fi
  fi

  log_info "  Waiting for maas-controller to be ready..."
  if ! kubectl rollout status deployment/maas-controller -n "$NAMESPACE" --timeout="${ROLLOUT_TIMEOUT}s"; then
    log_error "maas-controller deployment not ready (timeout: ${ROLLOUT_TIMEOUT}s)"
    return 1
  fi
  log_info "  Controller ready."

  # Wait for the Tenant reconciler to deploy maas-api.
  # The controller creates AITenant/models-as-a-service on startup; the AITenant
  # reconciler then creates/adopts MaasTenantConfig/default-tenant, and the Tenant
  # reconciler renders and SSA-applies maas-api manifests + gateway policies.
  # All maas-api instances deploy to infrastructure namespace (controlled by INFRA_NAMESPACE).
  # Infrastructure namespace is configurable via deployment overlays (params.env).
  log_info ""
  log_info "Waiting for Tenant reconciler to deploy maas-api..."
  local infra_namespace_raw="${INFRA_NAMESPACE-AUTO}"
  local infra_namespace
  if [ "$infra_namespace_raw" = "AUTO" ]; then
    infra_namespace=$(derive_infra_namespace "$NAMESPACE")
  elif [ -z "$infra_namespace_raw" ]; then
    infra_namespace="$NAMESPACE"
  else
    infra_namespace="$infra_namespace_raw"
  fi

  # Apply infra RBAC for secret migration when namespace separation is active
  if [ "$infra_namespace" != "$NAMESPACE" ] && [ -n "$infra_namespace" ]; then
    apply_infra_secret_migration_rbac "$infra_namespace" "$NAMESPACE"
  fi

  local maas_api_timeout="${CUSTOM_RESOURCE_TIMEOUT:-600}"
  local elapsed=0
  while [[ $elapsed -lt $maas_api_timeout ]]; do
    if kubectl get deployment maas-api -n "$infra_namespace" &>/dev/null; then
      log_info "  maas-api deployment found in $infra_namespace, waiting for rollout..."
      if kubectl rollout status deployment/maas-api -n "$infra_namespace" --timeout="$((maas_api_timeout - elapsed))s" 2>/dev/null; then
        log_info "  maas-api is ready"
        break
      fi
    fi
    sleep 10
    elapsed=$((elapsed + 10))
    if (( elapsed % 60 == 0 )); then
      log_info "  Still waiting for maas-api deployment... (${elapsed}s / ${maas_api_timeout}s)"
    fi
  done

  if ! kubectl get deployment maas-api -n "$infra_namespace" &>/dev/null; then
    log_error "maas-api deployment not created by Tenant reconciler after ${maas_api_timeout}s"
    log_error "Expected in namespace: $infra_namespace"
    log_error "Check maas-controller logs: kubectl logs -l app.kubernetes.io/name=maas-controller -n $NAMESPACE"
    return 1
  fi

  # External OIDC: Patch the default AITenant (source of truth for tenant OIDC)
  # so the MaaSAuthPolicy controller can add oidc-identities authentication
  log_info ""
  log_info "MaaS API and MaaS Controller deployment completed successfully!"
  local deployed_api_image deployed_ctrl_image
  deployed_api_image=$(kubectl get deployment/maas-api -n "$infra_namespace" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "unknown")
  deployed_ctrl_image=$(kubectl get deployment/maas-controller -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null || echo "unknown")
  log_info "  maas-api image:        $deployed_api_image (namespace: $infra_namespace)"
  log_info "  maas-controller image: $deployed_ctrl_image (namespace: $NAMESPACE)"

  log_info "==================================================="
  log_info "  Models-as-a-Service Deployment completed successfully!"
  log_info "==================================================="
}

#──────────────────────────────────────────────────────────────
# OPERATOR-BASED DEPLOYMENT
#──────────────────────────────────────────────────────────────

deploy_via_operator() {
  log_info "Starting operator-based deployment..."

  # Install shared platform dependencies via Helm chart
  # (cert-manager, LWS, RHCL/Kuadrant, ODH/RHOAI operator, DSCI, DSC, Gateway)
  "${SCRIPT_DIR}/setup-shared-deps.sh"

  # Wait for ai-gateway-operator (deployed by the ODH operator's AIGateway module reconciler)
  # to roll out with the requested image before proceeding.
  if [[ -n "$AI_GATEWAY_OPERATOR_IMAGE" ]]; then
    log_info "Waiting for ai-gateway-operator to be deployed..."
    if wait_for_resource "deployment" "ai-gateway-operator" "$NAMESPACE" "$ROLLOUT_TIMEOUT"; then
      kubectl rollout status deployment/ai-gateway-operator -n "$NAMESPACE" --timeout="${ROLLOUT_TIMEOUT}s" || {
        log_error "ai-gateway-operator deployment not ready (timeout: ${ROLLOUT_TIMEOUT}s)"
        exit 1
      }
      log_info "ai-gateway-operator ready."
    else
      log_error "ai-gateway-operator deployment not found in $NAMESPACE after ${ROLLOUT_TIMEOUT}s"
      exit 1
    fi
  fi

  # Deploy PostgreSQL for API key storage (requires namespace to exist)
  deploy_postgresql

  # Deploy Keycloak identity provider (optional, if enabled)
  if [[ "$ENABLE_KEYCLOAK" == "true" ]]; then
    deploy_keycloak
  fi

  # Wait for maas-controller (deployed by ai-gateway-operator via DSC reconciliation).
  # The deployment may not exist yet — wait for it to be created, then wait for rollout.
  local controller_wait=${CONTROLLER_WAIT_TIMEOUT:-600}
  log_info "Waiting for maas-controller deployment to be created (timeout: ${controller_wait}s)..."
  if ! wait_for_resource "deployment" "maas-controller" "$NAMESPACE" "$controller_wait"; then
    log_error "maas-controller deployment was not created within ${controller_wait}s."
    log_error "Check DSC reconciliation status and ai-gateway-operator logs."
    local dsc_name_diag
    dsc_name_diag=$(kubectl get datasciencecluster -A -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    if [[ -n "$dsc_name_diag" ]]; then
      log_error "Failing DataScienceCluster module conditions:"
      kubectl get datasciencecluster "$dsc_name_diag" \
        -o jsonpath='{range .status.conditions[?(@.status=="False")]}  {.type}: {.reason} - {.message}{"\n"}{end}' 2>/dev/null \
        | while IFS= read -r line; do log_error "$line"; done
    fi
    exit 1
  fi
  # Apply latest RBAC from local repo after operator has deployed maas-controller.
  # The operator bundles an older copy of the ClusterRole (e.g. missing HPA permissions).
  # Applying here overwrites it. No pod restart needed — RBAC takes effect immediately
  # and the running controller picks up permissions on its next reconcile retry.
  log_info "Applying latest MaaS RBAC (cluster-scoped) from local repo..."
  local project_root
  project_root="$(cd "$SCRIPT_DIR/.." && pwd)"
  local rbac_dir="${project_root}/deployment/base/maas-controller/rbac"
  kubectl apply -f "${rbac_dir}/clusterrole.yaml" \
                -f "${rbac_dir}/clusterrole_maas_configs.yaml" \
                -f "${rbac_dir}/clusterrole_binding.yaml" \
                -f "${rbac_dir}/clusterrolebinding_maas_configs.yaml"
  local ocp_rbac_dir="${rbac_dir}/ocp"
  if [[ -d "$ocp_rbac_dir" ]]; then
    kubectl apply -f "${ocp_rbac_dir}/clusterrole_ocp.yaml" \
                  -f "${ocp_rbac_dir}/clusterrolebinding_ocp.yaml" 2>/dev/null || true
  fi
  log_info "Waiting for maas-controller rollout..."
  if ! kubectl rollout status deployment/maas-controller -n "$NAMESPACE" --timeout="${POD_TIMEOUT:-300}s"; then
    log_error "maas-controller not ready (timeout: ${POD_TIMEOUT:-300}s)"
    exit 1
  fi
  log_info "  maas-controller ready."

  # Wait for maas-api (deployed by maas-controller via AITenant reconciler).
  wait_for_operator_maas_api

  # Configure TLS backend (if enabled)
  if [[ "$ENABLE_TLS_BACKEND" == "true" ]]; then
    configure_tls_backend
  fi

  log_info "Operator deployment completed"
}

#──────────────────────────────────────────────────────────────
# POSTGRESQL DEPLOYMENT
#──────────────────────────────────────────────────────────────

validate_postgres_connection() {
  local conn="$1"
  if [[ ! "$conn" =~ ^postgres(ql)?:// ]]; then
    log_error "Invalid PostgreSQL connection string format"
    log_error "Expected: postgresql://USER:PASSWORD@HOST:PORT/DATABASE?sslmode=require"
    return 1
  fi
}

# wait_for_operator_maas_api waits for maas-api to be deployed by the Tenant
# reconciler (maas-controller) in the infrastructure namespace.
wait_for_operator_maas_api() {
  local infra_namespace_raw="${INFRA_NAMESPACE-AUTO}"
  local infra_namespace
  if [ "$infra_namespace_raw" = "AUTO" ]; then
    infra_namespace=$(derive_infra_namespace "$NAMESPACE")
  elif [ -z "$infra_namespace_raw" ]; then
    infra_namespace="$NAMESPACE"
  else
    infra_namespace="$infra_namespace_raw"
  fi

  log_info "Waiting for Tenant reconciler to deploy maas-api in $infra_namespace..."
  local maas_api_timeout="${CUSTOM_RESOURCE_TIMEOUT:-600}"
  local elapsed=0
  while [[ $elapsed -lt $maas_api_timeout ]]; do
    if kubectl get deployment maas-api -n "$infra_namespace" &>/dev/null; then
      log_info "  maas-api deployment found, waiting for rollout..."
      if kubectl rollout status deployment/maas-api -n "$infra_namespace" --timeout="$((maas_api_timeout - elapsed))s" 2>/dev/null; then
        log_info "  maas-api is ready"
        return 0
      fi
    fi
    sleep 10
    elapsed=$((elapsed + 10))
    (( elapsed % 60 == 0 )) && log_info "  Still waiting for maas-api... (${elapsed}s / ${maas_api_timeout}s)"
  done

  log_error "maas-api not created after ${maas_api_timeout}s in $infra_namespace"
  log_error "Check: kubectl logs -l app.kubernetes.io/name=maas-controller -n $NAMESPACE"
  return 1
}

deploy_postgresql() {
  # Infrastructure namespace where maas-api runs (AUTO = derive from controller namespace)
  local controller_ns="${NAMESPACE:-opendatahub}"
  local infra_ns_raw="${INFRA_NAMESPACE-AUTO}"
  local infra_ns
  if [ "$infra_ns_raw" = "AUTO" ]; then
    infra_ns=$(derive_infra_namespace "$controller_ns")
  elif [ -z "$infra_ns_raw" ]; then
    infra_ns="$controller_ns"
  else
    infra_ns="$infra_ns_raw"
  fi

  if [[ -n "$POSTGRES_CONNECTION" ]]; then
    validate_postgres_connection "$POSTGRES_CONNECTION" || exit 1
    log_info "Using external PostgreSQL connection"
    # Create secret in infrastructure namespace for maas-api access
    create_maas_db_config_secret "$infra_ns" "$POSTGRES_CONNECTION"
    log_info "Created maas-db-config secret in $infra_ns with external connection"
  else
    log_warn "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    log_warn "  DEPLOYING POC POSTGRESQL — NOT INTENDED FOR PRODUCTION USE"
    log_warn "  Data is stored in ephemeral storage and will be lost on pod restart."
    log_warn "  For production, use --postgres-connection with an external database"
    log_warn "  (AWS RDS, Crunchy Operator, Azure Database, etc.)"
    log_warn "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    # setup-database.sh handles upgrade detection and namespace selection
    NAMESPACE="$controller_ns" "${SCRIPT_DIR}/setup-database.sh"
  fi
}

#──────────────────────────────────────────────────────────────
# KEYCLOAK DEPLOYMENT
#──────────────────────────────────────────────────────────────

deploy_keycloak() {
  log_info "Deploying Keycloak identity provider for external OIDC support..."
  "${SCRIPT_DIR}/setup-keycloak.sh"
}

#──────────────────────────────────────────────────────────────
# AUDIENCE CONFIGURATION FOR HYPERSHIFT/ROSA CLUSTERS
#──────────────────────────────────────────────────────────────

# get_odh_overlay_param
#   Reads a value from the canonical maas-controller params.env.
get_odh_overlay_param() {
  local key="$1"
  local project_root
  project_root="$(find_project_root)" || return 1

  local params_file="$project_root/deployment/base/maas-controller/default/params.env"
  [[ -f "$params_file" ]] || return 1

  awk -F= -v key="$key" '$1 == key { print substr($0, index($0, "=") + 1); exit }' "$params_file"
}

resolve_external_oidc_issuer() {
  local oidc_issuer_url="${OIDC_ISSUER_URL:-}"

  if [[ -z "$oidc_issuer_url" || "$oidc_issuer_url" == "https://oidc.example.invalid/realms/maas" ]]; then
    return 1
  fi

  printf '%s\n' "$oidc_issuer_url"
}

resolve_external_oidc_client_id() {
  local oidc_client_id="${OIDC_CLIENT_ID:-}"

  if [[ -z "$oidc_client_id" ]]; then
    return 1
  fi

  printf '%s\n' "$oidc_client_id"
}

patch_authpolicy_from_template() {
  local authpolicy_name="$1"
  local template_file="$2"
  local maas_namespace="$3"
  local oidc_issuer_url="${4:-}"
  local oidc_client_id="${5:-}"
  local cluster_audience="${6:-https://kubernetes.default.svc}"

  # Render placeholders in the YAML template.
  local rendered_rules
  rendered_rules=$(sed \
    -e "s|__MAAS_NAMESPACE__|${maas_namespace}|g" \
    -e "s|__OIDC_ISSUER_URL__|${oidc_issuer_url}|g" \
    -e "s|__OIDC_CLIENT_ID__|${oidc_client_id}|g" \
    -e "s|__CLUSTER_AUDIENCE__|${cluster_audience}|g" \
    "$template_file")

  # Use kubectl replace with a full manifest instead of merge patch.
  # Merge patch cannot reliably delete "when" arrays or replace "selector"
  # with "expression" inside CRD objects, causing stale fields to persist.
  local resource_version
  resource_version=$(kubectl get authpolicy "$authpolicy_name" -n "$maas_namespace" \
    -o jsonpath='{.metadata.resourceVersion}')

  local when_predicate
  when_predicate=$(kubectl get authpolicy "$authpolicy_name" -n "$maas_namespace" \
    -o jsonpath='{.spec.when[0].predicate}')

  local manifest
  manifest="$(mktemp)"
  cat > "$manifest" <<MANIFEST_EOF
apiVersion: kuadrant.io/v1
kind: AuthPolicy
metadata:
  name: ${authpolicy_name}
  namespace: ${maas_namespace}
  resourceVersion: "${resource_version}"
  annotations:
    opendatahub.io/managed: "false"
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: maas-api-route
  when:
    - predicate: '${when_predicate}'
$(echo "$rendered_rules" | sed -n '/^  rules:/,$p')
MANIFEST_EOF

  kubectl replace -f "$manifest"
  local rc=$?
  rm -f "$manifest"
  return $rc
}
# configure_tenant_external_oidc
#   Patches the default AITenant with spec.oidc.
configure_tenant_external_oidc() {
  local aitenant_name="${DEFAULT_AITENANT_NAME:-models-as-a-service}"
  local aitenant_ns="${AITENANT_NAMESPACE:-ai-tenants}"

  log_info "Configuring default tenant with external OIDC..."

  local oidc_issuer_url
  oidc_issuer_url="$(resolve_external_oidc_issuer)" || {
    log_error "External OIDC requested but no OIDC_ISSUER_URL was configured"
    return 1
  }

  local oidc_client_id
  oidc_client_id="$(resolve_external_oidc_client_id)" || {
    log_error "External OIDC requested but no OIDC_CLIENT_ID was configured"
    return 1
  }

  local aitenant_patch
  aitenant_patch=$(jq -nc \
    --arg issuerUrl "$oidc_issuer_url" \
    --arg clientId "$oidc_client_id" \
    '{spec:{oidc:{issuerUrl:$issuerUrl,clientId:$clientId}}}')

  if kubectl get aitenant "$aitenant_name" -n "$aitenant_ns" &>/dev/null; then
    log_info "  Patching AITenant '$aitenant_name' with external OIDC"
    if ! kubectl patch aitenant "$aitenant_name" -n "$aitenant_ns" --type=merge -p "$aitenant_patch"; then
      log_error "  Failed to patch AITenant with external OIDC"
      return 1
    fi
  else
    log_error "AITenant '$aitenant_name' not found in namespace '$aitenant_ns'; cannot configure external OIDC"
    return 1
  fi

  log_info "  Default tenant OIDC configuration patched successfully"
}

#──────────────────────────────────────────────────────────────
# TLS BACKEND CONFIGURATION
#──────────────────────────────────────────────────────────────

configure_tls_backend() {
  log_info "Configuring TLS backend for Authorino and MaaS API..."

  # Authorino and Kuadrant workloads run in kuadrant-system for both RHCL and community Kuadrant.
  local authorino_namespace="${RHCL_NAMESPACE:-kuadrant-system}"
  case "$POLICY_ENGINE" in
    rhcl|kuadrant)
      ;;
    *)
      log_warn "Unknown policy engine: $POLICY_ENGINE, defaulting to kuadrant-system"
      authorino_namespace="kuadrant-system"
      ;;
  esac

  # Wait for Authorino deployment to be created by Kuadrant operator
  # This is necessary because Kuadrant may not be fully ready yet (timing issue)
  wait_for_resource "deployment" "authorino" "$authorino_namespace" "$RESOURCE_TIMEOUT" || {
    log_warn "Authorino deployment not found after ${RESOURCE_TIMEOUT}s, TLS configuration may fail"
  }

  # Call TLS configuration script
  local tls_script="${SCRIPT_DIR}/setup-authorino-tls.sh"
  if [[ ! -f "$tls_script" ]]; then
    log_warn "TLS configuration script not found at $tls_script, skipping"
    return 0
  fi

  log_info "Running TLS configuration script..."
  # Capture output and exit code separately to avoid pipeline masking the script's exit status
  # (piping to while-read would check while's exit status, not the script's)
  local tls_output
  local tls_rc=0
  tls_output=$(AUTHORINO_NAMESPACE="$authorino_namespace" "$tls_script" 2>&1) || tls_rc=$?
  
  # Log each line of output
  while read -r line; do log_debug "$line"; done <<< "$tls_output"
  
  if [[ $tls_rc -eq 0 ]]; then
    log_info "TLS configuration script completed successfully"
  else
    log_warn "TLS configuration script had issues (exit code: $tls_rc, non-fatal, continuing)"
  fi

  # Restart deployments to pick up TLS config
  log_info "Restarting deployments to pick up TLS configuration..."

  # maas-api deploys to infrastructure namespace
  local infra_namespace_raw="${INFRA_NAMESPACE-AUTO}"
  local infra_namespace
  if [ "$infra_namespace_raw" = "AUTO" ]; then
    infra_namespace=$(derive_infra_namespace "$NAMESPACE")
  elif [ -z "$infra_namespace_raw" ]; then
    infra_namespace="$NAMESPACE"
  else
    infra_namespace="$infra_namespace_raw"
  fi
  kubectl rollout restart deployment/maas-api -n "$infra_namespace" 2>/dev/null || log_debug "maas-api deployment not found or not yet ready"
  kubectl rollout restart deployment/authorino -n "$authorino_namespace" 2>/dev/null || log_debug "authorino deployment not found or not yet ready"
  
  # Wait for Authorino to be ready after restart
  log_info "Waiting for Authorino deployment to be ready..."
  kubectl rollout status deployment/authorino -n "$authorino_namespace" --timeout="${ROLLOUT_TIMEOUT}s" 2>/dev/null || log_warn "Authorino rollout status check timed out (timeout: ${ROLLOUT_TIMEOUT}s)"

  log_info "TLS backend configuration complete"
}

#──────────────────────────────────────────────────────────────
# MAIN ENTRY POINT
#──────────────────────────────────────────────────────────────

main "$@"
