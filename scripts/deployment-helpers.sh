#!/bin/bash

# Minimum version requirements for operators
export KUADRANT_MIN_VERSION="1.3.1"
export AUTHORINO_MIN_VERSION="0.22.0"
export LIMITADOR_MIN_VERSION="0.16.0"
export DNS_OPERATOR_MIN_VERSION="0.15.0"

# Deployment helper functions for MaaS Platform
# Source this file in deployment scripts: source "$(dirname "$0")/deployment-helpers.sh"

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
#   If MAAS_API_IMAGE is not set, does nothing (uses default from core/kustomization.yaml).
#   Creates a backup and restores it after the calling script exits.
#
# Environment:
#   MAAS_API_IMAGE - Container image to use (e.g., quay.io/opendatahub/maas-api:pr-123)
set_maas_api_image() {
  # Skip if MAAS_API_IMAGE is not set
  if [ -z "${MAAS_API_IMAGE:-}" ]; then
    return 0
  fi

  local project_root="$(find_project_root)"
  local core_kustomization="$project_root/deployment/base/maas-api/core/kustomization.yaml"
  local backup_file="${core_kustomization}.backup"

  echo "   Setting MaaS API image: ${MAAS_API_IMAGE}"
  
  # Backup and set trap to restore on exit (use double quotes to expand paths now)
  cp "$core_kustomization" "$backup_file"
  trap "mv '$backup_file' '$core_kustomization' 2>/dev/null || true" EXIT INT TERM
  
  # Update the image
  (cd "$(dirname "$core_kustomization")" && kustomize edit set image "maas-api=${MAAS_API_IMAGE}")
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
      wait_for_csv "$csv_name" "$namespace" "$timeout"
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

# Helper function to wait for a resource to exist
# Usage: wait_for_resource <resource_type> <resource_name> <namespace> [timeout]
wait_for_resource() {
  local resource_type="$1"
  local resource_name="$2"
  local namespace="$3"
  local timeout="${4:-60}"
  local interval=2
  local elapsed=0

  echo "⏳ Waiting for ${resource_type}/${resource_name} in ${namespace} (timeout: ${timeout}s)..."
  while [ $elapsed -lt $timeout ]; do
    if kubectl get "$resource_type" "$resource_name" -n "$namespace" &>/dev/null; then
      echo "✅ ${resource_type}/${resource_name} exists"
      return 0
    fi
    sleep $interval
    elapsed=$((elapsed + interval))
  done

  echo "⚠️  Timed out waiting for ${resource_type}/${resource_name}" >&2
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

