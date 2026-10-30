#!/usr/bin/env bash
# =============================================================================
# Deploy ai-gateway-controller companion for MaaS e2e
# =============================================================================
# Clone the AIGC checkout and run deploy-ai-gateway-controller.sh so praxis can own
# payload-processing. Intended for direct invocation or from prow_run_smoke_test.sh when
# AI_GATEWAY_CONTROLLER_IMAGE is non-empty (prow defaults to AIGC :latest).
#
# Env:
#   AI_GATEWAY_CONTROLLER_IMAGE  Manager image (required; e.g. quay.io/.../odh-ai-gateway-controller:odh-stable)
#   AIGC_GIT_URL                 Default: opendatahub-io/ai-gateway-controller
#   AIGC_GIT_REF                 Default: main
#   PRAXIS_EXTPROC_IMAGE         Optional; AIGC script defaults to odh-stable
#   GATEWAY_NAMESPACE, GATEWAY_NAME, DEPLOYMENT_NAMESPACE
# =============================================================================

set -euo pipefail

if [[ -z "${PROJECT_ROOT:-}" ]]; then
  _dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  PROJECT_ROOT="$(cd "${_dir}/../../.." && pwd)"
fi

if [[ -z "${AI_GATEWAY_CONTROLLER_IMAGE:-}" ]]; then
  echo "ERROR: AI_GATEWAY_CONTROLLER_IMAGE must be set (prow_run_smoke_test.sh skips this script when it is empty)." >&2
  echo "  Example: AI_GATEWAY_CONTROLLER_IMAGE=quay.io/opendatahub/odh-ai-gateway-controller:odh-stable $0" >&2
  exit 1
fi
export AI_GATEWAY_CONTROLLER_IMAGE
AIGC_GIT_URL="${AIGC_GIT_URL:-https://github.com/opendatahub-io/ai-gateway-controller.git}"
AIGC_GIT_REF="${AIGC_GIT_REF:-main}"
export PRAXIS_EXTPROC_IMAGE="${PRAXIS_EXTPROC_IMAGE:-quay.io/opendatahub/odh-praxis-extproc:odh-stable}"
export GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-openshift-ingress}"
export GATEWAY_NAME="${GATEWAY_NAME:-maas-default-gateway}"
export DEPLOYMENT_NAMESPACE="${DEPLOYMENT_NAMESPACE:-opendatahub}"
export AI_GATEWAY_CONTROLLER_NAMESPACE="${AI_GATEWAY_CONTROLLER_NAMESPACE:-${DEPLOYMENT_NAMESPACE}}"
export REMOVE_MAAS_IPP="${REMOVE_MAAS_IPP:-true}"

echo "Installing AIGC companion for praxis dataplane..."
echo "  AI_GATEWAY_CONTROLLER_IMAGE=${AI_GATEWAY_CONTROLLER_IMAGE}"
echo "  AIGC_GIT_URL=${AIGC_GIT_URL}"
echo "  AIGC_GIT_REF=${AIGC_GIT_REF}"
echo "  PRAXIS_EXTPROC_IMAGE=${PRAXIS_EXTPROC_IMAGE}"

work_dir="$(mktemp -d -t aigc-companion.XXXXXXXXXX)"
cleanup() { rm -rf "${work_dir}"; }
trap cleanup EXIT

echo "Cloning ${AIGC_GIT_URL} @ ${AIGC_GIT_REF} ..."
git init -q "${work_dir}/aigc"
git -C "${work_dir}/aigc" remote add origin "${AIGC_GIT_URL}"
git -C "${work_dir}/aigc" fetch --depth 1 origin "${AIGC_GIT_REF}"
git -C "${work_dir}/aigc" checkout -q FETCH_HEAD

deploy_script="${work_dir}/aigc/test/e2e/scripts/deploy-ai-gateway-controller.sh"
if [[ ! -x "${deploy_script}" && ! -f "${deploy_script}" ]]; then
  echo "ERROR: missing ${deploy_script} in AIGC checkout" >&2
  exit 1
fi
chmod +x "${deploy_script}"

# AIGC deploy script discovers PROJECT_ROOT from its own tree (.git).
bash "${deploy_script}"

echo "✅ AIGC companion installed (ai-gateway-controller + praxis-extproc)"
