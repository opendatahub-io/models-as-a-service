#!/bin/bash
# =============================================================================
# E2E Test Token Setup
# =============================================================================
# Creates test users (admin + regular) and extracts authentication tokens.
# Sourced by prow_run_smoke_test.sh — assumes PROJECT_ROOT, deployment-helpers.sh,
# and env var defaults are already set.
#
# Exports: TOKEN, ADMIN_OC_TOKEN, E2E_TEST_TOKEN_SA_NAMESPACE, E2E_TEST_TOKEN_SA_NAME
# =============================================================================

set -euo pipefail

PREMIUM_USERS_NS="premium-users-namespace"
PREMIUM_SA="premium-service-account"
E2E_ADMIN_SA_NAMESPACE="${E2E_ADMIN_SA_NAMESPACE:-maas-admin}"

setup_test_user() {
    local username="$1"
    local cluster_role="$2"
    local namespace="${3:-default}"

    if ! oc get namespace "$namespace" &>/dev/null; then
        echo "Creating namespace: $namespace"
        oc create namespace "$namespace"
    fi

    if ! oc get serviceaccount "$username" -n "$namespace" >/dev/null 2>&1; then
        echo "Creating service account: $username in $namespace"
        oc create serviceaccount "$username" -n "$namespace"
    else
        echo "Service account $username already exists in $namespace"
    fi

    if ! oc get clusterrolebinding "${username}-binding" >/dev/null 2>&1; then
        echo "Creating cluster role binding for $username"
        oc create clusterrolebinding "${username}-binding" \
            --clusterrole="$cluster_role" \
            --serviceaccount="${namespace}:${username}"
    else
        echo "Cluster role binding for $username already exists"
    fi

    echo "✅ User setup completed: $username (namespace: $namespace)"
}

_grant_maas_admin_rbac() {
    local user="$1"
    local ns="${MAAS_SUBSCRIPTION_NAMESPACE}"
    local role_name="maas-admin-e2e"

    oc apply -f - <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: ${role_name}
  namespace: ${ns}
rules:
- apiGroups: ["maas.opendatahub.io"]
  resources: ["maasauthpolicies"]
  verbs: ["create"]
EOF

    local safe_name
    safe_name=$(echo "$user" | tr ':/' '-' | cut -c1-50)
    oc create rolebinding "${role_name}-${safe_name}" \
        --role="$role_name" \
        --user="$user" \
        -n "$ns" 2>/dev/null || true
}

_patch_auth_cr_for_sa_admin() {
    local admin_namespace="${1:-$E2E_ADMIN_SA_NAMESPACE}"
    local admin_group="system:serviceaccounts:${admin_namespace}"

    local auth_cr=""
    for gvr in "auths.services.platform.opendatahub.io" "auths.platform.opendatahub.io"; do
        if oc get "$gvr" auth &>/dev/null; then
            auth_cr="$gvr"
            break
        fi
    done
    if [[ -z "$auth_cr" ]]; then
        echo "⚠️  Auth CR not found - admin tests may fail (SA token not in adminGroups)"
        return 0
    fi
    local current
    current=$(oc get "$auth_cr" auth -o jsonpath='{.spec.adminGroups[*]}' 2>/dev/null || true)
    if [[ "$current" == *"${admin_group}"* ]]; then
        echo "✅ Auth CR already has ${admin_group} in adminGroups"
        return 0
    fi
    if oc patch "$auth_cr" auth --type=json -p="[{\"op\": \"add\", \"path\": \"/spec/adminGroups/-\", \"value\": \"${admin_group}\"}]" 2>/dev/null; then
        echo "✅ Added ${admin_group} to Auth CR adminGroups (SA admin fallback)"
    else
        echo "⚠️  Failed to patch Auth CR - admin tests may fail"
    fi
}

