# Manual kind verification for MaaS API guardrail selection

This guide verifies the MaaS API selection response without deploying the MaaS
controller, Authorino, or an inference backend. It creates the minimum
Kubernetes resources needed by the MaaS API process, then calls
`/internal/v1/subscriptions/select` directly.

The test verifies three outcomes:

1. A selected, accepted `AIGuardrail` produces its check ID in
   `selectedGuardrailChecks`.
2. An attachment-free selection returns `selectedGuardrailChecks: {}`.
3. A selected policy that cannot be read returns `guardrail_unavailable` and
   no partial selection.

The manually patched `AIGuardrail.status` is intentional. This is an API
selection integration test, not an AI Gateway controller reconciliation test.

## Prerequisites

Run the commands from the repository root. The sibling
`../ai-gateway-controller` checkout must contain the merged check ID helper and
the AIGuardrail CRD.

Required local tools:

- Docker
- kind
- kubectl
- make
- curl
- jq
- sha256sum

The cluster needs Internet access to pull `postgres:16` and the Gateway API
CRDs. The MaaS API image build also pulls its base images and Go modules.

## Create the cluster and install CRDs

```bash
export KIND_CLUSTER=maas-guardrail
export TEST_NAMESPACE=maas-guardrail-test
export AITENANT_NAMESPACE=ai-tenants
export AIGW_DIR=../ai-gateway-controller

kind create cluster --name "${KIND_CLUSTER}"

kubectl apply -f \
  https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.2/standard-install.yaml

kubectl wait --for=condition=Established --timeout=60s \
  crd/gatewayclasses.gateway.networking.k8s.io \
  crd/gateways.gateway.networking.k8s.io

kubectl apply -f deployment/base/maas-controller/crd/bases/
kubectl apply -f "${AIGW_DIR}/config/crd/bases/aigateway.opendatahub.io_aiguardrails.yaml"

kubectl wait --for=condition=Established --timeout=60s \
  crd/aitenants.maas.opendatahub.io \
  crd/maastenantconfigs.maas.opendatahub.io \
  crd/maassubscriptions.maas.opendatahub.io \
  crd/maasmodelrefs.maas.opendatahub.io \
  crd/maasauthpolicies.maas.opendatahub.io \
  crd/aiguardrails.aigateway.opendatahub.io
```

## Build and load the MaaS API image

The local image name is deliberately different from a published image. The
deployment below uses `imagePullPolicy: Never`, so kind uses only this loaded
image.

```bash
make -C maas-api \
  CONTAINER_ENGINE=docker \
  REPO=maas-api \
  TAG=guardrail-manual \
  build-image

kind load docker-image maas-api:guardrail-manual --name "${KIND_CLUSTER}"
```

## Create the runtime prerequisites

MaaS API needs PostgreSQL for its normal startup path. It also resolves a
Gateway-owned Service during startup, even though this test does not send an
inference request. The Gateway fixture below supplies only that required
identity; it does not need a Gateway controller.

```bash
kubectl create namespace "${TEST_NAMESPACE}"
kubectl create namespace "${AITENANT_NAMESPACE}"
kubectl create namespace models

kubectl apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: maas-db-config
  namespace: ${TEST_NAMESPACE}
type: Opaque
stringData:
  DB_CONNECTION_URL: postgresql://maas:maas@postgres.${TEST_NAMESPACE}.svc:5432/maas?sslmode=disable
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: maas-api
  namespace: ${TEST_NAMESPACE}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: maas-api-manual-guardrail
rules:
- apiGroups: [""]
  resources: ["secrets"]
  resourceNames: ["maas-db-config"]
  verbs: ["get"]
- apiGroups: [""]
  resources: ["services"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["maas.opendatahub.io"]
  resources: ["aitenants", "maasauthpolicies", "maasmodelrefs", "maassubscriptions", "maastenantconfigs"]
  verbs: ["get", "list", "watch"]
- apiGroups: ["aigateway.opendatahub.io"]
  resources: ["aiguardrails"]
  verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: maas-api-manual-guardrail
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: maas-api-manual-guardrail
subjects:
- kind: ServiceAccount
  name: maas-api
  namespace: ${TEST_NAMESPACE}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
  namespace: ${TEST_NAMESPACE}
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
        image: postgres:16
        env:
        - name: POSTGRES_DB
          value: maas
        - name: POSTGRES_USER
          value: maas
        - name: POSTGRES_PASSWORD
          value: maas
        ports:
        - containerPort: 5432
---
apiVersion: v1
kind: Service
metadata:
  name: postgres
  namespace: ${TEST_NAMESPACE}
spec:
  selector:
    app: postgres
  ports:
  - port: 5432
    targetPort: 5432
---
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: manual-gateway-class
spec:
  controllerName: example.com/manual-gateway
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: manual-gateway
  namespace: ${TEST_NAMESPACE}
spec:
  gatewayClassName: manual-gateway-class
  listeners:
  - name: http
    protocol: HTTP
    port: 80
EOF

kubectl rollout status deployment/postgres -n "${TEST_NAMESPACE}" --timeout=120s

export GATEWAY_UID="$(kubectl get gateway manual-gateway -n "${TEST_NAMESPACE}" -o jsonpath='{.metadata.uid}')"

kubectl apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: manual-gateway-service
  namespace: ${TEST_NAMESPACE}
  labels:
    gateway.networking.k8s.io/gateway-name: manual-gateway
  ownerReferences:
  - apiVersion: gateway.networking.k8s.io/v1
    kind: Gateway
    name: manual-gateway
    uid: ${GATEWAY_UID}
spec:
  ports:
  - name: https
    port: 443
    targetPort: 443
EOF
```

