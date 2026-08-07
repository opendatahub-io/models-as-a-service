#!/bin/bash
# Canonical operator-mode MaaS install for local clusters and PR image testing.
# Always use this script (or the env block in .cursor/skills/maas-operator-install/SKILL.md)
# instead of invoking prow_run_smoke_test.sh without OPERATOR_CATALOG / OPERATOR_CHANNEL.
#
# Usage:
#   ./test/e2e/scripts/install-maas-operator-e2e.sh
#
# With custom MaaS / IPP images:
#   MAAS_CONTROLLER_IMAGE=quay.io/you/maas-controller:tag \
#   PAYLOAD_PROCESSING_IMAGE=quay.io/you/odh-ai-gateway-payload-processing:tag \
#   ./test/e2e/scripts/install-maas-operator-e2e.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

# shellcheck source=../../scripts/deployment-helpers.sh
source "${PROJECT_ROOT}/scripts/deployment-helpers.sh"

export DEPLOY_MODE=operator
export DEPLOYMENT_NAMESPACE="${DEPLOYMENT_NAMESPACE:-opendatahub}"
export OPERATOR_CATALOG="${OPERATOR_CATALOG:-quay.io/opendatahub/opendatahub-operator-catalog:latest}"
export OPERATOR_CHANNEL="${OPERATOR_CHANNEL:-fast}"
# Konflux latest 3.5 on catalog :latest / channel fast (NOT community-operators fast-3 → 3.5.0-ea.2)
export OPERATOR_STARTING_CSV="${OPERATOR_STARTING_CSV:-opendatahub-operator.v3.5.0-snapshot}"
export AI_GATEWAY_OPERATOR_IMAGE="${AI_GATEWAY_OPERATOR_IMAGE:-quay.io/opendatahub/odh-ai-gateway-operator:odh-stable}"

export MAAS_API_IMAGE="${MAAS_API_IMAGE:-}"
export MAAS_CONTROLLER_IMAGE="${MAAS_CONTROLLER_IMAGE:-}"
export PAYLOAD_PROCESSING_IMAGE="${PAYLOAD_PROCESSING_IMAGE:-}"
export OPERATOR_IMAGE="${OPERATOR_IMAGE:-}"

echo "MaaS operator-mode install"
echo "  DEPLOY_MODE=${DEPLOY_MODE}"
echo "  OPERATOR_CATALOG=${OPERATOR_CATALOG}"
echo "  OPERATOR_CHANNEL=${OPERATOR_CHANNEL}"
echo "  OPERATOR_STARTING_CSV=${OPERATOR_STARTING_CSV}"
echo "  AI_GATEWAY_OPERATOR_IMAGE=${AI_GATEWAY_OPERATOR_IMAGE}"
[[ -n "${MAAS_CONTROLLER_IMAGE}" ]] && echo "  MAAS_CONTROLLER_IMAGE=${MAAS_CONTROLLER_IMAGE}"
[[ -n "${PAYLOAD_PROCESSING_IMAGE}" ]] && echo "  PAYLOAD_PROCESSING_IMAGE=${PAYLOAD_PROCESSING_IMAGE}"
[[ -n "${MAAS_API_IMAGE}" ]] && echo "  MAAS_API_IMAGE=${MAAS_API_IMAGE}"
echo ""

echo "Ensuring ODH Subscription uses Konflux catalog (not community-operators fast-3 / ea.2)..."
ensure_odh_konflux_subscription "${DEPLOYMENT_NAMESPACE}"

exec "${PROJECT_ROOT}/test/e2e/scripts/prow_run_smoke_test.sh" "$@"
