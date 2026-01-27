#!/bin/bash
#
# deploy-rhoai-stable.sh - Deploy Red Hat OpenShift AI v3 or OpenDataHub with Models-as-a-Service capability
#
# DESCRIPTION:
#   This script automates the deployment of Red Hat OpenShift AI (RHOAI) v3 or OpenDataHub (ODH)
#   along with its required prerequisites and the Models-as-a-Service (MaaS) capability.
#
#   The deployment includes:
#   - cert-manager
#   - Leader Worker Set (LWS)
#   - Red Hat Connectivity Link (Kuadrant)
#   - RHOAI v3 or ODH with KServe for model serving
#   - MaaS capability (core components managed by the operator)
#   - MaaS Gateway (not managed by operator)
#   - Rate Limit and Token Limit policies (not managed by operator)
#
# PREREQUISITES:
#   - OpenShift cluster v4.19.9+
#   - Cluster administrator privileges
#   - kubectl CLI tool configured and connected to cluster
#   - kustomize tool available in PATH (for usage policies)
#   - jq tool for JSON processing
#
# USAGE:
#   ./deploy-rhoai-stable.sh [OPTIONS]
#
# OPTIONS:
#   -h, --help              Show this help message and exit (use -v for advanced options)
#   -t, --operator-type     Which operator to install: "odh" (default) or "rhoai"
#   -r, --maas-ref          Git ref for MaaS manifests (default: main)
#   -c, --cert-name         TLS certificate secret name (default: auto-detected)
#
# ADVANCED OPTIONS (use --help -v to see these):
#   -b, --operator-catalog  Custom operator catalog/index image
#   --operator-image        Custom operator image (patches CSV after installation)
#   --channel               Operator channel to use
#
# ENVIRONMENT VARIABLES:
#   Options can also be set via environment variables:
#   OPERATOR_TYPE, MAAS_REF, CERT_NAME, OPERATOR_CATALOG, OPERATOR_IMAGE, OPERATOR_CHANNEL
#   CLI arguments take precedence over environment variables.
#
# EXAMPLES:
#   ./deploy-rhoai-stable.sh                           # Install ODH (default)
#   ./deploy-rhoai-stable.sh --operator-type rhoai     # Install RHOAI
#   ./deploy-rhoai-stable.sh -t rhoai -r v1.0.0        # Install RHOAI with specific ref
#   OPERATOR_TYPE=rhoai ./deploy-rhoai-stable.sh       # Install RHOAI via env var
#
# NOTES:
#   - The script is idempotent for most operations
#   - Core MaaS components (deployment, auth policy) are managed by the RHOAI/ODH operator
#   - Gateway and usage policies are installed separately by this script

set -e

# Source common helper functions
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/deployment-helpers.sh"

# Show help message
show_help() {
  local verbose="${1:-false}"
  
  cat << EOF
Deploy Red Hat OpenShift AI v3 or OpenDataHub with Models-as-a-Service capability

Usage: $(basename "$0") [OPTIONS]

Options:
  -h, --help              Show this help message and exit (use -v for advanced options)
  -t, --operator-type     Which operator to install: "odh" (default) or "rhoai"
  -r, --maas-ref          Git ref for MaaS manifests (default: main)
  -c, --cert-name         TLS certificate secret name (default: auto-detected)

Environment Variables:
  OPERATOR_TYPE           Same as --operator-type
  MAAS_REF                Same as --maas-ref
  CERT_NAME               Same as --cert-name

Examples:
  $(basename "$0")                           # Install ODH (default)
  $(basename "$0") --operator-type rhoai     # Install RHOAI
  $(basename "$0") -t rhoai -r v1.0.0        # Install RHOAI with specific git ref
  OPERATOR_TYPE=rhoai $(basename "$0")       # Install RHOAI via env var

EOF

  if [[ "$verbose" == "true" ]]; then
    cat << EOF
Advanced Options (for development/testing):
  -b, --operator-catalog  Custom operator catalog/index image to use instead of default catalog
                          (e.g., quay.io/opendatahub/opendatahub-operator-catalog:latest)
                          NOTE: This must be a CATALOG image, not a bundle image!
  --operator-image        Custom operator image to use (patches the CSV after installation)
                          (e.g., quay.io/opendatahub/opendatahub-operator:pr-1234)
  --channel               Operator channel to use (default: fast-3 for catalog, fast for custom)

Advanced Environment Variables:
  OPERATOR_CATALOG        Same as --operator-catalog
  OPERATOR_IMAGE          Same as --operator-image
  OPERATOR_CHANNEL        Same as --channel

Advanced Examples:
  $(basename "$0") -t odh -b quay.io/opendatahub/opendatahub-operator-catalog:pr-3063 --channel fast
  $(basename "$0") -t odh --operator-image quay.io/opendatahub/opendatahub-operator:pr-1234

EOF
  else
    echo "Use '$(basename "$0") --help -v' to see advanced options for custom catalogs and images."
    echo ""
  fi

  exit 0
}

