#!/usr/bin/env bash

# verify-rbac-aggregation.sh
#
# PURPOSE:
#   Manual validation helper for platform administrators to verify that MaaS RBAC
#   aggregation is correctly configured after deployment.
#
# USAGE:
#   ./scripts/verify-rbac-aggregation.sh
#
# REQUIREMENTS:
#   - Kubernetes cluster with MaaS deployed
#   - kubectl configured with cluster-admin permissions
#   - jq command-line JSON processor
#   - ClusterRoles must be created (maas-owner-role, maas-user-view-role)
#
# WHAT IT CHECKS:
#   1. Aggregated ClusterRoles exist (maas-owner-role, maas-user-view-role)
#   2. ClusterRoles have correct aggregation labels
#   3. Built-in admin/edit/view roles include MaaS permissions via aggregation
#   4. Correct verbs are assigned to each role (create/delete for admin, read-only for view)
#
# WHEN TO USE:
#   - After initial MaaS deployment
#   - When troubleshooting namespace user permission issues
#   - After MaaS upgrades to verify RBAC configuration
#
# NOT USED IN CI/CD:
#   This is a manual diagnostic tool. CI validates manifests via validate-manifests.sh,
#   but runtime cluster state validation requires a live deployment and is done manually.

set -euo pipefail

# Color output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Test results
PASSED=0
FAILED=0

log_info() {
    echo -e "${BLUE}ℹ${NC} $*"
}

log_success() {
    echo -e "${GREEN}✓${NC} $*"
    ((PASSED++)) || true
}

log_error() {
    echo -e "${RED}✗${NC} $*"
    ((FAILED++)) || true
}

log_warning() {
    echo -e "${YELLOW}⚠${NC} $*"
}

echo "=========================================="
echo "MaaS RBAC Aggregation Verification"
echo "=========================================="
echo ""

# Verify jq is installed
if ! command -v jq &>/dev/null; then
    echo -e "${RED}✗${NC} jq is not installed. This script requires jq for precise RBAC verification."
    echo "  Install jq: https://jqlang.github.io/jq/download/"
    exit 1
fi

# Check 1: Verify aggregated ClusterRoles exist
log_info "Checking for aggregated ClusterRoles..."

if kubectl get clusterrole maas-owner-role &>/dev/null; then
    log_success "ClusterRole 'maas-owner-role' exists"
else
    log_error "ClusterRole 'maas-owner-role' not found"
fi

if kubectl get clusterrole maas-user-view-role &>/dev/null; then
    log_success "ClusterRole 'maas-user-view-role' exists"
else
    log_error "ClusterRole 'maas-user-view-role' not found"
fi

echo ""

# Check 2: Verify aggregation labels on maas-owner-role
log_info "Checking aggregation labels on maas-owner-role..."

AGGREGATE_TO_ADMIN=$(kubectl get clusterrole maas-owner-role -o jsonpath='{.metadata.labels.rbac\.authorization\.k8s\.io/aggregate-to-admin}' 2>/dev/null || echo "")
if [ "$AGGREGATE_TO_ADMIN" = "true" ]; then
    log_success "maas-owner-role has 'aggregate-to-admin: true' label"
else
    log_error "maas-owner-role missing 'aggregate-to-admin: true' label"
fi

AGGREGATE_TO_EDIT=$(kubectl get clusterrole maas-owner-role -o jsonpath='{.metadata.labels.rbac\.authorization\.k8s\.io/aggregate-to-edit}' 2>/dev/null || echo "")
if [ "$AGGREGATE_TO_EDIT" = "true" ]; then
    log_success "maas-owner-role has 'aggregate-to-edit: true' label"
else
    log_error "maas-owner-role missing 'aggregate-to-edit: true' label"
fi

echo ""

# Check 3: Verify aggregation labels on maas-user-view-role
log_info "Checking aggregation labels on maas-user-view-role..."

AGGREGATE_TO_VIEW=$(kubectl get clusterrole maas-user-view-role -o jsonpath='{.metadata.labels.rbac\.authorization\.k8s\.io/aggregate-to-view}' 2>/dev/null || echo "")
if [ "$AGGREGATE_TO_VIEW" = "true" ]; then
    log_success "maas-user-view-role has 'aggregate-to-view: true' label"
