#!/bin/bash
################################################################################
# MaaS Cluster Diagnostic Script
#
# Scans the cluster for MaaS-related components and displays current state.
# Useful for:
# - Understanding what's already deployed before running deploy.sh
# - Troubleshooting deployment issues
# - Verifying idempotent behavior
#
# USAGE:
#   ./scripts/diagnose-cluster.sh [--verbose]
#
# OPTIONS:
#   --verbose   Show detailed debug information
#
################################################################################

set -euo pipefail

# Source helpers
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deployment-helpers.sh
source "${SCRIPT_DIR}/deployment-helpers.sh"

# Default: auto-detect deployment mode
DEPLOYMENT_MODE=""

# Parse arguments
while [[ $# -gt 0 ]]; do
  case $1 in
    --verbose)
      CURRENT_LOG_LEVEL=$LOG_LEVEL_DEBUG
      shift
      ;;
    --deployment-mode)
      if [[ -z "${2:-}" ]]; then
        echo "Error: --deployment-mode requires a value (operator or kustomize)"
        exit 1
      fi
      DEPLOYMENT_MODE="$2"
      DEPLOYMENT_MODE_EXPLICIT=true
      shift 2
      ;;
    --help|-h)
      echo "Usage: $0 [--verbose] [--deployment-mode <operator|kustomize>]"
      echo ""
      echo "Scans cluster for MaaS components and displays deployment state."
      echo ""
      echo "Options:"
      echo "  --verbose                     Show detailed debug information"
      echo "  --deployment-mode <mode>      Validate DSC for this mode (operator or kustomize)"
      echo "                                Default: auto-detected from cluster state"
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      echo "Use --help for usage information" >&2
      exit 1
      ;;
  esac
done

################################################################################
# Helper Functions
################################################################################

# format_status
#   Formats a component status with emoji and color
format_status() {
  local status=$1
  case "$status" in
    ready|programmed|true)
      echo "✓ Ready"
      ;;
    exists)
      echo "⚠ Exists (not ready)"
      ;;
    missing|false)
      echo "✗ Not found"
      ;;
    *)
      echo "$status"
      ;;
  esac
}

# format_component
#   Formats component display with validation status
format_component() {
  local name=$1
  local status=$2
  local details=${3:-}

  if [[ "$status" == "missing" || "$status" == "false" ]]; then
    echo "Not detected"
  elif [[ -n "$details" ]]; then
    echo "$name ($details)"
  else
    echo "$name"
  fi
}

################################################################################
# Main Diagnostic Logic
################################################################################

echo ""
echo "═══════════════════════════════════════════════════════════"
echo "  🔍 MaaS CLUSTER DIAGNOSTICS"
echo "═══════════════════════════════════════════════════════════"
echo ""

# Check cluster connection
if ! kubectl cluster-info &>/dev/null; then
  echo "✗ ERROR: Not connected to a cluster" >&2
  echo "" >&2
  echo "Please run 'oc login' or configure kubectl first." >&2
  exit 1
fi

cluster_name=$(kubectl config current-context 2>/dev/null || echo "unknown")
echo "Connected to cluster: $cluster_name"
echo ""

echo "Scanning cluster components..."
echo ""

# Detection phase
operator_type=$(detect_operator_type)
policy_engine=$(detect_policy_engine)
dsc_name=$(detect_dsc 2>/dev/null)

# Determine target namespace
if [[ -n "$operator_type" ]]; then
  case "$operator_type" in
    rhoai)
      target_namespace="redhat-ods-applications"
      ;;
    odh)
      target_namespace="opendatahub"
      ;;
  esac
else
  target_namespace="opendatahub"
fi

# Detect components in target namespace
postgres_status=$(detect_postgresql "$target_namespace")
gateway_status=$(detect_gateway)
maas_status=$(detect_maas_deployments "$target_namespace")
maas_api_status=$(echo "$maas_status" | jq -r '.api' 2>/dev/null || echo "missing")
maas_controller_status=$(echo "$maas_status" | jq -r '.controller' 2>/dev/null || echo "missing")

# Resolve deployment mode for DSC validation
# If not explicitly set, auto-detect: if MaaS components exist but modelsAsService is Removed,
# it's likely kustomize mode. Otherwise assume operator mode.
if [[ -z "$DEPLOYMENT_MODE" ]]; then
  if [[ -n "$dsc_name" ]]; then
    local_maas_state=$(kubectl get datasciencecluster "$dsc_name" -o jsonpath='{.spec.components.kserve.modelsAsService.managementState}' 2>/dev/null || echo "")
    if [[ "$local_maas_state" == "Removed" ]] && [[ "$maas_api_status" != "missing" || "$maas_controller_status" != "missing" ]]; then
      DEPLOYMENT_MODE="kustomize"
    else
      DEPLOYMENT_MODE="operator"
    fi
  else
    DEPLOYMENT_MODE="kustomize"
  fi