# Parse command line arguments
SHOW_HELP=false
VERBOSE_HELP=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help)
      SHOW_HELP=true
      shift
      ;;
    -v|--verbose)
      VERBOSE_HELP=true
      shift
      ;;
    -t|--operator-type)
      OPERATOR_TYPE="$2"
      shift 2
      ;;
    -r|--maas-ref)
      MAAS_REF="$2"
      shift 2
      ;;
    -c|--cert-name)
      CERT_NAME="$2"
      shift 2
      ;;
    -b|--operator-catalog)
      OPERATOR_CATALOG="$2"
      shift 2
      ;;
    --operator-image)
      OPERATOR_IMAGE="$2"
      shift 2
      ;;
    --channel)
      OPERATOR_CHANNEL="$2"
      shift 2
      ;;
    -*)
      echo "ERROR: Unknown option: $1"
      echo "Use --help for usage information"
      exit 1
      ;;
    *)
      echo "ERROR: Unexpected argument: $1"
      echo "Use --help for usage information"
      exit 1
      ;;
  esac
done

# Handle help after parsing all args (so -h -v works in any order)
if [[ "$SHOW_HELP" == "true" ]]; then
  show_help "$VERBOSE_HELP"
fi

# Set defaults for any unset variables
: "${OPERATOR_TYPE:=odh}"
: "${MAAS_REF:=main}"
# CERT_NAME will be detected dynamically later, but can be overridden via env var or CLI arg

# Validate OPERATOR_TYPE
if [[ "$OPERATOR_TYPE" != "rhoai" && "$OPERATOR_TYPE" != "odh" ]]; then
  echo "ERROR: OPERATOR_TYPE must be 'rhoai' or 'odh'. Got: $OPERATOR_TYPE"
  exit 1
fi

echo "========================================="
echo "Deploying with operator: ${OPERATOR_TYPE}"
if [[ "$OPERATOR_TYPE" == "rhoai" ]]; then
  echo "NOTE: RHOAI may not support all features if using an older operator version."
  echo "      If you encounter errors, try using ODH: OPERATOR_TYPE=odh $0"
fi
if [[ -n "${OPERATOR_CATALOG:-}" ]]; then
  echo "Using custom catalog: ${OPERATOR_CATALOG}"
fi
if [[ -n "${OPERATOR_IMAGE:-}" ]]; then
  echo "Using custom operator image: ${OPERATOR_IMAGE}"
fi
echo "========================================="

# Determine applications namespace based on operator type
get_applications_namespace() {
  if [[ "$OPERATOR_TYPE" == "rhoai" ]]; then
    echo "redhat-ods-applications"
  else
    echo "opendatahub"
  fi
}

# Set applications namespace variable
APPLICATIONS_NS=$(get_applications_namespace)

deploy_certmanager() {
  local certmanager_exists=$(checksubscriptionexists openshift-marketplace redhat-operators openshift-cert-manager-operator)
  if [[ $certmanager_exists -ne "0" ]]; then
    echo "* The cert-manager operator is present in the cluster. Skipping installation."
    return 0
  fi

  echo
  echo "* Installing cert-manager operator..."

  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Namespace
metadata:
  name: cert-manager-operator
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: cert-manager-operator
  namespace: cert-manager-operator
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: openshift-cert-manager-operator
  namespace: cert-manager-operator
spec:
  channel: stable-v1
  installPlanApproval: Automatic
  name: openshift-cert-manager-operator
  source: redhat-operators
  sourceNamespace: openshift-marketplace
EOF

  waitsubscriptioninstalled "cert-manager-operator" "openshift-cert-manager-operator"
}

