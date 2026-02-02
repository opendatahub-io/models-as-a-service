#!/bin/bash
################################################################################
# MaaS Quick Deployment Script
#
# This script is used for the quick deployment of all required objects for the
# Models-as-a-Service (MaaS) platform.
#
# Usage: ./scripts/deploy.sh [OPTIONS]
#
# For manual installation instructions, please see:
# https://opendatahub-io.github.io/models-as-a-service/latest/install/maas-setup/
################################################################################

set -euo pipefail

# Source helpers
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deployment-helpers.sh
source "${SCRIPT_DIR}/deployment-helpers.sh"

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

DEPLOYMENT_MODE="${DEPLOYMENT_MODE:-operator}"
OPERATOR_TYPE="${OPERATOR_TYPE:-rhoai}"
RATE_LIMITER="${RATE_LIMITER:-}"  # Auto-determined based on deployment mode
NAMESPACE="${NAMESPACE:-}"  # Auto-determined based on operator type
SKIP_CERT_MANAGER="${SKIP_CERT_MANAGER:-auto}"
SKIP_LWS="${SKIP_LWS:-auto}"
ENABLE_TLS_BACKEND="${ENABLE_TLS_BACKEND:-true}"
VERBOSE="${VERBOSE:-false}"
DRY_RUN="${DRY_RUN:-false}"
OPERATOR_CATALOG="${OPERATOR_CATALOG:-}"
OPERATOR_IMAGE="${OPERATOR_IMAGE:-}"
OPERATOR_CHANNEL="${OPERATOR_CHANNEL:-}"

#──────────────────────────────────────────────────────────────
# HELP TEXT
#──────────────────────────────────────────────────────────────

show_help() {
  cat <<EOF
Unified deployment script for Models-as-a-Service

USAGE:
  ./scripts/deploy.sh [OPTIONS]

OPTIONS:
  --deployment-mode <operator|kustomize>
      Deployment method (default: operator)

  --operator-type <rhoai|odh>
      Which operator to install (default: rhoai)
      Only applies when --deployment-mode=operator

  --rate-limiter <rhcl|kuadrant>
      Rate limiting component (auto-determined by default)
      - rhcl: Red Hat Connectivity Link (downstream)
        Required for: RHOAI operator mode
        Supported for: RHOAI operator, ODH operator, kustomize modes
        Default for: operator mode
      - kuadrant: Kuadrant operator (upstream)
        Supported for: ODH operator mode, kustomize mode
        Default for: kustomize mode

  --enable-tls-backend
      Enable TLS backend for Authorino and MaaS API (default: enabled)
      Configures HTTPS tier lookup URL

  --skip-certmanager
      Skip cert-manager installation (auto-detected by default)

  --skip-lws
      Skip LeaderWorkerSet installation (auto-detected by default)

  --namespace <namespace>
      Target namespace for deployment
      Default: redhat-ods-applications (RHOAI) or opendatahub (ODH)

  --verbose
      Enable verbose/debug logging

  --dry-run
      Show what would be done without applying changes

  --help
      Display this help message

ADVANCED OPTIONS (PR Testing):
  --operator-catalog <image>
      Custom operator catalog/index image (for testing PRs)
      Example: quay.io/opendatahub/opendatahub-operator-catalog:pr-456

  --operator-image <image>
      Custom operator image (patches CSV after install)
      Example: quay.io/opendatahub/opendatahub-operator:pr-456

  --channel <channel>
      Operator channel override
      Default: fast (RHOAI and ODH)

ENVIRONMENT VARIABLES:
  MAAS_API_IMAGE        Custom MaaS API container image
  OPERATOR_CATALOG      Custom operator catalog
  OPERATOR_IMAGE        Custom operator image
  OPERATOR_TYPE         Operator type (rhoai/odh)
  RATE_LIMITER          Rate limiter (rhcl/kuadrant)
  LOG_LEVEL             Logging verbosity (DEBUG, INFO, WARN, ERROR)

EXAMPLES:
  # Deploy RHOAI (default)
  ./scripts/deploy.sh

  # Deploy ODH with upstream Kuadrant
  ./scripts/deploy.sh --operator-type odh --rate-limiter kuadrant

  # Deploy via Kustomize
  ./scripts/deploy.sh --deployment-mode kustomize

  # Test MaaS API PR #123
  MAAS_API_IMAGE=quay.io/myuser/maas-api:pr-123 ./scripts/deploy.sh --operator-type odh

  # Test ODH operator PR #456 with manifests
  ./scripts/deploy.sh \\
    --operator-type odh \\
    --operator-catalog quay.io/opendatahub/opendatahub-operator-catalog:pr-456 \\
    --operator-image quay.io/opendatahub/opendatahub-operator:pr-456

  # Deploy without optional operators
  ./scripts/deploy.sh --skip-certmanager --skip-lws

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
      --deployment-mode)
        require_flag_value "$1" "${2:-}"
        DEPLOYMENT_MODE="$2"
        shift 2
        ;;
      --operator-type)
        require_flag_value "$1" "${2:-}"
        OPERATOR_TYPE="$2"
        shift 2
        ;;
      --rate-limiter)
        require_flag_value "$1" "${2:-}"
        RATE_LIMITER="$2"
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
      --skip-certmanager)
        SKIP_CERT_MANAGER="true"
        shift
        ;;
      --skip-lws)
        SKIP_LWS="true"
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
      --channel)
        require_flag_value "$1" "${2:-}"
        OPERATOR_CHANNEL="$2"
        shift 2
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
# CONFIGURATION VALIDATION
#──────────────────────────────────────────────────────────────

