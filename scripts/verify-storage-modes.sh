#!/bin/bash
# Verifies all 3 storage modes: In-Memory SQLite, SQLite with PVC, PostgreSQL
# Usage: ./scripts/verify-storage-modes.sh

set -uo pipefail

R='\033[0;31m'
G='\033[0;32m'
Y='\033[1;33m'
B='\033[0;34m'
C='\033[0;36m'
M='\033[0;35m'
W='\033[1;37m'
NC='\033[0m'

NAMESPACE="maas-api"
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

banner() {
    echo ""
    echo -e "${C}╔══════════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${C}║${W}  $1${C}$(printf '%*s' $((62 - ${#1})) '')║${NC}"
    echo -e "${C}╚══════════════════════════════════════════════════════════════════╝${NC}"
}

step() { echo -e "\n${M}▶ $1${NC}"; }
ok() { echo -e "  ${G}✓ $1${NC}"; }
fail() { echo -e "  ${R}✗ $1${NC}"; }
info() { echo -e "  ${B}ℹ $1${NC}"; }

wait_for_api() {
    local timeout=90
    local start=$(date +%s)
    
    while true; do
        local ready=$(kubectl get pod -n "$NAMESPACE" -l app.kubernetes.io/name=maas-api \
            -o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
        [ "$ready" == "True" ] && break
        [ $(($(date +%s) - start)) -gt $timeout ] && return 1
        sleep 2
    done
    
    sleep 3
    while true; do
        local status=$(curl -sSk -o /dev/null -w "%{http_code}" "${GATEWAY_URL}/maas-api/health" 2>/dev/null || echo "000")
        [ "$status" == "401" ] || [ "$status" == "200" ] && return 0
        [ $(($(date +%s) - start)) -gt $timeout ] && return 1
        sleep 2
    done
}

get_token() {
    oc whoami -t 2>/dev/null || cat /var/run/secrets/kubernetes.io/serviceaccount/token 2>/dev/null || true
}

discover_gateway() {
    [ -n "${GATEWAY_URL:-}" ] && return
    local hostname=$(kubectl get gateway maas-default-gateway -n openshift-ingress \
        -o jsonpath='{.spec.listeners[0].hostname}' 2>/dev/null || echo "")
    if [ -z "$hostname" ]; then
        echo -e "${R}Failed to discover gateway. Set GATEWAY_URL manually.${NC}"
        exit 1
    fi
    if curl -sSk -o /dev/null -m 5 "https://${hostname}/maas-api/health" 2>/dev/null; then
        GATEWAY_URL="https://${hostname}"
    else
        GATEWAY_URL="http://${hostname}"
    fi
}

cleanup_all() {
    step "Cleaning up previous deployments..."
    kubectl delete deployment maas-api -n "$NAMESPACE" --ignore-not-found=true --wait=true --timeout=60s 2>/dev/null || true
    kubectl delete pvc maas-api-data -n "$NAMESPACE" --ignore-not-found=true --wait=true --timeout=30s 2>/dev/null || true
    kubectl delete secret database-config -n "$NAMESPACE" --ignore-not-found=true 2>/dev/null || true
    
    # Clean up CloudNativePG cluster
    kubectl delete cluster maas-postgres -n "$NAMESPACE" --ignore-not-found=true --wait=true --timeout=120s 2>/dev/null || true
    
    # Clean up old community CNPG webhooks (if they exist from previous installs, not for everyone but it was an issue in my case)
    kubectl delete mutatingwebhookconfiguration cnpg-mutating-webhook-configuration --ignore-not-found=true 2>/dev/null || true
    kubectl delete validatingwebhookconfiguration cnpg-validating-webhook-configuration --ignore-not-found=true 2>/dev/null || true
    
    local waited=0
    while [ $waited -lt 30 ]; do
        local count=$(kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/name=maas-api --no-headers 2>/dev/null | wc -l || echo "0")
        [ "$count" -eq 0 ] && break
        sleep 2
        waited=$((waited + 2))
    done
    ok "Cleanup complete"
}

deploy_in_memory() {
    step "Deploying In-Memory SQLite mode..."
    kubectl delete deployment maas-api -n "$NAMESPACE" --ignore-not-found=true --wait=true --timeout=60s >/dev/null 2>&1
    kustomize build "${PROJECT_ROOT}/deployment/base/maas-api" | kubectl apply -f - >/dev/null 2>&1
    kubectl rollout status deployment/maas-api -n "$NAMESPACE" --timeout=120s >/dev/null 2>&1
    wait_for_api
    ok "Deployed (ephemeral storage)"
}

deploy_sqlite_pvc() {
    step "Deploying Disk storage mode (persistent volume)..."
    kubectl delete deployment maas-api -n "$NAMESPACE" --ignore-not-found=true --wait=true --timeout=60s >/dev/null 2>&1
    kustomize build "${PROJECT_ROOT}/deployment/overlays/sqlite-pvc" | kubectl apply -f - >/dev/null 2>&1
    kubectl rollout status deployment/maas-api -n "$NAMESPACE" --timeout=120s >/dev/null 2>&1
    wait_for_api
    ok "Deployed (persistent volume)"
}

deploy_postgresql() {
    step "Deploying PostgreSQL mode (CloudNativePG)..."
    
    # Install CloudNativePG operator from OperatorHub if not present
    if ! kubectl get crd clusters.postgresql.cnpg.io >/dev/null 2>&1; then
        info "Installing CloudNativePG operator from OperatorHub..."
        kubectl apply -f - >/dev/null 2>&1 <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: cloudnative-pg
  namespace: openshift-operators
spec:
  channel: stable-v1
  name: cloudnative-pg
  source: certified-operators
  sourceNamespace: openshift-marketplace
EOF
        
        local waited=0
        while [ $waited -lt 180 ]; do
            local phase=$(kubectl get csv -n openshift-operators -l operators.coreos.com/cloudnative-pg.openshift-operators -o jsonpath='{.items[0].status.phase}' 2>/dev/null || echo "")
            [ "$phase" == "Succeeded" ] && break
            
            # Auto-approve install plan if needed
            local plan=$(kubectl get subscription cloudnative-pg -n openshift-operators -o jsonpath='{.status.installPlanRef.name}' 2>/dev/null || echo "")
            if [ -n "$plan" ]; then
                kubectl patch installplan "$plan" -n openshift-operators --type merge -p '{"spec":{"approved":true}}' >/dev/null 2>&1 || true
            fi
            
            sleep 10
            waited=$((waited + 10))
        done
        
        # Verify CRD exists now
        if ! kubectl get crd clusters.postgresql.cnpg.io >/dev/null 2>&1; then
            fail "CloudNativePG operator installation failed"
            return 1
        fi
    fi
    ok "CloudNativePG operator ready"
    
    info "Creating PostgreSQL cluster..."
    kubectl apply -n "$NAMESPACE" -f - >/dev/null 2>&1 <<EOF
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: maas-postgres
spec:
  instances: 1
  storage:
    size: 1Gi
EOF

    local waited=0
    while [ $waited -lt 300 ]; do
        local ready=$(kubectl get cluster maas-postgres -n "$NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        [ "$ready" == "Cluster in healthy state" ] && break
        sleep 5
        waited=$((waited + 5))
    done
    
    if [ "$ready" != "Cluster in healthy state" ]; then
        fail "PostgreSQL cluster not ready after 5 minutes"
        return 1
    fi
    ok "PostgreSQL cluster ready"
    
    info "Configuring database credentials..."
    local pgpassword=$(kubectl get secret maas-postgres-app -n "$NAMESPACE" -o jsonpath='{.data.password}' | base64 -d)
    kubectl create secret generic database-config \
        --from-literal=DB_CONNECTION_URL="postgresql://app:${pgpassword}@maas-postgres-rw:5432/app?sslmode=require" \
        -n "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null 2>&1
    
    kubectl delete deployment maas-api -n "$NAMESPACE" --ignore-not-found=true --wait=true --timeout=60s >/dev/null 2>&1
    # Apply base deployment with external storage mode
    kustomize build "${PROJECT_ROOT}/deployment/overlays/openshift" | kubectl apply -f - >/dev/null 2>&1
    # Patch deployment to use external storage (must set command explicitly for args to work)
    kubectl patch deployment maas-api -n "$NAMESPACE" --type='json' \
        -p='[{"op":"add","path":"/spec/template/spec/containers/0/command","value":["./maas-api"]},{"op":"add","path":"/spec/template/spec/containers/0/args","value":["--storage=external"]},{"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{"name":"DB_CONNECTION_URL","valueFrom":{"secretKeyRef":{"name":"database-config","key":"DB_CONNECTION_URL"}}}}]' >/dev/null 2>&1
    kubectl rollout status deployment/maas-api -n "$NAMESPACE" --timeout=120s >/dev/null 2>&1
    wait_for_api
    ok "Deployed (PostgreSQL via CloudNativePG)"
}

create_api_key() {
    local name="$1"
    local token=$(get_token)
    local response=$(curl -sSk \
        -H "Authorization: Bearer $token" \
        -H "Content-Type: application/json" \
        -X POST \
        -d "{\"name\": \"$name\", \"description\": \"Test key for $name\", \"expiration\": \"1h\"}" \
        -w "\n%{http_code}" \
        "${GATEWAY_URL}/maas-api/v1/api-keys")
    local status=$(echo "$response" | tail -1)
    local body=$(echo "$response" | sed '$d')
    [ "$status" == "201" ] && echo "$body" && return 0
    return 1
}

list_models() {
    local api_key="$1"
    local response=$(curl -sSk -H "Authorization: Bearer $api_key" -w "\n%{http_code}" "${GATEWAY_URL}/maas-api/v1/models")
    local status=$(echo "$response" | tail -1)
    [ "$status" == "200" ]
}

get_api_key() {
    local jti="$1"
    local token=$(get_token)
    local response=$(curl -sSk -H "Authorization: Bearer $token" -w "\n%{http_code}" "${GATEWAY_URL}/maas-api/v1/api-keys/$jti")
    local status=$(echo "$response" | tail -1)
    local body=$(echo "$response" | sed '$d')
    [ "$status" == "200" ] && echo "$body" && return 0
    return 1
}

revoke_all_tokens() {
    local token=$(get_token)
    local response=$(curl -sSk -H "Authorization: Bearer $token" -X DELETE -w "\n%{http_code}" "${GATEWAY_URL}/maas-api/v1/tokens")
    local status=$(echo "$response" | tail -1)
    [ "$status" == "204" ]
}

restart_pod() {
    local old_pod=$(kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/name=maas-api -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
    kubectl delete pod "$old_pod" -n "$NAMESPACE" --wait=false >/dev/null 2>&1
    
    local waited=0
    while [ $waited -lt 120 ]; do
        local new_pod=$(kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/name=maas-api -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || echo "")
        if [ -n "$new_pod" ] && [ "$new_pod" != "$old_pod" ]; then
            local ready=$(kubectl get pod "$new_pod" -n "$NAMESPACE" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "")
            if [ "$ready" == "True" ]; then
                sleep 2
                wait_for_api
                return 0
            fi
        fi
        sleep 2
        waited=$((waited + 2))
    done
    return 1
}

run_storage_test() {
    local mode="$1"
    local expect_persist="$2"
    local key_name="test-${mode}-$(date +%s)"
    
    step "Creating API Key with metadata..."
    local key_response
    key_response=$(create_api_key "$key_name")
    if [ $? -ne 0 ] || [ -z "$key_response" ]; then
        fail "Failed to create API key"
        return 1
    fi
    
    local api_token=$(echo "$key_response" | jq -r '.token // empty')
    local jti=$(echo "$key_response" | jq -r '.jti // empty')
    local name=$(echo "$key_response" | jq -r '.name // empty')
    local desc=$(echo "$key_response" | jq -r '.description // empty')
    
    if [ -z "$api_token" ] || [ -z "$jti" ]; then
        fail "Invalid API key response"
        return 1
    fi
    
    ok "Created: name='$name', jti='${jti:0:8}...'"
    info "Description: '$desc'"
    
    step "Using API Key to list models..."
    if list_models "$api_token"; then
        ok "API Key is valid and working"
    else
        fail "API Key failed to authenticate"
        return 1
    fi
    
    step "Verifying metadata via GET /api-keys/:id..."
    local get_response=$(get_api_key "$jti")
    if [ $? -eq 0 ]; then
        local status=$(echo "$get_response" | jq -r '.status')
        ok "Metadata retrieved: status='$status'"
    else
        fail "Failed to retrieve metadata"
        return 1
    fi
    
    step "Restarting pod to test persistence..."
    if restart_pod; then
        ok "Pod restarted successfully"
    else
        fail "Pod restart failed"
        return 1
    fi
    
    step "Checking data after restart..."
    local found=false
    get_api_key "$jti" >/dev/null 2>&1 && found=true
    
    if [ "$expect_persist" == "true" ]; then
        if [ "$found" == "true" ]; then
            ok "Data PERSISTED after restart ✓"
        else
            fail "Data LOST after restart (expected to persist!)"
            return 1
        fi
    else
        if [ "$found" == "false" ]; then
            ok "Data correctly LOST after restart (ephemeral mode)"
        else
            fail "Data unexpectedly persisted (should be ephemeral!)"
            return 1
        fi
    fi
    
    if [ "$found" == "true" ] || [ "$expect_persist" == "false" ]; then
        if [ "$found" == "false" ]; then
            key_response=$(create_api_key "cleanup-key")
        fi
        
        step "Revoking all tokens (DELETE /v1/tokens)..."
        if revoke_all_tokens; then
            ok "All tokens revoked"
        else
            fail "Failed to revoke tokens"
            return 1
        fi
    fi
    
    return 0
}

main() {
    banner "MaaS API Storage Modes Verification"
    
    echo -e "\n${W}Testing 3 storage modes:${NC}"
    echo -e "  ${B}1.${NC} In-Memory (--storage=in-memory)  ${Y}(ephemeral - data lost on restart)${NC}"
    echo -e "  ${B}2.${NC} Disk (--storage=disk)            ${G}(persistent - survives restart)${NC}"
    echo -e "  ${B}3.${NC} External (--storage=external)    ${G}(persistent - survives restart)${NC}"
    
    discover_gateway
    info "Gateway: $GATEWAY_URL"
    info "Namespace: $NAMESPACE"
    
    local total_pass=0
    local total_fail=0
    
    banner "Mode 1: In-Memory SQLite"
    cleanup_all
    deploy_in_memory
    if run_storage_test "memory" "false"; then
        ((total_pass++))
        echo -e "\n${G}═══ In-Memory SQLite: PASS ═══${NC}"
    else
        ((total_fail++))
        echo -e "\n${R}═══ In-Memory SQLite: FAIL ═══${NC}"
    fi
    
    banner "Mode 2: Disk Storage"
    cleanup_all
    deploy_sqlite_pvc
    if run_storage_test "disk" "true"; then
        ((total_pass++))
        echo -e "\n${G}═══ Disk Storage: PASS ═══${NC}"
    else
        ((total_fail++))
        echo -e "\n${R}═══ Disk Storage: FAIL ═══${NC}"
    fi
    
    banner "Mode 3: External Database"
    cleanup_all
    deploy_postgresql
    if run_storage_test "external" "true"; then
        ((total_pass++))
        echo -e "\n${G}═══ External Database: PASS ═══${NC}"
    else
        ((total_fail++))
        echo -e "\n${R}═══ External Database: FAIL ═══${NC}"
    fi
    
    banner "Verification Complete"
    
    echo ""
    echo -e "${W}Results:${NC}"
    echo -e "  ${G}Passed: $total_pass${NC}"
    echo -e "  ${R}Failed: $total_fail${NC}"
    echo ""
    
    if [ $total_fail -eq 0 ]; then
        echo -e "${G}╔══════════════════════════════════════════════════════════════════╗${NC}"
        echo -e "${G}║  All 3 storage modes working correctly!                          ║${NC}"
        echo -e "${G}╚══════════════════════════════════════════════════════════════════╝${NC}"
        return 0
    else
        echo -e "${R}╔══════════════════════════════════════════════════════════════════╗${NC}"
        echo -e "${R}║  Some tests failed - check output above                          ║${NC}"
        echo -e "${R}╚══════════════════════════════════════════════════════════════════╝${NC}"
        return 1
    fi
}

main "$@"