deploy_lws() {
  local lws_exists=$(checksubscriptionexists openshift-marketplace redhat-operators leader-worker-set)
  if [[ $lws_exists -ne "0" ]]; then
    echo "* The LWS operator is present in the cluster. Skipping installation."
    return 0
  fi

  echo
  echo "* Installing LWS operator..."

  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Namespace
metadata:
  name: openshift-lws-operator
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: leader-worker-set
  namespace: openshift-lws-operator
spec:
  targetNamespaces:
  - openshift-lws-operator
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: leader-worker-set
  namespace: openshift-lws-operator
spec:
  channel: stable-v1.0
  installPlanApproval: Automatic
  name: leader-worker-set
  source: redhat-operators
  sourceNamespace: openshift-marketplace
EOF

  waitsubscriptioninstalled "openshift-lws-operator" "leader-worker-set"
  echo "* Setting up LWS instance and letting it deploy asynchronously."

  cat <<EOF | kubectl apply -f -
apiVersion: operator.openshift.io/v1
kind: LeaderWorkerSetOperator
metadata:
  name: cluster
  namespace: openshift-lws-operator
spec:
  managementState: Managed
EOF
}

deploy_rhcl() {
  echo
  echo "* Initializing Gateway API provider..."

  cat <<EOF | kubectl apply -f -
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: openshift-default
spec:
  controllerName: "openshift.io/gateway-controller/v1"
EOF

  echo "  * Waiting for GatewayClass openshift-default to transition to Accepted status..."
  kubectl wait --timeout=300s --for=condition=Accepted=True GatewayClass/openshift-default

  local rhcl_exists=$(checksubscriptionexists openshift-marketplace redhat-operators rhcl-operator)
  if [[ $rhcl_exists -ne "0" ]]; then
    echo "* The RHCL operator is present in the cluster. Skipping installation."
    echo "  WARNING: Creating an instance of RHCL is also skipped."
    return 0
  fi

  echo
  echo "* Installing RHCL operator..."

  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Namespace
metadata:
  name: kuadrant-system
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: kuadrant-operator-group
  namespace: kuadrant-system
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: rhcl-operator
  namespace: kuadrant-system
spec:
  channel: stable
  installPlanApproval: Automatic
  name: rhcl-operator
  source: redhat-operators
  sourceNamespace: openshift-marketplace
EOF

  waitsubscriptioninstalled "kuadrant-system" "rhcl-operator"
  echo "* Setting up RHCL instance..."

  cat <<EOF | kubectl apply -f -
apiVersion: kuadrant.io/v1beta1
kind: Kuadrant
metadata:
  name: kuadrant
  namespace: kuadrant-system
EOF
}

deploy_rhoai() {
  # Check if ODH is already installed - can't have both
  local odh_csv_count=$(checkcsvexists "opendatahub-operator")
  if [[ $odh_csv_count -ne "0" ]]; then
    echo "ERROR: OpenDataHub operator is already installed in the cluster."
    echo "       Cannot install RHOAI when ODH is present. Please uninstall ODH first,"
    echo "       or use OPERATOR_TYPE=odh to continue with ODH."
    exit 1
  fi

  # Check if RHOAI is already installed
  local rhoai_csv_count=$(checkcsvexists "rhods-operator")
  if [[ $rhoai_csv_count -ne "0" ]]; then
    echo "* The RHOAI operator is present in the cluster. Skipping installation."
    return 0
  fi

  echo
  echo "* Installing RHOAI v3 operator..."

  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Namespace
metadata:
  name: redhat-ods-operator
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: rhoai3-operatorgroup
  namespace: redhat-ods-operator
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: rhoai3-operator
  namespace: redhat-ods-operator
spec:
  channel: fast-3.x
  installPlanApproval: Automatic
  name: rhods-operator
  source: redhat-operators
  sourceNamespace: openshift-marketplace
EOF

  waitsubscriptioninstalled "redhat-ods-operator" "rhoai3-operator"
}

