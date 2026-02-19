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
#                                   odh → kuadrant (community v1.3.1)
#                                   rhoai → rhcl (Red Hat Connectivity Link)
#   --enable-tls-backend          Enable TLS for Authorino/MaaS API (default: on)
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
#   # Deploy ODH (default, uses kuadrant policy engine)
#   ./scripts/deploy.sh
#
#   # Deploy RHOAI (uses rhcl policy engine)
#   ./scripts/deploy.sh --operator-type rhoai
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
OPERATOR_TYPE="${OPERATOR_TYPE:-odh}"
POLICY_ENGINE=""  # Auto-determined: odh→kuadrant, rhoai→rhcl
NAMESPACE="${NAMESPACE:-}"  # Auto-determined based on operator type
ENABLE_TLS_BACKEND="${ENABLE_TLS_BACKEND:-true}"
ENABLE_SUBSCRIPTIONS="${ENABLE_SUBSCRIPTIONS:-false}"
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

  --operator-type <odh|rhoai>
      Which operator to install (default: odh)
      Policy engine is auto-selected based on operator type:
      - rhoai → rhcl (Red Hat Connectivity Link)
      - odh → kuadrant (community v1.3.1 with AuthPolicy v1)
      Only applies when --deployment-mode=operator

  --enable-tls-backend
      Enable TLS backend for Authorino and MaaS API (default: enabled)
      Configures HTTPS tier lookup URL

  --enable-subscriptions
      Install the MaaS subscription controller (per-model auth + rate limiting)
      Runs alongside existing tier-based flow. Does not replace it.
  --disable-tls-backend
      Disable TLS backend for Authorino and MaaS API
      Uses HTTP tier lookup URL instead

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
      Default: fast-3 (ODH), fast-3.x (RHOAI)

ENVIRONMENT VARIABLES:
  MAAS_API_IMAGE        Custom MaaS API container image
  OPERATOR_CATALOG      Custom operator catalog
  OPERATOR_IMAGE        Custom operator image
  OPERATOR_TYPE         Operator type (rhoai/odh)
  LOG_LEVEL             Logging verbosity (DEBUG, INFO, WARN, ERROR)

