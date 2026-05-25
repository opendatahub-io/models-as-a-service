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

# 1. Delete gateway-auth-policy AuthPolicy
if kubectl get authpolicy gateway-auth-policy -n openshift-ingress >/dev/null 2>&1; then
    if [[ "$DRY_RUN" == "true" ]]; then
        log_warn "[dry-run] Would delete AuthPolicy gateway-auth-policy in openshift-ingress"
    else
        if kubectl delete authpolicy gateway-auth-policy -n openshift-ingress 2>/dev/null; then
            log_success "Deleted AuthPolicy gateway-auth-policy in openshift-ingress"
        else
            log_error "Failed to delete AuthPolicy gateway-auth-policy in openshift-ingress"
            cleanup_failed=true
        fi
    fi
else
    log_verbose "AuthPolicy gateway-auth-policy not found in openshift-ingress (already clean)"
fi

# 2. Delete gateway-tier-rate-limits TokenRateLimitPolicy
if kubectl get tokenratelimitpolicy gateway-tier-rate-limits -n openshift-ingress >/dev/null 2>&1; then
    if [[ "$DRY_RUN" == "true" ]]; then
        log_warn "[dry-run] Would delete TokenRateLimitPolicy gateway-tier-rate-limits in openshift-ingress"
    else
        if kubectl delete tokenratelimitpolicy gateway-tier-rate-limits -n openshift-ingress 2>/dev/null; then
            log_success "Deleted TokenRateLimitPolicy gateway-tier-rate-limits in openshift-ingress"
        else
            log_error "Failed to delete TokenRateLimitPolicy gateway-tier-rate-limits in openshift-ingress"
            cleanup_failed=true
        fi
    fi
else
    log_verbose "TokenRateLimitPolicy gateway-tier-rate-limits not found in openshift-ingress (already clean)"
fi

# 3. Delete tier-to-group-mapping ConfigMap
if kubectl get configmap tier-to-group-mapping -n maas-api >/dev/null 2>&1; then
    if [[ "$DRY_RUN" == "true" ]]; then
        log_warn "[dry-run] Would delete ConfigMap tier-to-group-mapping in maas-api"
    else
        if kubectl delete configmap tier-to-group-mapping -n maas-api 2>/dev/null; then
            log_success "Deleted ConfigMap tier-to-group-mapping in maas-api"
        else
            log_error "Failed to delete ConfigMap tier-to-group-mapping in maas-api"
            cleanup_failed=true
        fi
    fi
else
    log_verbose "ConfigMap tier-to-group-mapping not found in maas-api (already clean)"
fi

# 4. Remove alpha.maas.opendatahub.io/tiers annotation from LLMInferenceServices
log_info "Removing tier annotations from LLMInferenceServices in $MODEL_NAMESPACE..."
models=$(kubectl get llminferenceservice -n "$MODEL_NAMESPACE" -o name 2>/dev/null) || true

if [[ -z "$models" ]]; then
    log_verbose "No LLMInferenceServices found in $MODEL_NAMESPACE (nothing to clean)"
else
    while IFS= read -r model; do
        [[ -z "$model" ]] && continue
        if [[ "$DRY_RUN" == "true" ]]; then
            has_annotation=$(kubectl get "$model" -n "$MODEL_NAMESPACE" -o jsonpath='{.metadata.annotations.alpha\.maas\.opendatahub\.io/tiers}' 2>/dev/null) || true
            if [[ -n "$has_annotation" ]]; then
                log_warn "[dry-run] Would remove tier annotation from $model"
            else
                log_verbose "$model has no tier annotation (already clean)"
            fi
        else
            if kubectl annotate "$model" -n "$MODEL_NAMESPACE" alpha.maas.opendatahub.io/tiers- --ignore-not-found 2>/dev/null; then
                log_verbose "Removed tier annotation from $model"
            else
                log_error "Failed to remove tier annotation from $model"
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