else
    log_error "maas-user-view-role missing 'aggregate-to-view: true' label"
fi

AGGREGATE_TO_ADMIN=$(kubectl get clusterrole maas-user-view-role -o jsonpath='{.metadata.labels.rbac\.authorization\.k8s\.io/aggregate-to-admin}' 2>/dev/null || echo "")
if [ "$AGGREGATE_TO_ADMIN" = "true" ]; then
    log_success "maas-user-view-role has 'aggregate-to-admin: true' label"
else
    log_error "maas-user-view-role missing 'aggregate-to-admin: true' label"
fi

AGGREGATE_TO_EDIT=$(kubectl get clusterrole maas-user-view-role -o jsonpath='{.metadata.labels.rbac\.authorization\.k8s\.io/aggregate-to-edit}' 2>/dev/null || echo "")
if [ "$AGGREGATE_TO_EDIT" = "true" ]; then
    log_success "maas-user-view-role has 'aggregate-to-edit: true' label"
else
    log_error "maas-user-view-role missing 'aggregate-to-edit: true' label"
fi

echo ""

# Check 4: Verify built-in admin role includes MaaS permissions
log_info "Checking if 'admin' ClusterRole includes MaaS permissions..."

ADMIN_RULES=$(kubectl get clusterrole admin -o yaml 2>/dev/null || echo "")

if echo "$ADMIN_RULES" | grep -q "maas.opendatahub.io"; then
    log_success "'admin' ClusterRole includes maas.opendatahub.io API group"

    # Check for specific resources - fail if missing
    if echo "$ADMIN_RULES" | grep -A5 "maas.opendatahub.io" | grep -q "maasmodelrefs"; then
        log_success "'admin' ClusterRole includes maasmodelrefs resource"
    else
        log_error "'admin' ClusterRole missing required maasmodelrefs resource"
    fi

    if echo "$ADMIN_RULES" | grep -A5 "maas.opendatahub.io" | grep -q "externalmodels"; then
        log_success "'admin' ClusterRole includes externalmodels resource"
    else
        log_error "'admin' ClusterRole missing required externalmodels resource"
    fi
else
    log_error "'admin' ClusterRole does not include maas.opendatahub.io API group"
    log_warning "RBAC aggregation may take a few seconds after ClusterRole creation"
fi

echo ""

# Check 5: Verify built-in edit role includes MaaS permissions
log_info "Checking if 'edit' ClusterRole includes MaaS permissions..."

EDIT_RULES=$(kubectl get clusterrole edit -o yaml 2>/dev/null || echo "")

if echo "$EDIT_RULES" | grep -q "maas.opendatahub.io"; then
    log_success "'edit' ClusterRole includes maas.opendatahub.io API group"

    # Check for specific resources - fail if missing
    if echo "$EDIT_RULES" | grep -A5 "maas.opendatahub.io" | grep -q "maasmodelrefs"; then
        log_success "'edit' ClusterRole includes maasmodelrefs resource"
    else
        log_error "'edit' ClusterRole missing required maasmodelrefs resource"
    fi

    if echo "$EDIT_RULES" | grep -A5 "maas.opendatahub.io" | grep -q "externalmodels"; then
        log_success "'edit' ClusterRole includes externalmodels resource"
    else
        log_error "'edit' ClusterRole missing required externalmodels resource"
    fi
else
    log_error "'edit' ClusterRole does not include maas.opendatahub.io API group"
    log_warning "RBAC aggregation may take a few seconds after ClusterRole creation"
fi

echo ""

# Check 6: Verify built-in view role includes MaaS permissions
log_info "Checking if 'view' ClusterRole includes MaaS permissions..."

VIEW_RULES=$(kubectl get clusterrole view -o yaml 2>/dev/null || echo "")

