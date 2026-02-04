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
#   --operator-type <rhoai|odh>   Operator to install (default: rhoai)
#   --policy-engine <rhcl|kuadrant> Gateway policy engine (auto-determined)
#   --enable-dashboard            Enable dashboard (disabled by default)
#   --enable-tls-backend          Enable TLS for Authorino/MaaS API (default: on)
#   --skip-certmanager            Skip cert-manager installation
#   --skip-lws                    Skip LeaderWorkerSet installation
#   --namespace <namespace>       Target namespace
#   --verbose                     Enable debug logging
#   --dry-run                     Show what would be done
#   --help                        Show full help with all options
#
# ADVANCED OPTIONS (PR Testing):
#   --operator-catalog <image>    Custom operator catalog image
#   --operator-image <image>      Custom operator image (patches CSV)
#   --channel <channel>           Operator channel override
#
# ENVIRONMENT VARIABLES:
#   MAAS_API_IMAGE    Custom MaaS API container image
#   OPERATOR_TYPE     Operator type (rhoai/odh)
#   LOG_LEVEL         Logging verbosity (DEBUG, INFO, WARN, ERROR)
#
# EXAMPLES:
#   # Deploy RHOAI (default)
#   ./scripts/deploy.sh
#
#   # Deploy ODH
#   ./scripts/deploy.sh --operator-type odh
#
#   # Deploy ODH with dashboard enabled
#   ./scripts/deploy.sh --operator-type odh --enable-dashboard
#
#   # Test custom MaaS API image
#   MAAS_API_IMAGE=quay.io/myuser/maas-api:pr-123 ./scripts/deploy.sh
#
# For detailed documentation, see:
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
POLICY_ENGINE="${POLICY_ENGINE:-}"  # Auto-determined based on deployment mode (rhcl/kuadrant)
NAMESPACE="${NAMESPACE:-}"  # Auto-determined based on operator type
SKIP_CERT_MANAGER="${SKIP_CERT_MANAGER:-auto}"
SKIP_LWS="${SKIP_LWS:-auto}"
ENABLE_DASHBOARD="${ENABLE_DASHBOARD:-false}"
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

  --policy-engine <rhcl|kuadrant>
      Gateway policy engine for AuthPolicy/RateLimitPolicy (auto-determined)
      - rhcl: Red Hat Connectivity Link (downstream)
        Default for: operator mode (RHOAI and ODH)
        Note: Works with OpenShift Gateway (shows warning without Istio)
      - kuadrant: Kuadrant operator (upstream)
        Default for: kustomize mode
        Note: May have API compatibility issues with some operator versions

  --enable-tls-backend
      Enable TLS backend for Authorino and MaaS API (default: enabled)
      Configures HTTPS tier lookup URL

  --skip-certmanager
      Skip cert-manager installation (auto-detected by default)

  --skip-lws
      Skip LeaderWorkerSet installation (auto-detected by default)

  --enable-dashboard
      Enable dashboard component installation (disabled by default)
      Dashboard includes BFF services that increase resource usage
      Enable when you need the dashboard UI

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
  POLICY_ENGINE         Gateway policy engine (rhcl/kuadrant)
  LOG_LEVEL             Logging verbosity (DEBUG, INFO, WARN, ERROR)

