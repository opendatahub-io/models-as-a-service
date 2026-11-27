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
    
    local dir
    dir=$(dirname "$kustomization_file")
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
    echo -e "${bold}Checking canonical production roots exist${normal}"
    for root in "${CANONICAL_ROOTS[@]}"; do
        local abs_path="$project_root/$root/kustomization.yaml"
        if [[ ! -f "$abs_path" ]]; then
            echo -e "❌ ${bold}MISSING${normal} canonical root: ${underline}$root${normal}"
            exit_code=1
        else
            echo -e "✅ ${underline}$root${normal}"
        fi
    done

    return $exit_code
}

# ---------------------------------------------------------------------------
# Prometheus recording rule and alert unit tests
# ---------------------------------------------------------------------------
# Every *.unit-tests.yaml under deployment/base/observability is executed with promtool,
# so rule changes cannot land without their assertions running. CI installs promtool;
# locally the check is skipped when the binary is absent.

validate_prometheus_rules() {
    local project_root="${1:-$(git rev-parse --show-toplevel)}"
    local rules_dir="$project_root/deployment/base/observability"
    local exit_code=0

    shopt -s nullglob
    local test_files=("$rules_dir"/*.unit-tests.yaml)
    shopt -u nullglob

    if [[ ${#test_files[@]} -eq 0 ]]; then
        return 0
    fi

    if ! command -v promtool >/dev/null 2>&1; then
        if [[ -n "${CI:-}" ]]; then
            echo -e "❌ ${bold}promtool not found${normal} - required to run ${underline}${#test_files[@]}${normal} rule test file(s)"
            return 1
        fi
        echo -e "⏭️  ${bold}Skipping${normal} Prometheus rule tests (promtool not installed)"
        return 0
    fi

    for test_file in "${test_files[@]}"; do
        local relative_path=${test_file#"$project_root/"}
        local message="${bold}Validating${normal} ${underline}$relative_path${normal}"
        echo -n -e "⏳ ${message}"
        if output=$(promtool test rules "$test_file" 2>&1); then
            echo -e "\r✅ ${message}"
        else
            echo -e "\r❌ ${message}"
            echo "$output"
            exit_code=1
        fi
    done

    return $exit_code
}

# When script is not sourced, but directly invoked, run all validations
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
    PROJECT_ROOT=$(git rev-parse --show-toplevel)
    EXIT_CODE=0

    validate_all "$PROJECT_ROOT" || EXIT_CODE=1
    validate_canonical_roots "$PROJECT_ROOT" || EXIT_CODE=1
    validate_prometheus_rules "$PROJECT_ROOT" || EXIT_CODE=1

    exit $EXIT_CODE
fi
