#!/usr/bin/env bash
# Run in-cluster OpenShift tls-scanner with PQC_CHECK after MaaS E2E deploy (Konflux / manual validation).
# Requires: oc logged in, cluster with MaaS workloads up, pull access to SCANNER_IMAGE.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# CI (Prow/Konflux) sets ARTIFACT_DIR; local runs use a temp dir (never under the git tree).
if [[ -n "${ARTIFACT_DIR:-}" ]]; then
  mkdir -p "${ARTIFACT_DIR}"
  ARTIFACT_DIR="$(cd "${ARTIFACT_DIR}" && pwd -P)"
else
  ARTIFACT_DIR="$(mktemp -d "${TMPDIR:-/tmp}/maas-tls-scanner-artifacts.XXXXXXXX")"
fi
TLS_SCANNER_GIT="${TLS_SCANNER_GIT:-https://github.com/openshift/tls-scanner.git}"
# Pinned commit for deploy.sh job flow (PQC flag injected into job template below when PQC_CHECK=true).
TLS_SCANNER_REF="${TLS_SCANNER_REF:-df6f8d462cd22d4a515946602936a4ac117e4c74}"

export NAMESPACE="${NAMESPACE:-default}"

# Scan workloads in the controller + infra namespaces (matches scripts/deploy.sh / prow e2e defaults).
# ODH: maas-controller in opendatahub, maas-api in odh-ai-gateway-infra. RHOAI: redhat-ods-applications + redhat-ai-gateway-infra.
if [[ -z "${NAMESPACE_FILTER:-}" ]]; then
  _deploy_ns="${DEPLOYMENT_NAMESPACE:-opendatahub}"
  case "${_deploy_ns}" in
    redhat-ods-applications) _infra_ns="redhat-ai-gateway-infra" ;;
    opendatahub) _infra_ns="odh-ai-gateway-infra" ;;
    *) _infra_ns="${_deploy_ns}" ;;
  esac
  if [[ -n "${E2E_MAAS_API_DEPLOYMENT_NAMESPACE:-}" ]]; then
    _infra_ns="${E2E_MAAS_API_DEPLOYMENT_NAMESPACE}"
  elif [[ "${INFRA_NAMESPACE+x}" == "x" ]]; then
    if [[ -z "${INFRA_NAMESPACE}" ]]; then
      _infra_ns="${_deploy_ns}"
    elif [[ "${INFRA_NAMESPACE}" != "AUTO" ]]; then
      _infra_ns="${INFRA_NAMESPACE}"
    fi
  fi
  export NAMESPACE_FILTER="${_deploy_ns},${_infra_ns}"
else
  export NAMESPACE_FILTER
fi

export PQC_CHECK="${PQC_CHECK:-true}"
export SCANNER_IMAGE="${SCANNER_IMAGE:-registry.ci.openshift.org/tls-scanner/tls-scanner:tls-scanner-tool}"
export ARTIFACT_WAIT="${ARTIFACT_WAIT:-60}"

SCANNER_WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/tls-scanner.XXXXXXXX")"
cleanup() {
  rm -rf "${SCANNER_WORKDIR}"
}
trap cleanup EXIT

git clone --filter=blob:none --quiet "${TLS_SCANNER_GIT}" "${SCANNER_WORKDIR}"
git -C "${SCANNER_WORKDIR}" fetch --depth 1 origin "${TLS_SCANNER_REF}"
git -C "${SCANNER_WORKDIR}" checkout FETCH_HEAD
_checked_out="$(git -C "${SCANNER_WORKDIR}" rev-parse HEAD)"
if [[ "${TLS_SCANNER_REF}" =~ ^[0-9a-f]{40}$ ]] && [[ "${_checked_out}" != "${TLS_SCANNER_REF}" ]]; then
  echo "TLS scanner checkout ${_checked_out} does not match pinned ref ${TLS_SCANNER_REF}" >&2
  exit 1
fi

# openshift/tls-scanner deploy.sh does not read PQC_CHECK; inject --pqc-check into the Job template.
if [[ "${PQC_CHECK}" == "true" ]]; then
  _job_template="${SCANNER_WORKDIR}/scanner-job.yaml.template"
  sed -i.bak 's|\${LIMIT_IPS_ARG}|--pqc-check \${LIMIT_IPS_ARG}|' "${_job_template}"
  rm -f "${_job_template}.bak"
fi

pushd "${SCANNER_WORKDIR}" >/dev/null
./deploy.sh deploy

for _required in results.json results.csv; do
  if [[ ! -s "artifacts/${_required}" ]]; then
    echo "Missing or empty scanner artifact: artifacts/${_required}" >&2
    [[ -f artifacts/scan.log ]] && tail -30 artifacts/scan.log >&2 || true
    exit 1
  fi
done

if [[ -f artifacts/scan.log ]] && grep -E 'Scanner finished with exit code: [1-9][0-9]*' artifacts/scan.log >/dev/null; then
  echo "TLS scanner reported a non-zero exit code" >&2
  tail -20 artifacts/scan.log >&2 || true
  exit 1
fi

cp -a artifacts/. "${ARTIFACT_DIR}/"
popd >/dev/null

echo "TLS PQC scan artifacts: ${ARTIFACT_DIR}"