if echo "$VIEW_RULES" | grep -q "maas.opendatahub.io"; then
    log_success "'view' ClusterRole includes maas.opendatahub.io API group"

    # Check for specific resources - fail if missing
    if echo "$VIEW_RULES" | grep -A5 "maas.opendatahub.io" | grep -q "maasmodelrefs"; then
        log_success "'view' ClusterRole includes maasmodelrefs resource"
    else
        log_error "'view' ClusterRole missing required maasmodelrefs resource"
    fi

    if echo "$VIEW_RULES" | grep -A5 "maas.opendatahub.io" | grep -q "externalmodels"; then
        log_success "'view' ClusterRole includes externalmodels resource"
    else
        log_error "'view' ClusterRole missing required externalmodels resource"
    fi
else
    log_error "'view' ClusterRole does not include maas.opendatahub.io API group"
    log_warning "RBAC aggregation may take a few seconds after ClusterRole creation"
fi

echo ""

# Check 7: Verify correct verbs for admin role
log_info "Checking verbs for 'admin' ClusterRole MaaS permissions..."

# Extract verbs only from the MaaS rule using jq
ADMIN_VERBS=$(kubectl get clusterrole admin -o json 2>/dev/null | jq -r '.rules[] | select(.apiGroups[]? == "maas.opendatahub.io") | .verbs[]' 2>/dev/null || echo "")

EXPECTED_VERBS=("create" "delete" "get" "list" "patch" "update" "watch")
for verb in "${EXPECTED_VERBS[@]}"; do
    if echo "$ADMIN_VERBS" | grep -Fx "$verb" >/dev/null; then
        log_success "'admin' role has '$verb' verb for MaaS resources"
    else
        log_error "'admin' role missing required '$verb' verb for MaaS resources"
    fi
done

echo ""

# Check 8: Verify correct verbs for view role (read-only)
log_info "Checking verbs for 'view' ClusterRole MaaS permissions..."

# Extract verbs only from the MaaS rule using jq
VIEW_VERBS=$(kubectl get clusterrole view -o json 2>/dev/null | jq -r '.rules[] | select(.apiGroups[]? == "maas.opendatahub.io") | .verbs[]' 2>/dev/null || echo "")

READ_VERBS=("get" "list" "watch")
for verb in "${READ_VERBS[@]}"; do
    if echo "$VIEW_VERBS" | grep -Fx "$verb" >/dev/null; then
        log_success "'view' role has '$verb' verb for MaaS resources"
    else
        log_error "'view' role missing required '$verb' verb for MaaS resources"
    fi
done

# Ensure view role doesn't have write verbs
WRITE_VERBS=("create" "delete" "patch" "update")
for verb in "${WRITE_VERBS[@]}"; do
    if echo "$VIEW_VERBS" | grep -Fx "$verb" >/dev/null; then
        log_error "'view' role incorrectly has '$verb' verb (should be read-only)"
    fi
done

echo ""
echo "=========================================="
echo "Summary"
echo "=========================================="
echo -e "${GREEN}Passed:${NC} $PASSED"
echo -e "${RED}Failed:${NC} $FAILED"
echo ""

if [[ $FAILED -eq 0 ]]; then
    echo -e "${GREEN}✓ All RBAC aggregation checks passed!${NC}"
    echo ""
    echo "Next steps:"
    echo "  1. Grant namespace users 'admin' or 'edit' role to enable MaaSModelRef creation"
    echo "  2. Grant namespace users 'view' role for read-only access"
    echo ""
    echo "Example: Grant admin role to a user in namespace 'my-models'"
    echo "  kubectl create rolebinding my-models-admin \\"
    echo "    --clusterrole=admin \\"
    echo "    --user=user@example.com \\"
    echo "    -n my-models"
    exit 0
else
    echo -e "${RED}✗ Some RBAC aggregation checks failed${NC}"
    echo ""
    echo "Troubleshooting:"
    echo "  1. Verify MaaS controller is deployed: kubectl get deployment maas-controller -n opendatahub"
    echo "  2. Check ClusterRole definitions: kubectl get clusterrole | grep maas-user"
    echo "  3. Wait a few seconds for RBAC aggregation to propagate"
    echo "  4. Check for RBAC controller errors: kubectl logs -n kube-system -l component=kube-controller-manager"
    exit 1
fi
