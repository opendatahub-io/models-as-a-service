#!/bin/bash

# Deployment Helper Functions
# This file contains reusable helper functions for MaaS platform deployment scripts

# ============================================================================
# JWT Decoding Functions
# ============================================================================

# _base64_decode
#   Cross-platform base64 decode wrapper.
#   Linux uses 'base64 -d', macOS (BSD) uses 'base64 -D'.
#   Reads from stdin and writes decoded output to stdout.
_base64_decode() {
  if [[ "$OSTYPE" == "darwin"* ]]; then
    base64 -D 2>/dev/null
  else
    base64 -d 2>/dev/null
  fi
}

# decode_jwt_payload <jwt_token>
#   Decodes the payload (second part) of a JWT token.
#   Handles base64url to standard base64 conversion and padding.
#   Returns the decoded JSON payload.
#   Works on both Linux and macOS.
#
# Usage:
#   PAYLOAD=$(decode_jwt_payload "$TOKEN")
#   echo "$PAYLOAD" | jq -r '.sub'
#
# Example:
#   TOKEN="<header>.<payload>.<signature>"  # Your JWT token
#   decode_jwt_payload "$TOKEN"  # Returns decoded JSON payload
decode_jwt_payload() {
  local jwt_token="$1"

  if [ -z "$jwt_token" ]; then
    echo ""
    return 1
  fi

  # Extract the payload (second part of JWT, separated by dots)
  local payload_b64url
  payload_b64url=$(echo "$jwt_token" | cut -d. -f2)

  if [ -z "$payload_b64url" ]; then
    echo ""
    return 1
  fi

  # Convert base64url to standard base64:
  # - Replace '-' with '+' and '_' with '/'
  # - Add padding (base64 must be multiple of 4 chars)
  local payload_b64
  payload_b64=$(echo "$payload_b64url" | tr '_-' '/+' | awk '{while(length($0)%4)$0=$0"=";print}')

  # Decode base64 to JSON (cross-platform)
  echo "$payload_b64" | _base64_decode
}

# get_jwt_claim <jwt_token> <claim_name>
#   Extracts a specific claim from a JWT token payload.
#   Returns the claim value or empty string if not found.
#
# Usage:
#   SUB=$(get_jwt_claim "$TOKEN" "sub")
#   AUD=$(get_jwt_claim "$TOKEN" "aud[0]")
#
# Example:
#   get_jwt_claim "$TOKEN" "sub"  # Returns: system:serviceaccount:...
get_jwt_claim() {
  local jwt_token="$1"
  local claim="$2"

  local payload
  payload=$(decode_jwt_payload "$jwt_token")

  if [ -z "$payload" ]; then
    echo ""
    return 1
  fi

  echo "$payload" | jq -r ".$claim // empty" 2>/dev/null
}

# get_cluster_audience
#   Retrieves the default audience from the current Kubernetes cluster.
#   Creates a temporary token and extracts the audience claim.
#
# Usage:
#   AUD=$(get_cluster_audience)
#   echo "Cluster audience: $AUD"
get_cluster_audience() {
  local temp_token
  temp_token=$(kubectl create token default --duration=10m 2>/dev/null)

  if [ -z "$temp_token" ]; then
    echo ""
    return 1
  fi

  get_jwt_claim "$temp_token" "aud[0]"
}

# ============================================================================
# Constants and Configuration
# ============================================================================

# Timeout values (seconds)
readonly DEFAULT_TIMEOUT=300
readonly CSV_TIMEOUT=180
readonly NAMESPACE_TIMEOUT=60
readonly POD_READY_TIMEOUT=120
readonly CRD_TIMEOUT=90
readonly WEBHOOK_TIMEOUT=120
readonly CUSTOM_RESOURCE_TIMEOUT=600

# Logging levels
readonly LOG_LEVEL_DEBUG=0
readonly LOG_LEVEL_INFO=1
readonly LOG_LEVEL_WARN=2
readonly LOG_LEVEL_ERROR=3

# Current log level (can be overridden by LOG_LEVEL env var)
CURRENT_LOG_LEVEL=${LOG_LEVEL_INFO}

# ============================================================================
# Version Management
# ============================================================================

# Minimum version requirements for operators
export KUADRANT_MIN_VERSION="1.3.1"
export AUTHORINO_MIN_VERSION="0.22.0"
export LIMITADOR_MIN_VERSION="0.16.0"
export DNS_OPERATOR_MIN_VERSION="0.15.0"

# ==========================================
# Logging Functions
# ==========================================

log_debug() {
  [[ ${CURRENT_LOG_LEVEL:-1} -le $LOG_LEVEL_DEBUG ]] || return 0
  echo "[DEBUG] $*"
}

log_info() {
  [[ ${CURRENT_LOG_LEVEL:-1} -le $LOG_LEVEL_INFO ]] || return 0
  echo "[INFO] $*"
}

log_warn() {
  [[ ${CURRENT_LOG_LEVEL:-1} -le $LOG_LEVEL_WARN ]] || return 0
  echo "[WARN] $*" >&2
}

log_error() {
  [[ ${CURRENT_LOG_LEVEL:-1} -le $LOG_LEVEL_ERROR ]] || return 0
  echo "[ERROR] $*" >&2
}

# ==========================================
# OLM Subscription and CSV Helper Functions
# ==========================================