EXAMPLES:
  # Deploy RHOAI (default)
  ./scripts/deploy.sh

  # Deploy ODH
  ./scripts/deploy.sh --operator-type odh

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
      --policy-engine|--rate-limiter)
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
      --skip-certmanager)
        SKIP_CERT_MANAGER="true"
        shift
        ;;
      --skip-lws)
        SKIP_LWS="true"
        shift
        ;;
      --enable-dashboard)
        ENABLE_DASHBOARD="true"
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
  if [[ -z "$POLICY_ENGINE" ]]; then
    if [[ "$DEPLOYMENT_MODE" == "operator" ]]; then
      # Operator mode: default to RHCL (production/downstream)
      POLICY_ENGINE="rhcl"
      log_debug "Using auto-determined policy engine for operator mode: $POLICY_ENGINE"
    else
      # Kustomize mode: default to Kuadrant (development/upstream)
      POLICY_ENGINE="kuadrant"
      log_debug "Using auto-determined policy engine for kustomize mode: $POLICY_ENGINE"
    fi
  fi

  # Validate rate limiter choice
  if [[ ! "$POLICY_ENGINE" =~ ^(rhcl|kuadrant)$ ]]; then
    log_error "Invalid policy engine: $POLICY_ENGINE"
    log_error "Must be 'rhcl' or 'kuadrant'"
    exit 1
  fi

  # RHOAI requires RHCL (only applicable in operator mode)
  if [[ "$DEPLOYMENT_MODE" == "operator" && "$OPERATOR_TYPE" == "rhoai" && "$POLICY_ENGINE" != "rhcl" ]]; then
    log_error "RHOAI requires RHCL (Red Hat Connectivity Link)"
    log_error "Cannot use upstream Kuadrant with RHOAI"
    log_error "Use --policy-engine rhcl or deploy ODH instead"
    exit 1
  fi

  # Auto-determine namespace if not specified
  if [[ -z "$NAMESPACE" ]]; then
    if [[ "$DEPLOYMENT_MODE" == "kustomize" ]]; then
      # Kustomize mode: use maas-api namespace (matching kustomize overlay default)
      NAMESPACE="maas-api"
      log_debug "Using auto-determined namespace for kustomize mode: $NAMESPACE"
    else
      # Operator mode: namespace depends on operator type
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
      log_debug "Using auto-determined namespace for operator mode: $NAMESPACE"
    fi
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
  if [[ "$DEPLOYMENT_MODE" == "operator" ]]; then
    log_info "  Operator: $OPERATOR_TYPE"
  fi
  log_info "  Policy Engine: $POLICY_ENGINE"
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
  install_policy_engine

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
  install_policy_engine

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