## Create the selection fixtures

The AITenant attachment selects `privacy/pii`. The other attachment scopes are
empty so the expected response contains exactly one check ID. The
MaaSAuthPolicy grants the requested group access to `models/qwen3`.

```bash
kubectl apply -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: AITenant
metadata:
  name: tenant-a
  namespace: ${AITENANT_NAMESPACE}
spec:
  guardrails:
  - ref:
      name: privacy
    checks:
    - pii
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaasTenantConfig
metadata:
  name: default-tenant
  namespace: ${TEST_NAMESPACE}
spec: {}
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: qwen3
  namespace: models
spec:
  modelRef:
    kind: ExternalModel
    name: manual-backend
  tenantRef: tenant-a
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: subscription-a
  namespace: ${TEST_NAMESPACE}
spec:
  owner:
    groups:
    - name: guardrail-testers
  modelRefs:
  - namespace: models
    name: qwen3
    unlimited: true
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: qwen3-access
  namespace: ${TEST_NAMESPACE}
spec:
  modelRefs:
  - namespace: models
    name: qwen3
  subjects:
    groups:
    - name: guardrail-testers
---
apiVersion: aigateway.opendatahub.io/v1alpha1
kind: AIGuardrail
metadata:
  name: privacy
  namespace: ${TEST_NAMESPACE}
spec:
  provider:
    nemo:
      ref:
        name: manual-nemo
    timeout: 5s
  checks:
  - name: pii
    configId: pii
    phases:
    - Input
EOF

kubectl patch aitenant tenant-a -n "${AITENANT_NAMESPACE}" --subresource=status --type=merge \
  -p "{\"status\":{\"tenantNamespace\":\"${TEST_NAMESPACE}\"}}"

kubectl patch maasmodelref qwen3 -n models --subresource=status --type=merge \
  -p '{"status":{"resolvedTenantRef":"tenant-a"}}'

kubectl patch maassubscription subscription-a -n "${TEST_NAMESPACE}" --subresource=status --type=merge \
  -p '{"status":{"phase":"Active"}}'

export GUARDRAIL_GENERATION="$(kubectl get aiguardrail privacy -n "${TEST_NAMESPACE}" -o jsonpath='{.metadata.generation}')"
export CONDITION_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

kubectl patch aiguardrail privacy -n "${TEST_NAMESPACE}" --subresource=status --type=merge \
  -p "{\"status\":{\"bindingRevision\":\"manual-1\",\"conditions\":[{\"type\":\"Accepted\",\"status\":\"True\",\"observedGeneration\":${GUARDRAIL_GENERATION},\"lastTransitionTime\":\"${CONDITION_TIME}\",\"reason\":\"ManualTest\",\"message\":\"Manually accepted for MaaS API selection verification\"}]}}"
```

## Deploy MaaS API