fi

# Select the right DSC manifest based on deployment mode
dsc_manifest=""
if [[ "$DEPLOYMENT_MODE" == "kustomize" ]]; then
  dsc_manifest="${SCRIPT_DIR}/data/datasciencecluster-kustomize.yaml"
fi

# DSC validation (if exists)
dsc_validated="N/A"
dsc_validation_errors=""
if [[ -n "$dsc_name" ]]; then
  if validation_errors=$(validate_dsc_for_maas "$dsc_name" "$dsc_manifest" 2>&1); then
    dsc_validated="true"
  else
    dsc_validated="false"
    dsc_validation_errors="$validation_errors"
  fi
fi

# Policy engine details
kuadrant_status="N/A"
csv_gateway_patch="N/A"
if [[ -n "$policy_engine" ]]; then
  kuadrant_status=$(detect_kuadrant_cr "$policy_engine")
  if detect_csv_gateway_patch "$policy_engine"; then
    csv_gateway_patch="true"
  else
    csv_gateway_patch="false"
  fi
fi

# Display results
echo "───────────────────────────────────────────────────────────"
echo "  DETECTED COMPONENTS"
echo "───────────────────────────────────────────────────────────"
echo ""

# Format display values
operator_upper=$(echo "$operator_type" | tr '[:lower:]' '[:upper:]')
policy_engine_upper=$(echo "$policy_engine" | tr '[:lower:]' '[:upper:]')
operator_display=$(format_component "$operator_upper" "${operator_type:-missing}")
policy_engine_display=$(format_component "$policy_engine_upper" "${policy_engine:-missing}")
# Format DSC validation status (don't use format_status - "false" would show "Not found")
case "$dsc_validated" in
  true)  dsc_validation_display="✓ Valid" ;;
  false) dsc_validation_display="✗ Invalid" ;;
  *)     dsc_validation_display="" ;;
esac
dsc_display=$(format_component "$dsc_name" "${dsc_name:-missing}" "$dsc_validation_display")
postgres_display=$(format_status "$postgres_status")
gateway_display=$(format_status "$gateway_status")
kuadrant_display=$(format_status "$kuadrant_status")
csv_patch_display=$(format_status "$csv_gateway_patch")
api_display=$(format_status "$maas_api_status")
controller_display=$(format_status "$maas_controller_status")

if [[ -n "${DEPLOYMENT_MODE_EXPLICIT:-}" ]]; then
  printf "  %-32s → %s\n" "Deployment Mode" "$DEPLOYMENT_MODE"
else
  printf "  %-32s → %s (auto-detected)\n" "Deployment Mode" "$DEPLOYMENT_MODE"
fi
printf "  %-32s → %s\n" "Operator Type" "$operator_display"
printf "  %-32s → %s\n" "Target Namespace" "$target_namespace"
printf "  %-32s → %s\n" "Policy Engine" "$policy_engine_display"
printf "  %-32s → %s\n" "  └─ Kuadrant CR" "$kuadrant_display"
printf "  %-32s → %s\n" "  └─ CSV Gateway Patch" "$csv_patch_display"
printf "  %-32s → %s\n" "DataScienceCluster" "$dsc_display"
printf "  %-32s → %s\n" "PostgreSQL" "$postgres_display"
printf "  %-32s → %s\n" "Gateway (openshift-ingress)" "$gateway_display"
printf "  %-32s → %s\n" "MaaS API" "$api_display"
printf "  %-32s → %s\n" "MaaS Controller" "$controller_display"

echo ""

# Show DSC validation errors if any
if [[ "$dsc_validated" == "false" ]]; then
  echo "───────────────────────────────────────────────────────────"
  echo "  ⚠ DSC VALIDATION ERRORS"
  echo "───────────────────────────────────────────────────────────"
  echo ""
  while IFS= read -r line || [[ -n "$line" ]]; do
    echo "  • $line"
  done <<< "$dsc_validation_errors"
  echo ""
fi

# Deployment recommendations
echo "───────────────────────────────────────────────────────────"
echo "  📋 DEPLOYMENT ANALYSIS"
echo "───────────────────────────────────────────────────────────"
echo ""

# Determine deployment scenario
if [[ -z "$operator_type" && -z "$policy_engine" && "$postgres_status" == "missing" \
      && "$maas_api_status" == "missing" && "$maas_controller_status" == "missing" ]]; then
  echo "Scenario: Greenfield Install"
  echo ""
  echo "No MaaS components detected. Running deploy.sh will:"
  echo "  → Install policy engine (Kuadrant)"
  echo "  → Deploy PostgreSQL"
  echo "  → Deploy MaaS API and Controller"
  echo "  → Create Gateway resources"
  echo ""
  echo "Recommended command:"
  echo "  ./scripts/deploy.sh --deployment-mode kustomize"
  echo ""