# Patch Kuadrant/RHCL CSV to recognize OpenShift Gateway controller
# This is required because Kuadrant needs to know about the Gateway API provider
# Without this patch, Kuadrant shows "MissingDependency" and AuthPolicies won't be enforced
patch_kuadrant_csv_for_gateway() {
  local namespace=$1
  local operator_prefix=$2

  log_info "Patching $operator_prefix CSV for OpenShift Gateway controller..."

  # Find the CSV
  local csv_name
  csv_name=$(kubectl get csv -n "$namespace" --no-headers 2>/dev/null | grep "^${operator_prefix}" | awk '{print $1}' | head -1)

  if [[ -z "$csv_name" ]]; then
    log_warn "Could not find CSV for $operator_prefix in $namespace, skipping Gateway controller patch"
    return 0
  fi

  # Check if ISTIO_GATEWAY_CONTROLLER_NAMES already has both values
  local current_value
  current_value=$(kubectl get csv "$csv_name" -n "$namespace" -o jsonpath='{.spec.install.spec.deployments[0].spec.template.spec.containers[0].env[?(@.name=="ISTIO_GATEWAY_CONTROLLER_NAMES")].value}' 2>/dev/null || echo "")

  if [[ "$current_value" == *"istio.io/gateway-controller"* && "$current_value" == *"openshift.io/gateway-controller"* ]]; then
    log_debug "CSV already has correct ISTIO_GATEWAY_CONTROLLER_NAMES value"
    return 0
  fi

  # Find the index of ISTIO_GATEWAY_CONTROLLER_NAMES env var
  local env_index
  env_index=$(kubectl get csv "$csv_name" -n "$namespace" -o json | jq '.spec.install.spec.deployments[0].spec.template.spec.containers[0].env | to_entries | .[] | select(.value.name=="ISTIO_GATEWAY_CONTROLLER_NAMES") | .key' 2>/dev/null || echo "")

  if [[ -z "$env_index" ]]; then
    # Env var doesn't exist, add it
    log_debug "Adding ISTIO_GATEWAY_CONTROLLER_NAMES to CSV"
    kubectl patch csv "$csv_name" -n "$namespace" --type='json' -p='[
      {
        "op": "add",
        "path": "/spec/install/spec/deployments/0/spec/template/spec/containers/0/env/-",
        "value": {
          "name": "ISTIO_GATEWAY_CONTROLLER_NAMES",
          "value": "istio.io/gateway-controller,openshift.io/gateway-controller/v1"
        }
      }
    ]' 2>/dev/null || log_warn "Failed to add ISTIO_GATEWAY_CONTROLLER_NAMES to CSV"
  else
    # Env var exists, update it
    log_debug "Updating ISTIO_GATEWAY_CONTROLLER_NAMES in CSV (index: $env_index)"
    kubectl patch csv "$csv_name" -n "$namespace" --type='json' -p="[
      {
        \"op\": \"replace\",
        \"path\": \"/spec/install/spec/deployments/0/spec/template/spec/containers/0/env/${env_index}/value\",
        \"value\": \"istio.io/gateway-controller,openshift.io/gateway-controller/v1\"
      }
    ]" 2>/dev/null || log_warn "Failed to update ISTIO_GATEWAY_CONTROLLER_NAMES in CSV"
  fi

  log_info "CSV patched for OpenShift Gateway controller"

  # CRITICAL: Force delete the operator pod to pick up the new env var
  # OLM updates the deployment spec but doesn't always trigger a pod restart
  # The operator must have ISTIO_GATEWAY_CONTROLLER_NAMES set BEFORE Kuadrant CR is created
  log_info "Forcing operator restart to apply new Gateway controller configuration..."
  
  # The kuadrant operator deployment is always named kuadrant-operator-controller-manager
  # regardless of whether we're using rhcl-operator or kuadrant-operator
  local operator_deployment="kuadrant-operator-controller-manager"
  if kubectl get deployment "$operator_deployment" -n "$namespace" &>/dev/null; then
    # Force delete the operator pod - this ensures the new env var is picked up
    kubectl delete pod -n "$namespace" -l control-plane=controller-manager --force --grace-period=0 2>/dev/null || \
      kubectl delete pod -n "$namespace" -l app.kubernetes.io/name=kuadrant-operator --force --grace-period=0 2>/dev/null || \
      kubectl delete pod -n "$namespace" -l app=kuadrant --force --grace-period=0 2>/dev/null || true
    
    # Wait for the new pod to be ready
    log_info "Waiting for operator pod to restart..."
    sleep 5
    kubectl rollout status deployment/"$operator_deployment" -n "$namespace" --timeout=120s 2>/dev/null || \
      log_warn "Operator rollout status check timed out"
    
    # Verify the env var is in the RUNNING pod
    local pod_env
    pod_env=$(kubectl exec -n "$namespace" deployment/"$operator_deployment" -- env 2>/dev/null | grep ISTIO_GATEWAY_CONTROLLER_NAMES || echo "")
    
    if [[ "$pod_env" == *"openshift.io/gateway-controller/v1"* ]]; then
      log_info "Operator pod is running with OpenShift Gateway controller configuration"
    else
      log_warn "Operator pod may not have correct env yet: $pod_env"
    fi
    
    # Give the operator time to fully initialize with the new Gateway controller configuration
    # This is critical - the operator needs to register as a Gateway controller before Kuadrant CR is created
    log_info "Waiting 30s for operator to fully initialize with Gateway controller configuration..."
    sleep 30
  else
    log_warn "Could not find operator deployment, waiting 60s for env propagation"
    sleep 60
  fi
}