EXAMPLES:
  # Deploy ODH (default, uses kuadrant policy engine)
  ./scripts/deploy.sh

  # Deploy RHOAI (uses rhcl policy engine)
  ./scripts/deploy.sh --operator-type rhoai

  # Deploy via Kustomize
  ./scripts/deploy.sh --deployment-mode kustomize

  # Test MaaS API PR #123
  MAAS_API_IMAGE=quay.io/myuser/maas-api:pr-123 ./scripts/deploy.sh --operator-type odh

  # Test ODH operator PR #456 with manifests
  ./scripts/deploy.sh \\
    --operator-type odh \\
    --operator-catalog quay.io/opendatahub/opendatahub-operator-catalog:pr-456 \\
    --operator-image quay.io/opendatahub/opendatahub-operator:pr-456

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
      --enable-tls-backend)
        ENABLE_TLS_BACKEND="true"
        shift
        ;;
      --disable-tls-backend)
        ENABLE_TLS_BACKEND="false"
        shift
        ;;
      --enable-subscriptions)
        ENABLE_SUBSCRIPTIONS="true"
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

  # Auto-determine policy engine based on operator type
  # - ODH uses community Kuadrant (v1.3.1 from upstream catalog has AuthPolicy v1)
  # - RHOAI uses RHCL (Red Hat Connectivity Link - downstream)
  if [[ "$DEPLOYMENT_MODE" == "operator" ]]; then
    case "$OPERATOR_TYPE" in
      odh)
        POLICY_ENGINE="kuadrant"
        log_debug "Auto-selected policy engine for ODH: kuadrant (community v1.3.1)"
        ;;
      rhoai)
        POLICY_ENGINE="rhcl"
        log_debug "Auto-selected policy engine for RHOAI: rhcl (Red Hat Connectivity Link)"
        ;;
    esac
  else
    # Kustomize mode: default to kuadrant (community)
    POLICY_ENGINE="kuadrant"
    log_debug "Using auto-determined policy engine for kustomize mode: $POLICY_ENGINE"
  fi

  # Determine namespace based on deployment mode
  if [[ "$DEPLOYMENT_MODE" == "kustomize" ]]; then
    # Kustomize mode: use provided namespace or default to maas-api
    if [[ -z "$NAMESPACE" ]]; then
      NAMESPACE="maas-api"
    fi
    log_debug "Using namespace for kustomize mode: $NAMESPACE"
  else
    # Operator mode: ALWAYS use fixed namespace based on operator type
    # This matches upstream deploy-rhoai-stable.sh behavior where the
    # applications namespace is determined by DSCInitialization, not env vars.
    # The $NAMESPACE env var (e.g., from Prow CI) is intentionally ignored.
    case "$OPERATOR_TYPE" in
      rhoai)
        NAMESPACE="redhat-ods-applications"
        ;;
      odh|*)
        NAMESPACE="opendatahub"
        ;;
    esac
    log_debug "Using fixed namespace for operator mode: $NAMESPACE"
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
  if [[ "$ENABLE_SUBSCRIPTIONS" == "true" ]]; then
    log_info "  Subscriptions: ENABLED (per-model auth + rate limiting)"
  fi

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

  # Install subscription controller if enabled (additive, doesn't replace tier-based flow)
  if [[ "$ENABLE_SUBSCRIPTIONS" == "true" ]]; then
    log_info ""
    log_info "Installing MaaS Subscription Controller..."
    local script_dir
    script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    local controller_dir="$script_dir/../maas-controller"

    if [[ ! -d "$controller_dir" ]]; then
      log_warn "maas-controller directory not found at $controller_dir — skipping subscription controller"
    else
      log_info "  Installing controller (CRDs, RBAC, deployment, default-deny policy)..."
      "$controller_dir/scripts/install-maas-controller.sh" "$NAMESPACE"

      log_info "  Waiting for maas-controller to be ready..."
      if ! kubectl rollout status deployment/maas-controller -n "$NAMESPACE" --timeout=120s; then
        log_error "maas-controller deployment not ready — skipping gateway-auth-policy disable to avoid inconsistent state"
        return 1
      fi

      log_info "  Disabling old gateway-auth-policy (replaced by per-route policies)..."
      "$controller_dir/hack/disable-gateway-auth-policy.sh"

      log_info "  Subscription controller installed."
      log_info "  Create MaaSModel, MaaSAuthPolicy, and MaaSSubscription to enable per-model auth and rate limiting."
    fi
  fi

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

  # Configure audience for non-standard clusters (Hypershift/ROSA)
  configure_cluster_audience

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

  # Create namespace if it doesn't exist (kustomize mode uses maas-api namespace)
  # This must be done before applying manifests that target this namespace
  if ! kubectl get namespace "$NAMESPACE" &>/dev/null; then
    log_info "Creating namespace: $NAMESPACE"
    kubectl create namespace "$NAMESPACE"
  fi

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
  log_info "Installing optional operators in parallel..."

  local data_dir="${SCRIPT_DIR}/data"

  # Apply both subscriptions in parallel (they're independent)
  log_info "Applying cert-manager and LeaderWorkerSet subscriptions..."
  kubectl apply -f "${data_dir}/cert-manager-subscription.yaml" &
  local cert_manager_pid=$!
  kubectl apply -f "${data_dir}/lws-subscription.yaml" &
  local lws_pid=$!

  # Wait for both apply commands to complete and capture individual exit codes
  local cert_manager_apply_rc=0
  local lws_apply_rc=0
  wait $cert_manager_pid || cert_manager_apply_rc=$?
  wait $lws_pid || lws_apply_rc=$?

  if [[ $cert_manager_apply_rc -ne 0 ]]; then
    log_error "Failed to apply cert-manager subscription (exit code: $cert_manager_apply_rc)"
    return 1
  fi
  if [[ $lws_apply_rc -ne 0 ]]; then
    log_error "Failed to apply LWS subscription (exit code: $lws_apply_rc)"
    return 1
  fi

  # Wait for both subscriptions to be installed (can run in parallel too)
  log_info "Waiting for operators to be installed..."
  waitsubscriptioninstalled "cert-manager-operator" "openshift-cert-manager-operator" &
  local cert_wait_pid=$!
  waitsubscriptioninstalled "openshift-lws-operator" "leader-worker-set" &
  local lws_wait_pid=$!

  # Wait for both to complete and capture individual exit codes
  local cert_wait_rc=0
  local lws_wait_rc=0
  wait $cert_wait_pid || cert_wait_rc=$?
  wait $lws_wait_pid || lws_wait_rc=$?

  if [[ $cert_wait_rc -ne 0 ]]; then
    log_error "cert-manager operator installation failed"
    return 1
  fi
  if [[ $lws_wait_rc -ne 0 ]]; then
    log_error "LWS operator installation failed"
    return 1
  fi

  # Create LeaderWorkerSetOperator CR to activate the LWS controller-manager.
  # The operator subscription alone only installs the operator pod; the CR is
  # required to actually deploy the LWS API (controller-manager pods).
  # See: https://docs.redhat.com/en/documentation/openshift_container_platform/latest/html/ai_workloads/leader-worker-set-operator
  log_info "Activating LeaderWorkerSet API..."
  kubectl apply -f "${data_dir}/lws-operator-cr.yaml"

  log_info "Optional operators installed"
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
    log_info "Waiting 15s for operator to fully initialize with Gateway controller configuration..."
    sleep 15
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
      log_info "Installing Kuadrant v1.3.1 (upstream community)"

      # Create custom catalog for upstream Kuadrant v1.3.1
      # This version provides AuthPolicy v1 API required by ODH
      local kuadrant_catalog="kuadrant-operator-catalog"
      local kuadrant_ns="kuadrant-system"

      log_info "Creating Kuadrant v1.3.1 catalog source..."
      kubectl create namespace "$kuadrant_ns" 2>/dev/null || true

      cat <<EOF | kubectl apply -f -
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: $kuadrant_catalog
  namespace: $kuadrant_ns