deploy_odh() {
  # Check if RHOAI is already installed - can't have both
  local rhoai_csv_count=$(checkcsvexists "rhods-operator")
  if [[ $rhoai_csv_count -ne "0" ]]; then
    echo "ERROR: RHOAI operator is already installed in the cluster."
    echo "       Cannot install ODH when RHOAI is present. Please uninstall RHOAI first,"
    echo "       or use OPERATOR_TYPE=rhoai to continue with RHOAI."
    exit 1
  fi

  # Check if ODH is already installed
  local odh_csv_count=$(checkcsvexists "opendatahub-operator")
  if [[ $odh_csv_count -ne "0" ]]; then
    echo "* The ODH operator is present in the cluster. Skipping installation."
    return 0
  fi

  echo
  echo "* Installing OpenDataHub operator..."

  # Determine catalog source and channel based on whether a custom catalog is specified
  local catalog_source="community-operators"
  local catalog_namespace="openshift-marketplace"
  local channel="${OPERATOR_CHANNEL:-fast-3}"

  if [[ -n "${OPERATOR_CATALOG:-}" ]]; then
    echo "* Using custom operator catalog: ${OPERATOR_CATALOG}"
    create_custom_catalogsource "odh-custom-catalog" "openshift-marketplace" "${OPERATOR_CATALOG}"
    catalog_source="odh-custom-catalog"
    # Custom catalogs typically use 'fast' channel instead of 'fast-3'
    channel="${OPERATOR_CHANNEL:-fast}"
  fi

  echo "* Using channel: ${channel}"

  # Install in dedicated namespace to avoid conflicts with other operators
  # that may have Manual approval (e.g., servicemeshoperator3 in openshift-operators)
  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Namespace
metadata:
  name: odh-operator
  labels:
    openshift.io/cluster-monitoring: "true"
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: odh-operator-group
  namespace: odh-operator
spec: {}
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: opendatahub-operator
  namespace: odh-operator
spec:
  channel: ${channel}
  installPlanApproval: Automatic
  name: opendatahub-operator
  source: ${catalog_source}
  sourceNamespace: ${catalog_namespace}
EOF

  waitsubscriptioninstalled "odh-operator" "opendatahub-operator"

  # If a custom operator image is specified, patch the CSV
  if [[ -n "${OPERATOR_IMAGE:-}" ]]; then
    echo "* Patching operator with custom image: ${OPERATOR_IMAGE}"
    
    # Get the CSV name
    local csv_name
    csv_name=$(kubectl get csv -n odh-operator -o jsonpath='{.items[?(@.metadata.name)].metadata.name}' | tr ' ' '\n' | grep "^opendatahub-operator" | head -1)
    
    if [[ -z "$csv_name" ]]; then
      echo "  WARNING: Could not find ODH CSV to patch. Skipping image override."
    else
      echo "  * Found CSV: ${csv_name}"
      # Patch the deployment spec in the CSV to use the custom image
      kubectl patch csv "$csv_name" -n odh-operator --type='json' -p="[
        {\"op\": \"replace\", \"path\": \"/spec/install/spec/deployments/0/spec/template/spec/containers/0/image\", \"value\": \"${OPERATOR_IMAGE}\"}
      ]"
      echo "  * CSV patched with custom image"
      
      # Wait for the operator deployment to be updated
      echo "  * Waiting for operator deployment to update..."
      sleep 5
      kubectl rollout status deployment/opendatahub-operator-controller-manager -n odh-operator --timeout=120s 2>/dev/null || true
    fi
  fi
}

deploy_dscinitialization() {
  # Check if a DSCInitialization already exists, skip creation if so
  if kubectl get dscinitialization -A --no-headers 2>/dev/null | grep -q .; then
    echo "* A DSCInitialization already exists in the cluster. Skipping creation."
    return 0
  fi

  echo "* Setting up DSCInitialization..."

  # Use server-side apply to handle race conditions with operator creating DSCInitialization
  cat <<EOF | kubectl apply --server-side=true -f -
apiVersion: dscinitialization.opendatahub.io/v2
kind: DSCInitialization
metadata:
  name: default-dsci
  labels:
    app.kubernetes.io/name: dscinitialization
spec:
  applicationsNamespace: ${APPLICATIONS_NS}
  monitoring:
    managementState: Managed
    namespace: ${APPLICATIONS_NS}
    metrics: {}
  trustedCABundle:
    managementState: Managed
EOF
}

deploy_datasciencecluster() {
  # Check if a DataScienceCluster already exists, skip creation if so
  if kubectl get datasciencecluster -A --no-headers 2>/dev/null | grep -q .; then
    echo "* A DataScienceCluster already exists in the cluster. Skipping creation."
    return 0
  fi
  echo "* Setting up DataScienceCluster with MaaS capability..."

  # Use server-side apply to handle race conditions with operator
  cat <<EOF | kubectl apply --server-side=true -f -
apiVersion: datasciencecluster.opendatahub.io/v2
kind: DataScienceCluster
metadata:
  name: default-dsc
spec:
  components:
    # Components required for MaaS:
    kserve:
      managementState: Managed
      rawDeploymentServiceConfig: Headed

      # MaaS capability - managed by operator
      modelsAsService:
        managementState: Managed
    
    # Components recommended for MaaS:
    dashboard:
      managementState: Managed
    
    llamastackoperator:
      managementState: Removed 
EOF
}

