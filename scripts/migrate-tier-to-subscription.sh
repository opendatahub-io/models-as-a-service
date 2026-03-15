#!/bin/bash

# Migration script: Convert tier-based configuration to subscription model
# This script generates MaaSModelRef, MaaSAuthPolicy, and MaaSSubscription CRs
# from existing tier-to-group-mapping ConfigMap configuration.

set -euo pipefail

# Default values
TIER=""
MODELS=""
AUTH_GROUPS=""
RATE_LIMIT=""
WINDOW="1m"
OUTPUT_DIR="migration-crs"
SUBSCRIPTION_NAMESPACE="models-as-a-service"
MODEL_NAMESPACE="llm"
MAAS_NAMESPACE="opendatahub"
DRY_RUN=false
APPLY=false
VERBOSE=false

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Helper functions
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

# Validate Kubernetes resource name (DNS subdomain format)
# Usage: validate_resource_name <name> <field> [max_length]
validate_resource_name() {
    local name="$1"
    local field="$2"
    local max_length="${3:-253}"  # Default to 253 (DNS subdomain), override for stricter limits

    if [[ -z "$name" ]]; then
        log_error "$field cannot be empty"
        return 1
    fi

    if [[ ${#name} -gt $max_length ]]; then
        log_error "$field '$name' exceeds maximum length of $max_length characters (actual: ${#name})"
        return 1
    fi

    if [[ ! "$name" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$ ]]; then
        log_error "$field '$name' contains invalid characters"
        log_info "Valid format: lowercase alphanumeric, '-', '.'; must start/end with alphanumeric"
        return 1
    fi

    return 0
}

# Quote string for YAML output (escapes quotes and special chars)
yaml_quote() {
    local value="$1"
    # Escape backslashes and double quotes
    value="${value//\\/\\\\}"
    value="${value//\"/\\\"}"
    echo "\"$value\""
}

# Validate that an option has a value
validate_option_value() {
    local option="$1"
    local value="${2:-}"

    if [[ -z "$value" ]] || [[ "$value" == --* ]]; then
        log_error "Option $option requires a value"
        usage
        exit 1
    fi
}

usage() {
    cat <<EOF
Usage: $0 [OPTIONS]

Migrate tier-based configuration to subscription model by generating MaaS CRs.

OPTIONS:
    --tier <name>               Tier name from ConfigMap (required)
    --models <list>             Comma-separated model names (required)
    --groups <list>             Comma-separated group names (optional, auto-detected from ConfigMap)
    --rate-limit <limit>        Token rate limit (required)
    --window <duration>         Rate limit window (default: 1m)
    --output <dir>              Output directory for generated CRs (default: migration-crs)
    --subscription-ns <ns>      Subscription namespace (default: models-as-a-service)
    --model-ns <ns>             Model namespace (default: llm)
    --maas-ns <ns>              MaaS namespace (default: opendatahub)
    --dry-run                   Generate files without applying
    --apply                     Apply generated CRs to cluster
    --verbose                   Enable verbose logging
    --help                      Show this help message

EXAMPLES:
    # Generate CRs for premium tier
    $0 --tier premium \\
       --models model-a,model-b,model-c \\
       --groups premium-users \\
       --rate-limit 50000 \\
       --output migration-crs/premium/

    # Generate and apply for free tier
    $0 --tier free \\
       --models simulator,qwen3 \\
       --groups system:authenticated \\
       --rate-limit 100 \\
       --apply

    # Extract tier config from ConfigMap and generate CRs
    $0 --tier enterprise \\
       --models \$(kubectl get llminferenceservice -n llm -o name | cut -d/ -f2 | tr '\\n' ',') \\
       --groups enterprise-users \\
       --rate-limit 100000 \\
       --dry-run \\
       --verbose

EOF
}

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --tier)
            validate_option_value "$1" "${2:-}"
            TIER="$2"
            shift 2
            ;;
        --models)
            validate_option_value "$1" "${2:-}"
            MODELS="$2"
            shift 2
            ;;
        --groups)
            validate_option_value "$1" "${2:-}"
            AUTH_GROUPS="$2"
            shift 2
            ;;
        --rate-limit)
            validate_option_value "$1" "${2:-}"
            RATE_LIMIT="$2"
            shift 2
            ;;
        --window)
            validate_option_value "$1" "${2:-}"
            WINDOW="$2"
            shift 2
            ;;
        --output)
            validate_option_value "$1" "${2:-}"
            OUTPUT_DIR="$2"
            shift 2
            ;;
        --subscription-ns)
            validate_option_value "$1" "${2:-}"
            SUBSCRIPTION_NAMESPACE="$2"
            shift 2
            ;;
        --model-ns)
            validate_option_value "$1" "${2:-}"
            MODEL_NAMESPACE="$2"
            shift 2
            ;;
        --maas-ns)
            validate_option_value "$1" "${2:-}"
            MAAS_NAMESPACE="$2"
            shift 2
            ;;
        --dry-run)
            DRY_RUN=true
            shift
            ;;
        --apply)
            APPLY=true
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