spec:
  sourceType: grpc
  image: quay.io/kuadrant/kuadrant-operator-catalog:v1.3.1
  displayName: Kuadrant Operator Catalog
  publisher: Kuadrant
  updateStrategy:
    registryPoll:
      interval: 45m
EOF

      # Wait for catalog to be ready
      log_info "Waiting for Kuadrant catalog to be ready..."
      sleep 10

      # Create OperatorGroup for Kuadrant
      cat <<EOF | kubectl apply -f -
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: kuadrant-operator-group
  namespace: $kuadrant_ns
spec: {}
EOF

      # Install Kuadrant operator from the custom catalog
      # IMPORTANT: source_namespace must match where CatalogSource was created (kuadrant_ns)
      install_olm_operator \
        "kuadrant-operator" \
        "$kuadrant_ns" \
        "$kuadrant_catalog" \
        "stable" \
        "" \
        "AllNamespaces" \
        "$kuadrant_ns"  # source_namespace - must match CatalogSource namespace

      # Patch Kuadrant CSV to recognize OpenShift Gateway controller
      patch_kuadrant_csv_for_gateway "$kuadrant_ns" "kuadrant-operator"

      # Apply Kuadrant custom resource
      apply_kuadrant_cr "$kuadrant_ns"
      ;;
  esac
}

#──────────────────────────────────────────────────────────────
# PRIMARY OPERATOR INSTALLATION
#──────────────────────────────────────────────────────────────

