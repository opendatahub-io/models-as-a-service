#!/usr/bin/env bash
# Run in-cluster OpenShift tls-scanner with PQC_CHECK after MaaS E2E deploy (Konflux / manual validation).
# Requires: oc logged in, cluster with MaaS workloads up, pull access to SCANNER_IMAGE.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ARTIFACT_DIR="${ARTIFACT_DIR:-${SCRIPT_DIR}/../../tls-scanner-artifacts}"
TLS_SCANNER_GIT="${TLS_SCANNER_GIT:-https://github.com/openshift/tls-scanner.git}"
TLS_SCANNER_REF="${TLS_SCANNER_REF:-main}"
SCANNER_WORKDIR="${SCANNER_WORKDIR:-/tmp/tls-scanner-checkout.$$}"

export NAMESPACE="${NAMESPACE:-default}"
export NAMESPACE_FILTER="${NAMESPACE_FILTER:-redhat-ods-applications,redhat-ai-gateway-infra}"
export PQC_CHECK="${PQC_CHECK:-true}"
export SCANNER_IMAGE="${SCANNER_IMAGE:-registry.ci.openshift.org/tls-scanner/tls-scanner:tls-scanner-tool}"
export ARTIFACT_WAIT="${ARTIFACT_WAIT:-60}"

mkdir -p "${ARTIFACT_DIR}"
rm -rf "${SCANNER_WORKDIR}"
git clone --depth 1 --branch "${TLS_SCANNER_REF}" "${TLS_SCANNER_GIT}" "${SCANNER_WORKDIR}"

pushd "${SCANNER_WORKDIR}" >/dev/null
./deploy.sh deploy
cp -a artifacts/. "${ARTIFACT_DIR}/"
popd >/dev/null

rm -rf "${SCANNER_WORKDIR}"
echo "TLS PQC scan artifacts: ${ARTIFACT_DIR}"