# waitsubscriptioninstalled namespace subscription_name
#   Waits for an OLM Subscription to finish installing its CSV.
#   Exits with error if the installation times out.
waitsubscriptioninstalled() {
  local ns=${1?namespace is required}; shift
  local name=${1?subscription name is required}; shift

  echo "  * Waiting for Subscription $ns/$name to start setup..."
  # Use fully qualified resource name to avoid conflicts with Knative subscriptions
  kubectl wait subscription.operators.coreos.com --timeout=300s -n "$ns" "$name" --for=jsonpath='{.status.currentCSV}'
  local csv
  csv=$(kubectl get subscription.operators.coreos.com -n "$ns" "$name" -o jsonpath='{.status.currentCSV}')

  # Because, sometimes, the CSV is not there immediately.
  while ! kubectl get -n "$ns" csv "$csv" > /dev/null 2>&1; do
    sleep 1
  done

  echo "  * Waiting for Subscription setup to finish setup. CSV = $csv ..."
  if ! kubectl wait -n "$ns" --for=jsonpath="{.status.phase}"=Succeeded csv "$csv" --timeout=300s; then
    echo "    * ERROR: Timeout while waiting for Subscription to finish installation."
    return 1
  fi
}

# checksubscriptionexists catalog_namespace catalog_name operator_name
#   Checks if a subscription exists for the given operator from the specified catalog.
#   Returns the count of matching subscriptions (0 if none found).
checksubscriptionexists() {
  local catalog_ns=${1?catalog namespace is required}; shift
  local catalog_name=${1?catalog name is required}; shift
  local operator_name=${1?operator name is required}; shift

  local catalogns_cond=".spec.sourceNamespace == \"${catalog_ns}\""
  local catalog_cond=".spec.source == \"${catalog_name}\""
  local op_cond=".spec.name == \"${operator_name}\""
  local query="${catalogns_cond} and ${catalog_cond} and ${op_cond}"

  # Use fully qualified resource name to avoid conflicts with Knative subscriptions
  kubectl get subscriptions.operators.coreos.com -A -ojson | jq ".items | map(select(${query})) | length"
}

# checkcsvexists csv_prefix
#   Checks if a CSV exists by name prefix (e.g., "opendatahub-operator" matches "opendatahub-operator.v3.2.0").
#   Returns the count of matching CSVs (0 if none found).
checkcsvexists() {
  local csv_prefix=${1?csv prefix is required}; shift

  # Count CSVs whose name starts with the given prefix
  local count
  count=$(kubectl get csv -A -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -c "^${csv_prefix}" 2>/dev/null) || count=0
  echo "$count"
}

# ==========================================
# Operator Installation Helper Functions
# ==========================================

# is_operator_installed operator_name namespace
#   Checks if an operator subscription exists.
#   Returns 0 if installed, 1 if not found.
is_operator_installed() {
  local operator_name=${1?operator name is required}; shift
  local namespace=${1:-}  # namespace is optional

  # Use fully qualified resource name to avoid conflicts with Knative subscriptions
  if [[ -n "$namespace" ]]; then
    kubectl get subscription.operators.coreos.com -n "$namespace" 2>/dev/null | grep -q "$operator_name"
  else
    kubectl get subscription.operators.coreos.com --all-namespaces 2>/dev/null | grep -q "$operator_name"
  fi
}

# should_install_operator operator_name skip_flag namespace
#   Determines if an operator should be installed based on skip flag and existing installation.
#   Returns 0 if should install, 1 if should skip.
should_install_operator() {
  local operator_name=${1?operator name is required}; shift
  local skip_flag=${1?skip flag is required}; shift
  local namespace=${1:-}  # namespace is optional

  # Explicit skip
  [[ "$skip_flag" == "true" ]] && return 1

  # Auto mode: check if already installed
  if [[ "$skip_flag" == "auto" ]]; then
    is_operator_installed "$operator_name" "$namespace" && return 1
  fi

  return 0
}

# install_olm_operator operator_name namespace catalog_source channel starting_csv operatorgroup_target source_namespace
#   Generic function to install an OLM operator.
#
# Arguments:
#   operator_name - Name of the operator (e.g., "rhods-operator")
#   namespace - Target namespace for the operator
#   catalog_source - CatalogSource name (e.g., "redhat-operators")
#   channel - Subscription channel (e.g., "fast-3")
#   starting_csv - Starting CSV (optional, can be empty)
#   operatorgroup_target - Target namespace for OperatorGroup (optional, uses namespace if empty)
#   source_namespace - Catalog source namespace (optional, defaults to openshift-marketplace)
install_olm_operator() {
  local operator_name=${1?operator name is required}; shift
  local namespace=${1?namespace is required}; shift
  local catalog_source=${1?catalog source is required}; shift
  local channel=${1?channel is required}; shift
  local starting_csv=${1:-}; shift || true
  local operatorgroup_target=${1:-}; shift || true
  local source_namespace=${1:-openshift-marketplace}; shift || true

  log_info "Installing operator: $operator_name in namespace: $namespace"

  # Check if subscription already exists
  # Use fully qualified resource name to avoid conflicts with Knative subscriptions
  if kubectl get subscription.operators.coreos.com "$operator_name" -n "$namespace" &>/dev/null; then
    log_info "Subscription $operator_name already exists in $namespace, skipping"
    return 0
  fi

  # Create namespace if not exists
  if ! kubectl get namespace "$namespace" &>/dev/null; then
    log_info "Creating namespace: $namespace"
    kubectl create namespace "$namespace"
  fi

  # Wait for namespace to be ready
  wait_for_namespace "$namespace" 60

  # Create OperatorGroup if needed
  local og_count=$(kubectl get operatorgroup -n "$namespace" --no-headers 2>/dev/null | wc -l | tr -d ' ')
  if [ "$og_count" -eq 0 ]; then
    # If operatorgroup_target is "AllNamespaces", omit targetNamespaces field
    if [ "$operatorgroup_target" = "AllNamespaces" ]; then
      log_info "Creating OperatorGroup in $namespace for AllNamespaces mode"
      cat <<EOF | kubectl apply -f -
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: ${namespace}-operatorgroup
  namespace: ${namespace}
spec: {}
EOF
    else
      local og_target_ns="${operatorgroup_target:-$namespace}"
      log_info "Creating OperatorGroup in $namespace targeting $og_target_ns"
      cat <<EOF | kubectl apply -f -
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: ${namespace}-operatorgroup
  namespace: ${namespace}
spec:
  targetNamespaces:
  - ${og_target_ns}
EOF
    fi
  fi

  # Create Subscription
  log_info "Creating Subscription for $operator_name from $catalog_source (channel: $channel)"
  local subscription_yaml="
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: ${operator_name}
  namespace: ${namespace}
spec:
  channel: ${channel}
  name: ${operator_name}
  source: ${catalog_source}
  sourceNamespace: ${source_namespace}
"

  if [[ -n "$starting_csv" ]]; then
    subscription_yaml="${subscription_yaml}  startingCSV: ${starting_csv}
"
  fi

  echo "$subscription_yaml" | kubectl apply -f -

  # Wait for subscription to be installed
  log_info "Waiting for subscription to install..."
  waitsubscriptioninstalled "$namespace" "$operator_name"

  log_info "Operator $operator_name installed successfully"
}