validate_configuration() {
  log_info "Validating configuration..."

  # Validate deployment mode
  if [[ ! "$DEPLOYMENT_MODE" =~ ^(operator|kustomize)$ ]]; then
    log_error "Invalid deployment mode: $DEPLOYMENT_MODE"
    log_error "Must be 'operator' or 'kustomize'"
    exit 1
  fi

  # Validate operator type
  if [[ "$DEPLOYMENT_MODE" == "operator" ]]; then
    if [[ ! "$OPERATOR_TYPE" =~ ^(rhoai|odh)$ ]]; then
      log_error "Invalid operator type: $OPERATOR_TYPE"
      log_error "Must be 'rhoai' or 'odh'"
      exit 1
    fi
  fi

  # Auto-determine rate limiter if not specified
  if [[ -z "$RATE_LIMITER" ]]; then
    if [[ "$DEPLOYMENT_MODE" == "operator" ]]; then
      # Operator mode: default to RHCL (production/downstream)
      RATE_LIMITER="rhcl"
      log_debug "Using auto-determined rate limiter for operator mode: $RATE_LIMITER"
    else
      # Kustomize mode: default to Kuadrant (development/upstream)
      RATE_LIMITER="kuadrant"
      log_debug "Using auto-determined rate limiter for kustomize mode: $RATE_LIMITER"
    fi
  fi

  # Validate rate limiter choice
  if [[ ! "$RATE_LIMITER" =~ ^(rhcl|kuadrant)$ ]]; then
    log_error "Invalid rate limiter: $RATE_LIMITER"
    log_error "Must be 'rhcl' or 'kuadrant'"
    exit 1
  fi

  # RHOAI requires RHCL (only applicable in operator mode)
  if [[ "$DEPLOYMENT_MODE" == "operator" && "$OPERATOR_TYPE" == "rhoai" && "$RATE_LIMITER" != "rhcl" ]]; then
    log_error "RHOAI requires RHCL (Red Hat Connectivity Link)"
    log_error "Cannot use upstream Kuadrant with RHOAI"
    log_error "Use --rate-limiter rhcl or deploy ODH instead"
    exit 1
  fi

  # Auto-determine namespace if not specified
  if [[ -z "$NAMESPACE" ]]; then
    case "$OPERATOR_TYPE" in
      rhoai)
        NAMESPACE="redhat-ods-applications"
        ;;
      odh)
        NAMESPACE="opendatahub"
        ;;
      *)
        NAMESPACE="opendatahub"
        ;;
    esac
    log_debug "Using auto-determined namespace: $NAMESPACE"
  fi

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
  validate_configuration

  log_info "Deployment configuration:"
  log_info "  Mode: $DEPLOYMENT_MODE"
  log_info "  Operator: $OPERATOR_TYPE"
  log_info "  Rate Limiter: $RATE_LIMITER"
  log_info "  Namespace: $NAMESPACE"
  log_info "  TLS Backend: $ENABLE_TLS_BACKEND"

  if [[ "$DRY_RUN" == "true" ]]; then
    log_info "DRY RUN MODE - no changes will be applied"
    log_info "Deployment plan validated. Exiting."
    exit 0
  fi

  case "$DEPLOYMENT_MODE" in
    operator)
      deploy_via_operator
      ;;
    kustomize)
      deploy_via_kustomize
      ;;
  esac

  log_info "==================================================="
  log_info "  Deployment completed successfully!"
  log_info "==================================================="
}

