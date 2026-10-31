#!/usr/bin/env bash
# =============================================================================
# Deploy the NeMo Guardrails controller for MaaS guardrail e2e
# =============================================================================
# Installs TrustyAI's mcp-guardrails overlay, which deploys the NeMo Guardrails
# controller and nothing else. The DataScienceCluster trustyai component stays
# Removed: the guardrail tests are the only consumer, and enabling the full
# TrustyAI service for every e2e run is a much larger change than they need.
#
# Env:
#   TRUSTYAI_GIT_URL          Default: trustyai-explainability/trustyai-service-operator
#   TRUSTYAI_GIT_REF          Default: main
#   TRUSTYAI_OPERATOR_IMAGE   Optional; overlay params.env default is used when empty.
#                             The CRD comes from the git ref, but status.endpoint
#                             and allowedConsumers are populated by the controller
#                             in this image, so the two have to match.
#   DEPLOYMENT_NAMESPACE      Default: opendatahub
# =============================================================================

set -euo pipefail

TRUSTYAI_GIT_URL="${TRUSTYAI_GIT_URL:-https://github.com/trustyai-explainability/trustyai-service-operator.git}"
TRUSTYAI_GIT_REF="${TRUSTYAI_GIT_REF:-main}"
DEPLOYMENT_NAMESPACE="${DEPLOYMENT_NAMESPACE:-opendatahub}"
NEMO_CRD="nemoguardrails.trustyai.opendatahub.io"

# A git URL may carry credentials in its userinfo, and CI logs are readable by
# anyone who can see the job. Printed in place of the raw value.
redacted_url() { printf '%s' "${1}" | sed -E 's#(://)[^/@]*@#\1***@#'; }

echo "Installing the NeMo Guardrails controller..."
echo "  TRUSTYAI_GIT_URL=$(redacted_url "${TRUSTYAI_GIT_URL}")"
echo "  TRUSTYAI_GIT_REF=${TRUSTYAI_GIT_REF}"
echo "  TRUSTYAI_OPERATOR_IMAGE=${TRUSTYAI_OPERATOR_IMAGE:-<overlay default>}"
echo "  DEPLOYMENT_NAMESPACE=${DEPLOYMENT_NAMESPACE}"

work_dir="$(mktemp -d -t trustyai-guardrails.XXXXXXXXXX)"
cleanup() { rm -rf "${work_dir}"; }
trap cleanup EXIT

echo "Cloning $(redacted_url "${TRUSTYAI_GIT_URL}") @ ${TRUSTYAI_GIT_REF} ..."
git init -q "${work_dir}/trustyai"
git -C "${work_dir}/trustyai" remote add origin "${TRUSTYAI_GIT_URL}"
git -C "${work_dir}/trustyai" fetch --depth 1 origin "${TRUSTYAI_GIT_REF}"
git -C "${work_dir}/trustyai" checkout -q FETCH_HEAD

overlay="${work_dir}/trustyai/config/overlays/mcp-guardrails"
if [[ ! -d "${overlay}" ]]; then
  echo "ERROR: missing ${overlay} in the TrustyAI checkout" >&2
  exit 1
fi

if [[ -n "${TRUSTYAI_OPERATOR_IMAGE:-}" ]]; then
  # Rewrite the one key: params.env also carries the service, rbac-proxy and
  # nemo-guardrails images the overlay resolves through vars.
  sed -i.bak "s|^trustyaiOperatorImage=.*|trustyaiOperatorImage=${TRUSTYAI_OPERATOR_IMAGE}|" \
    "${overlay}/params.env"
  rm -f "${overlay}/params.env.bak"
fi

oc create namespace "${DEPLOYMENT_NAMESPACE}" --dry-run=client -o yaml | oc apply -f -

# The overlay stamps every resource with namespace "system", which the ODH
# operator substitutes at install time. A direct apply sets it here instead. A
# wrapper kustomization does that through the build, so the kubectl fallback
# works too -- `kustomize edit` has no kubectl equivalent.
render_dir="${work_dir}/render"
mkdir -p "${render_dir}"
cat > "${render_dir}/kustomization.yaml" <<EOF
resources:
  - ../trustyai/config/overlays/mcp-guardrails
namespace: ${DEPLOYMENT_NAMESPACE}
EOF

rendered="${work_dir}/rendered.yaml"
if command -v kustomize >/dev/null 2>&1; then
  kustomize build "${render_dir}" > "${rendered}"
elif command -v kubectl >/dev/null 2>&1; then
  kubectl kustomize "${render_dir}" > "${rendered}"
else
  echo "ERROR: kustomize or kubectl kustomize required" >&2
  exit 1
fi

# The overlay carries the NemoGuardrails CRD, so it needs no separate apply.
# Server-side: the CRD schema is past the client-side annotation size limit.
oc apply --server-side -f "${rendered}"

oc wait --for=condition=Established "crd/${NEMO_CRD}" --timeout=120s

# An established CRD says nothing about the controller that serves it. Reporting
# success here would hand the tests a cluster where no provider ever reconciles,
# and they would spend their full endpoint timeout discovering it. The names come
# out of what was rendered, so another git ref or namePrefix still resolves.
deployments="$(awk 'BEGIN { RS = "---\n" } {
  kind = ""; name = ""
  count = split($0, line, "\n")
  for (i = 1; i <= count; i++) {
    if (line[i] == "kind: Deployment") kind = line[i]
    else if (name == "" && line[i] ~ /^  name: /) { name = line[i]; sub(/^  name: /, "", name) }
  }
  if (kind != "" && name != "") print name
}' "${rendered}")"

if [[ -z "${deployments}" ]]; then
  echo "ERROR: the overlay rendered no Deployment to wait on" >&2
  exit 1
fi

while read -r deployment; do
  oc rollout status "deployment/${deployment}" -n "${DEPLOYMENT_NAMESPACE}" --timeout=300s
done <<< "${deployments}"

echo "✅ NeMo Guardrails controller running (${NEMO_CRD} established)"