# ========================================
# Main Deployment Flow
# ========================================

echo "## Installing prerequisites"

deploy_certmanager
deploy_lws
deploy_rhcl

echo
echo "## Installing $(echo "$OPERATOR_TYPE" | tr '[:lower:]' '[:upper:]') operator"

if [[ "$OPERATOR_TYPE" == "rhoai" ]]; then
  deploy_rhoai
else
  deploy_odh
fi

echo
echo "## Configuring DSCInitialization and DataScienceCluster"
deploy_dscinitialization
deploy_datasciencecluster

echo
echo "## Waiting for operator to initialize..."
wait_for_namespace "$APPLICATIONS_NS" 300

echo
echo "## Installing MaaS components (not managed by operator)"

export CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
export AUD="$(kubectl create token default --duration=10m 2>/dev/null | cut -d. -f2 | jq -Rr '@base64d | fromjson | .aud[0]' 2>/dev/null)"

echo "* Cluster domain: ${CLUSTER_DOMAIN}"
echo "* Cluster audience: ${AUD}"

# Detect TLS certificate if not explicitly set
if [[ -z "${CERT_NAME:-}" ]]; then
  echo "* Detecting TLS certificate secret..."

  # Try 1: Get certificate from GatewayConfig (operator-managed, most reliable)
  if kubectl get gatewayconfig.services.platform.opendatahub.io default-gateway &>/dev/null; then
    GATEWAY_CERT_TYPE=$(kubectl get gatewayconfig.services.platform.opendatahub.io default-gateway \
      -o jsonpath='{.spec.certificate.type}' 2>/dev/null)

    if [[ "$GATEWAY_CERT_TYPE" == "Provided" ]]; then
      CERT_NAME=$(kubectl get gatewayconfig.services.platform.opendatahub.io default-gateway \
        -o jsonpath='{.spec.certificate.secretName}' 2>/dev/null)
      if [[ -n "$CERT_NAME" ]]; then
        echo "  * Found certificate from GatewayConfig (Provided): ${CERT_NAME}"
      fi
    elif [[ "$GATEWAY_CERT_TYPE" == "OpenshiftDefaultIngress" ]]; then
      # Try to get the default ingress certificate
      CERT_NAME=$(kubectl get ingresscontroller default -n openshift-ingress-operator \
        -o jsonpath='{.spec.defaultCertificate.name}' 2>/dev/null)
      if [[ -n "$CERT_NAME" ]]; then
        echo "  * Found certificate from IngressController: ${CERT_NAME}"
      fi
    fi
  fi

  # Try 2: Check known certificate secret names (RHOAI/ODH defaults)
  if [[ -z "$CERT_NAME" ]]; then
    CERT_CANDIDATES=("data-science-gateway-service-tls" "default-gateway-cert")
    for cert in "${CERT_CANDIDATES[@]}"; do
      if kubectl get secret -n openshift-ingress "$cert" &>/dev/null; then
        CERT_NAME="$cert"
        echo "  * Found TLS certificate secret in openshift-ingress: ${cert}"
        break
      fi
    done
  fi

  # Try 3: Get certificate from router deployment (fallback for default cluster cert)
  if [[ -z "$CERT_NAME" ]]; then
    CERT_NAME=$(kubectl get deployment router-default -n openshift-ingress \
      -o jsonpath='{.spec.template.spec.volumes[?(@.name=="default-certificate")].secret.secretName}' 2>/dev/null)
    if [[ -n "$CERT_NAME" ]]; then
      echo "  * Found certificate from router deployment: ${CERT_NAME}"
    fi
  fi

  # Warning if no certificate found
  if [[ -z "$CERT_NAME" ]]; then
    echo "  ⚠️  WARNING: No TLS certificate found. HTTPS listener will not be configured."
    echo "     You can specify a certificate with: --cert-name <secret-name>"
    echo "     Or set CERT_NAME environment variable."
  fi
fi