#──────────────────────────────────────────────────────────────
# OPERATOR-BASED DEPLOYMENT
#──────────────────────────────────────────────────────────────

deploy_via_operator() {
  log_info "Starting operator-based deployment..."

  # Install optional operators
  install_optional_operators

  # Install rate limiter component
  install_rate_limiter

  # Install primary operator
  install_primary_operator

  # Apply custom resources
  apply_custom_resources

  # Inject custom MaaS API image if specified
  inject_maas_api_image_operator_mode "$NAMESPACE"

  # Configure TLS backend (if enabled)
  if [[ "$ENABLE_TLS_BACKEND" == "true" ]]; then
    configure_tls_backend
  fi

  log_info "Operator deployment completed"
}

#──────────────────────────────────────────────────────────────
# KUSTOMIZE-BASED DEPLOYMENT
#──────────────────────────────────────────────────────────────

deploy_via_kustomize() {
  log_info "Starting kustomize-based deployment..."

  local project_root
  project_root="$(find_project_root)" || {
    log_error "Could not find project root"
    exit 1
  }

  # Install rate limiter component (RHCL or Kuadrant)
  install_rate_limiter

  # Set up MaaS API image if specified
  trap cleanup_maas_api_image EXIT INT TERM
  set_maas_api_image

  # Apply kustomize manifests
  log_info "Applying kustomize manifests..."

  local overlay="$project_root/deployment/overlays/openshift"
  if [[ "$ENABLE_TLS_BACKEND" == "true" ]]; then
    log_info "Using TLS backend overlay"
    overlay="$project_root/deployment/overlays/tls-backend"
  fi

  kubectl apply --server-side=true -f <(kustomize build "$overlay")

  # Configure TLS backend (if enabled)
  if [[ "$ENABLE_TLS_BACKEND" == "true" ]]; then
    configure_tls_backend
  fi

  log_info "Kustomize deployment completed"
}

#──────────────────────────────────────────────────────────────
# OPTIONAL OPERATORS (cert-manager, LWS)
#──────────────────────────────────────────────────────────────

install_optional_operators() {
  log_info "Checking optional operators..."

  # cert-manager
  if should_install_operator "openshift-cert-manager-operator" "$SKIP_CERT_MANAGER" "cert-manager-operator"; then
    log_info "Installing cert-manager..."
    install_olm_operator \
      "openshift-cert-manager-operator" \
      "cert-manager-operator" \
      "redhat-operators" \
      "stable-v1" \
      "" \
      "openshift-operators"
  fi

  # LeaderWorkerSet
  if should_install_operator "leader-worker-set" "$SKIP_LWS" "openshift-lws-operator"; then
    log_info "Installing LeaderWorkerSet..."
    install_olm_operator \
      "leader-worker-set" \
      "openshift-lws-operator" \
      "redhat-operators" \
      "stable-v1.0" \
      "" \
      "openshift-lws-operator"
  fi
}

#──────────────────────────────────────────────────────────────
# RATE LIMITER INSTALLATION
#──────────────────────────────────────────────────────────────

install_rate_limiter() {
  log_info "Installing rate limiter: $RATE_LIMITER"

  case "$RATE_LIMITER" in
    rhcl)
      log_info "Installing RHCL (Red Hat Connectivity Link - downstream)"
      install_olm_operator \
        "rhcl-operator" \
        "rh-connectivity-link" \
        "redhat-operators" \
        "stable" \
        "" \
        "AllNamespaces"

      # Apply RHCL/Kuadrant custom resource
      apply_kuadrant_cr "rh-connectivity-link"
      ;;

    kuadrant)
      log_info "Installing Kuadrant (upstream)"
      install_olm_operator \
        "kuadrant-operator" \
        "kuadrant-system" \
        "community-operators" \
        "stable" \
        "" \
        "AllNamespaces"

      # Apply Kuadrant custom resource
      apply_kuadrant_cr "kuadrant-system"
      ;;
  esac
}

#──────────────────────────────────────────────────────────────
# PRIMARY OPERATOR INSTALLATION
#──────────────────────────────────────────────────────────────