install_policy_engine() {
  log_info "Installing policy engine: $POLICY_ENGINE"

  case "$POLICY_ENGINE" in
    rhcl)
      log_info "Installing RHCL (Red Hat Connectivity Link - downstream)"
      install_olm_operator \
        "rhcl-operator" \
        "rh-connectivity-link" \
        "redhat-operators" \
        "stable" \
        "" \
        "AllNamespaces"

      # Patch RHCL CSV to recognize OpenShift Gateway controller
      patch_kuadrant_csv_for_gateway "rh-connectivity-link" "rhcl-operator"

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

      # Patch Kuadrant CSV to recognize OpenShift Gateway controller
      patch_kuadrant_csv_for_gateway "kuadrant-system" "kuadrant-operator"

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

  # Wait for webhook deployment to be ready before applying CRs
  # This prevents "service not found" errors during conversion webhook calls
  log_info "Waiting for operator webhook to be ready..."

  local webhook_namespace
  if [[ "$OPERATOR_TYPE" == "rhoai" ]]; then
    webhook_namespace="redhat-ods-operator"
  else
    webhook_namespace="opendatahub-operator-system"
  fi

  local webhook_deployment
  if [[ "$OPERATOR_TYPE" == "rhoai" ]]; then
    webhook_deployment="rhods-operator-controller-manager"
  else
    webhook_deployment="opendatahub-operator-controller-manager"
  fi

  # Wait for webhook deployment to exist and be ready (ensures service + endpoints are ready)
  wait_for_resource "deployment" "$webhook_deployment" "$webhook_namespace" 120 || {
    log_warn "Webhook deployment not found after 120s, proceeding anyway..."
  }

  # Wait for deployment to be fully ready (replicas available)
  if kubectl get deployment "$webhook_deployment" -n "$webhook_namespace" >/dev/null 2>&1; then
    kubectl wait --for=condition=Available --timeout=120s \
      deployment/"$webhook_deployment" -n "$webhook_namespace" 2>/dev/null || {
      log_warn "Webhook deployment not fully ready, proceeding anyway..."
    }
  fi

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

  # Determine dashboard state based on flag
  local dashboard_state="Removed"
  if [[ "$ENABLE_DASHBOARD" == "true" ]]; then
    dashboard_state="Managed"
    log_info "Dashboard component will be enabled"
  else
    log_debug "Dashboard component disabled (use --enable-dashboard to enable)"
  fi

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
      managementState: ${dashboard_state}
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
      managementState: ${dashboard_state}
EOF
  fi
}

#──────────────────────────────────────────────────────────────
# GATEWAY API SETUP
#──────────────────────────────────────────────────────────────

# setup_gateway_api
#   Sets up the Gateway API infrastructure (GatewayClass).
#   This is general Gateway API setup that can be used by any Gateway resources.
setup_gateway_api() {
  log_info "Setting up Gateway API infrastructure..."

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
}

# setup_maas_gateway
#   Creates the Gateway resource required by ModelsAsService component.
#   ModelsAsService expects a gateway named "maas-default-gateway" in namespace "openshift-ingress".
#
#   This function:
#   1. Creates a self-signed TLS certificate for the Gateway
#   2. Creates the Gateway resource with HTTPS listener
#   3. Creates an OpenShift Route to expose the Gateway via the cluster's apps domain
setup_maas_gateway() {
  log_info "Setting up ModelsAsService gateway..."

  # Get cluster domain for Gateway hostname
  local cluster_domain
  cluster_domain=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null || echo "")
  if [[ -z "$cluster_domain" ]]; then
    log_warn "Could not determine cluster domain, Gateway hostname will not be set"
  fi
  local gateway_hostname="maas.${cluster_domain}"

  # Create a self-signed certificate for the Gateway
  # In production, this would be replaced with a proper certificate
  log_info "Creating TLS certificate for MaaS gateway..."
  if ! create_tls_secret "maas-gateway-tls" "openshift-ingress" "${gateway_hostname:-maas-gateway}"; then
    log_error "Failed to create TLS secret for gateway"
    return 1
  fi

  # Create the Gateway resource required by ModelsAsService
  # Allow routes from the deployment namespace (where HTTPRoute will be created)
  log_info "Creating maas-default-gateway resource (allowing routes from all namespaces)..."
  
  # Build hostname config if cluster domain is available
  local hostname_config=""
  if [[ -n "$cluster_domain" ]]; then
    hostname_config="hostname: ${gateway_hostname}"
  fi

  cat <<EOF | kubectl apply -f -
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: maas-default-gateway
  namespace: openshift-ingress
spec:
  gatewayClassName: openshift-default
  listeners:
  - name: https
    protocol: HTTPS
    port: 443
    ${hostname_config}
    tls:
      mode: Terminate
      certificateRefs:
      - kind: Secret
        name: maas-gateway-tls
    allowedRoutes:
      namespaces:
        from: All
