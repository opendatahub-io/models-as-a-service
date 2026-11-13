#!/usr/bin/env bash
# Standalone maas-api E2E: no MaaS subscription CRDs, no maas-controller, no Kuadrant.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
CLUSTER="${KIND_CLUSTER_NAME:-maas-standalone-e2e}"
NAMESPACE="maas-standalone-e2e"
IMAGE="maas-api:standalone-e2e"
PORT="${STANDALONE_E2E_PORT:-18080}"
PF_PID=""

cleanup() {
  if [[ -n "${PF_PID}" ]]; then
    kill "${PF_PID}" 2>/dev/null || true
    wait "${PF_PID}" 2>/dev/null || true
  fi
  if [[ "${KEEP_KIND_CLUSTER:-false}" != "true" ]]; then
    kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

for tool in kind kubectl docker python3; do
  if ! command -v "${tool}" >/dev/null 2>&1; then
    echo "missing required tool: ${tool}" >&2
    exit 1
  fi
done

start_port_forward() {
  if [[ -n "${PF_PID}" ]]; then
    kill "${PF_PID}" 2>/dev/null || true
    wait "${PF_PID}" 2>/dev/null || true
  fi
  kubectl -n "${NAMESPACE}" port-forward svc/maas-api "${PORT}:8080" >/tmp/maas-standalone-port-forward.log 2>&1 &
  PF_PID=$!
  for _ in $(seq 1 60); do
    if curl -fsS "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  echo "standalone maas-api did not become reachable" >&2
  return 1
}

if ! kind get clusters | grep -qx "${CLUSTER}"; then
  kind create cluster --name "${CLUSTER}" --wait 120s
fi

echo "Building ${IMAGE}..."
case "$(uname -m)" in
  arm64|aarch64) GOARCH=arm64 ;;
  x86_64|amd64) GOARCH=amd64 ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac
docker build \
  --build-arg "BUILDPLATFORM=linux/${GOARCH}" \
  --build-arg "TARGETPLATFORM=linux/${GOARCH}" \
  --build-arg TARGETOS=linux \
  --build-arg "TARGETARCH=${GOARCH}" \
  -t "${IMAGE}" -f "${ROOT}/maas-api/Dockerfile" "${ROOT}/maas-api"
kind load docker-image "${IMAGE}" --name "${CLUSTER}"

echo "Deploying standalone maas-api without MaaS subscription CRDs..."
kubectl apply -k "${ROOT}/test/e2e/standalone/manifests"
kubectl -n "${NAMESPACE}" rollout status deployment/postgres --timeout=180s
kubectl -n "${NAMESPACE}" rollout status deployment/maas-api --timeout=180s

VENV="$(mktemp -d)/venv"
python3 -m venv "${VENV}"
"${VENV}/bin/pip" install --quiet pytest requests

start_port_forward
STANDALONE_MAAS_API_URL="http://127.0.0.1:${PORT}" \
  GATEWAY_HOST="127.0.0.1:${PORT}" \
  "${VENV}/bin/python" -m pytest "${ROOT}/test/e2e/tests/test_standalone_mode.py" -v

echo "Installing the MaaSModelRef CRD and a ready model..."
kubectl apply -f "${ROOT}/test/e2e/standalone/manifests/maasmodelref-crd.yaml"
kubectl wait --for=condition=Established crd/maasmodelrefs.maas.opendatahub.io --timeout=60s
kubectl apply -f "${ROOT}/test/e2e/standalone/manifests/maasmodelref.yaml"
kubectl -n "${NAMESPACE}" rollout restart deployment/maas-api
kubectl -n "${NAMESPACE}" rollout status deployment/maas-api --timeout=180s

start_port_forward
STANDALONE_MAAS_API_URL="http://127.0.0.1:${PORT}" \
  GATEWAY_HOST="127.0.0.1:${PORT}" \
  STANDALONE_MODEL_CATALOG_EXPECTED=1 \
  "${VENV}/bin/python" -m pytest "${ROOT}/test/e2e/tests/test_standalone_model_catalog.py" -v
