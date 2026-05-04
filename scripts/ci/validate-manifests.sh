#!/bin/bash

set -euo pipefail

bold='\033[1m'
normal='\033[0m'
underline='\033[4m'

is_kustomize_component() {
    local kustomization_file="$1"
    grep -q "^kind: Component" "$kustomization_file" 2>/dev/null
}

validate_kustomization() {
    local kustomization_file="$1"
    local project_root="${2:-$(git rev-parse --show-toplevel)}"
    
    local dir=$(dirname "$kustomization_file")
    local relative_path=${kustomization_file#"$project_root/"}
    local message="${bold}Validating${normal} ${underline}$relative_path${normal}"
    
    # Skip Component files - they can't be built standalone, they must be included via 'components:' in another Kustomization
    if is_kustomize_component "$kustomization_file"; then
        echo -e "⏭️  ${bold}Skipping${normal} ${underline}$relative_path${normal} (Component - cannot be validated standalone)"
        return 0
    fi
    
    echo -n -e "⏳ ${message}"
    if output=$(kustomize build --stack-trace --load-restrictor LoadRestrictionsNone "$dir" 2>&1); then
        echo -e "\r✅ ${message}"
        return 0
    else
        echo -e "\r❌ ${message}"
        echo "$output"
        return 1
    fi
}

validate_all() {
    local project_root="${1:-$(git rev-parse --show-toplevel)}"
    local exit_code=0
    
    while IFS= read -r -d '' kustomization_file; do
        if ! validate_kustomization "$kustomization_file" "$project_root"; then
            exit_code=1
        fi
    done < <(find "$project_root" -name "kustomization.yaml" -type f -print0)
    
    return $exit_code
}

# ---------------------------------------------------------------------------
# Canonical root validation
# ---------------------------------------------------------------------------
# These are the two production kustomize roots. Failures here are critical.

CANONICAL_ROOTS=(
    "deployment/base/maas-controller/default"
    "maas-api/deploy/overlays/odh"
)

validate_canonical_roots() {
    local project_root="${1:-$(git rev-parse --show-toplevel)}"
    local exit_code=0

    echo ""
    echo -e "${bold}Validating canonical production roots${normal}"
    for root in "${CANONICAL_ROOTS[@]}"; do
        local abs_path="$project_root/$root/kustomization.yaml"
        if [[ ! -f "$abs_path" ]]; then
            echo -e "❌ ${bold}MISSING${normal} canonical root: ${underline}$root${normal}"
            exit_code=1
            continue
        fi
        if ! validate_kustomization "$abs_path" "$project_root"; then
            exit_code=1
        fi
    done

    return $exit_code
}

# ---------------------------------------------------------------------------
# params.env consistency check
# ---------------------------------------------------------------------------
# Shared keys between the source-of-truth (deployment/overlays/odh/params.env)
# and the tenant overlay copy must have identical values.

validate_params_env_consistency() {
    local project_root="${1:-$(git rev-parse --show-toplevel)}"
    local source="$project_root/deployment/overlays/odh/params.env"
    local tenant="$project_root/maas-api/deploy/overlays/odh/params.env"

    echo ""
    echo -e "${bold}Validating params.env consistency${normal}"

    if [[ ! -f "$source" ]]; then
        echo -e "❌ Source-of-truth params.env not found: $source"
        return 1
    fi
    if [[ ! -f "$tenant" ]]; then
        echo -e "❌ Tenant overlay params.env not found: $tenant"
        return 1
    fi

    local exit_code=0

    # For each key=value in the tenant copy, verify it matches the source
    while IFS='=' read -r key value; do
        [[ -z "$key" || "$key" == \#* ]] && continue
        local source_value
        source_value=$(grep -m1 "^${key}=" "$source" 2>/dev/null | cut -d= -f2-)
        if [[ -n "$source_value" && "$source_value" != "$value" ]]; then
            echo -e "❌ Key ${bold}${key}${normal} differs:"
            echo "     source: $source_value"
            echo "     tenant: $value"
            exit_code=1
        fi
    done < <(grep -v '^\s*#' "$tenant" | grep -v '^\s*$')

    if [[ $exit_code -eq 0 ]]; then
        echo -e "✅ params.env shared keys are consistent"
    fi

    return $exit_code
}

# When script is not sourced, but directly invoked, run all validations
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    PROJECT_ROOT=$(git rev-parse --show-toplevel)
    EXIT_CODE=0

    validate_all "$PROJECT_ROOT" || EXIT_CODE=1
    validate_canonical_roots "$PROJECT_ROOT" || EXIT_CODE=1
    validate_params_env_consistency "$PROJECT_ROOT" || EXIT_CODE=1

    exit $EXIT_CODE
fi
