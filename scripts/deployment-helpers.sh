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
# Version Management
# ============================================================================

# Minimum version requirements for operators
export KUADRANT_MIN_VERSION="1.3.1"
export AUTHORINO_MIN_VERSION="0.22.0"
export LIMITADOR_MIN_VERSION="0.16.0"
export DNS_OPERATOR_MIN_VERSION="0.15.0"

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
  kubectl wait subscription.operators.coreos.com --timeout=300s -n "$ns" "$name" --for=jsonpath='{.status.currentCSV}'
  local csv
  csv=$(kubectl get subscription.operators.coreos.com -n "$ns" "$name" -o jsonpath='{.status.currentCSV}')

  # Because, sometimes, the CSV is not there immediately.
  while ! kubectl get -n "$ns" csv "$csv" > /dev/null 2>&1; do
    sleep 1
  done

  echo "  * Waiting for Subscription setup to finish setup. CSV = $csv ..."
  if ! kubectl wait -n "$ns" --for=jsonpath="{.status.phase}"=Succeeded csv "$csv" --timeout=600s; then
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

# Helper function to wait for CRD to be established
wait_for_crd() {
  local crd="$1"
  local timeout="${2:-60}"  # timeout in seconds
  local interval=2
  local elapsed=0

  echo "⏳ Waiting for CRD ${crd} to appear (timeout: ${timeout}s)…"
  while [ $elapsed -lt $timeout ]; do
    if kubectl get crd "$crd" &>/dev/null; then
      echo "✅ CRD ${crd} detected, waiting for it to become Established..."
      if kubectl wait --for=condition=Established --timeout="${timeout}s" "crd/$crd" 2>/dev/null; then
        return 0
      else
        echo "❌ CRD ${crd} failed to become Established" >&2
        return 1
      fi
    fi
    sleep $interval
    elapsed=$((elapsed + interval))
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
  local elapsed=0
  local last_status_print=0

  echo "⏳ Waiting for CSV ${csv_name} to succeed (timeout: ${timeout}s)..."
  while [ $elapsed -lt $timeout ]; do
    local phase=$(kubectl get csv -n "$namespace" "$csv_name" -o jsonpath='{.status.phase}' 2>/dev/null || echo "NotFound")

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
        if [ $((elapsed - last_status_print)) -ge 30 ]; then
          echo "   CSV ${csv_name} status: ${phase} (${elapsed}s elapsed)"
          last_status_print=$elapsed
        fi
        ;;
    esac

    sleep $interval
    elapsed=$((elapsed + interval))
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

    local kserve_state kserve_ready maas_ready
    kserve_state=$(echo "$dsc_json" | jq -r '.status.components.kserve.managementState // ""')
    kserve_ready=$(echo "$dsc_json" | jq -r '.status.conditions[]? | select(.type=="KserveReady") | .status' | tail -n1)
    maas_ready=$(echo "$dsc_json" | jq -r '.status.conditions[]? | select(.type=="ModelsAsServiceReady") | .status' | tail -n1)

    if [[ "$kserve_state" == "Managed" && "$kserve_ready" == "True" && "$maas_ready" == "True" ]]; then
      echo "  * KServe and ModelsAsService are ready in DataScienceCluster '$name'"
      return 0
    else
      echo "  - KServe state: $kserve_state, KserveReady: $kserve_ready, ModelsAsServiceReady: $maas_ready"
    fi

    sleep $interval
    elapsed=$((elapsed + interval))
  done

  echo "  ERROR: KServe and/or ModelsAsService did not become ready in DataScienceCluster/$name within $timeout seconds."
  return 1
}

# wait_authorino_ready [timeout]
#   Waits for Authorino to be ready and accepting requests.
#   Note: Request are required because authorino will report ready status but still give 500 errors.
#   
#   This checks:
#   1. Authorino CR status is Ready
#   2. Auth service cluster is healthy in gateway's Envoy
#   3. Auth requests are actually succeeding (not erroring)
#
# Arguments:
#   timeout - Timeout in seconds (default: 120)
#
# Returns:
#   0 on success, 1 on failure
wait_authorino_ready() {
  local timeout=${1:-120}
  local interval=5
  local elapsed=0

  echo "* Waiting for Authorino to be ready (timeout: ${timeout}s)..."

  # First, wait for Authorino CR to be ready
  echo "  - Checking Authorino CR status..."
  while [[ $elapsed -lt $timeout ]]; do
    local authorino_ready
    authorino_ready=$(kubectl get authorino -n kuadrant-system -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
    
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

  # Then, wait for the auth service cluster to be healthy in the gateway
  echo "  - Checking auth service cluster health in gateway..."
  local gateway_pod
  gateway_pod=$(kubectl get pods -n openshift-ingress -l gateway.networking.k8s.io/gateway-name=maas-default-gateway -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
  
  if [[ -z "$gateway_pod" ]]; then
    echo "  WARNING: Could not find gateway pod, skipping cluster health check"
    return 0
  fi

  # Wait for auth cluster to show healthy
  while [[ $elapsed -lt $timeout ]]; do
    local health_status
    health_status=$(kubectl exec -n openshift-ingress "$gateway_pod" -- pilot-agent request GET /clusters 2>/dev/null | grep "kuadrant-auth-service" | grep "health_flags" | head -1 || echo "")
    
    if [[ "$health_status" == *"healthy"* ]]; then
      echo "  * Auth service cluster is healthy in gateway"
      break
    fi
    
    echo "  - Auth service cluster not healthy yet, waiting..."
    sleep $interval
    elapsed=$((elapsed + interval))
  done

  # Finally, verify auth requests are actually succeeding (not just cluster marked healthy)
  echo "  - Verifying auth requests are succeeding..."
  local cluster_domain
  cluster_domain=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null || echo "")
  
  if [[ -z "$cluster_domain" ]]; then
    echo "  WARNING: Could not determine cluster domain, skipping request verification"
    return 0
  fi

  local maas_url="https://maas.${cluster_domain}/maas-api/health"
  local consecutive_success=0
  local required_success=3

  while [[ $elapsed -lt $timeout ]]; do
    # Make a test request - we expect 401 (auth working) not 500 (auth failing)
    local http_code
    http_code=$(curl -sSk -o /dev/null -w "%{http_code}" "$maas_url" 2>/dev/null || echo "000")
    
    if [[ "$http_code" == "401" || "$http_code" == "200" ]]; then
      consecutive_success=$((consecutive_success + 1))
      echo "  - Auth request succeeded (HTTP $http_code) [$consecutive_success/$required_success]"
      
      if [[ $consecutive_success -ge $required_success ]]; then
        echo "  * Auth requests verified working"
        return 0
      fi
    else
      consecutive_success=0
      echo "  - Auth request returned HTTP $http_code, waiting for stabilization..."
    fi
    
    sleep 2
    elapsed=$((elapsed + 2))
  done

  echo "  WARNING: Auth request verification timed out, continuing anyway"
  return 0
}