# wait_for_custom_check description check_command timeout interval
#   Waits for a custom check command to succeed.
#
# Arguments:
#   description - Description of what we're waiting for
#   check_command - Command to execute (should return 0 on success)
#   timeout - Timeout in seconds
#   interval - Check interval in seconds
wait_for_custom_check() {
  local description=${1?description is required}; shift
  local check_command=${1?check command is required}; shift
  local timeout=${1:-120}; shift || true
  local interval=${1:-5}; shift || true

  log_info "Waiting for: $description (timeout: ${timeout}s)"

  local elapsed=0
  while [ $elapsed -lt $timeout ]; do
    if eval "$check_command"; then
      log_info "$description - Ready"
      return 0
    fi
    sleep "$interval"
    elapsed=$((elapsed + interval))
  done

  log_warn "$description - Timeout after ${timeout}s"
  return 1
}

# ==========================================
# Namespace Helper Functions
# ==========================================

# wait_for_namespace namespace [timeout]
#   Waits for a namespace to be created and become Active.
#   Returns 0 on success, 1 on timeout.
wait_for_namespace() {
  local namespace=${1?namespace is required}; shift
  local timeout=${1:-300}  # default 5 minutes

  if kubectl get namespace "$namespace" >/dev/null 2>&1; then
    kubectl wait namespace/"$namespace" --for=jsonpath='{.status.phase}'=Active --timeout=60s
    return $?
  fi

  echo "* Waiting for $namespace namespace to be created..."
  local elapsed=0
  local interval=5
  while [ $elapsed -lt $timeout ]; do
    if kubectl get namespace "$namespace" >/dev/null 2>&1; then
      kubectl wait namespace/"$namespace" --for=jsonpath='{.status.phase}'=Active --timeout=60s
      return $?
    fi
    sleep $interval
    elapsed=$((elapsed + interval))
  done

  echo "  WARNING: $namespace namespace was not created within timeout."
  return 1
}

# wait_for_resource kind name namespace [timeout]
#   Waits for a resource to be created.
#   Returns 0 when found, 1 on timeout.
wait_for_resource() {
  local kind=${1?kind is required}; shift
  local name=${1?name is required}; shift
  local namespace=${1?namespace is required}; shift
  local timeout=${1:-300}  # default 5 minutes

  echo "* Waiting for $kind/$name in $namespace..."
  local elapsed=0
  local interval=5
  while [ $elapsed -lt $timeout ]; do
    if kubectl get "$kind" "$name" -n "$namespace" >/dev/null 2>&1; then
      echo "  * Found $kind/$name"
      return 0
    fi
    sleep $interval
    elapsed=$((elapsed + interval))
  done

  echo "  WARNING: $kind/$name was not found within timeout."
  return 1
}

# create_tls_secret name namespace cn
#   Creates a self-signed TLS secret if it doesn't already exist.
#   Uses OpenSSL to generate a matching key/cert pair.
#
# Arguments:
#   name      - Name of the TLS secret
#   namespace - Namespace to create the secret in
#   cn        - Common Name for the certificate (e.g., hostname)
#
# Returns:
#   0 on success (created or already exists), 1 on failure
create_tls_secret() {
  local name=${1?secret name is required}; shift
  local namespace=${1?namespace is required}; shift
  local cn=${1:-$name}  # default CN to secret name

  # Check if secret already exists
  if kubectl get secret "$name" -n "$namespace" &>/dev/null; then
    echo "  * TLS secret $name already exists in $namespace"
    return 0
  fi

  echo "  * Creating TLS secret $name in $namespace (CN=$cn)..."
  
  # Create temp directory for key/cert files
  local temp_dir
  temp_dir=$(mktemp -d)
  
  # Helper to clean up temp directory
  _cleanup_temp() { rm -rf "$temp_dir"; }

  # Generate self-signed certificate with matching key
  if ! openssl req -x509 -newkey rsa:2048 \
      -keyout "${temp_dir}/tls.key" \
      -out "${temp_dir}/tls.crt" \
      -days 365 -nodes \
      -subj "/CN=${cn}" 2>/dev/null; then
    echo "  ERROR: Failed to generate TLS certificate"
    _cleanup_temp
    return 1
  fi

  # Verify files were created
  if [[ ! -f "${temp_dir}/tls.crt" || ! -f "${temp_dir}/tls.key" ]]; then
    echo "  ERROR: TLS certificate files not generated"
    _cleanup_temp
    return 1
  fi

  # Create the secret
  if kubectl create secret tls "$name" \
      --cert="${temp_dir}/tls.crt" \
      --key="${temp_dir}/tls.key" \
      -n "$namespace"; then
    echo "  * TLS secret $name created successfully"
    _cleanup_temp
    return 0
  else
    echo "  ERROR: Failed to create TLS secret $name"
    _cleanup_temp
    return 1
  fi
}

