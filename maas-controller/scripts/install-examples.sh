#!/usr/bin/env bash
# Installs the example MaaS CRs (examples/) and both regular and premium
# simulator LLMInferenceServices from docs/samples/models (simulator and simulator-premium).
# Run from maas-controller directory. Assumes maas-controller lives inside
# the models-as-a-service repo so that ../docs/samples/models exists.
#
# Prerequisites: maas-controller installed, gateway-auth-policy disabled.
# Usage: ./scripts/install-examples.sh

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
# Parent repo (models-as-a-service) contains docs/samples/models
REPO_PARENT="${REPO_PARENT:-$REPO_ROOT/..}"
MODELS_DIR="$REPO_PARENT/docs/samples/models"

echo "=== Installing MaaS examples and regular + premium simulator LLMInferenceServices ==="

# 1. Install both simulator LLMInferenceServices from docs samples
if [[ -d "$MODELS_DIR" ]]; then
  echo "Ensuring namespace llm exists ..."
  kubectl get namespace llm &>/dev/null || kubectl create namespace llm
  for sim in simulator simulator-premium; do
    if [[ -d "$MODELS_DIR/$sim" ]]; then
      echo "Deploying LLMInferenceService from docs/samples/models/$sim ..."
      (cd "$REPO_PARENT" && kustomize build "docs/samples/models/$sim") | kubectl apply -f -
    else
      echo "Warning: $MODELS_DIR/$sim not found. Skipping."
    fi
  done
else
  echo "Warning: $MODELS_DIR not found. Run from models-as-a-service repo or set REPO_PARENT. Skipping LLMInferenceServices."
fi

# 2. Install example MaaS CRs
echo "Applying example MaaS CRs (examples/) ..."
kubectl apply -k "$REPO_ROOT/examples"

echo "Done. Check: kubectl get maasmodel,maasauthpolicy,maassubscription -n opendatahub; kubectl get llminferenceservice -n llm"