install_primary_operator() {
  log_info "Installing primary operator: $OPERATOR_TYPE"

  # Handle custom catalog if specified
  if [[ -n "$OPERATOR_CATALOG" ]]; then
    log_info "Using custom operator catalog: $OPERATOR_CATALOG"
    create_custom_catalogsource \
      "${OPERATOR_TYPE}-custom-catalog" \
      "openshift-marketplace" \
      "$OPERATOR_CATALOG"
  fi

  local catalog_source
  local channel

  case "$OPERATOR_TYPE" in
    rhoai)
      catalog_source="${OPERATOR_CATALOG:+${OPERATOR_TYPE}-custom-catalog}"
      catalog_source="${catalog_source:-redhat-operators}"
      channel="${OPERATOR_CHANNEL:-fast}"

      log_info "Installing RHOAI operator..."
      # RHOAI operator goes in redhat-ods-operator namespace (not redhat-ods-applications)
      local operator_namespace="redhat-ods-operator"
      install_olm_operator \
        "rhods-operator" \
        "$operator_namespace" \
        "$catalog_source" \
        "$channel" \
        "" \
        "AllNamespaces"

      # Patch CSV with custom operator image if specified
      if [[ -n "$OPERATOR_IMAGE" ]]; then
        patch_operator_csv "rhods-operator" "$operator_namespace" "$OPERATOR_IMAGE"
      fi
      ;;

    odh)
      catalog_source="${OPERATOR_CATALOG:+${OPERATOR_TYPE}-custom-catalog}"
      catalog_source="${catalog_source:-community-operators}"
      # Use 'fast' channel for custom catalogs, 'fast-3' for default
      if [[ -n "$OPERATOR_CATALOG" ]]; then
        channel="${OPERATOR_CHANNEL:-fast}"
      else
        channel="${OPERATOR_CHANNEL:-fast-3}"
      fi

      log_info "Installing ODH operator..."
      install_olm_operator \
        "opendatahub-operator" \
        "$NAMESPACE" \
        "$catalog_source" \
        "$channel" \
        "" \
        "AllNamespaces"

      # Patch CSV with custom operator image if specified
      if [[ -n "$OPERATOR_IMAGE" ]]; then
        patch_operator_csv "opendatahub-operator" "$NAMESPACE" "$OPERATOR_IMAGE"
      fi
      ;;
  esac
}

#──────────────────────────────────────────────────────────────
# CUSTOM RESOURCES
#──────────────────────────────────────────────────────────────

apply_custom_resources() {
  log_info "Applying custom resources..."

  # Apply DSCInitialization
  apply_dsci

  # Apply DataScienceCluster
  apply_dsc

  # Wait for DataScienceCluster to be ready
  log_info "Waiting for DataScienceCluster to be ready..."
  wait_datasciencecluster_ready "default-dsc" "$CUSTOM_RESOURCE_TIMEOUT"
}

apply_dsci() {
  log_info "Applying DSCInitialization..."

  cat <<EOF | kubectl apply -f -
apiVersion: dscinitialization.opendatahub.io/v1
kind: DSCInitialization
metadata:
  name: default-dsci
spec:
  applicationsNamespace: ${NAMESPACE}
  monitoring:
    managementState: Managed
    namespace: ${NAMESPACE}
    metrics: {}
  trustedCABundle:
    managementState: Managed
EOF
}

apply_dsc() {
  log_info "Applying DataScienceCluster..."

  # RHOAI 3.x uses v2 API (MaaS auto-enabled when kserve is Managed)
  # ODH still uses v1 API (needs explicit modelsAsService configuration)
  if [[ "$OPERATOR_TYPE" == "rhoai" ]]; then
    cat <<EOF | kubectl apply -f -
apiVersion: datasciencecluster.opendatahub.io/v2
kind: DataScienceCluster
metadata:
  name: default-dsc
spec:
  components:
    kserve:
      managementState: Managed
      rawDeploymentServiceConfig: Headed
    dashboard:
      managementState: Managed
EOF
  else
    # ODH uses v1 API with explicit modelsAsService
    cat <<EOF | kubectl apply -f -
apiVersion: datasciencecluster.opendatahub.io/v1
kind: DataScienceCluster
metadata:
  name: default-dsc
spec:
  components:
    kserve:
      managementState: Managed
      rawDeploymentServiceConfig: Headed
      modelsAsService:
        managementState: Managed
    dashboard:
      managementState: Managed
EOF
  fi
}