# Validate required parameters
if [[ -z "$TIER" ]]; then
    log_error "Missing required parameter: --tier"
    usage
    exit 1
fi

if [[ -z "$MODELS" ]]; then
    log_error "Missing required parameter: --models"
    usage
    exit 1
fi

if [[ -z "$RATE_LIMIT" ]]; then
    log_error "Missing required parameter: --rate-limit"
    usage
    exit 1
fi

# Validate rate limit is a positive integer
if ! [[ "$RATE_LIMIT" =~ ^[0-9]+$ ]] || [[ "$RATE_LIMIT" -eq 0 ]]; then
    log_error "Rate limit must be a positive integer, got: '$RATE_LIMIT'"
    exit 1
fi

# Validate window format (e.g., 1m, 60s, 1h)
if ! [[ "$WINDOW" =~ ^[0-9]+(s|m|h|d)$ ]]; then
    log_error "Window must be a valid duration (e.g., 1m, 60s, 1h), got: '$WINDOW'"
    exit 1
fi

# Validate tier name (used in resource names like ${TIER}-models-access, max 63 chars)
if ! validate_resource_name "$TIER" "Tier name" 63; then
    log_info "Tier name is used in generated CR names and must not exceed 63 characters"
    exit 1
fi

# Extract groups from ConfigMap if not provided
if [[ -z "$AUTH_GROUPS" ]]; then
    log_info "Attempting to extract groups for tier '$TIER' from ConfigMap..."

    if ! command -v yq &> /dev/null; then
        log_error "yq is required for ConfigMap extraction but not found"
        log_info "Install yq (https://github.com/mikefarah/yq) or specify groups manually with --groups"
        exit 1
    fi

    if kubectl get configmap tier-to-group-mapping -n maas-api >/dev/null 2>&1; then
        AUTH_GROUPS=$(kubectl get configmap tier-to-group-mapping -n maas-api -o yaml | \
            yq eval '.data[]' - | \
            TIER="$TIER" yq eval '[.[] | select(.name == env(TIER)) | .groups[]] | join(",")' -)

        if [[ -n "$AUTH_GROUPS" ]]; then
            log_success "Extracted groups: $AUTH_GROUPS"
        else
            log_error "Could not extract groups for tier '$TIER' from ConfigMap"
            log_info "Please specify groups manually with --groups"
            exit 1
        fi
    else
        log_error "ConfigMap tier-to-group-mapping not found in maas-api namespace"
        log_info "Please specify groups manually with --groups"
        exit 1
    fi
fi

