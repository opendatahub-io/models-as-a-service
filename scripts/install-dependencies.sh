#!/usr/bin/env bash

set -euo pipefail

# Install Dependencies Script for MaaS Deployment
# Orchestrates installation of required platform components
# Supports both vanilla Kubernetes and OpenShift deployments

# Component definitions with installation order
COMPONENTS=("istio" "odh" "kserve" "prometheus" "kuadrant"  "grafana")

# OpenShift flag
OCP=false

KUADRANT_VERSION="v1.3.1"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALLERS_DIR="$SCRIPT_DIR/installers"

# Source helper functions
source "$SCRIPT_DIR/deployment-helpers.sh"

get_component_description() {
    case "$1" in
        istio) echo "Service mesh and Gateway API configuration" ;;
        odh) echo "OpenDataHub operator for ML/AI platform (OpenShift only)" ;;
        kserve) 
            if [[ "$OCP" == true ]]; then
                echo "Model serving platform (validates OpenShift Serverless)"
            else
                echo "Model serving platform"
            fi
            ;;
        prometheus) 
            if [[ "$OCP" == true ]]; then
                echo "Observability and metrics collection (validates OpenShift monitoring)"
            else
                echo "Observability and metrics collection (optional)"
            fi
            ;;
        grafana) 
            if [[ "$OCP" == true ]]; then
                echo "Dashboard visualization platform (OpenShift operator)"
            else
                echo "Dashboard visualization platform (not implemented for vanilla Kubernetes)"
            fi
            ;;
        kuadrant) echo "API gateway operators via OLM (Kuadrant, Authorino, Limitador)" ;;
        *) echo "Unknown component" ;;
    esac
}

usage() {
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Install required dependencies for MaaS deployment."
    echo ""
    echo "Options:"
    echo "  --all                    Install all components"
    echo "  --istio                  Install Istio service mesh"
    echo "  --odh                    Install OpenDataHub operator (OpenShift only)"
    echo "  --kserve                 Install KServe model serving platform"
    echo "  --prometheus             Install Prometheus operator"
    echo "  --grafana                Install Grafana dashboard platform"
    echo "  --kuadrant               Install Kuadrant operators via OLM"
    echo "  --ocp                    Use OpenShift-specific handling (validate instead of install)"
    echo "  -h, --help               Show this help message"
    echo ""
    echo "Examples:"
    echo "  $0 --all              # Install all components (vanilla Kubernetes)"
    echo "  $0 --all --ocp         # Validate all components (OpenShift)"
    echo "  $0 --kserve --ocp      # Validate OpenShift Serverless only"
    echo ""
    echo "If no options are provided, interactive mode will prompt for component selection."
    echo ""
    echo "Components are installed in the following order:"
    for component in "${COMPONENTS[@]}"; do
        echo "  - $component: $(get_component_description "$component")"
    done
}