# create_gateway_route name namespace hostname service tls_secret
#   Creates an OpenShift Route to expose a Gateway via the cluster's apps domain.
#   Uses 'reencrypt' TLS termination for better security (trusted cert to clients).
#
# Arguments:
#   name       - Name of the Route resource
#   namespace  - Namespace for the Route
#   hostname   - Hostname for the Route (e.g., maas.apps.cluster.domain)
#   service    - Target Service name
#   tls_secret - Name of the TLS secret containing the Gateway's certificate
#
# Returns:
#   0 on success, 1 on failure
create_gateway_route() {
  local name=${1?route name is required}; shift
  local namespace=${1?namespace is required}; shift
  local hostname=${1?hostname is required}; shift
  local service=${1?service name is required}; shift
  local tls_secret=${1?tls secret name is required}

  echo "  * Creating Route $name for $hostname..."

  # Get the Gateway's TLS certificate for reencrypt mode
  # This allows the Router to trust the Gateway's certificate
  local dest_ca_cert
  dest_ca_cert=$(kubectl get secret "$tls_secret" -n "$namespace" \
    -o jsonpath='{.data.tls\.crt}' 2>/dev/null | base64 -d || echo "")

  if [[ -n "$dest_ca_cert" ]]; then
    # Use reencrypt: Router terminates TLS with trusted cert, re-encrypts to Gateway
    # This gives clients a trusted certificate instead of our self-signed one
    cat <<EOF | kubectl apply -f -
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: ${name}
  namespace: ${namespace}
spec:
  host: ${hostname}
  port:
    targetPort: https
  to:
    kind: Service
    name: ${service}
    weight: 100
  tls:
    termination: reencrypt
    destinationCACertificate: |
$(echo "$dest_ca_cert" | sed 's/^/      /')
EOF
  else
    # Fallback to passthrough if we can't get the certificate
    echo "  WARNING: Could not retrieve TLS certificate from $tls_secret, using passthrough"
    cat <<EOF | kubectl apply -f -
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: ${name}
  namespace: ${namespace}
spec:
  host: ${hostname}
  port:
    targetPort: https
  to:
    kind: Service
    name: ${service}
    weight: 100
  tls:
    termination: passthrough
EOF
  fi
}

# find_project_root [start_dir] [marker]
#   Walks up the directory tree to find the project root.
#   Returns the path containing the marker (default: .git)
find_project_root() {
  local start_dir="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"
  local marker="${2:-.git}"
  local dir="$start_dir"

  while [[ "$dir" != "/" && ! -e "$dir/$marker" ]]; do
    dir="$(dirname "$dir")"
  done

  if [[ -e "$dir/$marker" ]]; then
    printf '%s\n' "$dir"
  else
    echo "Error: couldn't find '$marker' in any parent of '$start_dir'" >&2
    return 1
  fi
}

# set_maas_api_image
#   Sets the MaaS API container image in kustomization using MAAS_API_IMAGE env var.
#   If MAAS_API_IMAGE is not set, does nothing (uses default from kustomization.yaml).
#   Creates a backup that must be restored by calling cleanup_maas_api_image.
#   Idempotent: safe to call multiple times, only sets image on first call.
#
# Environment:
#   MAAS_API_IMAGE - Container image to use (e.g., quay.io/opendatahub/maas-api:pr-123)
#
# Usage:
#   trap cleanup_maas_api_image EXIT INT TERM  # Set trap FIRST
#   set_maas_api_image                          # Then set image
#   # ... do deployment ...
set_maas_api_image() {
  # Skip if MAAS_API_IMAGE is not set
  if [ -z "${MAAS_API_IMAGE:-}" ]; then
    return 0
  fi

  # Idempotent: skip if already set
  if [ -n "${_MAAS_API_IMAGE_SET:-}" ]; then
    return 0
  fi

  local project_root
  project_root="$(find_project_root)" || {
    echo "Error: failed to find project root" >&2
    return 1
  }

  # Exported so cleanup_maas_api_image can access them
  export _MAAS_API_KUSTOMIZATION="$project_root/deployment/base/maas-api/core/kustomization.yaml"
  export _MAAS_API_BACKUP="${_MAAS_API_KUSTOMIZATION}.backup"
  export _MAAS_API_IMAGE_SET=1

  echo "   Setting MaaS API image: ${MAAS_API_IMAGE}"
  
  cp "$_MAAS_API_KUSTOMIZATION" "$_MAAS_API_BACKUP" || {
    echo "Error: failed to create backup of kustomization.yaml" >&2
    return 1
  }
  
  (cd "$(dirname "$_MAAS_API_KUSTOMIZATION")" && kustomize edit set image "maas-api=${MAAS_API_IMAGE}") || {
    echo "Error: failed to set image in kustomization.yaml" >&2
    mv -f "$_MAAS_API_BACKUP" "$_MAAS_API_KUSTOMIZATION" 2>/dev/null || true
    return 1
  }
}

# cleanup_maas_api_image
#   Restores the original kustomization.yaml from backup.
#   Safe to call even if set_maas_api_image was not called or MAAS_API_IMAGE was not set.
cleanup_maas_api_image() {
  if [ -n "${_MAAS_API_BACKUP:-}" ] && [ -f "$_MAAS_API_BACKUP" ]; then
    mv -f "$_MAAS_API_BACKUP" "$_MAAS_API_KUSTOMIZATION" 2>/dev/null || true
  fi
}

