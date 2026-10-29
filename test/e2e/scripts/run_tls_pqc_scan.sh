#!/usr/bin/env bash
# Run in-cluster OpenShift tls-scanner with PQC_CHECK after MaaS E2E deploy (Konflux / manual validation).
# Requires: oc logged in, cluster with MaaS workloads up, pull access to SCANNER_IMAGE.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ARTIFACT_DIR="${ARTIFACT_DIR:-${SCRIPT_DIR}/../../tls-scanner-artifacts}"
TLS_SCANNER_GIT="${TLS_SCANNER_GIT:-https://github.com/openshift/tls-scanner.git}"
# Pinned commit (includes deploy.sh PQC_CHECK → --pqc-check wiring).
TLS_SCANNER_REF="${TLS_SCANNER_REF:-df6f8d462cd22d4a515946602936a4ac117e4c74}"

export NAMESPACE="${NAMESPACE:-default}"
export NAMESPACE_FILTER="${NAMESPACE_FILTER:-redhat-ods-applications,redhat-ai-gateway-infra}"
export PQC_CHECK="${PQC_CHECK:-true}"
export SCANNER_IMAGE="${SCANNER_IMAGE:-registry.ci.openshift.org/tls-scanner/tls-scanner:tls-scanner-tool}"
export ARTIFACT_WAIT="${ARTIFACT_WAIT:-60}"

mkdir -p "${ARTIFACT_DIR}"
ARTIFACT_DIR="$(cd "${ARTIFACT_DIR}" && pwd -P)"

SCANNER_WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/tls-scanner.XXXXXXXX")"
cleanup() {
  rm -rf "${SCANNER_WORKDIR}"
}
trap cleanup EXIT

git clone --filter=blob:none --quiet "${TLS_SCANNER_GIT}" "${SCANNER_WORKDIR}"
git -C "${SCANNER_WORKDIR}" fetch --depth 1 origin "${TLS_SCANNER_REF}"
git -C "${SCANNER_WORKDIR}" checkout FETCH_HEAD

pushd "${SCANNER_WORKDIR}" >/dev/null
./deploy.sh deploy

if [[ -f artifacts/scan.log ]] && grep -E 'Scanner finished with exit code: [1-9][0-9]*' artifacts/scan.log >/dev/null; then
  echo "TLS scanner reported a non-zero exit code" >&2
  tail -20 artifacts/scan.log >&2 || true
  exit 1
fi

cp -a artifacts/. "${ARTIFACT_DIR}/"
popd >/dev/null

echo "TLS PQC scan artifacts: ${ARTIFACT_DIR}"