EOF

  # Create OpenShift Route to expose the Gateway via the cluster's apps domain
  # This is REQUIRED because:
  # 1. *.apps.cluster-domain DNS points to OpenShift Router by default
  # 2. Without a Route, the Router doesn't know about maas.apps.cluster-domain
  # 3. The Gateway's LoadBalancer has a different external address (ELB hostname)
  # 4. The Route bridges: maas.apps.cluster-domain -> Router -> Gateway Service
  #
  # Note: The older scripts (deploy-rhoai-stable.sh, deploy-openshift.sh) didn't create
  # a Route explicitly, but they may have relied on different access patterns or
  # operator-managed routing. For this consolidated script, we create the Route
  # to ensure maas.apps.cluster-domain works reliably.
  if [[ -n "$cluster_domain" ]]; then
    log_info "Creating OpenShift Route for Gateway (hostname: ${gateway_hostname})..."
    create_gateway_route \
      "maas-gateway-route" \
      "openshift-ingress" \
      "${gateway_hostname}" \
      "maas-default-gateway-openshift-default" \
      "maas-gateway-tls"
  fi
}

#──────────────────────────────────────────────────────────────
# KUADRANT SETUP
#──────────────────────────────────────────────────────────────

apply_kuadrant_cr() {
  local namespace=$1

  log_info "Initializing Gateway API and ModelsAsService gateway..."

  # Setup Gateway API infrastructure (can be used by any Gateway resources)
  setup_gateway_api

  # Setup ModelsAsService-specific gateway (required by ModelsAsService component)
  setup_maas_gateway

  # Wait for Gateway to be Programmed (required before Kuadrant can become ready)
  # This ensures Service Mesh is installed and Gateway API provider is operational
  log_info "Waiting for Gateway to be Programmed (Service Mesh initialization)..."
  if ! kubectl wait --for=condition=Programmed gateway/maas-default-gateway -n openshift-ingress --timeout=300s 2>/dev/null; then
    log_warn "Gateway not yet Programmed after 300s - Kuadrant may take longer to become ready"
  fi

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
  # With the CSV patch, operator restart, and 30s initialization delay applied before this,
  # the operator should recognize the OpenShift Gateway controller.
  # Kuadrant becoming Ready confirms the Gateway controller is properly recognized.
  # Using 300s timeout because CI environments can be slower than local testing.
  wait_for_custom_check "Kuadrant ready in $namespace" \
    "kubectl get kuadrant kuadrant -n $namespace -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}' 2>/dev/null | grep -q True" \
    300 \
    5 || log_warn "Kuadrant not ready yet - AuthPolicy enforcement may fail on model HTTPRoutes"
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
  case "$POLICY_ENGINE" in
    rhcl)
      authorino_namespace="rh-connectivity-link"
      ;;
    kuadrant)
      authorino_namespace="kuadrant-system"
      ;;
    *)
      log_warn "Unknown policy engine: $POLICY_ENGINE, defaulting to kuadrant-system"
      authorino_namespace="kuadrant-system"
      ;;
  esac

  # Wait for Authorino deployment to be created by Kuadrant operator
  # This is necessary because Kuadrant may not be fully ready yet (timing issue)
  wait_for_resource "deployment" "authorino" "$authorino_namespace" 180 || {
    log_warn "Authorino deployment not found, TLS configuration may fail"
  }

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
  
  # Wait for Authorino to be ready after restart
  log_info "Waiting for Authorino deployment to be ready..."
  kubectl rollout status deployment/authorino -n "$authorino_namespace" --timeout=120s 2>/dev/null || log_warn "Authorino rollout status check timed out"

  log_info "TLS backend configuration complete"
  log_info "Tier lookup URL: https://maas-api.${maas_namespace}.svc.cluster.local:8443/v1/tiers/lookup"
}

#──────────────────────────────────────────────────────────────
# MAIN ENTRY POINT
#──────────────────────────────────────────────────────────────

main "$@"