apply_kuadrant_cr() {
  local namespace=$1

  log_info "Initializing Gateway API provider..."

  # Create GatewayClass for OpenShift Gateway API controller
  # This enables the built-in Gateway API implementation (OpenShift 4.14+)
  cat <<EOF | kubectl apply -f -
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: openshift-default
spec:
  controllerName: "openshift.io/gateway-controller/v1"
EOF

  log_info "Applying Kuadrant custom resource in $namespace..."

  cat <<EOF | kubectl apply -f -
apiVersion: kuadrant.io/v1beta1
kind: Kuadrant
metadata:
  name: kuadrant
  namespace: $namespace
spec: {}
EOF

  # Wait for Kuadrant to be ready
  wait_for_custom_check "Kuadrant ready in $namespace" \
    "kubectl get kuadrant kuadrant -n $namespace -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}' 2>/dev/null | grep -q True" \
    120 \
    10 || log_warn "Kuadrant not ready yet - may need additional time for Gateway API provider initialization"
}

patch_operator_csv() {
  local operator_prefix=$1
  local namespace=$2
  local operator_image=$3

  log_info "Patching operator CSV with custom image: $operator_image"

  # Wait a bit for CSV to be created
  sleep 10

  local csv_name
  csv_name=$(kubectl get csv -n "$namespace" --no-headers 2>/dev/null | grep "^${operator_prefix}" | head -n1 | awk '{print $1}')

  if [[ -z "$csv_name" ]]; then
    log_warn "Could not find CSV for $operator_prefix, skipping image patch"
    return 0
  fi

  # Add managed: false annotation to prevent operator reconciliation from reverting the patch
  log_info "Adding managed: false annotation to CSV $csv_name"
  kubectl annotate csv "$csv_name" -n "$namespace" opendatahub.io/managed=false --overwrite

  kubectl patch csv "$csv_name" -n "$namespace" --type='json' -p="[
    {\"op\": \"replace\", \"path\": \"/spec/install/spec/deployments/0/spec/template/spec/containers/0/image\", \"value\": \"$operator_image\"}
  ]"

  log_info "CSV $csv_name patched with image $operator_image"
}

#──────────────────────────────────────────────────────────────
# TLS BACKEND CONFIGURATION
#──────────────────────────────────────────────────────────────

configure_tls_backend() {
  log_info "Configuring TLS backend for Authorino and MaaS API..."

  local project_root
  project_root="$(find_project_root)" || {
    log_warn "Could not find project root, skipping TLS backend configuration"
    return 0
  }

  # Determine Authorino namespace based on rate limiter
  local authorino_namespace
  case "$RATE_LIMITER" in
    rhcl)
      authorino_namespace="rh-connectivity-link"
      ;;
    kuadrant)
      authorino_namespace="kuadrant-system"
      ;;
    *)
      log_warn "Unknown rate limiter: $RATE_LIMITER, defaulting to kuadrant-system"
      authorino_namespace="kuadrant-system"
      ;;
  esac

  # Call TLS configuration script
  local tls_script="${project_root}/deployment/overlays/tls-backend/configure-authorino-tls.sh"
  if [[ ! -f "$tls_script" ]]; then
    log_warn "TLS configuration script not found at $tls_script, skipping"
    return 0
  fi

  log_info "Running TLS configuration script..."
  if AUTHORINO_NAMESPACE="$authorino_namespace" "$tls_script" 2>&1 | while read -r line; do log_debug "$line"; done; then
    log_info "TLS configuration script completed successfully"
  else
    log_warn "TLS configuration script had issues (non-fatal, continuing)"
  fi

  # Restart deployments to pick up TLS config
  log_info "Restarting deployments to pick up TLS configuration..."

  # Determine maas-api namespace based on deployment mode
  local maas_namespace="${NAMESPACE:-maas-api}"
  kubectl rollout restart deployment/maas-api -n "$maas_namespace" 2>/dev/null || log_debug "maas-api deployment not found or not yet ready"
  kubectl rollout restart deployment/authorino -n "$authorino_namespace" 2>/dev/null || log_debug "authorino deployment not found or not yet ready"

  log_info "TLS backend configuration complete"
  log_info "Tier lookup URL: https://maas-api.${maas_namespace}.svc.cluster.local:8443/v1/tiers/lookup"
}

#──────────────────────────────────────────────────────────────
# MAIN ENTRY POINT
#──────────────────────────────────────────────────────────────

main "$@"