```bash
kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: maas-api
  namespace: ${TEST_NAMESPACE}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: maas-api
  template:
    metadata:
      labels:
        app: maas-api
    spec:
      serviceAccountName: maas-api
      containers:
      - name: maas-api
        image: maas-api:guardrail-manual
        imagePullPolicy: Never
        ports:
        - containerPort: 8080
        env:
        - name: NAMESPACE
          value: ${TEST_NAMESPACE}
        - name: SECURE
          value: "false"
        - name: METRICS_SECURE
          value: "false"
        - name: METRICS_PORT
          value: "9090"
        - name: LOG_FORMAT
          value: zap
        - name: TENANT_NAME
          value: tenant-a
        - name: AITENANT_NAME
          value: tenant-a
        - name: AITENANT_NAMESPACE
          value: ${AITENANT_NAMESPACE}
        - name: MAAS_SUBSCRIPTION_NAMESPACE
          value: ${TEST_NAMESPACE}
        - name: GATEWAY_NAME
          value: manual-gateway
        - name: GATEWAY_NAMESPACE
          value: ${TEST_NAMESPACE}
---
apiVersion: v1
kind: Service
metadata:
  name: maas-api
  namespace: ${TEST_NAMESPACE}
spec:
  selector:
    app: maas-api
  ports:
  - port: 8080
    targetPort: 8080
EOF

kubectl rollout status deployment/maas-api -n "${TEST_NAMESPACE}" --timeout=120s
kubectl logs deployment/maas-api -n "${TEST_NAMESPACE}" --tail=100
```

## Port-forward and verify the selected check

Run the port-forward in one terminal:

```bash
kubectl port-forward -n "${TEST_NAMESPACE}" service/maas-api 8080:8080
```

Run the following in another terminal:

```bash
export CHECK_ID="$(printf '%s' "${TEST_NAMESPACE}:privacy:pii" | sha256sum | awk '{print $1}')"

export RESPONSE="$(curl --fail-with-body --silent --show-error \
  -H 'Content-Type: application/json' \
  -X POST http://127.0.0.1:8080/internal/v1/subscriptions/select \
  --data '{
    "groups": ["guardrail-testers"],
    "username": "alice",
    "requestedModel": "models/qwen3"
  }')"

printf '%s\n' "${RESPONSE}" | jq .

printf '%s\n' "${RESPONSE}" | jq -e --arg id "${CHECK_ID}" '
  .accessAllowed == true and
  .name == "subscription-a" and
  .resolvedModel == "models/qwen3" and
  .selectedGuardrailChecks[$id] == true
'
```

The successful response contains the selected check ID:

```json
{
  "name": "subscription-a",
  "resolvedModel": "models/qwen3",
  "accessAllowed": true,
  "selectedGuardrailChecks": {
    "<sha256-of-maas-guardrail-test:privacy:pii>": true
  }
}
```

## Verify the empty and unavailable cases

Remove the only attachment and repeat the request. The field must remain
present as an empty object.

```bash
kubectl patch aitenant tenant-a -n "${AITENANT_NAMESPACE}" --type=merge \
  -p '{"spec":{"guardrails":[]}}'
kubectl rollout restart deployment/maas-api -n "${TEST_NAMESPACE}"
kubectl rollout status deployment/maas-api -n "${TEST_NAMESPACE}" --timeout=120s

curl --fail-with-body --silent --show-error \
  -H 'Content-Type: application/json' \
  -X POST http://127.0.0.1:8080/internal/v1/subscriptions/select \
  --data '{
    "groups": ["guardrail-testers"],
    "username": "alice",
    "requestedModel": "models/qwen3"
  }' | jq -e '.selectedGuardrailChecks == {}'
```

Restore the attachment, delete the selected policy, and repeat the request.
The response must not contain a partial `selectedGuardrailChecks` map.

```bash
kubectl patch aitenant tenant-a -n "${AITENANT_NAMESPACE}" --type=merge \
  -p '{"spec":{"guardrails":[{"ref":{"name":"privacy"},"checks":["pii"]}]}}'
kubectl delete aiguardrail privacy -n "${TEST_NAMESPACE}"
kubectl rollout restart deployment/maas-api -n "${TEST_NAMESPACE}"
kubectl rollout status deployment/maas-api -n "${TEST_NAMESPACE}" --timeout=120s

curl --fail-with-body --silent --show-error \
  -H 'Content-Type: application/json' \
  -X POST http://127.0.0.1:8080/internal/v1/subscriptions/select \
  --data '{
    "groups": ["guardrail-testers"],
    "username": "alice",
    "requestedModel": "models/qwen3"
  }' | jq -e '
    .error == "guardrail_unavailable" and
    .message == "selected guardrails are unavailable" and
    (has("selectedGuardrailChecks") | not)
  '
```

## Cleanup

This removes the entire disposable kind cluster and all test resources.

```bash
kind delete cluster --name "${KIND_CLUSTER}"
```
