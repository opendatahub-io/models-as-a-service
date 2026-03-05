#!/usr/bin/env bash
set -euo pipefail
trap 'echo "[bootstrap] ERROR line $LINENO: $BASH_COMMAND" >&2' ERR

# Repo/E2E dirs
REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
E2E_DIR="${REPO_ROOT}/test/e2e"

# Defaults (overridable via env)
NS="${NS:-llm}"
MODEL_PATH="${MODEL_PATH:-docs/samples/models/facebook-opt-125m-cpu}"
GATEWAY_NAME="${GATEWAY_NAME:-maas-default-gateway}"
GATEWAY_NS="${GATEWAY_NS:-openshift-ingress}"
WRITE_ENV="${WRITE_ENV:-true}"     # write test/e2e/.env
SKIP_DEPLOY="${SKIP_DEPLOY:-true}" # default true (don’t redeploy model unless you want to)

echo "[bootstrap] oc whoami: $(oc whoami || true)"
echo "[bootstrap] NS=${NS} MODEL_PATH=${MODEL_PATH} SKIP_DEPLOY=${SKIP_DEPLOY}"

command -v oc >/dev/null 2>&1 || { echo "oc missing"; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "jq missing"; exit 1; }
command -v kustomize >/dev/null 2>&1 || { echo "kustomize missing"; exit 1; }

# ---- Detect model CR name from kustomize
if command -v yq >/dev/null 2>&1; then
  DEDUCED_CR="$(kustomize build "${MODEL_PATH}" | yq -r 'select(.kind=="LLMInferenceService") | .metadata.name' | head -n1)"
else
  DEDUCED_CR="$(kustomize build "${MODEL_PATH}" | awk '/^kind: LLMInferenceService$/{f=1} f&&/^  name:/{print $2; exit}')"
fi
if [[ -z "${DEDUCED_CR:-}" ]]; then
  echo "[bootstrap] Could not detect LLMInferenceService name from ${MODEL_PATH}" >&2
  exit 1
fi
export MODEL_NAME="${MODEL_NAME:-$DEDUCED_CR}"
echo "[bootstrap] Using kind=llminferenceservice ns=${NS} (${MODEL_PATH##*/})"
echo "[bootstrap] Model CR name: ${MODEL_NAME}"

# ---- Deploy PostgreSQL for API key storage (required for E2E tests)
MAAS_NS="${MAAS_NS:-opendatahub}"
echo "[bootstrap] Checking PostgreSQL in namespace: ${MAAS_NS}"
if ! kubectl get deployment postgres -n "${MAAS_NS}" &>/dev/null; then
  echo "[bootstrap] PostgreSQL not found, deploying..."

  POSTGRES_USER="${POSTGRES_USER:-maas}"
  POSTGRES_DB="${POSTGRES_DB:-maas}"

  # Generate random password if not provided
  POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-}"
  if [[ -z "${POSTGRES_PASSWORD}" ]]; then
    POSTGRES_PASSWORD="$(openssl rand -base64 32 | tr -d '/+=' | cut -c1-32)"
    echo "[bootstrap] Generated random PostgreSQL password (stored in secret postgres-creds)"
  fi

  kubectl apply -n "${MAAS_NS}" -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: postgres-creds
  labels:
    app: postgres
    purpose: e2e-test
stringData:
  POSTGRES_USER: "${POSTGRES_USER}"
  POSTGRES_PASSWORD: "${POSTGRES_PASSWORD}"
  POSTGRES_DB: "${POSTGRES_DB}"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
  labels:
    app: postgres
    purpose: e2e-test
spec:
  replicas: 1
  selector:
    matchLabels:
      app: postgres
  template:
    metadata:
      labels:
        app: postgres
    spec:
      containers:
      - name: postgres
        image: registry.redhat.io/rhel9/postgresql-15:latest
        env:
        - name: POSTGRESQL_USER
          valueFrom:
            secretKeyRef:
              name: postgres-creds
              key: POSTGRES_USER
        - name: POSTGRESQL_PASSWORD
          valueFrom:
            secretKeyRef:
              name: postgres-creds
              key: POSTGRES_PASSWORD
        - name: POSTGRESQL_DATABASE
          valueFrom:
            secretKeyRef:
              name: postgres-creds
              key: POSTGRES_DB
        ports:
        - containerPort: 5432
        volumeMounts:
        - name: data
          mountPath: /var/lib/pgsql/data
        resources:
          requests:
            memory: "256Mi"
            cpu: "100m"
          limits:
            memory: "512Mi"
            cpu: "500m"
        readinessProbe:
          exec:
            command: ["/usr/libexec/check-container"]
          initialDelaySeconds: 5
          periodSeconds: 5
      volumes:
      - name: data
        emptyDir: {}