# inject_maas_api_image_operator_mode namespace
#   Patches the maas-api deployment with custom image when MAAS_API_IMAGE is set.
#   Used in operator mode after the operator creates the deployment.
#
# Arguments:
#   namespace - Namespace where maas-api is deployed (opendatahub or redhat-ods-applications)
#
# Environment:
#   MAAS_API_IMAGE - Custom MaaS API container image
#
# Returns:
#   0 on success, 0 if MAAS_API_IMAGE not set (skip), 1 on failure
inject_maas_api_image_operator_mode() {
  local namespace=${1?namespace is required}; shift

  # Skip if MAAS_API_IMAGE is not set
  if [ -z "${MAAS_API_IMAGE:-}" ]; then
    echo "  * MAAS_API_IMAGE not set, using operator default"
    return 0
  fi

  echo "  * Injecting custom MaaS API image: ${MAAS_API_IMAGE}"

  # Wait for maas-api deployment to be created by the operator
  echo "  * Waiting for maas-api deployment to be created by operator..."
  local timeout=300
  local elapsed=0
  while [ $elapsed -lt $timeout ]; do
    if kubectl get deployment maas-api -n "$namespace" >/dev/null 2>&1; then
      echo "  * maas-api deployment found"
      break
    fi
    sleep 5
    elapsed=$((elapsed + 5))
  done

  if [ $elapsed -ge $timeout ]; then
    echo "ERROR: Timeout waiting for maas-api deployment to be created" >&2
    return 1
  fi

  # Patch the deployment with custom image
  echo "  * Patching maas-api deployment with image: ${MAAS_API_IMAGE}"
  kubectl patch deployment maas-api -n "$namespace" --type='json' -p="[
    {\"op\": \"replace\", \"path\": \"/spec/template/spec/containers/0/image\", \"value\": \"${MAAS_API_IMAGE}\"}
  ]" || {
    echo "ERROR: Failed to patch maas-api deployment" >&2
    return 1
  }

  # Wait for rollout to complete
  echo "  * Waiting for deployment rollout to complete..."
  kubectl rollout status deployment/maas-api -n "$namespace" --timeout=180s || {
    echo "WARNING: Deployment rollout did not complete within timeout (continuing anyway)" >&2
  }

  echo "  * Successfully injected custom MaaS API image"
  return 0
}

# Helper function to wait for CRD to be established
wait_for_crd() {
  local crd="$1"
  local timeout="${2:-60}"  # timeout in seconds
  local interval=2
  local end_time=$((SECONDS + timeout))

  echo "⏳ Waiting for CRD ${crd} to appear (timeout: ${timeout}s)…"
  while [ $SECONDS -lt $end_time ]; do
    if kubectl get crd "$crd" &>/dev/null; then
      echo "✅ CRD ${crd} detected, waiting for it to become Established..."
      # Pass remaining time, not full timeout
      local remaining_time=$((end_time - SECONDS))
      [ $remaining_time -lt 1 ] && remaining_time=1
      if kubectl wait --for=condition=Established --timeout="${remaining_time}s" "crd/$crd" 2>/dev/null; then
        return 0
      else
        echo "❌ CRD ${crd} failed to become Established" >&2
        return 1
      fi
    fi
    sleep $interval
  done

  echo "❌ Timed out after ${timeout}s waiting for CRD $crd to appear." >&2
  return 1
}

# Helper function to extract version from CSV name (e.g., "operator.v1.2.3" -> "1.2.3")
extract_version_from_csv() {
  local csv_name="$1"
  echo "$csv_name" | sed -n 's/.*\.v\([0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*\).*/\1/p'
}

# Helper function to compare semantic versions (returns 0 if version1 >= version2)
#
# NOTE: This comparison is intentionally simple and may have edge cases (e.g., comparing
# 1.2.9 vs 1.2.10 with string comparison). The trade-off:
# - Loose validation = resilient to minor/patch version updates, easier maintenance
# - Strict semver = more accurate, but requires updates for every version bump
# For a deployment script, we prefer resilience over strict accuracy. If false positives
# become an issue in practice, we can implement proper semver comparison.
version_compare() {
  local version1="$1"
  local version2="$2"

  # Convert versions to comparable numbers (e.g., "1.2.3" -> "001002003")
  local v1=$(echo "$version1" | awk -F. '{printf "%03d%03d%03d", $1, $2, $3}')
  local v2=$(echo "$version2" | awk -F. '{printf "%03d%03d%03d", $1, $2, $3}')

  [ "$v1" -ge "$v2" ]
}

# Helper function to find CSV by operator name and check minimum version
find_csv_with_min_version() {
  local operator_prefix="$1"
  local min_version="$2"
  local namespace="${3:-kuadrant-system}"
  
  local csv_name=$(kubectl get csv -n "$namespace" --no-headers 2>/dev/null | grep "^${operator_prefix}" | head -n1 | awk '{print $1}')
  
  if [ -z "$csv_name" ]; then
    echo "   No CSV found for ${operator_prefix} in ${namespace}" >&2
    return 1
  fi
  
  local installed_version=$(extract_version_from_csv "$csv_name")
  if [ -z "$installed_version" ]; then
    echo "   Could not parse version from CSV name: ${csv_name}" >&2
    return 1
  fi
  
  if version_compare "$installed_version" "$min_version"; then
    echo "$csv_name"
    return 0
  fi
  
  echo "   ${csv_name} version ${installed_version} is below minimum ${min_version}" >&2
  return 1
}