elif [[ "$dsc_validated" == "false" ]]; then
  echo "Scenario: Incompatible DSC Detected"
  echo ""
  echo "DataScienceCluster exists but does not meet MaaS requirements."
  echo "Deployment will FAIL unless DSC is fixed."
  echo ""

  # Offer to patch if running interactively
  if [[ -t 0 ]]; then
    printf "  Apply required patches to '%s' automatically? [y/N] " "$dsc_name"
    read -r answer
    if [[ "$answer" =~ ^[Yy]$ ]]; then
      echo ""
      echo "  → Patching DataScienceCluster '$dsc_name'..."
      if patch_dsc_for_maas "$dsc_name" "$dsc_manifest"; then
        echo "  ✓ DataScienceCluster patched and validated"
        dsc_validated="true"
      else
        echo "  ✗ Failed to patch DataScienceCluster"
      fi
      echo ""
    else
      echo ""
      echo "Action required:"
      echo "  1. Edit the DataScienceCluster:"
      echo "     kubectl edit datasciencecluster \"$dsc_name\""
      echo ""
      echo "  2. Fix the configuration mismatches shown above"
      echo ""
      echo "  3. Re-run diagnostics to verify:"
      echo "     ./scripts/diagnose-cluster.sh"
      echo ""
    fi
  else
    echo "Action required:"
    echo "  1. Edit the DataScienceCluster:"
    echo "     kubectl edit datasciencecluster \"$dsc_name\""
    echo ""
    echo "  2. Fix the configuration mismatches shown above"
    echo ""
    echo "  3. Re-run diagnostics to verify:"
    echo "     ./scripts/diagnose-cluster.sh"
    echo ""
  fi

elif [[ -n "$operator_type" && -n "$policy_engine" ]] \
     && [[ "$maas_api_status" != "missing" || "$maas_controller_status" != "missing" || "$postgres_status" != "missing" ]]; then
  echo "Scenario: Existing Deployment Detected"
  echo ""
  echo "MaaS components already installed. Running deploy.sh will:"

  # Detailed component-by-component analysis
  if [[ "$policy_engine" == "rhcl" || "$policy_engine" == "kuadrant" ]]; then
    if [[ "$kuadrant_status" == "ready" && "$csv_gateway_patch" == "true" ]]; then
      printf "  ✓ %-28s %-8s %s\n" "Policy Engine" "SKIP" "Already healthy"
    else
      printf "  ⚠ %-28s %-8s %s\n" "Policy Engine" "VERIFY" "Check configuration"
    fi
  fi

  if [[ "$postgres_status" == "ready" ]]; then
    printf "  ✓ %-28s %-8s %s\n" "PostgreSQL" "SKIP" "Already deployed"
  elif [[ "$postgres_status" == "exists" ]]; then
    printf "  ⚠ %-28s %-8s %s\n" "PostgreSQL" "VERIFY" "Not ready"
  else
    printf "  → %-28s %-8s %s\n" "PostgreSQL" "APPLY" "Will install"
  fi

  if [[ "$maas_api_status" == "ready" && "$maas_controller_status" == "ready" ]]; then
    printf "  ✓ %-28s %-8s %s\n" "MaaS Platform" "SKIP" "Already deployed"
  else
    printf "  → %-28s %-8s %s\n" "MaaS Platform" "APPLY" "Will deploy/update"
  fi

  if [[ "$gateway_status" == "programmed" ]]; then
    printf "  ✓ %-28s %-8s %s\n" "Gateway" "SKIP" "Already configured"
  else
    printf "  → %-28s %-8s %s\n" "Gateway" "APPLY" "Will create/update"
  fi

  echo ""
  echo "This is an idempotent re-run. Most components will be skipped."
  echo ""
  echo "Recommended command:"
  echo "  ./scripts/deploy.sh --deployment-mode kustomize"
  echo ""

else
  echo "Scenario: Partial Installation"
  echo ""
  echo "Some components detected. Running deploy.sh will:"
  echo "  → Install missing components"
  echo "  → Verify existing components"
  echo ""
  echo "Recommended command:"
  echo "  ./scripts/deploy.sh --deployment-mode kustomize"
  echo ""
fi

echo "═══════════════════════════════════════════════════════════"
echo ""

# Exit code: 0 if ready to deploy, 1 if conflicts detected
if [[ "$dsc_validated" == "false" ]]; then
  exit 1
fi

exit 0