# Create output directory and clean any existing files to prevent stale YAML
if [[ "$DRY_RUN" == "false" ]]; then
    if [[ -d "$OUTPUT_DIR" ]]; then
        # Directory exists - check if it has files
        if [[ -n "$(find "$OUTPUT_DIR" -maxdepth 1 -name '*.yaml' -print -quit)" ]]; then
            log_warn "Output directory '$OUTPUT_DIR' contains existing YAML files"
            log_info "Cleaning directory to prevent applying stale manifests..."
            rm -f "$OUTPUT_DIR"/*.yaml
            log_success "Cleaned existing YAML files from: $OUTPUT_DIR"
        fi
    else
        # Directory doesn't exist - create it
        mkdir -p "$OUTPUT_DIR"
        log_success "Created output directory: $OUTPUT_DIR"
    fi
fi

# Convert comma-separated lists to arrays
IFS=',' read -ra MODEL_ARRAY <<< "$MODELS"
IFS=',' read -ra GROUP_ARRAY <<< "$AUTH_GROUPS"

# Validate model names (must be valid MaaSModelRef names, max 63 chars)
for model in "${MODEL_ARRAY[@]}"; do
    model=$(echo "$model" | xargs) # trim whitespace
    if ! validate_resource_name "$model" "Model name" 63; then
        log_error "Invalid model name in list: '$model'"
        log_info "Model names are used as MaaSModelRef names and must not exceed 63 characters"
        exit 1
    fi
done

# Validate group names (groups can contain ':' for system groups like 'system:authenticated')
for group in "${GROUP_ARRAY[@]}"; do
    group=$(echo "$group" | xargs) # trim whitespace
    if [[ -z "$group" ]]; then
        log_error "Group name cannot be empty"
        exit 1
    fi
    # Groups have more permissive naming (allow colons for system:* groups)
    if [[ ${#group} -gt 253 ]]; then
        log_error "Group name '$group' exceeds maximum length of 253 characters"
        exit 1
    fi
done

# Validate namespace names used in modelRefs (CRD limit: 63 characters)
if ! validate_resource_name "$MODEL_NAMESPACE" "Model namespace" 63; then
    log_error "Model namespace is used in modelRefs[].namespace and must not exceed 63 characters"
    exit 1
fi

# Validate subscription namespace (used as metadata.namespace for MaaSAuthPolicy/MaaSSubscription)
if ! validate_resource_name "$SUBSCRIPTION_NAMESPACE" "Subscription namespace" 63; then
    log_error "Subscription namespace is used as metadata.namespace for generated CRs and must not exceed 63 characters"
    exit 1
fi

log_info "Migration Configuration:"
log_verbose "  Tier: $TIER"
log_verbose "  Models: ${#MODEL_ARRAY[@]} (${MODELS})"
log_verbose "  Groups: ${#GROUP_ARRAY[@]} (${AUTH_GROUPS})"
log_verbose "  Rate Limit: $RATE_LIMIT tokens per $WINDOW"
log_verbose "  Output: $OUTPUT_DIR"
log_verbose "  Namespaces: MaaS=$MAAS_NAMESPACE, Subscription=$SUBSCRIPTION_NAMESPACE, Model=$MODEL_NAMESPACE"
log_verbose "  Mode: $([ "$DRY_RUN" == "true" ] && echo "DRY-RUN" || echo "GENERATE")$([ "$APPLY" == "true" ] && echo " + APPLY" || echo "")"

# Generate MaaSModelRef for each model
log_info "Generating MaaSModelRef CRs..."
for model in "${MODEL_ARRAY[@]}"; do
    model=$(echo "$model" | xargs) # trim whitespace

    MAASMODELREF_FILE="$OUTPUT_DIR/maasmodelref-${model}.yaml"

    if [[ "$DRY_RUN" == "false" ]]; then
        cat > "$MAASMODELREF_FILE" <<EOF
# MaaSModelRef: Register model '$model' with MaaS
# Generated by migrate-tier-to-subscription.sh for tier '$TIER'
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: $(yaml_quote "$model")
  namespace: $(yaml_quote "$MODEL_NAMESPACE")
  labels:
    migration.maas.opendatahub.io/from-tier: $(yaml_quote "$TIER")
    migration.maas.opendatahub.io/generated: "true"
  annotations:
    migration.maas.opendatahub.io/timestamp: $(yaml_quote "$(date -u +%Y-%m-%dT%H:%M:%SZ)")
    migration.maas.opendatahub.io/original-tier: $(yaml_quote "$TIER")
spec:
  modelRef:
    kind: LLMInferenceService
    name: $(yaml_quote "$model")
EOF
        log_success "Generated: $MAASMODELREF_FILE"
    else
        log_verbose "Would generate: $MAASMODELREF_FILE"
    fi
done

# Generate MaaSAuthPolicy (one for all models in this tier)
log_info "Generating MaaSAuthPolicy CR..."
AUTHPOLICY_FILE="$OUTPUT_DIR/maasauthpolicy-${TIER}.yaml"

if [[ "$DRY_RUN" == "false" ]]; then
    cat > "$AUTHPOLICY_FILE" <<EOF
# MaaSAuthPolicy: Grant access to tier '$TIER' models
# Generated by migrate-tier-to-subscription.sh
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: $(yaml_quote "${TIER}-models-access")
  namespace: $(yaml_quote "$SUBSCRIPTION_NAMESPACE")
  labels:
    migration.maas.opendatahub.io/from-tier: $(yaml_quote "$TIER")
    migration.maas.opendatahub.io/generated: "true"
  annotations:
    migration.maas.opendatahub.io/timestamp: $(yaml_quote "$(date -u +%Y-%m-%dT%H:%M:%SZ)")
    migration.maas.opendatahub.io/original-tier: $(yaml_quote "$TIER")
    description: $(yaml_quote "Access policy for $TIER tier models (migrated from tier-based system)")
spec:
  modelRefs:
EOF

    for model in "${MODEL_ARRAY[@]}"; do
        model=$(echo "$model" | xargs)
        cat >> "$AUTHPOLICY_FILE" <<MODELREF
    - name: $(yaml_quote "$model")
      namespace: $(yaml_quote "$MODEL_NAMESPACE")
MODELREF
    done

    cat >> "$AUTHPOLICY_FILE" <<EOF
  subjects:
    groups:
EOF

    for group in "${GROUP_ARRAY[@]}"; do
        group=$(echo "$group" | xargs)
        echo "      - name: $(yaml_quote "$group")" >> "$AUTHPOLICY_FILE"
    done

    cat >> "$AUTHPOLICY_FILE" <<EOF
    users: []
EOF
    log_success "Generated: $AUTHPOLICY_FILE"
else
    log_verbose "Would generate: $AUTHPOLICY_FILE"
fi

# Generate MaaSSubscription (one for all models in this tier)
log_info "Generating MaaSSubscription CR..."
SUBSCRIPTION_FILE="$OUTPUT_DIR/maassubscription-${TIER}.yaml"

if [[ "$DRY_RUN" == "false" ]]; then
    cat > "$SUBSCRIPTION_FILE" <<EOF
# MaaSSubscription: Rate limits for tier '$TIER' models
# Generated by migrate-tier-to-subscription.sh
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: $(yaml_quote "${TIER}-models-subscription")
  namespace: $(yaml_quote "$SUBSCRIPTION_NAMESPACE")
  labels:
    migration.maas.opendatahub.io/from-tier: $(yaml_quote "$TIER")
    migration.maas.opendatahub.io/generated: "true"
  annotations:
    migration.maas.opendatahub.io/timestamp: $(yaml_quote "$(date -u +%Y-%m-%dT%H:%M:%SZ)")
    migration.maas.opendatahub.io/original-tier: $(yaml_quote "$TIER")
    description: $(yaml_quote "Subscription for $TIER tier models with $RATE_LIMIT tokens/$WINDOW (migrated from tier-based system)")
spec:
  owner:
    groups:
EOF

    for group in "${GROUP_ARRAY[@]}"; do
        group=$(echo "$group" | xargs)
        echo "      - name: $(yaml_quote "$group")" >> "$SUBSCRIPTION_FILE"
    done

    cat >> "$SUBSCRIPTION_FILE" <<EOF
    users: []
  modelRefs:
EOF

    for model in "${MODEL_ARRAY[@]}"; do
        model=$(echo "$model" | xargs)
        cat >> "$SUBSCRIPTION_FILE" <<EOF
    - name: $(yaml_quote "$model")
      namespace: $(yaml_quote "$MODEL_NAMESPACE")
      tokenRateLimits:
        - limit: $RATE_LIMIT
          window: $(yaml_quote "$WINDOW")
EOF
    done

    log_success "Generated: $SUBSCRIPTION_FILE"
else
    log_verbose "Would generate: $SUBSCRIPTION_FILE"
fi

# Summary
echo ""
log_success "Migration CRs generated successfully!"
echo ""
log_info "Summary:"
log_verbose "  Tier: $TIER"
log_verbose "  Models: ${#MODEL_ARRAY[@]}"
log_verbose "  Groups: ${#GROUP_ARRAY[@]}"
log_verbose "  Files generated:"
log_verbose "    - ${#MODEL_ARRAY[@]} MaaSModelRef CRs"
log_verbose "    - 1 MaaSAuthPolicy CR"
log_verbose "    - 1 MaaSSubscription CR"
echo ""

if [[ "$DRY_RUN" == "true" ]]; then
    log_warn "DRY-RUN mode: Files were NOT created"
    log_info "Run without --dry-run to generate files"
    exit 0
fi

log_info "Output directory: $OUTPUT_DIR"
log_verbose "Files:"
ls -1 "$OUTPUT_DIR"
echo ""

# Apply to cluster if requested
if [[ "$APPLY" == "true" ]]; then
    log_info "Applying CRs to cluster..."

    # Check if kubectl is available
    if ! command -v kubectl &> /dev/null; then
        log_error "kubectl not found. Cannot apply CRs."
        exit 1
    fi

    # Check if subscription namespace exists
    if ! kubectl get namespace "$SUBSCRIPTION_NAMESPACE" >/dev/null 2>&1; then
        log_warn "Subscription namespace '$SUBSCRIPTION_NAMESPACE' does not exist"

        # Auto-create in non-interactive mode (CI or non-TTY)
        if [[ -n "${CI:-}" ]] || [[ ! -t 0 ]]; then
            log_info "Non-interactive mode detected, automatically creating namespace"
            kubectl create namespace "$SUBSCRIPTION_NAMESPACE"
            log_success "Created namespace: $SUBSCRIPTION_NAMESPACE"
        else
            # Interactive mode: prompt user
            read -p "Create namespace? (y/N) " -n 1 -r
            echo
            if [[ $REPLY =~ ^[Yy]$ ]]; then
                kubectl create namespace "$SUBSCRIPTION_NAMESPACE"
                log_success "Created namespace: $SUBSCRIPTION_NAMESPACE"
            else
                log_error "Cannot proceed without subscription namespace"
                exit 1
            fi
        fi
    fi

    # Apply CRs (only files generated in this run - directory was cleaned earlier)
    log_verbose "Applying YAML files from $OUTPUT_DIR:"
    if [[ "$VERBOSE" == "true" ]]; then
        ls -1 "$OUTPUT_DIR"/*.yaml 2>/dev/null | sed 's/^/  /'
    fi
    kubectl apply -f "$OUTPUT_DIR/"

    echo ""
    log_success "CRs applied successfully!"
    echo ""

    # Validate
    log_info "Validating deployment..."
    sleep 2

    # Check MaaSModelRef status
    log_info "Checking MaaSModelRef status..."
    for model in "${MODEL_ARRAY[@]}"; do
        model=$(echo "$model" | xargs)
        PHASE=$(kubectl get maasmodelref "$model" -n "$MODEL_NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || echo "NotFound")
        if [[ "$PHASE" == "Ready" ]]; then
            log_success "  $model: Ready"
        elif [[ "$PHASE" == "Pending" ]]; then
            log_warn "  $model: Pending (may need time to reconcile)"
        elif [[ "$PHASE" == "NotFound" ]]; then
            log_error "  $model: Not found"
        else
            log_warn "  $model: $PHASE"
        fi
    done

    # Check if AuthPolicy and TokenRateLimitPolicy were created
    echo ""
    log_info "Checking generated Kuadrant policies..."
    sleep 2

    AUTHPOLICY_COUNT=$(kubectl get authpolicy -n "$MODEL_NAMESPACE" -l app.kubernetes.io/managed-by=maas-controller,app.kubernetes.io/part-of=maas-auth-policy 2>/dev/null | wc -l | tr -d ' ')
    # Subtract 1 for header line
    AUTHPOLICY_COUNT=$((AUTHPOLICY_COUNT > 0 ? AUTHPOLICY_COUNT - 1 : 0))

    TRLP_COUNT=$(kubectl get tokenratelimitpolicy -n "$MODEL_NAMESPACE" -l app.kubernetes.io/managed-by=maas-controller,app.kubernetes.io/part-of=maas-subscription 2>/dev/null | wc -l | tr -d ' ')
    # Subtract 1 for header line
    TRLP_COUNT=$((TRLP_COUNT > 0 ? TRLP_COUNT - 1 : 0))

    log_verbose "  AuthPolicies created: $AUTHPOLICY_COUNT (expected: ${#MODEL_ARRAY[@]})"
    log_verbose "  TokenRateLimitPolicies created: $TRLP_COUNT (expected: ${#MODEL_ARRAY[@]})"

    if [[ "$AUTHPOLICY_COUNT" -eq "${#MODEL_ARRAY[@]}" ]] && [[ "$TRLP_COUNT" -eq "${#MODEL_ARRAY[@]}" ]]; then
        log_success "All policies created successfully!"
    else
        log_warn "Not all policies created yet. Controller may still be reconciling."
        log_info "Check maas-controller logs: kubectl logs -n $MAAS_NAMESPACE -l app=maas-controller"
    fi

    echo ""
    log_info "Next steps:"
    log_verbose "  1. Test model access with users in tier '$TIER' groups"
    log_verbose "  2. Validate rate limiting is working as expected"
    log_verbose "  3. Once validated, remove tier annotations from models"
    log_verbose "  4. Remove old gateway-auth-policy and tier-based TokenRateLimitPolicy"
    echo ""

else
    log_info "Next steps:"
    log_verbose "  1. Review generated CRs in: $OUTPUT_DIR"
    log_verbose "  2. Apply to cluster: kubectl apply -f $OUTPUT_DIR/"
    log_verbose "  3. Or run with --apply flag to apply automatically"
    echo ""
fi

log_success "Migration script completed!"
