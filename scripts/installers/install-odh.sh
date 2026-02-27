#!/bin/bash

# OpenDataHub Installation Script
# This script installs ODH operator and creates necessary resources in the correct order

set -e


# TODO: For now, default to DEV_INSTALL, because no suitable ODH version for MaaS has
# TODO: been released yet. Switch to false once ODH releases.
DEV_INSTALL=true
if [[ $# -eq 1 ]] && [[ "$1" == "--dev" ]]; then
    DEV_INSTALL=true
elif [[ $# -ne 0 ]]; then
    echo "This script only supports a single argument: --dev"
    exit 1
fi

ODH_OPERATOR_IMAGE="${ODH_OPERATOR_IMAGE:-quay.io/opendatahub/opendatahub-operator:latest}"

echo "========================================="
echo "🚀 OpenDataHub (ODH) Installation"
echo "========================================="
echo ""

# Step 1: Install ODH Operator
if [[ "$DEV_INSTALL" == true ]]; then
    ODH_OPERATOR_NS="opendatahub"
    echo "1️⃣ Installing ODH Operator from repository manifests..."
    echo "   Using operator image: $ODH_OPERATOR_IMAGE"
    cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Namespace
metadata:
  name: $ODH_OPERATOR_NS
EOF

    TMP_DIR=$(mktemp -d)
    trap 'rm -fr -- "$TMP_DIR"' EXIT

    pushd $TMP_DIR
    git clone -q --depth 1 "https://github.com/opendatahub-io/opendatahub-operator.git"
    if [[ $? -ne 0 ]]; then
        echo "   Failed cloning repository https://github.com/opendatahub-io/opendatahub-operator.git"
        popd
        exit 1
    fi

    pushd ./opendatahub-operator
    cp config/manager/kustomization.yaml.in config/manager/kustomization.yaml
    make manifests
    # Replace the image placeholder in manager.yaml after manifests are generated
    # Detect OS and use appropriate sed syntax
    if [[ "$OSTYPE" == "darwin"* ]]; then
        # macOS (BSD sed) - requires empty string after -i and different syntax
        sed -i '' "s#REPLACE_IMAGE:latest#${ODH_OPERATOR_IMAGE}#g" config/manager/manager.yaml
    else
        # Linux (GNU sed)
        sed -i "s#REPLACE_IMAGE:latest#${ODH_OPERATOR_IMAGE}#g" config/manager/manager.yaml
    fi
    if grep -q "REPLACE_IMAGE" config/manager/manager.yaml; then
        echo "   Failed to update manager image in config/manager/manager.yaml"
        exit 1
    fi
    kustomize build config/default | kubectl apply --namespace $ODH_OPERATOR_NS -f -
    popd
    popd

    echo "   Waiting for operator to be ready (this may take a few minutes)..."
    kubectl wait deployment/opendatahub-operator-controller-manager -n $ODH_OPERATOR_NS --for condition=Available=True --timeout=300s
else
    echo "1️⃣ Installing ODH Operator..."

    # Check if operator is already installed
    if kubectl get csv -n openshift-operators 2>/dev/null | grep -q opendatahub-operator; then
        echo "   ✅ ODH operator already installed"
    else
        echo "   Creating OperatorGroup..."
        cat <<EOF | kubectl apply -f -
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: opendatahub
  namespace: openshift-operators
EOF

        echo "   Creating Subscription..."
        cat <<EOF | kubectl apply -f -
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: opendatahub-operator
  namespace: openshift-operators
spec:
  channel: fast  # or stable, depending on your needs
  name: opendatahub-operator
  source: community-operators
  sourceNamespace: openshift-marketplace
EOF

        echo "   Waiting for operator to be ready (this may take a few minutes)..."
        sleep 30

      # Wait for operator to be ready
      for i in {1..30}; do
          if kubectl get deployment -n openshift-operators | grep -q opendatahub-operator; then
              echo "   Operator deployment found, waiting for ready state..."
              kubectl wait --for=condition=Available deployment -l app.kubernetes.io/name=opendatahub-operator -n openshift-operators --timeout=300s || true
              break
          fi
          echo "   Waiting for operator deployment to appear... ($i/30)"
          sleep 10
      done
    fi
fi

# Step 2: Create DSCInitialization (REQUIRED before DataScienceCluster)
echo ""
echo "2️⃣ Creating DSCInitialization resource..."
cat <<EOF | kubectl apply -f -
apiVersion: dscinitialization.opendatahub.io/v2
kind: DSCInitialization
metadata:
  name: default-dsci
spec:
  applicationsNamespace: opendatahub
  monitoring:
    managementState: Managed
    namespace: opendatahub
    metrics: {}
  trustedCABundle:
    managementState: Managed
EOF

echo "   Waiting for DSCInitialization to be ready..."
for i in {1..30}; do
    if kubectl get dscinitializations -n opendatahub default-dsci -o jsonpath='{.status.phase}' 2>/dev/null | grep -q "Ready"; then
        echo "   ✅ DSCInitialization is ready"
        break
    fi
    echo "   Waiting for DSCInitialization to be ready... ($i/30)"
    sleep 10
done

# Step 3: Create DataScienceCluster
echo ""
echo "3️⃣ Creating DataScienceCluster..."
cat <<EOF | kubectl apply -f -
apiVersion: datasciencecluster.opendatahub.io/v2
kind: DataScienceCluster
metadata:
  name: default-dsc
spec:
  components:
    kserve:
      managementState: Managed
      nim:
        managementState: Managed
      rawDeploymentServiceConfig: Headed

    # Components not needed for MaaS:
    dashboard:
      managementState: Removed
    workbenches:
      managementState: Removed
    aipipelines:
      managementState: Removed
    ray:
      managementState: Removed
    kueue:
      managementState: Removed
    modelregistry:
      managementState: Removed
    trustyai:
      managementState: Removed
    trainingoperator:
      managementState: Removed
    feastoperator:
      managementState: Removed
    llamastackoperator:
      managementState: Removed
EOF

echo "   Waiting for DataScienceCluster to be ready..."
for i in {1..60}; do
    PHASE=$(kubectl get datasciencecluster -n opendatahub default-dsc -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
    CONDITIONS=$(kubectl get datasciencecluster -n opendatahub default-dsc -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || echo "Unknown")
    
    if [[ "$PHASE" == "Ready" ]] || [[ "$CONDITIONS" == "True" ]]; then
        echo "   ✅ DataScienceCluster is ready"
        break
    fi
    echo "   Status: Phase=$PHASE, Ready=$CONDITIONS ($i/60)"
    sleep 10
done

# Step 4: Verify installation
echo ""
echo "========================================="
echo "📊 Verification"
echo "========================================="
echo ""

echo "DSCInitialization Status:"
kubectl get dscinitializations -n opendatahub

echo ""
echo "DataScienceCluster Status:"
kubectl get datasciencecluster -n opendatahub

echo ""
echo "========================================="
echo "✅ ODH Installation Complete!"
echo "========================================="
echo ""
echo "Next steps:"
echo "1. Deploy your models using KServe InferenceService"
echo ""
echo "If you encounter issues, check the logs:"
echo "- ODH Operator: kubectl logs -n openshift-operators deployment/opendatahub-operator-controller-manager"
echo "- DSCInitialization: kubectl describe dscinitializations default-dsci"
echo "- DataScienceCluster: kubectl describe datasciencecluster default-dsc"