install_primary_operator() {
  log_info "Installing primary operator: $OPERATOR_TYPE"

  local catalog_source
  local channel

  case "$OPERATOR_TYPE" in
    rhoai)
      # Support custom catalog for RHOAI snapshot/development builds
      # This allows testing with pre-release RHOAI versions that have modelsAsService support
      if [[ -n "$OPERATOR_CATALOG" ]]; then
        log_info "Using custom RHOAI catalog: $OPERATOR_CATALOG"
        create_custom_catalogsource "rhoai-custom-catalog" "openshift-marketplace" "$OPERATOR_CATALOG"
        catalog_source="rhoai-custom-catalog"
        # Custom catalogs typically use 'fast' channel
        channel="${OPERATOR_CHANNEL:-fast}"
      else
        catalog_source="redhat-operators"
        # Use 'fast-3.x' channel for RHOAI v3 (with MaaS support)
        # RHOAI 2.x (fast channel) does not support modelsAsService
        channel="${OPERATOR_CHANNEL:-fast-3.x}"
      fi

      log_info "Installing RHOAI v3 operator..."
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
      # Support custom catalog for ODH snapshot/development builds
      # This allows testing with pre-release ODH versions (e.g., v3.3.0-snapshot)
      if [[ -n "$OPERATOR_CATALOG" ]]; then
        log_info "Using custom ODH catalog: $OPERATOR_CATALOG"
        create_custom_catalogsource "odh-custom-catalog" "openshift-marketplace" "$OPERATOR_CATALOG"
        catalog_source="odh-custom-catalog"
        # Custom catalogs typically use 'fast' channel
        channel="${OPERATOR_CHANNEL:-fast}"
      else
        catalog_source="community-operators"
        # Use 'fast-3' channel for released versions
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

  # Wait for CRDs to be established - this is critical!
  # The operator creates CRDs when its CSV becomes active, but there can be a delay.
  # Both CRDs are installed together, so waiting for DataScienceCluster is sufficient.
  log_info "Waiting for operator CRDs to be established..."
  wait_for_crd "datascienceclusters.datasciencecluster.opendatahub.io" 180 || {
    log_error "DataScienceCluster CRD not available - operator may not have installed correctly"
    return 1
  }

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

  # Check if DSCI already exists (operator may create it automatically)
  if kubectl get dscinitializations default-dsci &>/dev/null; then
    log_info "DSCInitialization already exists, skipping creation (operator auto-created)"
    return 0
  fi

  # Create DSCI only if it doesn't exist
  cat <<EOF | kubectl apply -f -
apiVersion: dscinitialization.opendatahub.io/v1
kind: DSCInitialization
metadata:
  name: default-dsci
spec:
  applicationsNamespace: ${NAMESPACE}
  monitoring:
    managementState: Managed
    namespace: ${NAMESPACE}-monitoring
    metrics: {}
  trustedCABundle:
    managementState: Managed
EOF
}

apply_dsc() {
  log_info "Applying DataScienceCluster with ModelsAsService..."

  # Check if a DataScienceCluster already exists, skip creation if so
  if kubectl get datasciencecluster -A --no-headers 2>/dev/null | grep -q .; then
    log_info "DataScienceCluster already exists in the cluster. Skipping creation."
    return 0
  fi

  # Apply DSC with modelsAsService - this is REQUIRED for MaaS deployment
  # Without modelsAsService, only KServe deploys (no maas-api, no HTTPRoutes, no AuthPolicy)
  # If the operator doesn't support modelsAsService, kubectl will fail with a clear error
  #
  # Note: RHOAI 3.2.0 does NOT support modelsAsService in DSC schema
  #       Only ODH currently supports this feature
  local data_dir="${SCRIPT_DIR}/data"
  kubectl apply --server-side=true -f "${data_dir}/datasciencecluster.yaml"
}

#──────────────────────────────────────────────────────────────
# GATEWAY API SETUP
#──────────────────────────────────────────────────────────────

# setup_gateway_api
#   Sets up the Gateway API infrastructure (GatewayClass).
#   This is general Gateway API setup that can be used by any Gateway resources.
setup_gateway_api() {
  log_info "Setting up Gateway API infrastructure..."

  local data_dir="${SCRIPT_DIR}/data"

  # Create GatewayClass for OpenShift Gateway API controller
  # This enables the built-in Gateway API implementation (OpenShift 4.14+)
  kubectl apply -f "${data_dir}/gatewayclass.yaml"
}