# Helper function to wait for CSV with minimum version requirement
wait_for_csv_with_min_version() {
  local operator_prefix="$1"
  local min_version="$2"
  local namespace="${3:-kuadrant-system}"
  local timeout="${4:-180}"
  
  echo "⏳ Looking for ${operator_prefix} (minimum version: ${min_version})..."
  
  local end_time=$((SECONDS + timeout))
  
  while [ $SECONDS -lt $end_time ]; do
    local csv_name=$(find_csv_with_min_version "$operator_prefix" "$min_version" "$namespace")
    
    if [ -n "$csv_name" ]; then
      # Found a CSV with suitable version
      local installed_version=$(extract_version_from_csv "$csv_name")
      echo "✅ Found CSV: ${csv_name} (version: ${installed_version} >= ${min_version})"
      # Pass remaining time, not full timeout
      local remaining_time=$((end_time - SECONDS))
      [ $remaining_time -lt 1 ] && remaining_time=1
      wait_for_csv "$csv_name" "$namespace" "$remaining_time"
      return $?
    fi
    
    # Check if any version exists (for progress feedback)
    local any_csv=$(kubectl get csv -n "$namespace" --no-headers 2>/dev/null | grep "^${operator_prefix}" | head -n1 | awk '{print $1}' || echo "")
    if [ -n "$any_csv" ]; then
      local installed_version=$(extract_version_from_csv "$any_csv")
      echo "   Found ${any_csv} with version ${installed_version}, waiting for version >= ${min_version}..."
    else
      echo "   No CSV found for ${operator_prefix} yet, waiting for installation..."
    fi
    
    sleep 10
  done
  
  # Timeout reached
  echo "❌ Timed out waiting for ${operator_prefix} with minimum version ${min_version}"
  return 1
}

# Helper function to wait for CSV to reach Succeeded state
wait_for_csv() {
  local csv_name="$1"
  local namespace="${2:-kuadrant-system}"
  local timeout="${3:-180}"  # timeout in seconds
  local interval=5
  local end_time=$((SECONDS + timeout))
  local last_status_print=$SECONDS

  echo "⏳ Waiting for CSV ${csv_name} to succeed (timeout: ${timeout}s)..."
  while [ $SECONDS -lt $end_time ]; do
    local phase=$(kubectl get csv -n "$namespace" "$csv_name" -o jsonpath='{.status.phase}' 2>/dev/null || echo "NotFound")
    local elapsed=$((SECONDS - (end_time - timeout)))

    case "$phase" in
      "Succeeded")
        echo "✅ CSV ${csv_name} succeeded"
        return 0
        ;;
      "Failed")
        echo "❌ CSV ${csv_name} failed" >&2
        kubectl get csv -n "$namespace" "$csv_name" -o jsonpath='{.status.message}' 2>/dev/null
        return 1
        ;;
      *)
        if [ $((SECONDS - last_status_print)) -ge 30 ]; then
          echo "   CSV ${csv_name} status: ${phase} (${elapsed}s elapsed)"
          last_status_print=$SECONDS
        fi
        ;;
    esac

    sleep $interval
  done

  echo "❌ Timed out after ${timeout}s waiting for CSV ${csv_name}" >&2
  return 1
}

# Helper function to wait for pods in a namespace to be ready
wait_for_pods() {
  local namespace="$1"
  local timeout="${2:-120}"
  
  kubectl get namespace "$namespace" &>/dev/null || return 0
  
  echo "⏳ Waiting for pods in $namespace to be ready..."
  local end=$((SECONDS + timeout))
  local not_ready
  while [ $SECONDS -lt $end ]; do
    not_ready=$(kubectl get pods -n "$namespace" --no-headers 2>/dev/null | grep -v -E 'Running|Completed|Succeeded' | wc -l)
    [ "$not_ready" -eq 0 ] && return 0
    sleep 5
  done
  echo "⚠️  Timeout waiting for pods in $namespace" >&2
  return 1
}

wait_for_validating_webhooks() {
    local namespace="$1"
    local timeout="${2:-60}"
    local interval=2
    local end=$((SECONDS+timeout))

    echo "⏳ Waiting for validating webhooks in namespace $namespace (timeout: $timeout sec)..."

    while [ $SECONDS -lt $end ]; do
        local not_ready=0

        local services
        services=$(kubectl get validatingwebhookconfigurations \
          -o jsonpath='{range .items[*].webhooks[*].clientConfig.service}{.namespace}/{.name}{"\n"}{end}' \
          | grep "^$namespace/" | sort -u)

        if [ -z "$services" ]; then
            echo "⚠️  No validating webhooks found in namespace $namespace"
            return 0
        fi

        for svc in $services; do
            local ns name ready
            ns=$(echo "$svc" | cut -d/ -f1)
            name=$(echo "$svc" | cut -d/ -f2)

            ready=$(kubectl get endpoints -n "$ns" "$name" -o jsonpath='{.subsets[*].addresses[*].ip}' 2>/dev/null || true)
            if [ -z "$ready" ]; then
                echo "🔴 Webhook service $ns/$name not ready"
                not_ready=1
            else
                echo "✅ Webhook service $ns/$name has ready endpoints"
            fi
        done

        if [ "$not_ready" -eq 0 ]; then
            echo "🎉 All validating webhook services in $namespace are ready"
            return 0
        fi

        sleep $interval
    done

    echo "❌ Timed out waiting for validating webhooks in $namespace"
    return 1
}

# ==========================================
# Custom Catalog Source Functions
# ==========================================