install_component() {
    local component="$1"
    local installer_script="$INSTALLERS_DIR/install-${component}.sh"
    
    # Special handler for ODH (OpenShift only)
    if [[ "$component" == "odh" ]]; then
        if [[ "$OCP" != true ]]; then
            echo "⚠️  ODH is only available on OpenShift clusters, skipping..."
            return 0
        fi
        if [[ -f "$installer_script" ]]; then
            echo "🚀 Installing $component..."
            bash "$installer_script"
        else
            echo "❌ Installer script not found: $installer_script"
            return 1
        fi
        return 0
    fi
    
    # Inline handler for Kuadrant (installed via OLM)
    if [[ "$component" == "kuadrant" ]]; then
        # Ensure kuadrant-system namespace exists
        kubectl create namespace kuadrant-system 2>/dev/null || echo "✅ Namespace kuadrant-system already exists"


        echo "🚀 Creating Kuadrant OperatorGroup..."
        kubectl apply -f - <<EOF
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: kuadrant-operator-group
  namespace: kuadrant-system
spec: {}
EOF

        # Check if the CatalogSource already exists before applying
        if kubectl get catalogsource kuadrant-operator-catalog -n kuadrant-system &>/dev/null; then
            echo "✅ Kuadrant CatalogSource already exists in namespace kuadrant-system, skipping creation."
        else
            echo "🚀 Creating Kuadrant CatalogSource..."
            kubectl apply -f - <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: kuadrant-operator-catalog
  namespace: kuadrant-system
spec:
  displayName: Kuadrant Operators
  grpcPodConfig:
    securityContextConfig: restricted
  image: 'quay.io/kuadrant/kuadrant-operator-catalog:v1.3.1'
  publisher: grpc
  sourceType: grpc
EOF
        fi


        echo "🚀 Installing kuadrant (via OLM Subscription)..."
        kubectl apply -f - <<EOF
  apiVersion: operators.coreos.com/v1alpha1
  kind: Subscription
  metadata:
    name: kuadrant-operator
    namespace: kuadrant-system
  spec:
    channel: stable
    installPlanApproval: Automatic
    name: kuadrant-operator
    source: kuadrant-operator-catalog
    sourceNamespace: kuadrant-system
EOF
        # Wait for kuadrant-operator-controller-manager deployment to exist before waiting for Available condition
        ATTEMPTS=0
        MAX_ATTEMPTS=7
        while true; do

            if kubectl get deployment/kuadrant-operator-controller-manager -n kuadrant-system &>/dev/null; then
                break
            else
                ATTEMPTS=$((ATTEMPTS+1))
                if [[ $ATTEMPTS -ge $MAX_ATTEMPTS ]]; then
                    echo "❌ kuadrant-operator-controller-manager deployment not found after $MAX_ATTEMPTS attempts."
                    return 1
                fi
                echo "⏳ Waiting for kuadrant-operator-controller-manager deployment to be created... (attempt $ATTEMPTS/$MAX_ATTEMPTS)"
                sleep $((10 + 10 * $ATTEMPTS))
            fi
        done

        echo "⏳ Waiting for operators to be ready..."
        kubectl wait --for=condition=Available deployment/kuadrant-operator-controller-manager -n kuadrant-system --timeout=300s
        kubectl wait --for=condition=Available deployment/limitador-operator-controller-manager -n kuadrant-system --timeout=300s
        kubectl wait --for=condition=Available deployment/authorino-operator -n kuadrant-system --timeout=300s

        sleep 5

        # Patch Kuadrant for OpenShift Gateway Controller
        echo "   Patching Kuadrant operator..."
        if ! kubectl -n kuadrant-system get deployment kuadrant-operator-controller-manager -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="ISTIO_GATEWAY_CONTROLLER_NAMES")]}' | grep -q "ISTIO_GATEWAY_CONTROLLER_NAMES"; then
          # Find the actual CSV name instead of hardcoding it
          KUADRANT_CSV=$(find_csv_with_min_version "kuadrant-operator" "$KUADRANT_MIN_VERSION" "kuadrant-system" || echo "")
          if [ -n "$KUADRANT_CSV" ]; then
            kubectl patch csv "$KUADRANT_CSV" -n kuadrant-system --type='json' -p='[
              {
                "op": "add",
                "path": "/spec/install/spec/deployments/0/spec/template/spec/containers/0/env/-",
                "value": {
                  "name": "ISTIO_GATEWAY_CONTROLLER_NAMES",
                  "value": "istio.io/gateway-controller,openshift.io/gateway-controller/v1"
                }
              }
            ]'
            echo "   ✅ Kuadrant operator patched ($KUADRANT_CSV)"
          else
            echo "   ⚠️  Kuadrant CSV not found, skipping patch"
          fi
        else
          echo "   ✅ Kuadrant operator already configured"
        fi

        echo "✅ Successfully installed kuadrant"
        echo ""
        return 0
    fi

    if [[ ! -f "$installer_script" ]]; then
        echo "❌ Installer not found: $installer_script"
        return 1
    fi
    
    if [[ "$OCP" == true ]]; then
        echo "🚀 Setting up $component for OpenShift..."
    else
        echo "🚀 Installing $component..."
    fi
    
    # Pass --ocp flag to scripts that support it
    local script_args=()
    if [[ "$OCP" == true ]] && [[ "$component" == "kserve" || "$component" == "prometheus" || "$component" == "grafana" ]]; then
        script_args+=("--ocp")
    fi
    
    if ! "$installer_script" "${script_args[@]:-""}"; then
        if [[ "$OCP" == true ]]; then
            echo "❌ Failed to set up $component for OpenShift"
        else
            echo "❌ Failed to install $component"
        fi
        return 1
    fi
    
    if [[ "$OCP" == true ]]; then
        echo "✅ Successfully set up $component for OpenShift"
    else
        echo "✅ Successfully installed $component"
    fi
    echo ""
}