# setup_maas_gateway
#   Creates the Gateway resource required by ModelsAsService component.
#   ModelsAsService expects a gateway named "maas-default-gateway" in namespace "openshift-ingress".
#
#   This function:
#   1. Detects or uses the router's TLS certificate
#   2. Creates the Gateway resource with both HTTP and HTTPS listeners
#   3. Uses the kustomize manifest from deployment/base/networking/maas/
#
#   The Gateway includes:
#   - HTTP listener (port 80) - required for model discovery URLs
#   - HTTPS listener (port 443) - for secure API access
#   - Annotations for operator management and TLS bootstrap
#   - Labels for app identification
setup_maas_gateway() {
  log_info "Setting up ModelsAsService gateway..."

  # Get cluster domain for Gateway hostname
  local cluster_domain
  cluster_domain=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null || echo "")
  if [[ -z "$cluster_domain" ]]; then
    log_error "Could not determine cluster domain - required for Gateway hostname"
    return 1
  fi
  
  export CLUSTER_DOMAIN="$cluster_domain"
  log_info "  Cluster domain: ${CLUSTER_DOMAIN}"

  # Detect TLS certificate if not explicitly set (matches upstream deploy-rhoai-stable.sh logic)
  local cert_name="${CERT_NAME:-}"
  if [[ -z "$cert_name" ]]; then
    log_info "  Detecting TLS certificate secret..."

    # Primary: Get certificate from IngressController (most reliable source of truth)
    cert_name=$(kubectl get ingresscontroller default -n openshift-ingress-operator \
      -o jsonpath='{.spec.defaultCertificate.name}' 2>/dev/null || echo "")
    if [[ -n "$cert_name" ]] && kubectl get secret -n openshift-ingress "$cert_name" &>/dev/null; then
      log_info "  * Found certificate from IngressController: ${cert_name}"
    else
      [[ -n "$cert_name" ]] && log_debug "  * IngressController cert '${cert_name}' not found, trying alternatives..."
      cert_name=""
    fi

    # Fallback 1: Get certificate from router deployment
    if [[ -z "$cert_name" ]]; then
      cert_name=$(kubectl get deployment router-default -n openshift-ingress \
        -o jsonpath='{.spec.template.spec.volumes[?(@.name=="default-certificate")].secret.secretName}' 2>/dev/null || echo "")
      if [[ -n "$cert_name" ]] && kubectl get secret -n openshift-ingress "$cert_name" &>/dev/null; then
        log_info "  * Found certificate from router deployment: ${cert_name}"
      else
        cert_name=""
      fi
    fi

    # Fallback 2: Check known certificate secret names
    if [[ -z "$cert_name" ]]; then
      local cert_candidates=("default-gateway-cert" "router-certs-default")
      for cert in "${cert_candidates[@]}"; do
        if kubectl get secret -n openshift-ingress "$cert" &>/dev/null; then
          cert_name="$cert"
          log_info "  * Found TLS certificate secret: ${cert}"
          break
        fi
      done
    fi

    # Warning if no certificate found
    if [[ -z "$cert_name" ]]; then
      log_warn "  No TLS certificate found. Creating self-signed certificate..."
      local gateway_hostname="maas.${cluster_domain}"
      if create_tls_secret "maas-gateway-tls" "openshift-ingress" "${gateway_hostname}"; then
        cert_name="maas-gateway-tls"
        log_info "  * Created self-signed certificate: ${cert_name}"
      else
        log_error "Failed to create TLS certificate for gateway"
        return 1
      fi
    fi
  fi

  export CERT_NAME="$cert_name"
  log_info "  TLS certificate secret: ${CERT_NAME}"

  # Create the Gateway resource using the kustomize manifest
  # This includes both HTTP and HTTPS listeners, required annotations and labels
  log_info "Creating maas-default-gateway resource (allowing routes from all namespaces)..."
  
  local maas_networking_dir="${SCRIPT_DIR}/../deployment/base/networking/maas"
  if [[ -d "$maas_networking_dir" ]]; then
    # Use local kustomize manifest with envsubst for variable substitution
    kustomize build "$maas_networking_dir" | envsubst '$CLUSTER_DOMAIN $CERT_NAME' | kubectl apply --server-side=true -f -
  else
    # Fallback: fetch from GitHub (for standalone script usage)
    log_debug "  Local manifest not found, fetching from GitHub..."
    kubectl apply --server-side=true \
      -f <(kustomize build "https://github.com/opendatahub-io/models-as-a-service.git/deployment/base/networking/maas?ref=main" | \
           envsubst '$CLUSTER_DOMAIN $CERT_NAME')
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
  if ! kubectl wait --for=condition=Programmed gateway/maas-default-gateway -n openshift-ingress --timeout=120s 2>/dev/null; then
    log_warn "Gateway not yet Programmed after 120s - Kuadrant may take longer to become ready"
  fi

  log_info "Applying Kuadrant custom resource in $namespace..."

  local data_dir="${SCRIPT_DIR}/data"
  kubectl apply -f "${data_dir}/kuadrant.yaml" -n "$namespace"

  # Wait for Kuadrant to be ready (initial attempt - 60s)
  # If it fails with MissingDependency, restart the operator and retry
  log_info "Waiting for Kuadrant to become ready (initial check)..."
  if ! wait_for_custom_check "Kuadrant ready in $namespace" \
    "kubectl get kuadrant kuadrant -n $namespace -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}' 2>/dev/null | grep -q True" \
    60 \
    5; then
    
    # Check if it's a MissingDependency issue
    local kuadrant_reason
    kuadrant_reason=$(kubectl get kuadrant kuadrant -n "$namespace" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}' 2>/dev/null || echo "")
    
    if [[ "$kuadrant_reason" == "MissingDependency" ]]; then
      log_info "Kuadrant shows MissingDependency - restarting operator to re-register Gateway controller..."
      kubectl delete pod -n "$namespace" -l control-plane=controller-manager --force --grace-period=0 2>/dev/null || true
      sleep 15
      
      # Retry waiting for Kuadrant
      log_info "Retrying Kuadrant readiness check after operator restart..."
      wait_for_custom_check "Kuadrant ready in $namespace" \
        "kubectl get kuadrant kuadrant -n $namespace -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].status}' 2>/dev/null | grep -q True" \
        120 \
        5 || log_warn "Kuadrant not ready yet - AuthPolicy enforcement may fail on model HTTPRoutes"
    else
      log_warn "Kuadrant not ready (reason: $kuadrant_reason) - AuthPolicy enforcement may fail"
    fi
  fi
  
  log_info "Kuadrant setup complete"

  # Deploy usage policies (TokenRateLimitPolicy)
  deploy_usage_policies
}

