#!/usr/bin/env bash
# =============================================================================
# Deploy ai-gateway-controller companion for MaaS e2e (temporary pairing)
# =============================================================================
# When AI_GATEWAY_CONTROLLER_IMAGE is set, clone the AIGC checkout and run its
# deploy-ai-gateway-controller.sh so praxis can own payload-processing.
#
# Short-term defaults pair MaaS #1579 with AIGC #91 (absent → UsesPraxis).
#
# Env:
#   AI_GATEWAY_CONTROLLER_IMAGE  Manager image (required to run; default below)
#   AIGC_GIT_URL                 Default: opendatahub-io/ai-gateway-controller
#   AIGC_GIT_REF                 Default: pull/91/head (jland-redhat branch)
#   PRAXIS_EXTPROC_IMAGE         Optional; AIGC script defaults to odh-stable
#   GATEWAY_NAMESPACE, GATEWAY_NAME, DEPLOYMENT_NAMESPACE
# =============================================================================

set -euo pipefail

if [[ -z "${PROJECT_ROOT:-}" ]]; then
  _dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  PROJECT_ROOT="$(cd "${_dir}/../../.." && pwd)"
fi

# TEMP: pair with https://github.com/opendatahub-io/ai-gateway-controller/pull/91
# until absent→praxis lands on AIGC main and Konflux installs AIGC by default.
export AI_GATEWAY_CONTROLLER_IMAGE="${AI_GATEWAY_CONTROLLER_IMAGE:-quay.io/opendatahub/odh-ai-gateway-controller:odh-pr-91}"
AIGC_GIT_URL="${AIGC_GIT_URL:-https://github.com/opendatahub-io/ai-gateway-controller.git}"
AIGC_GIT_REF="${AIGC_GIT_REF:-refs/pull/91/head}"
export PRAXIS_EXTPROC_IMAGE="${PRAXIS_EXTPROC_IMAGE:-quay.io/opendatahub/odh-praxis-extproc:odh-stable}"
export GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-openshift-ingress}"
export GATEWAY_NAME="${GATEWAY_NAME:-maas-default-gateway}"
export DEPLOYMENT_NAMESPACE="${DEPLOYMENT_NAMESPACE:-opendatahub}"
export AI_GATEWAY_CONTROLLER_NAMESPACE="${AI_GATEWAY_CONTROLLER_NAMESPACE:-${DEPLOYMENT_NAMESPACE}}"
# MaaS #1579 already SkipIPP-on-absent; still let AIGC script clear leftovers / wait.
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
