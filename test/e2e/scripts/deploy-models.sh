#!/bin/bash
# =============================================================================
# MaaS Model Deployment
# =============================================================================
# Deploys e2e test fixtures (LLMIS, MaaSModelRef, MaaSAuthPolicy, MaaSSubscription)
# and waits for models, governed refs, and AuthPolicies to be ready.
# Sourced by prow_run_smoke_test.sh — assumes PROJECT_ROOT, deployment-helpers.sh,
# and env var defaults are already set.
# =============================================================================

set -euo pipefail

wait_for_gateway_programmed() {
    local gateway_name="${1:-$GATEWAY_NAME}"
    local gateway_ns="${2:-$GATEWAY_NAMESPACE}"
    local timeout="${3:-$GATEWAY_PROGRAMMED_TIMEOUT}"

    echo "Waiting for Gateway ${gateway_ns}/${gateway_name} to be Programmed=True (timeout: ${timeout}s)..."

    if oc wait "gateway/${gateway_name}" -n "${gateway_ns}" --for=condition=Programmed --timeout="${timeout}s"; then
        echo "✅ Gateway ${gateway_ns}/${gateway_name} is Programmed"
        return 0
    fi

    echo "❌ ERROR: Gateway ${gateway_ns}/${gateway_name} did not reach Programmed=True within ${timeout}s"
    oc get "gateway/${gateway_name}" -n "${gateway_ns}" -o wide || true
    oc describe "gateway/${gateway_name}" -n "${gateway_ns}" || true
    return 1
}

wait_for_auth_policies_enforced() {
    local timeout="$AUTHPOLICY_TIMEOUT"
    echo "Waiting for Kuadrant AuthPolicies to be enforced (timeout: ${timeout}s)..."

    local llm_namespaces
    llm_namespaces=$(oc get llminferenceservices -A -o jsonpath='{range .items[*]}{.metadata.namespace}{"\n"}{end}' 2>/dev/null | sort -u)
    local namespaces
    namespaces=$(printf '%s\n%s\n' "${GATEWAY_NAMESPACE:-openshift-ingress}" "$llm_namespaces" | sort -u | xargs)

    local deadline=$((SECONDS + timeout))
    while [[ $SECONDS -lt $deadline ]]; do
        local all_enforced=true
        local total=0
        for ns in $namespaces; do
            while IFS= read -r status; do
                total=$((total + 1))
                if [[ "$status" != "True" ]]; then
                    all_enforced=false
                fi
            done < <(oc get authpolicies -n "$ns" -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Enforced")].status}{"\n"}{end}' 2>/dev/null)
        done
        if $all_enforced && [[ $total -gt 0 ]]; then
            echo "✅ All AuthPolicies enforced ($total policies)"
            return 0
        fi
        echo "  Waiting... ($total policies found, not all enforced yet)"
        sleep 10
    done
    echo "⚠️  WARNING: AuthPolicies not all enforced after ${timeout}s, tests may fail"
    oc get authpolicies -A -o wide 2>/dev/null || true
}

deploy_models() {
    echo "Deploying MaaS system (free + premium: LLMIS + MaaSModelRef + MaaSAuthPolicy + MaaSSubscription)"
    if ! wait_for_gateway_programmed "$GATEWAY_NAME" "$GATEWAY_NAMESPACE" "$GATEWAY_PROGRAMMED_TIMEOUT"; then
        exit 1
    fi

    if ! kubectl get namespace llm >/dev/null 2>&1; then
        echo "Creating 'llm' namespace..."
        kubectl create namespace llm || { echo "❌ ERROR: Failed to create 'llm' namespace"; exit 1; }
    else
        echo "'llm' namespace already exists"
    fi

    if ! kubectl get namespace "$MAAS_SUBSCRIPTION_NAMESPACE" >/dev/null 2>&1; then
        echo "Creating '$MAAS_SUBSCRIPTION_NAMESPACE' namespace..."
        kubectl create namespace "$MAAS_SUBSCRIPTION_NAMESPACE" || { echo "❌ ERROR: Failed to create '$MAAS_SUBSCRIPTION_NAMESPACE' namespace"; exit 1; }
    else
        echo "'$MAAS_SUBSCRIPTION_NAMESPACE' namespace already exists"
    fi

    if ! [[ "$MAAS_SUBSCRIPTION_NAMESPACE" =~ ^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]]; then
        echo "❌ ERROR: MAAS_SUBSCRIPTION_NAMESPACE is not a valid DNS-1123 label: '$MAAS_SUBSCRIPTION_NAMESPACE'"
        exit 1
    fi
    if ! (cd "$PROJECT_ROOT" && kustomize build test/e2e/fixtures/ | \
            sed "s/namespace: models-as-a-service/namespace: $MAAS_SUBSCRIPTION_NAMESPACE/g" | \
            kubectl apply -f -); then
        echo "❌ ERROR: Failed to deploy MaaS system with e2e fixtures"
        exit 1
    fi
    echo "✅ MaaS system deployed (free + premium + e2e test fixtures)"

    echo "Waiting for models to be ready (timeout: ${LLMIS_TIMEOUT}s)..."
    local models=("facebook-opt-125m-simulated" "premium-simulated-simulated-premium" "e2e-unconfigured-facebook-opt-125m-simulated")
    for model in "${models[@]}"; do
        if ! oc wait "llminferenceservice/$model" -n llm --for=condition=Ready --timeout="${LLMIS_TIMEOUT}s"; then
            echo "❌ ERROR: Timed out waiting for $model to be ready"
            dump_llmis_diagnostics "$model" "llm"
            exit 1
        fi
    done
    echo "✅ Simulator models ready"

    local governed_models=("facebook-opt-125m-simulated" "premium-simulated-simulated-premium")
    echo "Waiting for governed MaaSModelRefs to be Ready (timeout: ${MAASMODELREF_TIMEOUT}s)..."
    local deadline=$((SECONDS + MAASMODELREF_TIMEOUT))
    local all_ready=false

    while [[ $SECONDS -lt $deadline ]]; do
        all_ready=true
        for model in "${governed_models[@]}"; do
            local phase
            phase=$(oc get maasmodelref "$model" -n "$MODEL_NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
            if [[ "$phase" != "Ready" ]]; then
                all_ready=false
                break
            fi
        done
        if $all_ready; then
            echo "✅ Governed MaaSModelRefs ready"
            break
        fi
        sleep 5
    done

    if ! $all_ready; then
        echo "❌ ERROR: Governed MaaSModelRefs did not reach Ready state within ${MAASMODELREF_TIMEOUT}s"
        oc get maasmodelrefs -n "$MODEL_NAMESPACE" -o yaml || true
        kubectl logs deployment/maas-controller -n "$DEPLOYMENT_NAMESPACE" --tail=100 || true
        exit 1
    fi

    wait_for_auth_policies_enforced
}

deploy_models