---
apiVersion: v1
kind: Service
metadata:
  name: postgres
  labels:
    app: postgres
    purpose: e2e-test
spec:
  selector:
    app: postgres
  ports:
  - port: 5432
    targetPort: 5432
---
apiVersion: v1
kind: Secret
metadata:
  name: maas-db-config
  labels:
    app: maas-api
    purpose: e2e-test
stringData:
  DB_CONNECTION_URL: "postgresql://${POSTGRES_USER}:${POSTGRES_PASSWORD}@postgres:5432/${POSTGRES_DB}?sslmode=disable"
EOF

  echo "[bootstrap] Waiting for PostgreSQL to be ready..."
  kubectl wait -n "${MAAS_NS}" --for=condition=available deployment/postgres --timeout=120s || {
    echo "[bootstrap] WARNING: PostgreSQL deployment failed, E2E tests may fail"
  }
  echo "[bootstrap] PostgreSQL deployed successfully"
else
  echo "[bootstrap] PostgreSQL already deployed in ${MAAS_NS}"
fi

# ---- (Optional) deploy/redeploy the model
if [[ "${SKIP_DEPLOY}" != "true" ]]; then
  oc get ns "${NS}" >/dev/null 2>&1 || oc create ns "${NS}"
  echo "[bootstrap] Applying from: ${MODEL_PATH}/"
  kustomize build "${MODEL_PATH}" | kubectl apply -f -
  echo "[bootstrap] Waiting for llminferenceservice/${MODEL_NAME} to be Ready (timeout 15m)…"
  oc -n "${NS}" wait --for=condition=Ready "llminferenceservice/${MODEL_NAME}" --timeout=15m
else
  echo "[bootstrap] Skipping model deployment (SKIP_DEPLOY=${SKIP_DEPLOY})"
fi

# ---- Discover gateway host and MaaS API URL
HOST="${HOST:-}"
if [[ -z "${HOST}" ]]; then
  HOST="$(oc -n "${GATEWAY_NS}" get gateway "${GATEWAY_NAME}" -o jsonpath='{.status.addresses[0].value}' 2>/dev/null || true)"
fi
if [[ -z "${HOST}" ]]; then
  # Fallback to cluster apps domain
  APPS="$(oc get ingresses.config/cluster -o jsonpath='{.spec.domain}' 2>/dev/null || oc get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null || true)"
  HOST="gateway.${APPS}"
fi
if [[ -z "${HOST}" ]]; then
  echo "[bootstrap] ERROR: could not determine HOST" >&2
  exit 1
fi

# Prefer https if healthz responds; otherwise http
SCHEME="https"
if ! curl -skS -m 5 "${SCHEME}://${HOST}/maas-api/healthz" -o /dev/null ; then
  SCHEME="http"
fi

export HOST
export MAAS_API_BASE_URL="${SCHEME}://${HOST}/maas-api"

# Try to discover model base URL via catalog (nice-to-have)
FREE_OC_TOKEN="$(oc whoami -t || true)"
MODEL_URL_DISC=""
if [[ -n "${FREE_OC_TOKEN}" ]]; then
  # Note: /v1/tokens endpoint was removed, now use OC token directly for management APIs
  MODELS_JSON="$(curl -sSk -H "Authorization: Bearer ${FREE_OC_TOKEN}" "${MAAS_API_BASE_URL}/v1/models" || true)"
  MODEL_URL_DISC="$(echo "${MODELS_JSON}" | jq -r '(.data // .models // [])[0]?.url // empty' 2>/dev/null || true)"
fi

# Compose model URL if catalog didn’t give us one
if [[ -z "${MODEL_URL_DISC}" ]]; then
  MODEL_URL_DISC="${SCHEME}://${HOST}/llm/${MODEL_NAME}"
fi
MODEL_URL="${MODEL_URL_DISC%/}/v1"

echo "[bootstrap] MAAS_API_BASE_URL=${MAAS_API_BASE_URL}"
echo "[bootstrap] MODEL_URL=${MODEL_URL}"

# ---- Write .env for convenience
if [[ "${WRITE_ENV}" == "true" ]]; then
  mkdir -p "${E2E_DIR}"
  cat > "${E2E_DIR}/.env" <<EOF
export HOST="${HOST}"
export MAAS_API_BASE_URL="${MAAS_API_BASE_URL}"
export FREE_OC_TOKEN="$(oc whoami -t || true)"
export MODEL_NAME="${MODEL_NAME}"
export MODEL_URL="${MODEL_URL}"
export ROUTER_MODE="gw"  # informational only
EOF
  echo "[bootstrap] wrote ${E2E_DIR}/.env"
fi

echo "[bootstrap] Done."