export CERT_NAME
echo "* TLS certificate secret: ${CERT_NAME:-<none>}"

echo
echo "## Installing MaaS Gateway"
echo "* Deploying maas-default-gateway..."
kubectl apply --server-side=true \
  -f <(kustomize build "https://github.com/opendatahub-io/models-as-a-service.git/deployment/base/networking/maas?ref=${MAAS_REF}" | \
       envsubst '$CLUSTER_DOMAIN $CERT_NAME')

echo
echo "## Applying usage policies (RateLimit and TokenRateLimit)"
echo "* Deploying rate-limit and token-limit policies..."
kubectl apply --server-side=true \
  -f <(kustomize build "https://github.com/opendatahub-io/models-as-a-service.git/deployment/base/policies/usage-policies?ref=${MAAS_REF}")

# Fix audience for ROSA/non-standard clusters
# =====================================================
# Background:
# - Hypershift/ROSA clusters use custom OIDC providers with non-standard audiences
# - Default AuthPolicy expects audience: https://kubernetes.default.svc
# - Service account tokens from these clusters have different audiences
# - Without this patch, authentication fails with HTTP 401
#
# Problem:
# - When modelsAsService.managementState: Managed, the operator creates AuthPolicy
# - Operator's reconciliation loop may revert manual patches back to defaults
# - This causes authentication to break after initial successful patching
#
# Solution:
# - Annotate AuthPolicies with opendatahub.io/managed=false to prevent reconciliation
# - Patch with cluster-specific audience
# - Verify the patch persisted after giving operator time to reconcile
# - Warn user if operator reverts the change
# =====================================================
if [[ -n "$AUD" && "$AUD" != "https://kubernetes.default.svc" ]]; then
  echo
  echo "## Configuring audience for non-standard cluster"
  echo "* Detected non-default audience: ${AUD}"

  if wait_for_resource "authpolicy" "maas-api-auth-policy" "$APPLICATIONS_NS" 300; then
    # Step 1: Annotate to prevent operator reconciliation
    # This tells the operator to not manage this resource, allowing our custom config to persist
    kubectl annotate authpolicy maas-api-auth-policy -n "$APPLICATIONS_NS" \
      opendatahub.io/managed="false" --overwrite 2>/dev/null || true

    # Step 2: Patch main AuthPolicy with cluster-specific audience (accepts both OpenShift tokens and SA tokens)
    # The custom audience allows service account tokens from Hypershift/ROSA to be validated
    kubectl patch authpolicy maas-api-auth-policy -n "$APPLICATIONS_NS" --type=merge --patch-file <(echo "
spec:
  rules:
    authentication:
      default:
        kubernetesTokenReview:
          audiences:
            - $AUD
            - maas-default-gateway-sa")
    echo "  * AuthPolicy 'maas-api-auth-policy' patched with custom audience."

    # Step 3: Verify the patch persisted
    # Wait briefly to allow operator reconciliation cycle to run, then check if our patch survived
    sleep 3
    ACTUAL_AUD=$(kubectl get authpolicy maas-api-auth-policy -n "$APPLICATIONS_NS" \
      -o jsonpath='{.spec.rules.authentication.cluster-identity.kubernetesTokenReview.audiences[0]}' 2>/dev/null || echo "")
    if [[ "$ACTUAL_AUD" == "$AUD" ]]; then
      echo "  * Verified: Custom audience configuration persisted"
    else
      echo "  ⚠️  WARNING: AuthPolicy audience may have been reverted to: ${ACTUAL_AUD}"
      echo "     This may cause authentication failures on Hypershift/ROSA clusters"
      echo "     The operator might be reconciling the AuthPolicy. Consider disabling operator management."
    fi
  else
    echo "  WARNING: Could not find AuthPolicy 'maas-api-auth-policy' to patch. Skipping audience configuration."
  fi

  # Patch token issuance AuthPolicy (accepts only OpenShift tokens - prevents token-to-token issuance)
  if wait_for_resource "authpolicy" "maas-api-token-issuance-auth-policy" "$APPLICATIONS_NS" 60; then
    kubectl annotate authpolicy maas-api-token-issuance-auth-policy -n "$APPLICATIONS_NS" \
      opendatahub.io/managed="false" --overwrite 2>/dev/null || true

    kubectl patch authpolicy maas-api-token-issuance-auth-policy -n "$APPLICATIONS_NS" --type=merge --patch-file <(echo "
spec:
  rules:
    authentication:
      default:
        kubernetesTokenReview:
          audiences:
            - $AUD")
    echo "  * AuthPolicy 'maas-api-token-issuance-auth-policy' patched with custom audience."
  else
    echo "  WARNING: Could not find AuthPolicy 'maas-api-token-issuance-auth-policy' to patch. Skipping."
  fi
fi

echo
echo "## Observability Setup (Optional)"
echo "* NOTE: Observability (Prometheus/Grafana integration) is not installed by default."
echo "* To enable observability, apply the observability manifests:"
echo "   kubectl apply --server-side=true \\"
echo "     -f <(kustomize build \"https://github.com/opendatahub-io/models-as-a-service.git/deployment/base/observability?ref=${MAAS_REF}\")"

echo ""
echo "========================================="
echo "Deployment is complete."
echo ""
echo "Next Steps:"
echo "1. Deploy a sample model:"
echo "   kubectl create namespace llm"
echo "   kustomize build 'https://github.com/opendatahub-io/models-as-a-service.git/docs/samples/models/simulator?ref=${MAAS_REF}' | kubectl apply -f -"
echo ""
echo "2. Get Gateway endpoint:"
echo "   CLUSTER_DOMAIN=\$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}' -n openshift-ingress)"
echo "   HOST=\"maas.\${CLUSTER_DOMAIN}\""
echo ""
echo "3. Get authentication token:"
echo "   TOKEN_RESPONSE=\$(curl -sSk  -H \"Authorization: Bearer \$(oc whoami -t)\" --json '{\"expiration\": \"10m\"}' \"\${HOST}/maas-api/v1/tokens\")"
echo "   TOKEN=\$(echo \$TOKEN_RESPONSE | jq -r .token)"
echo "   echo \$TOKEN"
echo ""
echo ""
echo "4. Test model endpoint:"
echo "   MODELS=\$(curl -sSk \${HOST}/maas-api/v1/models -H \"Content-Type: application/json\" -H \"Authorization: Bearer \$TOKEN\" | jq -r .)"
echo "   MODEL_NAME=\$(echo \$MODELS | jq -r '.data[0].id')"
echo "   MODEL_URL=\"\${HOST}/llm/facebook-opt-125m-simulated/v1/chat/completions\" # Note: This may be different for your model"
echo "   curl -sSk -H \"Authorization: Bearer \$TOKEN\" -H \"Content-Type: application/json\" -d \"{\\\"model\\\": \\\"\${MODEL_NAME}\\\", \\\"prompt\\\": \\\"Hello\\\", \\\"max_tokens\\\": 50}\" \"\${MODEL_URL}\""
echo ""
echo "5. Test authorization limiting (no token 401 error):"
echo "   curl -sSk -H \"Content-Type: application/json\" -d \"{\\\"model\\\": \\\"\${MODEL_NAME}\\\", \\\"prompt\\\": \\\"Hello\\\", \\\"max_tokens\\\": 50}\" \"\${MODEL_URL}\" -v"
echo ""
echo "6. Test rate limiting (200 OK followed by 429 Rate Limit Exceeded after about 4 requests):"
echo "   for i in {1..16}; do curl -sSk -o /dev/null -w \"%{http_code}\\n\" -H \"Authorization: Bearer \$TOKEN\" -H \"Content-Type: application/json\" -d \"{\\\"model\\\": \\\"\${MODEL_NAME}\\\", \\\"prompt\\\": \\\"Hello\\\", \\\"max_tokens\\\": 50}\" \"\${MODEL_URL}\"; done"
echo ""
echo "7. Run validation script (Runs all the checks again):"
echo "   curl https://raw.githubusercontent.com/opendatahub-io/models-as-a-service/refs/heads/${MAAS_REF}/scripts/validate-deployment.sh | sh -v -"
echo ""
echo "8. Check metrics generation:"
echo "   kubectl port-forward -n kuadrant-system svc/limitador-limitador 8080:8080 &"
echo "   curl http://localhost:8080/metrics | grep -E '(authorized_hits|authorized_calls|limited_calls)'"
echo ""
echo "9. Access Prometheus to view metrics:"
echo "   kubectl port-forward -n openshift-monitoring svc/prometheus-k8s 9090:9091 &"
echo "   # Open http://localhost:9090 in browser and search for: authorized_hits, authorized_calls, limited_calls"
echo ""