# deploy_usage_policies
#   Deploys rate limiting and token limiting policies for the gateway.
#   These policies enable tier-based usage limits for API consumers.
deploy_usage_policies() {
  log_info "Deploying usage policies (TokenRateLimitPolicy)..."

  local project_root
  project_root="$(find_project_root)" || {
    log_warn "Could not find project root, skipping usage policies deployment"
    return 0
  }

  local policies_dir="$project_root/deployment/base/policies/usage-policies"
  if [[ ! -d "$policies_dir" ]]; then
    log_warn "Usage policies directory not found at $policies_dir, skipping"
    return 0
  fi

  # Capture kubectl output to temp file (piping to while loop loses the exit status)
  local temp_output
  temp_output=$(mktemp)

  local kubectl_exit=0
  if ! kustomize build "$policies_dir" | kubectl apply --server-side=true -f - > "$temp_output" 2>&1; then
    kubectl_exit=1
  fi

  # Log all output lines
  while read -r line; do log_debug "$line"; done < "$temp_output"
  rm -f "$temp_output"

  if [[ $kubectl_exit -eq 0 ]]; then
    log_info "Usage policies deployed successfully"
  else
    log_warn "Failed to deploy usage policies (non-fatal, rate limiting may not work)"
  fi
}

patch_operator_csv() {
  local operator_prefix=$1
  local namespace=$2
  local operator_image=$3

  log_info "Patching operator CSV with custom image: $operator_image"

  # Poll for CSV to be created instead of hardcoded sleep
  local csv_name=""
  local timeout=60
  local elapsed=0
  local interval=5

  log_info "Waiting for CSV to be created (timeout: ${timeout}s)..."
  while [[ $elapsed -lt $timeout ]]; do
    csv_name=$(kubectl get csv -n "$namespace" --no-headers 2>/dev/null | grep "^${operator_prefix}" | head -n1 | awk '{print $1}')
    if [[ -n "$csv_name" ]]; then
      log_debug "Found CSV: $csv_name after ${elapsed}s"
      break
    fi
    sleep $interval
    elapsed=$((elapsed + interval))
  done

  if [[ -z "$csv_name" ]]; then
    log_warn "Could not find CSV for $operator_prefix after ${timeout}s, skipping image patch"
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
# AUDIENCE CONFIGURATION FOR HYPERSHIFT/ROSA CLUSTERS
#──────────────────────────────────────────────────────────────

# configure_cluster_audience
#   Configures the AuthPolicy with the correct OIDC audience for the cluster.
#   This is required for Hypershift/ROSA clusters which use non-standard audiences.
#
#   Background:
#   - Standard Kubernetes clusters use audience: https://kubernetes.default.svc
#   - Hypershift/ROSA clusters use custom OIDC providers with different audiences
#   - Without this patch, JWT validation fails with HTTP 401
#
#   This function:
#   1. Detects the cluster's OIDC audience from a service account token
#   2. If non-standard, patches the AuthPolicy with the cluster-specific audience
#   3. Annotates the AuthPolicy to prevent operator from reverting the patch
configure_cluster_audience() {
  log_info "Checking cluster OIDC audience..."

  # Get cluster audience using helper from deployment-helpers.sh
  local cluster_aud
  cluster_aud=$(get_cluster_audience 2>/dev/null || echo "")

  if [[ -z "$cluster_aud" ]]; then
    log_warn "Could not determine cluster audience, skipping audience configuration"
    return 0
  fi

  log_debug "Detected cluster audience: $cluster_aud"

  # Check if this is a non-standard audience (Hypershift/ROSA)
  if [[ "$cluster_aud" == "https://kubernetes.default.svc" ]]; then
    log_info "Standard Kubernetes audience detected, no patching needed"
    return 0
  fi

  log_info "Configuring AuthPolicy for non-standard cluster audience..."
  log_info "  Detected audience: $cluster_aud"

  # Wait for AuthPolicy to be created by the operator
  local authpolicy_name="maas-api-auth-policy"
  local wait_timeout=120
  local elapsed=0

  log_info "  Waiting for AuthPolicy '$authpolicy_name' to be created (timeout: ${wait_timeout}s)..."
  while [[ $elapsed -lt $wait_timeout ]]; do
    if kubectl get authpolicy "$authpolicy_name" -n "$NAMESPACE" &>/dev/null; then
      log_info "  Found AuthPolicy '$authpolicy_name'"
      break
    fi
    sleep 5
    elapsed=$((elapsed + 5))
  done

  if ! kubectl get authpolicy "$authpolicy_name" -n "$NAMESPACE" &>/dev/null; then
    log_warn "AuthPolicy '$authpolicy_name' not found after ${wait_timeout}s, skipping audience configuration"
    log_warn "Authentication may fail on Hypershift/ROSA clusters"
    return 0
  fi

  # Step 1: Annotate to prevent operator reconciliation from reverting our patch
  log_info "  Annotating AuthPolicy to prevent operator reconciliation..."
  kubectl annotate authpolicy "$authpolicy_name" -n "$NAMESPACE" \
    opendatahub.io/managed="false" --overwrite 2>/dev/null || true

  # Step 2: Patch AuthPolicy with cluster-specific audience
  log_info "  Patching AuthPolicy with cluster audience..."
  if kubectl patch authpolicy "$authpolicy_name" -n "$NAMESPACE" --type=merge --patch-file <(cat <<EOF
spec:
  rules:
    authentication:
      openshift-identities:
        kubernetesTokenReview:
          audiences:
            - $cluster_aud
            - maas-default-gateway-sa
EOF
  ); then
    log_info "  AuthPolicy '$authpolicy_name' patched with custom audience"
  else
    log_warn "  Failed to patch AuthPolicy with custom audience"
    log_warn "  Authentication may fail on this cluster"
    return 0
  fi

  # Step 3: Verify the patch persisted (operator might revert it)
  sleep 3
  local actual_aud
  actual_aud=$(kubectl get authpolicy "$authpolicy_name" -n "$NAMESPACE" \
    -o jsonpath='{.spec.rules.authentication.openshift-identities.kubernetesTokenReview.audiences[0]}' 2>/dev/null || echo "")

  if [[ "$actual_aud" == "$cluster_aud" ]]; then
    log_info "  Verified: Custom audience configuration persisted"
  else
    log_warn "  WARNING: AuthPolicy audience may have been reverted to: ${actual_aud}"
    log_warn "  This may cause authentication failures on Hypershift/ROSA clusters"
  fi
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