setup_premium_test_token() {
    echo "Setting up premium test token (SA-based, works when oc whoami -t is unavailable)..."
    if ! kubectl get namespace "$PREMIUM_USERS_NS" &>/dev/null; then
        echo "Creating namespace: $PREMIUM_USERS_NS"
        kubectl create namespace "$PREMIUM_USERS_NS"
    fi
    if ! kubectl get sa "$PREMIUM_SA" -n "$PREMIUM_USERS_NS" &>/dev/null; then
        echo "Creating service account: $PREMIUM_SA"
        kubectl create sa "$PREMIUM_SA" -n "$PREMIUM_USERS_NS"
    fi

    local sa_user="system:serviceaccount:${PREMIUM_USERS_NS}:${PREMIUM_SA}"
    echo "Patching MaaSAuthPolicy premium-simulator-access to include $sa_user..."
    oc patch maasauthpolicy premium-simulator-access -n "$MAAS_SUBSCRIPTION_NAMESPACE" --type=merge -p="{\"spec\": {\"subjects\": {\"groups\": [{\"name\": \"premium-user\"}], \"users\": [\"$sa_user\"]}}}"

    echo "Patching MaaSSubscription premium-simulator-subscription to include $sa_user..."
    oc patch maassubscription premium-simulator-subscription -n "$MAAS_SUBSCRIPTION_NAMESPACE" --type=merge -p="{\"spec\": {\"owner\": {\"groups\": [{\"name\": \"premium-user\"}], \"users\": [\"$sa_user\"]}}}"

    export E2E_TEST_TOKEN_SA_NAMESPACE="$PREMIUM_USERS_NS"
    export E2E_TEST_TOKEN_SA_NAME="$PREMIUM_SA"

    echo "Waiting for MaaSSubscriptions to reconcile after patch (timeout: 60s)..."
    local timeout=60
    local deadline=$((SECONDS + timeout))
    local both_ready=false

    while [[ $SECONDS -lt $deadline ]]; do
        local sim_phase premium_phase
        sim_phase=$(oc get maassubscription simulator-subscription -n "$MAAS_SUBSCRIPTION_NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
        premium_phase=$(oc get maassubscription premium-simulator-subscription -n "$MAAS_SUBSCRIPTION_NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")

        if [[ "$sim_phase" == "Active" || "$sim_phase" == "Degraded" ]] && \
           [[ "$premium_phase" == "Active" || "$premium_phase" == "Degraded" ]]; then
            echo "✅ Both subscriptions ready: simulator-subscription=$sim_phase, premium-simulator-subscription=$premium_phase"
            both_ready=true
            break
        fi
        sleep 2
    done

    if ! $both_ready; then
        echo "❌ ERROR: Subscriptions did not reach Active/Degraded phase within ${timeout}s"
        oc get maassubscriptions -n "$MAAS_SUBSCRIPTION_NAMESPACE" -o yaml || true
        exit 1
    fi

    echo "✅ Premium test token setup complete (E2E_TEST_TOKEN_SA_* exported)"
}

setup_test_tokens() {
    echo "Setting up test tokens (admin + regular user)..."

    local current_user api_server
    current_user=$(oc whoami)
    api_server=$(oc whoami --show-server)
    echo "Current admin session: $current_user (will be preserved)"

    export ADMIN_OC_TOKEN=""
    export TOKEN=""

    local temp_kubeconfig
    temp_kubeconfig=$(mktemp)
    trap "rm -f '$temp_kubeconfig'" RETURN

    if [[ -f "${SHARED_DIR:-}/runtime_env" ]]; then
        # shellcheck source=/dev/null
        source "${SHARED_DIR}/runtime_env"
        if [[ -n "${USERS:-}" ]]; then
            echo "Found htpasswd users from idp-htpasswd step"

            local admin_creds
            admin_creds=$(echo "$USERS" | tr ',' '\n' | grep "^testuser-1:" | head -1)
            if [[ -n "$admin_creds" ]]; then
                local admin_user admin_pass
                admin_user="${admin_creds%%:*}"
                admin_pass="${admin_creds#*:}"

                oc adm groups add-users odh-admins "$admin_user" 2>/dev/null || true
                _grant_maas_admin_rbac "$admin_user"

                if KUBECONFIG="$temp_kubeconfig" oc login "$api_server" -u "$admin_user" -p "$admin_pass" --insecure-skip-tls-verify=true &>/dev/null; then
                    ADMIN_OC_TOKEN=$(KUBECONFIG="$temp_kubeconfig" oc whoami -t)
                    echo "✅ Admin token for $admin_user (htpasswd)"
                fi
            fi

            local regular_creds
            regular_creds=$(echo "$USERS" | tr ',' '\n' | grep "^testuser-2:" | head -1)
            if [[ -n "$regular_creds" ]]; then
                local regular_user regular_pass
                regular_user="${regular_creds%%:*}"
                regular_pass="${regular_creds#*:}"

                if KUBECONFIG="$temp_kubeconfig" oc login "$api_server" -u "$regular_user" -p "$regular_pass" --insecure-skip-tls-verify=true &>/dev/null; then
                    TOKEN=$(KUBECONFIG="$temp_kubeconfig" oc whoami -t)
                    echo "✅ Regular user token for $regular_user (htpasswd)"
                fi
            fi
        fi
    fi

    if [[ -z "$ADMIN_OC_TOKEN" ]]; then
        ADMIN_OC_TOKEN=$(oc whoami -t 2>/dev/null || true)
        if [[ -n "$ADMIN_OC_TOKEN" ]]; then
            oc adm groups add-users odh-admins "$current_user" 2>/dev/null || true
            _grant_maas_admin_rbac "$current_user"
            echo "✅ Admin token for $current_user (added to odh-admins)"
        else
            echo "⚠️  No htpasswd token available - using SA token (admin tests may fail)"
            setup_test_user "tester-admin-user" "view" "$E2E_ADMIN_SA_NAMESPACE"
            _grant_maas_admin_rbac "system:serviceaccount:${E2E_ADMIN_SA_NAMESPACE}:tester-admin-user"
            _patch_auth_cr_for_sa_admin "$E2E_ADMIN_SA_NAMESPACE"
            ADMIN_OC_TOKEN=$(oc create token tester-admin-user -n "$E2E_ADMIN_SA_NAMESPACE" --duration=1h)
        fi
    fi

    oc apply -f - <<RBAC_EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: maas-admin
rules:
- apiGroups: ["maas.opendatahub.io"]
  resources: ["maasauthpolicies", "maassubscriptions"]
  verbs: ["create", "delete", "get", "list", "patch", "update", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: odh-admins-maas-admin
  namespace: $MAAS_SUBSCRIPTION_NAMESPACE
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: maas-admin
subjects:
- apiGroup: rbac.authorization.k8s.io
  kind: Group
  name: odh-admins
RBAC_EOF

    if [[ -z "$TOKEN" ]]; then
        echo "Creating separate SA token for regular user (required for IDOR tests)..."
        setup_test_user "tester-regular-user" "view" "default"
        TOKEN=$(oc create token tester-regular-user -n default --duration=1h)
        echo "✅ Regular user token for tester-regular-user (SA-based, namespace: default)"
    fi

    echo "Token setup complete (main session unchanged: $(oc whoami))"
}

setup_premium_test_token
setup_test_tokens