install_all() {
    if [[ "$OCP" == true ]]; then
        echo "🔧 Setting up all MaaS dependencies for OpenShift..."
    else
        echo "🔧 Installing all MaaS dependencies..."
    fi
    echo ""
    
    for component in "${COMPONENTS[@]}"; do
        install_component "$component"
    done
    
    if [[ "$OCP" == true ]]; then
        echo "🎉 All components set up successfully for OpenShift!"
    else
        echo "🎉 All components installed successfully!"
    fi
}

interactive_install() {
    echo "MaaS Dependency Installer"
    echo "========================"
    echo ""
    if [[ "$OCP" == true ]]; then
        echo "The following components will be set up for OpenShift:"
    else
        echo "The following components will be installed:"
    fi
    for component in "${COMPONENTS[@]}"; do
        echo "  - $component: $(get_component_description "$component")"
    done
    echo ""
    
    if [[ "$OCP" == true ]]; then
        read -p "Set up all components for OpenShift? (y/N): " -n 1 -r
    else
        read -p "Install all components? (y/N): " -n 1 -r
    fi
    echo ""
    
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        install_all
    else
        echo "Setup cancelled."
        exit 0
    fi
}

# Parse command line arguments
# Handle special case: only --ocp flag provided (should go to interactive mode)
if [[ $# -eq 1 ]] && [[ "$1" == "--ocp" ]]; then
    OCP=true
    interactive_install
    exit 0
elif [[ $# -eq 0 ]]; then
    # No arguments - use interactive mode
    interactive_install
    exit 0
fi

# First pass: check for --ocp flag (scan without consuming arguments)
for arg in "$@"; do
    if [[ "$arg" == "--ocp" ]]; then
        OCP=true
        break
    fi
done

# Second pass: process component and action flags
COMPONENT_SELECTED=false
while [[ $# -gt 0 ]]; do
    case $1 in
        --all)
            install_all
            exit 0
            ;;
        --istio)
            install_component "istio"
            COMPONENT_SELECTED=true
            ;;
        --odh)
            install_component "odh"
            COMPONENT_SELECTED=true
            ;;
        --kserve)
            install_component "kserve"
            COMPONENT_SELECTED=true
            ;;
        --prometheus)
            install_component "prometheus"
            COMPONENT_SELECTED=true
            ;;
        --grafana)
            install_component "grafana"
            COMPONENT_SELECTED=true
            ;;
        --ocp)
            # Already processed in first pass, skip
            ;;
        --kuadrant)
            install_component "kuadrant"
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            echo ""
            usage
            exit 1
            ;;
    esac
    shift
done

# Show success message if components were installed
if [[ "$COMPONENT_SELECTED" == true ]]; then
    if [[ "$OCP" == true ]]; then
        echo "🎉 Selected components set up successfully for OpenShift!"
    else
        echo "🎉 Selected components installed successfully!"
    fi
fi