# create_custom_catalogsource name namespace catalog_image
#   Creates a CatalogSource from a catalog/index image.
#   This allows installing operators from custom catalog images instead of the default catalog.
#
#   IMPORTANT: This requires a CATALOG/INDEX image, NOT a bundle image!
#   - Catalog image: Contains the FBC database and runs 'opm serve' (e.g., quay.io/opendatahub/opendatahub-operator-catalog:latest)
#   - Bundle image: Contains operator manifests only, cannot be used directly (e.g., quay.io/opendatahub/opendatahub-operator-bundle:latest)
#
# Arguments:
#   name          - Name for the CatalogSource
#   namespace     - Namespace for the CatalogSource (usually openshift-marketplace)
#   catalog_image - The operator catalog/index image (e.g., quay.io/opendatahub/opendatahub-operator-catalog:latest)
#
# Returns:
#   0 on success, 1 on failure
create_custom_catalogsource() {
  local name=${1?catalogsource name is required}; shift
  local namespace=${1?namespace is required}; shift
  local catalog_image=${1?catalog image is required}; shift

  echo "* Creating CatalogSource '$name' from catalog image: $catalog_image"

  # Check if CatalogSource already exists
  if kubectl get catalogsource "$name" -n "$namespace" &>/dev/null; then
    echo "  * CatalogSource '$name' already exists. Updating..."
    kubectl delete catalogsource "$name" -n "$namespace" --ignore-not-found
    sleep 5
  fi

  cat <<EOF | kubectl apply -f -
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: ${name}
  namespace: ${namespace}
spec:
  sourceType: grpc
  image: ${catalog_image}
  displayName: "Custom ${name} Catalog"
  publisher: "Custom"
  updateStrategy:
    registryPoll:
      interval: 10m
EOF

  echo "  * Waiting for CatalogSource to be ready..."
  local timeout=120

  if ! kubectl wait catalogsource "$name" -n "$namespace" \
      --for=jsonpath='{.status.connectionState.lastObservedState}'=READY \
      --timeout="${timeout}s" 2>/dev/null; then
    local state=$(kubectl get catalogsource "$name" -n "$namespace" \
      -o jsonpath='{.status.connectionState.lastObservedState}' 2>/dev/null)
    echo "  ERROR: CatalogSource may not be fully ready yet (state: $state)"
    return 1
  fi
  echo "  * CatalogSource '$name' is ready"
  return 0
}

# cleanup_custom_catalogsource name namespace
#   Removes a custom CatalogSource created by create_custom_catalogsource.
cleanup_custom_catalogsource() {
  local name=${1?catalogsource name is required}; shift
  local namespace=${1?namespace is required}; shift

  if kubectl get catalogsource "$name" -n "$namespace" &>/dev/null; then
    echo "* Removing CatalogSource '$name'..."
    kubectl delete catalogsource "$name" -n "$namespace" --ignore-not-found
  fi
}

# wait_datasciencecluster_ready [name] [timeout]
#   Waits for a DataScienceCluster's KServe and ModelsAsService components to be ready.
#
# Arguments:
#   name    - Name of the DataScienceCluster (default: default-dsc)
#   timeout - Timeout in seconds (default: 600)
#
# Returns:
#   0 on success, 1 on failure
wait_datasciencecluster_ready() {
  local name="${1:-default-dsc}"
  local timeout="${2:-600}"
  local interval=20
  local elapsed=0

  echo "* Waiting for DataScienceCluster '$name' KServe and ModelsAsService components to be ready..."

  while [ $elapsed -lt $timeout ]; do
    # Grab full DSC status as JSON
    local dsc_json
    dsc_json=$(kubectl get datasciencecluster "$name" -o json 2>/dev/null || echo "")
    
    if [ -z "$dsc_json" ]; then
      echo "  - Waiting for DataScienceCluster/$name resource to appear..."
      sleep $interval
      elapsed=$((elapsed + interval))
      continue
    fi

    local kserve_state kserve_ready maas_ready model_controller_ready
    kserve_state=$(echo "$dsc_json" | jq -r '.status.components.kserve.managementState // ""')
    kserve_ready=$(echo "$dsc_json" | jq -r '.status.conditions[]? | select(.type=="KserveReady") | .status' | tail -n1)
    maas_ready=$(echo "$dsc_json" | jq -r '.status.conditions[]? | select(.type=="ModelsAsServiceReady") | .status' | tail -n1)
    model_controller_ready=$(echo "$dsc_json" | jq -r '.status.conditions[]? | select(.type=="ModelControllerReady") | .status' | tail -n1)

    # v2 API: ModelsAsServiceReady doesn't exist, use ModelControllerReady instead
    # v1 API: ModelsAsServiceReady exists but may stay False
    # Use ModelControllerReady as fallback if ModelsAsServiceReady is not True
    # This handles both v2 API (no ModelsAsServiceReady condition) and v1 API (condition exists but may stay False)
    local maas_check="$maas_ready"
    if [[ "$maas_ready" != "True" && "$model_controller_ready" == "True" ]]; then
      maas_check="$model_controller_ready"
    fi

    if [[ "$kserve_state" == "Managed" && "$kserve_ready" == "True" && "$maas_check" == "True" ]]; then
      echo "  * KServe and ModelsAsService are ready in DataScienceCluster '$name'"
      return 0
    else
      echo "  - KServe state: $kserve_state, KserveReady: $kserve_ready, ModelsAsServiceReady: $maas_ready, ModelControllerReady: $model_controller_ready"
    fi

    sleep $interval
    elapsed=$((elapsed + interval))
  done

  echo "  ERROR: KServe and/or ModelsAsService did not become ready in DataScienceCluster/$name within $timeout seconds."
  echo "  Final status: KServe=$kserve_state, KserveReady=$kserve_ready, ModelsAsServiceReady=$maas_ready, ModelControllerReady=$model_controller_ready"
  echo "  Tip: Check 'kubectl describe datasciencecluster $name' for more details"
  return 1
}

