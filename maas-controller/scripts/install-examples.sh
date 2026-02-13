#!/usr/bin/env bash
# Installs the example MaaS CRs (examples/) and the simulator LLMInferenceService
# from the docs samples (../docs/samples/models/simulator).
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
SIMULATOR_DIR="$REPO_PARENT/docs/samples/models/simulator"

echo "=== Installing MaaS examples and simulator LLMInferenceService ==="

# 1. Install the simulator LLMInferenceService from docs samples
if [[ -d "$SIMULATOR_DIR" ]]; then
  echo "Ensuring namespace llm exists ..."
  kubectl get namespace llm &>/dev/null || kubectl create namespace llm
  echo "Deploying LLMInferenceService from docs/samples/models/simulator ..."
  (cd "$REPO_PARENT" && kustomize build docs/samples/models/simulator) | kubectl apply -f -
else
  echo "Warning: $SIMULATOR_DIR not found. Run from models-as-a-service repo or set REPO_PARENT. Skipping LLMInferenceService."
fi

# 2. Install example MaaS CRs
echo "Applying example MaaS CRs (examples/) ..."
kubectl apply -k "$REPO_ROOT/examples"

echo "Done. Check: kubectl get maasmodel,maasauthpolicy,maassubscription -n opendatahub; kubectl get llminferenceservice -n llm"
