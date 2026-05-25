#!/bin/bash

# Cleanup script: Remove old tier-based resources after migrating to subscription model
#
# This script removes legacy tier resources that conflict with the new MaaS
# subscription architecture. Run it AFTER validating that the new subscription
# CRs are working correctly (see docs/content/migration/tier-to-subscription.md).
#
# Safe to run multiple times — all operations are idempotent.

set -euo pipefail

# Default values
MODEL_NAMESPACE="llm"
DRY_RUN=false
VERBOSE=false

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${BLUE}ℹ️  ${NC}$1"
}

log_success() {
    echo -e "${GREEN}✅ ${NC}$1"
}

log_warn() {
    echo -e "${YELLOW}⚠️  ${NC}$1"
}

log_error() {
    echo -e "${RED}❌ ${NC}$1"
}

log_verbose() {
    if [[ "$VERBOSE" == "true" ]]; then
        echo -e "${BLUE}   ${NC}$1"
    fi
}

usage() {
    cat <<EOF
Usage: $0 [OPTIONS]

Remove old tier-based resources after migrating to the MaaS subscription model.

This script is Phase 4 of the tier-to-subscription migration. Run it only AFTER:
  1. Creating subscription CRs (migrate-tier-to-subscription.sh)
  2. Validating that the new system works correctly

Resources removed:
  - AuthPolicy 'gateway-auth-policy' in openshift-ingress
  - TokenRateLimitPolicy 'gateway-tier-rate-limits' in openshift-ingress
  - ConfigMap 'tier-to-group-mapping' in maas-api
  - 'alpha.maas.opendatahub.io/tiers' annotation from LLMInferenceServices

OPTIONS:
    --model-ns <ns>     Model namespace (default: llm)
    --dry-run           Show what would be removed without making changes
    --verbose           Enable verbose logging
    --help              Show this help message

EXAMPLES:
    # Preview what will be removed
    $0 --dry-run --verbose

    # Remove old tier resources
    $0 --verbose

    # Remove tier resources with custom model namespace
    $0 --model-ns my-models --verbose

EOF
}

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --model-ns)
            if [[ -z "${2:-}" ]] || [[ "${2:-}" == --* ]]; then
                log_error "Option $1 requires a value"
                usage
                exit 1
            fi
            MODEL_NAMESPACE="$2"
            shift 2
            ;;
        --dry-run)
            DRY_RUN=true
            shift
            ;;
        --verbose)
            VERBOSE=true
            shift
            ;;
        --help)
            usage
            exit 0
            ;;
        *)
            log_error "Unknown option: $1"
            usage
            exit 1
            ;;
    esac
done

# Check kubectl is available
if ! command -v kubectl &> /dev/null; then
    log_error "kubectl not found. Cannot proceed."
    exit 1
fi

log_info "Cleaning up old tier-based resources..."
if [[ "$DRY_RUN" == "true" ]]; then
    log_warn "DRY-RUN mode: no changes will be made"
fi
echo ""

cleanup_failed=false

# delete_resource: delete a resource using kubectl delete --ignore-not-found.
# Avoids get-then-delete TOCTOU and surfaces real errors (RBAC, network).
# Usage: delete_resource <kind> <name> <namespace>
delete_resource() {
    local kind="$1" name="$2" namespace="$3"

    if [[ "$DRY_RUN" == "true" ]]; then
        local output
        if output=$(kubectl get "$kind" "$name" -n "$namespace" 2>&1); then
            log_warn "[dry-run] Would delete $kind $name in $namespace"
        elif [[ "$output" == *"NotFound"* ]] || [[ "$output" == *"not found"* ]]; then
            log_verbose "$kind $name not found in $namespace (already clean)"
        else
            log_error "Failed to check $kind $name in $namespace: $(echo "$output" | grep '^error:' | tail -1)"
            cleanup_failed=true
        fi
    else
        local output
        if output=$(kubectl delete "$kind" "$name" -n "$namespace" --ignore-not-found 2>&1); then
            if [[ -n "$output" ]]; then
                log_success "Deleted $kind $name in $namespace"
            else
                log_verbose "$kind $name not found in $namespace (already clean)"
            fi
        else
            log_error "Failed to delete $kind $name in $namespace: $(echo "$output" | grep '^error:' | tail -1)"
            cleanup_failed=true
        fi
    fi
}

# 1. Delete gateway-auth-policy AuthPolicy
delete_resource authpolicy gateway-auth-policy openshift-ingress

# 2. Delete gateway-tier-rate-limits TokenRateLimitPolicy
delete_resource tokenratelimitpolicy gateway-tier-rate-limits openshift-ingress

# 3. Delete tier-to-group-mapping ConfigMap
delete_resource configmap tier-to-group-mapping maas-api

# 4. Remove alpha.maas.opendatahub.io/tiers annotation from LLMInferenceServices
log_info "Removing tier annotations from LLMInferenceServices in $MODEL_NAMESPACE..."
local_models_output=$(kubectl get llminferenceservice -n "$MODEL_NAMESPACE" -o name 2>&1) || true
models=""

if [[ "$local_models_output" == *"NotFound"* ]] || [[ "$local_models_output" == *"the server doesn't have a resource type"* ]] || [[ -z "$local_models_output" ]]; then
    log_verbose "No LLMInferenceServices found in $MODEL_NAMESPACE (nothing to clean)"
elif [[ "$local_models_output" == *"error"* ]] || [[ "$local_models_output" == *"Error"* ]]; then
    log_error "Failed to list LLMInferenceServices in $MODEL_NAMESPACE: $(echo "$local_models_output" | grep '^error:' | tail -1)"
    cleanup_failed=true
else
    models="$local_models_output"
fi

if [[ -n "$models" ]]; then
    while IFS= read -r model; do
        [[ -z "$model" ]] && continue
        if [[ "$DRY_RUN" == "true" ]]; then
            local has_annotation
            if has_annotation=$(kubectl get "$model" -n "$MODEL_NAMESPACE" -o jsonpath='{.metadata.annotations.alpha\.maas\.opendatahub\.io/tiers}' 2>&1); then
                if [[ -n "$has_annotation" ]]; then
                    log_warn "[dry-run] Would remove tier annotation from $model"
                else
                    log_verbose "$model has no tier annotation (already clean)"
                fi
            else
                log_error "Failed to check annotation on $model: $(echo "$has_annotation" | grep '^error:' | tail -1)"
                cleanup_failed=true
            fi
        else
            local annotate_output
            if annotate_output=$(kubectl annotate "$model" -n "$MODEL_NAMESPACE" alpha.maas.opendatahub.io/tiers- --ignore-not-found 2>&1); then
                log_verbose "Removed tier annotation from $model"
            else
                log_error "Failed to remove tier annotation from $model: $(echo "$annotate_output" | grep '^error:' | tail -1)"
                cleanup_failed=true
            fi
        fi
    done <<< "$models"
fi

echo ""
if [[ "$cleanup_failed" == "true" ]]; then
    log_error "Some cleanup steps failed. Review errors above."
    exit 1
fi

if [[ "$DRY_RUN" == "true" ]]; then
    log_success "Dry-run complete. Run without --dry-run to apply changes."
else
    log_success "Tier resource cleanup completed successfully!"
fi
