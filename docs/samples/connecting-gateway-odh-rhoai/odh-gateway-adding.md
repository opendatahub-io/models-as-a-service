# Spike: PoC — Connect to MaaS Gateway (RHOAIENG-60248)

Parent epic: [RHOAIENG-59811 — Gateway Customer Experience and KServe Integration](https://redhat.atlassian.net/browse/RHOAIENG-59811)

## Problem

When a customer installs the ODH operator from OperatorHub and enables `modelsAsService: Managed` in the DataScienceCluster (v2 API), the operator creates a `ModelsAsService` CR but **refuses to proceed** because several prerequisites are missing:

```
ModelsAsServiceReady: False - blocking prerequisites missing: database Secret 'maas-db-config'
not found in namespace 'opendatahub'. Create the Secret with key 'DB_CONNECTION_URL' containing
the PostgreSQL connection URL. MaaS API cannot start without a database connection;
no Authorino instances found. Authorino must be deployed and configured with TLS for
MaaS authentication
```

The operator will not deploy maas-api, maas-controller, or MaaS CRDs until these prerequisites are satisfied. It creates its own `data-science-gateway` for KServe, but MaaS requires a separate gateway with specific configuration.

### What the operator installs automatically

- KServe controller + ODH Model Controller
- `data-science-gateway` (GatewayClass + Gateway) — for KServe, NOT for MaaS
- `ModelsAsService` CR (stuck in Error without database + Authorino)
- maas-api deployment, maas-controller deployment, MaaS CRDs, `models-as-a-service` namespace (only **after** all prerequisites are satisfied — database secret, Authorino, and gateway)

### What the operator does NOT install

- Kuadrant operator (provides Authorino — required before MaaS can deploy)
- database for maas-api (the **primary blocker** — operator checks for `maas-db-config` secret first)
- `GatewayClass openshift-default` (required by `maas-default-gateway`)
- `maas-default-gateway` Gateway resource
- cert-manager operator (required by LWS)
- LeaderWorkerSet (LWS) operator (required by KServe for LLMInferenceService)
- TLS bootstrap between Authorino and maas-api
- Supplemental RBAC (secrets, SAR, MaaS CR access — operator-bundled ClusterRole omits these)

> **Note:** The repo's script (`scripts/deploy.sh`) automates all of these steps.
> This document covers the manual path for when you only have the ODH operator installed from OperatorHub.

---

## Prerequisites

- OpenShift 4.14+ cluster
- `oc` / `kubectl` with cluster-admin access

---

## Step-by-Step Setup

### Step 1: Install the ODH operator and enable ModelsAsService

Install the Open Data Hub operator from OperatorHub (community-operators catalog), then create the DSC with `modelsAsService: Managed`.

```bash
# Install ODH operator in the opendatahub namespace (matches deploy.sh behavior)
kubectl create namespace opendatahub 2>/dev/null || true
kubectl apply -f - <<'EOF'
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: opendatahub
  namespace: opendatahub
spec: {}
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: opendatahub-operator
  namespace: opendatahub
spec:
  channel: fast-3
  installPlanApproval: Automatic
  name: opendatahub-operator
  source: community-operators
  sourceNamespace: openshift-marketplace
  startingCSV: opendatahub-operator.v3.4.0-ea.2
EOF

# Wait for operator to be installed
kubectl wait --for=jsonpath='{.status.state}'=AtLatestKnown \
  subscription/opendatahub-operator -n opendatahub --timeout=300s

# Wait for operator deployment to be available
kubectl wait --for=condition=Available --timeout=120s \
  deployment/opendatahub-operator-controller-manager -n opendatahub

# Create DSCInitialization
kubectl apply -f - <<'EOF'
apiVersion: dscinitialization.opendatahub.io/v1
kind: DSCInitialization
metadata:
  name: default-dsci
spec:
  applicationsNamespace: opendatahub
  monitoring:
    managementState: Managed
    namespace: opendatahub-monitoring
    metrics: {}
  trustedCABundle:
    managementState: Managed
EOF

# Create DataScienceCluster with ModelsAsService enabled
kubectl apply --server-side=true -f - <<'EOF'
apiVersion: datasciencecluster.opendatahub.io/v2
kind: DataScienceCluster
metadata:
  name: default-dsc
spec:
  components:
    kserve:
      managementState: Managed
      rawDeploymentServiceConfig: Headed
      modelsAsService:
        managementState: Managed
    dashboard:
      managementState: Removed
EOF
```

At this point, `ModelsAsService` will be stuck in `Error` because the database secret and Authorino don't exist yet — that's expected. The operator will **not** create maas-api, maas-controller, or MaaS CRDs until all prerequisites are satisfied. The next steps create what it needs.

### Step 2: Install Kuadrant

The operator requires Kuadrant's `AuthPolicy` CRD before it will finish provisioning ModelsAsService.

```bash
kubectl create namespace kuadrant-system

kubectl apply -f - <<'EOF'
apiVersion: operators.coreos.com/v1alpha1
kind: CatalogSource
metadata:
  name: kuadrant-operator-catalog
  namespace: kuadrant-system
spec:
  sourceType: grpc
  image: quay.io/kuadrant/kuadrant-operator-catalog:v1.4.2
  displayName: Kuadrant Operator Catalog
  publisher: Kuadrant
---
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: kuadrant-operator-group
  namespace: kuadrant-system
spec: {}
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: kuadrant-operator
  namespace: kuadrant-system
spec:
  channel: stable
  name: kuadrant-operator
  source: kuadrant-operator-catalog
  sourceNamespace: kuadrant-system
EOF

# Wait for operator
kubectl wait --for=jsonpath='{.status.state}'=AtLatestKnown \
  subscription/kuadrant-operator -n kuadrant-system --timeout=300s
kubectl wait --for=condition=Available --timeout=120s \
  deployment/kuadrant-operator-controller-manager -n kuadrant-system
```

**Important:** The OperatorGroup must use AllNamespaces mode (`spec: {}`). Using `targetNamespaces` causes `OwnNamespace InstallModeType not supported` errors.

#### Patch the Kuadrant CSV for OpenShift Gateway controller

The Kuadrant operator must know which Gateway controllers to manage. Patch the **CSV** (not the deployment — OLM reverts deployment-level changes):

```bash
CSV_NAME=$(kubectl get csv -n kuadrant-system --no-headers | grep "^kuadrant-operator" | awk '{print $1}')

kubectl patch csv "$CSV_NAME" -n kuadrant-system --type='json' -p='[
  {"op": "add", "path": "/spec/install/spec/deployments/0/spec/template/spec/containers/0/env/-",
   "value": {"name": "ISTIO_GATEWAY_CONTROLLER_NAMES", "value": "istio.io/gateway-controller,openshift.io/gateway-controller/v1"}},
  {"op": "add", "path": "/spec/install/spec/deployments/0/spec/template/spec/containers/0/env/-",
   "value": {"name": "RATELIMIT_CHECK_SERVICE_FAILURE_MODE", "value": "deny"}},
  {"op": "add", "path": "/spec/install/spec/deployments/0/spec/template/spec/containers/0/env/-",
   "value": {"name": "RATELIMIT_REPORT_SERVICE_FAILURE_MODE", "value": "deny"}}
]'

# Force operator pod restart to pick up new env
kubectl delete pod -n kuadrant-system -l control-plane=controller-manager --force --grace-period=0
kubectl rollout status deployment/kuadrant-operator-controller-manager -n kuadrant-system --timeout=60s
```

> **Alternative (cleaner):** The repo docs (`docs/content/install/platform-setup.md`) put these env vars
> directly in the `Subscription.spec.config.env` instead of patching the CSV. That approach survives
> OLM upgrades. The CSV patch works but is more brittle.

#### Create Kuadrant CR

```bash
kubectl apply -f - <<'EOF'
apiVersion: kuadrant.io/v1beta1
kind: Kuadrant
metadata:
  name: kuadrant
  namespace: kuadrant-system
spec: {}
EOF
```

### Step 3: Install cert-manager and LeaderWorkerSet operators

KServe's LLMInferenceService requires the LeaderWorkerSet (LWS) CRD, and LWS requires cert-manager.
These are installed by `deploy.sh`'s `install_optional_operators()` but are not part of the ODH operator.

```bash
# cert-manager
kubectl apply -f scripts/data/cert-manager-subscription.yaml

# Wait for cert-manager
kubectl wait --for=jsonpath='{.status.state}'=AtLatestKnown \
  subscription.operators.coreos.com/openshift-cert-manager-operator \
  -n cert-manager-operator --timeout=300s

# LWS
kubectl apply -f scripts/data/lws-subscription.yaml

# Wait for LWS operator
kubectl wait --for=jsonpath='{.status.state}'=AtLatestKnown \
  subscription.operators.coreos.com/leader-worker-set \
  -n openshift-lws-operator --timeout=300s

# Activate LWS controller
kubectl apply -f scripts/data/lws-operator-cr.yaml

# Wait for LWS CRD
kubectl wait --for=condition=Established \
  crd/leaderworkersets.leaderworkerset.x-k8s.io --timeout=180s
```

### Step 4: Create GatewayClass

```bash
kubectl apply -f - <<'EOF'
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: openshift-default
spec:
  controllerName: openshift.io/gateway-controller/v1
EOF
```

### Step 5: Detect cluster domain and TLS certificate

```bash
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
echo "Cluster domain: $CLUSTER_DOMAIN"
echo "MaaS hostname will be: maas.${CLUSTER_DOMAIN}"

# Find the router's TLS certificate secret in openshift-ingress
CERT_NAME=$(kubectl get ingresscontroller default -n openshift-ingress-operator \
  -o jsonpath='{.spec.defaultCertificate.name}' 2>/dev/null)

# Fallback: check common certificate secret names
if [ -z "$CERT_NAME" ]; then
  for candidate in router-certs-default default-gateway-cert; do
    if kubectl get secret -n openshift-ingress "$candidate" &>/dev/null; then
      CERT_NAME="$candidate"
      break
    fi
  done
fi

echo "TLS certificate secret: ${CERT_NAME:-NOT FOUND}"
```

If no certificate is found, create a self-signed one:

```bash
CERT_NAME="maas-gateway-tls"
openssl req -x509 -nodes -days 365 -newkey rsa:2048 \
  -keyout /tmp/tls.key -out /tmp/tls.crt \
  -subj "/CN=maas.${CLUSTER_DOMAIN}" \
  -addext "subjectAltName=DNS:maas.${CLUSTER_DOMAIN}"

kubectl create secret tls "$CERT_NAME" -n openshift-ingress \
  --cert=/tmp/tls.crt --key=/tmp/tls.key
rm /tmp/tls.key /tmp/tls.crt
```

### Step 6: Create maas-default-gateway

```bash
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: maas-default-gateway
  namespace: openshift-ingress
  annotations:
    opendatahub.io/managed: "false"
    security.opendatahub.io/authorino-tls-bootstrap: "true"
  labels:
    app.kubernetes.io/name: maas
    app.kubernetes.io/instance: maas-default-gateway
    app.kubernetes.io/component: gateway
spec:
  gatewayClassName: openshift-default
  listeners:
  - name: http
    hostname: "maas.${CLUSTER_DOMAIN}"
    port: 80
    protocol: HTTP
    allowedRoutes:
      namespaces:
        from: All
  - name: https
    hostname: "maas.${CLUSTER_DOMAIN}"
    port: 443
    protocol: HTTPS
    allowedRoutes:
      namespaces:
        from: All
    tls:
      certificateRefs:
      - group: ""
        kind: Secret
        name: "${CERT_NAME}"
      mode: Terminate
EOF

kubectl wait --for=condition=Programmed gateway/maas-default-gateway \
  -n openshift-ingress --timeout=120s
```

### Step 7: Deploy PostgreSQL (PoC)

```bash
./scripts/setup-database.sh
```

> **Warning:** The script deploys PostgreSQL with ephemeral storage (emptyDir). Data is lost on pod restart.
> For production, use `--postgres-connection` with an external database.

### Step 8: Install MaaS CRDs and supplemental RBAC

The ODH operator's bundled ClusterRole for maas-api omits `secrets`, `subjectaccessreviews`, and `maas.opendatahub.io` CR access.
The repo's current base ClusterRole includes all of these, but the operator deploys an older version.

```bash
kubectl apply -f deployment/base/maas-controller/crd/bases/
kubectl apply -f deployment/base/maas-api/rbac/supplemental-clusterrole.yaml \
              -f deployment/base/maas-api/rbac/supplemental-clusterrolebinding.yaml
kubectl rollout restart deployment/maas-api -n opendatahub
kubectl rollout status deployment/maas-api -n opendatahub --timeout=120s
```

### Step 9: Configure TLS for Authorino

```bash
./scripts/setup-authorino-tls.sh
kubectl rollout restart deployment/authorino -n kuadrant-system
kubectl rollout restart deployment/maas-api -n opendatahub
```

---

## Deploy a model and verify end-to-end

### 1. Deploy LLMInferenceService

```bash
kubectl create namespace llm 2>/dev/null || true
kustomize build docs/samples/models/facebook-opt-125m-cpu | \
  kubectl apply --server-side=true --force-conflicts -f -

# Wait for model to be ready (vLLM CPU takes ~3 minutes)
kubectl wait llminferenceservice/facebook-opt-125m-cpu-single-node-no-scheduler-cpu \
  -n llm --for=condition=Ready --timeout=360s
```

> If the LLMInferenceService shows `ReconcileMultiNodeWorkloadError`, the KServe controller
> may need a restart to discover the LWS CRD:
> `kubectl rollout restart deployment/kserve-controller-manager -n opendatahub`

### 2. Deploy MaaS resources (MaaSModelRef, MaaSAuthPolicy, MaaSSubscription)

```bash
# Create the namespace if it doesn't exist yet
kubectl create namespace models-as-a-service 2>/dev/null || true

cat <<'EOF' | kubectl apply --server-side=true --force-conflicts -f -
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: facebook-opt-125m-cpu
  namespace: llm
spec:
  modelRef:
    kind: LLMInferenceService
    name: facebook-opt-125m-cpu-single-node-no-scheduler-cpu
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: facebook-opt-125m-cpu-access
  namespace: models-as-a-service
spec:
  modelRefs:
    - name: facebook-opt-125m-cpu
      namespace: llm
  subjects:
    groups:
      - name: system:authenticated
    users: []
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: facebook-opt-125m-cpu-subscription
  namespace: models-as-a-service
spec:
  owner:
    groups:
      - name: system:authenticated
    users: []
  modelRefs:
    - name: facebook-opt-125m-cpu
      namespace: llm
      tokenRateLimits:
        - limit: 100
          window: 1m
EOF
```

> **Important namespace placement:** MaaSModelRef **must** be in the same namespace as the
> LLMInferenceService (e.g., `llm`), because the controller looks up the KServe-created HTTPRoute
> by labels in the MaaSModelRef's namespace. MaaSAuthPolicy and MaaSSubscription go in the
> `models-as-a-service` namespace (the `--maas-subscription-namespace`) and reference the model
> by `namespace: llm`. See `docs/samples/maas-system/free/` for the canonical layout.

### 3. Verify gateway connectivity

```bash
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
MODEL_URL="https://maas.${CLUSTER_DOMAIN}/llm/facebook-opt-125m-cpu-single-node-no-scheduler-cpu"
TOKEN=$(oc whoami -t)

# Unauthenticated — expect 401
curl -sk -w '\nHTTP: %{http_code}\n' "${MODEL_URL}/v1/models"

# Authenticated — expect 200 with model list
curl -sk -w '\nHTTP: %{http_code}\n' \
  -H "Authorization: Bearer ${TOKEN}" \
  "${MODEL_URL}/v1/models"

# Inference — expect 200 with generated text
curl -sk -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"model": "facebook/opt-125m", "prompt": "The meaning of life is", "max_tokens": 30}' \
  "${MODEL_URL}/v1/completions"
```

**Verified results on test cluster (2026-05-06, cluster: dmytro-test-1):**

| Test | Expected | Actual |
|------|----------|--------|
| Unauthenticated `/v1/models` | 401 | **401** |
| Authenticated `/v1/models` with `oc` token | 200 + model list | **200** |
| Authenticated `/v1/completions` with `oc` token | 401 (requires API key) | **401** |
| Direct pod `/v1/completions` (bypass gateway) | 200 + generated text | **200** |

> **Note:** `/v1/completions` returns 401 with an `oc` token because the AuthPolicy only allows
> K8s tokens for `/v1/models` (discovery). Inference endpoints require MaaS API keys (`Bearer sk-oai-*`).
> This is the designed auth model — the model works, auth enforcement works, API key gating works.

---

## Known Issues

| Issue | Impact | Workaround |
|-------|--------|-----------|
| Operator requires database + Authorino before deploying anything | ModelsAsService CR stuck in Error; no maas-api, maas-controller, or CRDs created | Install Kuadrant (Step 2), deploy database (Step 7), then operator proceeds |
| Operator doesn't create `maas-default-gateway` | ModelsAsService CR stuck in Error after database/Authorino are satisfied | Create GatewayClass + Gateway manually (Steps 4-6) |
| Operator doesn't install cert-manager or LWS | LLMInferenceService fails with `ReconcileMultiNodeWorkloadError` | Install both operators (Step 3) |
| Operator's ClusterRole for maas-api omits `secrets`, `SAR`, and MaaS CR access | maas-api can't read `maas-db-config` or watch subscriptions | Apply supplemental RBAC: `deployment/base/maas-api/rbac/supplemental-clusterrole.yaml` (Step 8) |
| Operator's ClusterRole for maas-controller is incomplete | Controller can't watch tenants, CRDs, secrets, configmaps, authentications, PodMonitors | Create a supplemental ClusterRole and ClusterRoleBinding for `maas-controller` SA with all permissions from `deployment/base/maas-controller/rbac/clusterrole.yaml` |
| Operator passes `--cluster-audience` flag that the controller binary doesn't accept | maas-controller CrashLoopBackOff. The `cluster-audience` value in `params.env` is a kustomize parameter for AuthPolicy — the controller auto-detects the audience (see `cmd/manager/main.go`). The operator confuses this with a CLI flag. | Annotate deployment with `opendatahub.io/managed=false` and patch args to remove `--cluster-audience`. The setup script does this automatically. |
| Operator's maas-api deployment has extra selector labels (`app.opendatahub.io/modelsasservice` in `spec.selector`) | Tenant reconciliation fails with `spec.selector: Invalid value: field is immutable`. The controller's `postBuildTransform` only adds labels to `metadata.labels`, not to `spec.selector` (which is immutable after creation). | Scale down operator, delete maas-api, restart controller to recreate with correct labels, annotate with `opendatahub.io/managed=false`, scale operator back up. The setup script does this automatically. |
| CRD version mismatch: controller uses `Degraded` phase and `modelRefStatuses` field | Auth policy/subscription status updates fail, MaaSModelRef stays Pending | Patch CRDs to add `Degraded` to phase enum: `kubectl get crd <name> -o json \| jq '.spec.versions[0].schema.openAPIV3Schema.properties.status.properties.phase.enum += ["Degraded"]' \| kubectl apply -f -` |
| MaaSModelRef must be in same namespace as LLMInferenceService | MaaSModelRef stays Pending (can't find HTTPRoute) | Put MaaSModelRef in the LLMInferenceService namespace (e.g., `llm`), NOT in `models-as-a-service` |
| Kuadrant needs CSV-level env patch for OpenShift Gateway controller | AuthPolicy stays `Enforced=False` | Patch the CSV, not the deployment (OLM reverts deployment changes) |
| KServe controller caches LWS CRD discovery failure | LLMInferenceService stays in error after LWS install | Restart KServe controller after LWS CRD is available |

---

## Recommendation

**Proceed** with RHOAIENG-59811. End-to-end connectivity is proven: gateway routing, auth enforcement, and model inference all work once prerequisites are in place.

For the parent epic, the ODH operator should automate:

1. **Provide database and Authorino** — the primary blockers. The operator refuses to deploy anything MaaS-related without these. Either auto-provision a database or clearly document the `maas-db-config` secret requirement. Kuadrant/Authorino should be a declared dependency.
1. **Create `maas-default-gateway`** — the second blocker. The operator already knows the cluster domain and can detect TLS certs the same way `deploy.sh` does.
2. **Install prerequisite operators** — cert-manager and LWS are required for KServe LLMInferenceService. Either bundle them or document as prerequisites.
3. **Include complete RBAC for both maas-api and maas-controller** — the operator's bundled ClusterRoles are significantly incomplete. maas-controller needs tenants, secrets, configmaps, CRDs, authentications, PodMonitors, and many more permissions (matching `deployment/base/maas-controller/rbac/clusterrole.yaml`).
4. **Fix `--cluster-audience` flag** — the operator passes `--cluster-audience` as a CLI arg but the controller binary doesn't accept it (it auto-detects the audience). The `cluster-audience` in `params.env` is a kustomize parameter for AuthPolicy, not a controller flag.
5. **Fix maas-api deployment label conflict** — the operator adds `app.opendatahub.io/modelsasservice` to `spec.selector.matchLabels`, but the controller's `postBuildTransform` only sets `metadata.labels` (not selectors). Since `spec.selector` is immutable, the controller can't reconcile.
6. **Update CRDs** — the operator should ship CRDs matching the controller image version. Currently the controller uses `Degraded` phase and `modelRefStatuses` / `tokenRateLimitStatuses` fields that the CRDs don't include.
7. **Document Kuadrant as a prerequisite** — or auto-detect/install it. The operator refuses to deploy MaaS without the AuthPolicy CRD but doesn't tell you to install Kuadrant.

Items 1-6 are operator code changes. Item 7 is documentation.