# wait_authorino_ready <namespace> [timeout]
#   Waits for Authorino to be ready and accepting requests.
#   Note: Request are required because authorino will report ready status but still give 500 errors.
#   
#   This checks:
#   1. Authorino CR status is Ready
#   2. Auth service cluster is healthy in gateway's Envoy
#   3. Auth requests are actually succeeding (not erroring)
#
# Arguments:
#   namespace - Authorino namespace (required)
#               "kuadrant-system" for Kuadrant (upstream/ODH)
#               "rh-connectivity-link" for RHCL (downstream/RHOAI)
#   timeout   - Timeout in seconds (default: 120)
#
# Returns:
#   0 on success, 1 on failure
wait_authorino_ready() {
  local authorino_namespace="${1:?ERROR: namespace is required (kuadrant-system or rh-connectivity-link)}"
  local timeout=${2:-120}
  local interval=5
  local elapsed=0

  echo "* Waiting for Authorino to be ready (timeout: ${timeout}s)..."
  echo "  - Checking Authorino in namespace: $authorino_namespace"

  if ! kubectl get authorino -n "$authorino_namespace" &>/dev/null; then
    echo "  ERROR: No Authorino CR found in namespace: $authorino_namespace"
    return 1
  fi

  # First, wait for Authorino CR to be ready
  echo "  - Checking Authorino CR status..."
  while [[ $elapsed -lt $timeout ]]; do
    local authorino_ready
    authorino_ready=$(kubectl get authorino -n "$authorino_namespace" -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")

    if [[ "$authorino_ready" == "True" ]]; then
      echo "  * Authorino CR is Ready"
      break
    fi

    echo "  - Authorino CR not ready yet (status: ${authorino_ready:-not found}), waiting..."
    sleep $interval
    elapsed=$((elapsed + interval))
  done

  if [[ $elapsed -ge $timeout ]]; then
    echo "  ERROR: Authorino CR did not become ready within ${timeout} seconds"
    return 1
  fi

  # Verify Gateway resource is ready
  echo "  - Verifying Gateway resource is ready..."
  local gateway_programmed
  gateway_programmed=$(kubectl get gateway maas-default-gateway -n openshift-ingress -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}' 2>/dev/null || echo "")

  if [[ "$gateway_programmed" != "True" ]]; then
    echo "  WARNING: Gateway is not Programmed yet (status: ${gateway_programmed:-not found})"
    echo "  WARNING: This may cause auth service routing issues"
  else
    echo "  * Gateway is Programmed and ready"
  fi

  # Try to check auth service cluster health in gateway (Istio-specific, may not work with OpenShift Gateway)
  echo "  - Checking if auth service is registered in gateway..."
  local gateway_pod
  gateway_pod=$(kubectl get pods -n openshift-ingress -l gateway.networking.k8s.io/gateway-name=maas-default-gateway -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")

  if [[ -z "$gateway_pod" ]]; then
    echo "  - No dedicated gateway pod found (expected with OpenShift Gateway controller)"
    echo "  - OpenShift Gateway uses shared router infrastructure"
  else
    # Try to check cluster health, but don't wait long since this might not work with OpenShift Gateway
    local health_status
    health_status=$(timeout 10s kubectl exec -n openshift-ingress "$gateway_pod" -- pilot-agent request GET /clusters 2>/dev/null | grep -E "kuadrant-auth-service|authorino.*authorization" | grep "health_flags" | head -1 || echo "")

    if [[ "$health_status" == *"healthy"* ]]; then
      echo "  * Auth service cluster is healthy in gateway"
    else
      echo "  - Gateway pod found but cluster health check not available (not Istio-based)"
    fi
  fi

  # Finally, verify auth requests are actually succeeding (not just cluster marked healthy)
  echo "  - Verifying auth requests are succeeding..."

  # Get gateway URL from the gateway spec (aligned with verify-models-and-limits.sh)
  local maas_url=""
  local https_hostname
  https_hostname=$(kubectl get gateway maas-default-gateway -n openshift-ingress \
    -o jsonpath='{.spec.listeners[?(@.protocol=="HTTPS")].hostname}' 2>/dev/null | awk '{print $1}')

  if [[ -n "$https_hostname" ]]; then
    maas_url="https://${https_hostname}/maas-api/health"
  else
    local http_hostname
    http_hostname=$(kubectl get gateway maas-default-gateway -n openshift-ingress \
      -o jsonpath='{.spec.listeners[?(@.protocol=="HTTP")].hostname}' 2>/dev/null | awk '{print $1}')

    if [[ -n "$http_hostname" ]]; then
      maas_url="http://${http_hostname}/maas-api/health"
    fi
  fi

  if [[ -z "$maas_url" ]]; then
    echo "  WARNING: Could not determine gateway URL, skipping request verification"
    return 0
  fi

  echo "  - Using gateway URL: $maas_url"
  local consecutive_success=0
  local required_success=3

  while [[ $elapsed -lt $timeout ]]; do
    # Make a test request - we expect 401 (auth working) not 500 (auth failing)
    # Capture both response body and HTTP code for better diagnostics
    local response_file
    response_file=$(mktemp)
    local http_code
    http_code=$(curl -sSk -o "$response_file" -w "%{http_code}" "$maas_url" 2>&1)

    # If http_code is not a 3-digit number, curl failed
    if ! [[ "$http_code" =~ ^[0-9]{3}$ ]]; then
      local curl_error="$http_code"
      http_code="000"
    fi

    if [[ "$http_code" == "401" || "$http_code" == "200" ]]; then
      consecutive_success=$((consecutive_success + 1))
      echo "  - Auth request succeeded (HTTP $http_code) [$consecutive_success/$required_success]"
      rm -f "$response_file"

      if [[ $consecutive_success -ge $required_success ]]; then
        echo "  * Auth requests verified working"
        return 0
      fi
    else
      consecutive_success=0
      if [[ "$http_code" == "000" ]]; then
        # Show actual curl error
        local error_msg
        error_msg=$(echo "$curl_error" | head -1 | sed 's/^curl: ([0-9]*) //')
        echo "  - Auth request failed: ${error_msg:-Connection failed}"
      elif [[ "$http_code" == "500" || "$http_code" == "502" || "$http_code" == "503" ]]; then
        # Show response body for server errors
        local error_body
        error_body=$(cat "$response_file" 2>/dev/null | head -c 200 | tr '\n' ' ')
        echo "  - Auth request returned HTTP $http_code: ${error_body:-no details}"
      else
        echo "  - Auth request returned HTTP $http_code, waiting for stabilization..."
      fi
      rm -f "$response_file"
    fi

    sleep 2
    elapsed=$((elapsed + 2))
  done

  echo "  WARNING: Auth request verification timed out, continuing anyway"
  return 0
}
