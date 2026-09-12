# Install MaaS on xKS (non-OpenShift Kubernetes)

This guide covers the end-to-end installation and validation of Models-as-a-Service (MaaS) on
non-OpenShift Kubernetes clusters (xKS), specifically Azure Kubernetes Service (AKS).

Reference: [Official MaaS Quickstart](https://opendatahub-io.github.io/models-as-a-service/dev/quickstart/)
| [External Model Setup](https://opendatahub-io.github.io/models-as-a-service/dev/install/external-model-setup/)

!!! note "Difference from OpenShift"
    On OpenShift, MaaS is deployed via OLM operators and DataScienceCluster CRs. On xKS,
    the entire stack is deployed via two Helm charts that bundle the operators, CRDs, and
    dependencies together. The auth/rate-limit plane uses Kuadrant (Authorino + Limitador)
    deployed by RHCL, while OpenShift uses OSSM (OpenShift Service Mesh).

## Prerequisites

### Required Tools

| Tool | Minimum Version | Purpose |
|------|----------------|---------|
| `kubectl` | 1.28+ | Cluster access |
| `helm` | 3.17+ | Chart installation |
| `az` CLI | 2.60+ | AKS cluster management (Azure only) |
| `jq` | 1.6+ | JSON parsing for validation |
| `curl` | any | API testing |

### Cluster Requirements

- A running AKS cluster (or other CNCF-conformant Kubernetes 1.28+)
- `cluster-admin` access via `kubectl`
- A Red Hat pull secret (`auth.json`) for accessing `registry.redhat.io` images
- Recommended: 16 vCPUs, 32GB RAM (for all MaaS components + model serving)

### Repository

Clone the `odh-gitops` repository:

```bash
git clone https://github.com/opendatahub-io/odh-gitops.git
cd odh-gitops
```

### Azure Login (AKS only)

```bash
az login --tenant <TENANT_ID>
az account set --subscription <SUBSCRIPTION_ID>
az aks get-credentials --resource-group <RESOURCE_GROUP> --name <CLUSTER_NAME>
```

Verify connectivity:

```bash
kubectl cluster-info
```

## Architecture Overview

The xKS deployment uses a two-chart approach:

```text
┌─────────────────────────────────────────────────────────────────────┐
│  Chart 1: rhcl-operator (RHCL / Kuadrant)                           │
│  ┌────────────────────┐  ┌──────────────────┐  ┌────────────────┐   │
│  │ Kuadrant Operator  │  │    Authorino     │  │   Limitador    │   │
│  │ (kuadrant-operators)│  │ (kuadrant-system)│  │(kuadrant-system)│ │
│  └────────────────────┘  └──────────────────┘  └────────────────┘   │
└─────────────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────────────┐
│  Chart 2: rhai-on-xks-chart (MaaS + Dependencies)                   │
│                                                                     │
│  Dependencies:                                                      │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────────────────┐   │
│  │ cert-manager │  │    Istio     │  │ Azure Cloud Manager (CA) │   │
│  └──────────────┘  └──────────────┘  └──────────────────────────┘   │
│                                                                     │
│  Operators:                                                         │
│  ┌──────────────────────────────────────────────────────────────┐   │
│  │ rhai-operator → ai-gateway-operator → maas-controller        │   │
│  │                                     → kserve-module           │  │
│  └──────────────────────────────────────────────────────────────┘   │
│                                                                     │
│  Workloads:                                                         │
│  ┌──────────────┐  ┌──────────────────────┐  ┌─────────────────┐    │
│  │   maas-api   │  │  payload-processing  │  │   llmisvc-ctrl  │    │
│  └──────────────┘  └──────────────────────┘  └─────────────────┘    │
└─────────────────────────────────────────────────────────────────────┘
```

### Component Chain

```text
Helm chart → rhai-operator → ai-gateway-operator → maas-controller → maas-api
                                                  → kserve-module → llmisvc-controller
```

### Namespaces

| Namespace | Contains |
|-----------|----------|
| `redhat-ods-operator` | rhai-operator |
| `redhat-ods-applications` | ai-gateway-operator, maas-controller, kserve-module, gateways |
| `redhat-ai-gateway-infra` | maas-api, payload-processing, PostgreSQL (user-provided) |
| `models-as-a-service` | User models, ExternalModels, subscriptions, auth policies |
| `ai-tenants` | AITenant CRs |
| `kuadrant-operators` | Kuadrant operator pods |
| `kuadrant-system` | Authorino, Limitador |
| `cert-manager` | cert-manager pods |
| `istio-system` | Istiod, sail-operator |
| `rhai-cloudmanager-system` | Azure cloud manager (CA + ClusterIssuer) |

## Step 1: Install RHCL (Red Hat Connectivity Link)

RHCL provides Kuadrant (Authorino for authentication + Limitador for rate limiting). It must
be installed **before** the main chart because the MaaS auth flow depends on Authorino and
Limitador being available.

```bash
helm install rhcl-operator charts/dependencies/rhcl-operator \
  --set-file imagePullSecret.dockerConfigJson=/path/to/auth.json
```

!!! note "Pull Secret"
    The `imagePullSecret.dockerConfigJson` value should point to your Red Hat registry
    pull secret (typically `~/.config/containers/auth.json` or downloaded from
    [console.redhat.com](https://console.redhat.com/openshift/install/pull-secret)).
    Use the **absolute path** — tilde (`~`) expansion may not work.

### Verify RHCL

Wait for Kuadrant operators to come up:

```bash
kubectl wait --for=condition=available deploy --all -n kuadrant-operators --timeout=120s
```

Check that Authorino and Limitador are running:

```bash
kubectl get pods -n kuadrant-operators
kubectl get pods -n kuadrant-system
```

Expected output:

```text
# kuadrant-operators
NAME                                 READY   STATUS    RESTARTS   AGE
kuadrant-operator-xxxxx              1/1     Running   0          60s

# kuadrant-system
NAME                                 READY   STATUS    RESTARTS   AGE
authorino-xxxxx                      1/1     Running   0          45s
limitador-xxxxx                      1/1     Running   0          45s
```

## Step 2: Install the Main Chart (rhai-on-xks)

Once RHCL is ready, install the main chart which deploys the entire MaaS stack:

```bash
helm upgrade --install rhaii-on-xks charts/rhai-on-xks-chart \
  --set azure.enabled=true \
  --set components.kserve.enabled=true \
  --set components.aigateway.enabled=true \
  --set components.aigateway.modelsAsAService.enabled=true \
  --set-file imagePullSecret.dockerConfigJson=/path/to/auth.json \
  --timeout 15m
```

After helm finishes, restart the Kuadrant operator so it detects Gateway API CRDs
(installed by Istio/sail-operator which deploys alongside):

```bash
kubectl rollout restart deploy/kuadrant-operator-controller-manager -n kuadrant-operators 2>/dev/null || true
```

### Cross-Namespace HTTPRoute Access (Required for MaaS)

MaaS creates HTTPRoutes in the model namespace (e.g., `models-as-a-service`) that must attach
to the MaaS Gateway in `redhat-ods-applications`. By default, the gateway only allows routes
from its own namespace (`from: Same`).

To enable cross-namespace routing, label the model namespace and configure the gateway with a
selector:

```bash
# Label the model namespace
kubectl label ns models-as-a-service maas-gateway-access=true

# Patch the gateway to allow routes from labeled namespaces
kubectl patch gateway maas-default-gateway -n redhat-ods-applications --type=merge -p '{
  "spec": {
    "listeners": [{
      "name": "http",
      "allowedRoutes": {"namespaces": {"from": "Selector", "selector": {"matchLabels": {"maas-gateway-access": "true"}}}}
    }, {
      "name": "https",
      "allowedRoutes": {"namespaces": {"from": "Selector", "selector": {"matchLabels": {"maas-gateway-access": "true"}}}}
    }]
  }
}'
```

!!! tip "Why Selector instead of All?"
    `from: All` is insecure — it allows any namespace to attach routes to the gateway.
    The `Selector` approach restricts access to only namespaces you explicitly label.

### Authorino TLS Configuration

On xKS, the Kuadrant EnvoyFilter connects to Authorino's gRPC port in plaintext, but
Authorino defaults to TLS enabled. This causes HTTP 500 for all authenticated requests.

Disable TLS on the Authorino listener (traffic is cluster-internal):

```bash
kubectl patch authorino authorino -n kuadrant-system --type=merge \
  -p '{"spec":{"listener":{"tls":{"enabled":false}}}}'
```

!!! info "Why is this needed?"
    The Kuadrant operator creates an EnvoyFilter cluster without a TLS transport socket.
    On OpenShift, service mesh mTLS handles encryption transparently. On xKS with standalone
    Istio, the gateway connects directly to Authorino without automatic mTLS, causing a
    protocol mismatch. This is tracked as a Kuadrant operator bug — the operator should
    configure the EnvoyFilter with TLS when Authorino has TLS enabled.

### Helm Values Reference

| Value | Default | Description |
|-------|---------|-------------|
| `azure.enabled` | `false` | Enable Azure Cloud Manager (provides CA + ClusterIssuer) |
| `components.kserve.enabled` | `false` | Enable KServe module for model serving |
| `components.aigateway.enabled` | `false` | Enable AI Gateway operator |
| `components.aigateway.modelsAsAService.enabled` | `false` | Enable MaaS sub-component |
| `imagePullSecret.dockerConfigJson` | `""` | Pull secret for Red Hat registry images |
| `gateway.tls.enabled` | `true` | Enable TLS on gateways |
| `gateway.tls.issuerRef.name` | `rhai-ca-issuer` | ClusterIssuer for TLS certificates |

### Verify the Installation

Wait for core components:

```bash
# cert-manager
kubectl wait --for=condition=available deploy --all -n cert-manager --timeout=180s

# rhai-operator
kubectl wait --for=condition=available deploy/rhai-operator -n redhat-ods-operator --timeout=300s

# ai-gateway-operator
kubectl wait --for=condition=available deploy/ai-gateway-operator -n redhat-ods-applications --timeout=300s

# maas-controller
kubectl wait --for=condition=available deploy/maas-controller -n redhat-ods-applications --timeout=300s
```

Check Custom Resources:

```bash
kubectl get aigateways -A
kubectl get aitenants -A
kubectl get platform -A
```

Check for unhealthy pods:

```bash
kubectl get pods -A --no-headers | grep -vE "Running|Completed|kube-system"
```

!!! note "maas-api CrashLoopBackOff"
    `maas-api` will crash-loop until PostgreSQL is available. This is expected — proceed to
    Step 3 to deploy the database.

## Step 3: Deploy PostgreSQL

`maas-api` requires a PostgreSQL database for API key metadata persistence. On xKS, this is
an external dependency that you must provide.

=== "Development (In-Cluster PostgreSQL)"

    For development and testing, deploy a simple PostgreSQL instance:

    ```bash
    kubectl apply -n redhat-ai-gateway-infra -f - <<'EOF'
    apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: postgres
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
            image: docker.io/library/postgres:15
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
    stringData:
      DB_CONNECTION_URL: "postgres://maas:maas@postgres.redhat-ai-gateway-infra.svc.cluster.local:5432/maas?sslmode=disable"
    EOF
    ```

=== "Production (External Database)"

    Create the `maas-db-config` Secret pointing to your external PostgreSQL:

    ```bash
    kubectl create secret generic maas-db-config \
      -n redhat-ai-gateway-infra \
      --from-literal=DB_CONNECTION_URL='postgresql://USERNAME:PASSWORD@HOSTNAME:5432/DATABASE?sslmode=require'
    ```

After deploying the database, restart `maas-api`:

```bash
kubectl rollout restart deploy/maas-api -n redhat-ai-gateway-infra
kubectl wait --for=condition=available deploy/maas-api -n redhat-ai-gateway-infra --timeout=120s
```

## Step 4: Validate the Deployment

At this point, all core components should be running. Verify:

```bash
# All deployments should be Available
kubectl get deploy -A --no-headers | grep -vE "kube-system" | awk '{print $1, $2, $3}'

# Key CRs should exist
kubectl get aigateways -A
kubectl get aitenants -A
kubectl get maastenantconfigs -A

# Gateway should be programmed
kubectl get gateway maas-default-gateway -n redhat-ods-applications
```

Expected state:

- `AIGateway` CR exists with `Ready=True`
- `AITenant/models-as-a-service` exists in `ai-tenants`
- `MaasTenantConfig/default-tenant` exists in `models-as-a-service`
- `maas-api` is running in `redhat-ai-gateway-infra`
- Gateway shows `Programmed=True`

## Step 5: Deploy an External Model (E2E Validation)

Follow this section to validate the full MaaS auth + inference flow using the
[official External Model Setup guide](https://opendatahub-io.github.io/models-as-a-service/dev/install/external-model-setup/).

### 5a. Deploy a Test Backend (vLLM Simulator)

For testing without a real model provider, use the llm-d inference simulator:

```bash
kubectl apply -n models-as-a-service -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: vllm-sim
spec:
  replicas: 1
  selector:
    matchLabels:
      app: vllm-sim
  template:
    metadata:
      labels:
        app: vllm-sim
    spec:
      containers:
      - name: vllm-sim
        image: ghcr.io/llm-d/llm-d-inference-sim:latest
        command: ["/app/llm-d-inference-sim"]
        args: ["--port=8000", "--model=sim-model", "--enable-kvcache=false"]
        env:
        - name: POD_IP
          valueFrom:
            fieldRef:
              fieldPath: status.podIP
        ports:
        - containerPort: 8000
---
apiVersion: v1
kind: Service
metadata:
  name: vllm-sim
spec:
  selector:
    app: vllm-sim
  ports:
  - port: 8000
    targetPort: 8000
EOF
```

### 5b. Create the Credential Secret

The credential Secret **must** have the label `inference.llm-d.ai/ipp-managed=true` for the
Inference Payload Processor (IPP) to read it:

```bash
kubectl apply -n models-as-a-service -f - <<'EOF'
apiVersion: v1
kind: Secret
metadata:
  name: sim-api-key
  labels:
    inference.llm-d.ai/ipp-managed: "true"
stringData:
  api-key: "test-backend-key"
EOF
```

### 5c. Create MaaS Resources

Per the [official docs](https://opendatahub-io.github.io/models-as-a-service/dev/install/external-model-setup/),
create the ExternalModel, MaaSModelRef, MaaSAuthPolicy, and MaaSSubscription:

```bash
# ExternalModel — defines the backend connection
# Note: annotations go under metadata, not spec
kubectl apply -n models-as-a-service -f - <<'EOF'
apiVersion: maas.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: sim-model
  annotations:
    maas.opendatahub.io/tls: "false"
    maas.opendatahub.io/port: "8000"
spec:
  credentialRef:
    name: sim-api-key
  endpoint: "vllm-sim.models-as-a-service.svc.cluster.local"
  provider: "openai"
  targetModel: "sim-model"
EOF

# MaaSModelRef — registers the model in the MaaS catalog
kubectl apply -n models-as-a-service -f - <<'EOF'
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: sim-model
spec:
  modelRef:
    kind: ExternalModel
    name: sim-model
EOF

# MaaSAuthPolicy — defines who can access the model
kubectl apply -n models-as-a-service -f - <<'EOF'
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: sim-auth
spec:
  modelRefs:
  - name: sim-model
    namespace: models-as-a-service
  subjects:
    users:
    - "test-user"
    groups:
    - name: "system:authenticated"
EOF

# MaaSSubscription — defines rate limits
kubectl apply -n models-as-a-service -f - <<'EOF'
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: sim-subscription
spec:
  modelRefs:
  - name: sim-model
    namespace: models-as-a-service
    tokenRateLimits:
    - limit: 100000
      window: "1h"
  owner:
    users:
    - "test-user"
EOF
```

Wait for all resources to be ready:

```bash
kubectl get externalmodel -n models-as-a-service
kubectl get maasmodelref -n models-as-a-service
kubectl get maasauthpolicy -n models-as-a-service
kubectl get maassubscription -n models-as-a-service
```

### 5d. Apply Routing Workarounds (Known Issues)

Due to known bugs in the current `maas-controller` (tracked in Jira), apply these fixes
before testing inference:

**Fix 1: Delete conflicting ServiceEntries (RHOAIENG-80084)**

`maas-controller` creates `MESH_EXTERNAL` ServiceEntries that conflict with Istio's native
service discovery, causing `cluster_not_found` errors:

```bash
kubectl delete serviceentry -n models-as-a-service --all
```

**Fix 2: Create DestinationRule for non-sidecar backends (RHOAIENG-80085)**

Istio mesh mTLS attempts TLS to model backends that don't have sidecars:

```bash
kubectl apply -f - <<'EOF'
apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: model-backend-no-mtls
  namespace: redhat-ods-applications
spec:
  host: vllm-sim.models-as-a-service.svc.cluster.local
  trafficPolicy:
    tls:
      mode: DISABLE
EOF
```

**Fix 3: Correct HTTPRoute port (RHOAIENG-80086)**

`llmisvc-controller` creates an HTTPRoute with hardcoded port 443 for backends serving HTTP:

```bash
# Get the HTTPRoute name created by llmisvc-controller
ROUTE_NAME=$(kubectl get httproute -n models-as-a-service -o name | grep -v maas | head -1)
if [ -n "$ROUTE_NAME" ]; then
  RULES_COUNT=$(kubectl get $ROUTE_NAME -n models-as-a-service -o jsonpath='{.spec.rules}' | jq length)
  PATCH="["
  for i in $(seq 0 $(($RULES_COUNT - 1))); do
    [ $i -gt 0 ] && PATCH="$PATCH,"
    PATCH="$PATCH{\"op\":\"replace\",\"path\":\"/spec/rules/$i/backendRefs/0/port\",\"value\":8000}"
  done
  PATCH="$PATCH]"
  kubectl patch $ROUTE_NAME -n models-as-a-service --type=json -p="$PATCH"
fi
```

### 5e. Create an API Key

```bash
kubectl port-forward svc/maas-api 8443:8443 -n redhat-ai-gateway-infra &
PF_PID=$!
sleep 3

API_KEY=$(curl -sk https://localhost:8443/v1/api-keys \
  -H "Content-Type: application/json" \
  -H "X-MaaS-Username: test-user" \
  -H 'X-MaaS-Group: ["system:authenticated"]' \
  -d '{"name": "test-key", "subscription": "sim-subscription"}' | jq -r '.key')

echo "API Key: ${API_KEY}"
kill $PF_PID
```

!!! note "Header Format"
    `X-MaaS-Group` must be a JSON array string (e.g., `["system:authenticated"]`),
    not a plain string. The `name` field is required for non-ephemeral keys.

### 5f. Test the Auth Flow

Test from inside the cluster (external LB may not be reachable from your machine):

```bash
kubectl run curl-e2e -n redhat-ods-applications --rm -i --restart=Never \
  --image=curlimages/curl -- sh -c "
echo '=== No auth → 401 ==='
curl -s --max-time 10 -w '\nHTTP: %{http_code}\n' \
  'http://maas-default-gateway-istio.redhat-ods-applications.svc.cluster.local:80/models-as-a-service/sim-model/v1/chat/completions' \
  -H 'Content-Type: application/json' \
  -d '{\"model\":\"sim-model\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}'

echo ''
echo '=== Wrong key → 403 ==='
curl -s --max-time 10 -w '\nHTTP: %{http_code}\n' \
  'http://maas-default-gateway-istio.redhat-ods-applications.svc.cluster.local:80/models-as-a-service/sim-model/v1/chat/completions' \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer sk-oai-wrongkey' \
  -d '{\"model\":\"sim-model\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}'

echo ''
echo '=== Valid key → 200 ==='
curl -s --max-time 10 -w '\nHTTP: %{http_code}\n' \
  'http://maas-default-gateway-istio.redhat-ods-applications.svc.cluster.local:80/models-as-a-service/sim-model/v1/chat/completions' \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer YOUR_API_KEY_HERE' \
  -d '{\"model\":\"sim-model\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}]}'
"
```

Expected results:

| Test | Expected Code | Description |
|------|--------------|-------------|
| No auth | 401 | Unauthorized — no credentials provided |
| Wrong key | 403 | Forbidden — invalid API key |
| Valid key | 200 | Success — inference response from backend |

## Known Issues & Workarounds

### Deployment Issues (auto-resolved by chart/hooks)

| Issue | Root Cause | Resolution |
|-------|-----------|------------|
| Kuadrant doesn't detect Gateway API | Starts before Istio CRDs installed | Restart kuadrant-operator (done in Step 2) |
| `maas-api` CrashLoopBackOff | No PostgreSQL | Deploy DB (Step 3) |
| ClusterIssuer name mismatch | KServe hardcodes `opendatahub-ca-issuer` | Hook creates `odh-kserve-config` ConfigMap (PR #153) |
| Authorino CA trust | Authorino can't verify maas-api certs | Hook mounts CA bundle into Authorino (PR #153) |

### Model Routing Issues (require manual workarounds)

| Issue | Jira | Root Cause | Workaround |
|-------|------|-----------|------------|
| Gateway routing 500 (cluster_not_found) | [RHOAIENG-80084](https://issues.redhat.com/browse/RHOAIENG-80084) | MESH_EXTERNAL ServiceEntries conflict with native discovery | Delete ServiceEntries |
| mTLS failure to model backends (503) | [RHOAIENG-80085](https://issues.redhat.com/browse/RHOAIENG-80085) | No DestinationRule for non-sidecar pods | Create DestinationRule with tls.mode: DISABLE |
| HTTPRoute port 443 mismatch | [RHOAIENG-80086](https://issues.redhat.com/browse/RHOAIENG-80086) | llmisvc-controller hardcodes port 443 | Patch HTTPRoute port to actual backend port |
| HTTPRoute parentRef wrong namespace | [RHOAIENG-80079](https://issues.redhat.com/browse/RHOAIENG-80079) | CRD default `openshift-ingress` | Patch parentRef namespace |
| Authorino TLS mismatch (500 on all auth) | [RHOAIENG-78603](https://issues.redhat.com/browse/RHOAIENG-78603) | EnvoyFilter plaintext ↔ Authorino TLS | Disable Authorino TLS (Step 2) |

### Platform Issues

| Issue | Jira | Root Cause | Workaround |
|-------|------|-----------|------------|
| PodMonitor CRD missing | [RHOAIENG-80075](https://issues.redhat.com/browse/RHOAIENG-80075) | Not deployed on xKS | Apply PodMonitor CRD manually |
| `maas-controller` RBAC for PodMonitor | — | Tries to create PodMonitor unconditionally | PR in models-as-a-service repo |

## Troubleshooting

### 500 on All Inference Requests

**Symptom:** All requests return HTTP 500, even with valid API keys.

**Check Authorino TLS:**

```bash
kubectl get authorino authorino -n kuadrant-system -o jsonpath='{.spec.listener.tls.enabled}'
```

If `true`, the EnvoyFilter is connecting in plaintext but Authorino expects TLS. Fix:

```bash
kubectl patch authorino authorino -n kuadrant-system --type=merge \
  -p '{"spec":{"listener":{"tls":{"enabled":false}}}}'
```

**Check gateway logs for `cluster_not_found`:**

```bash
GW_POD=$(kubectl get pods -n redhat-ods-applications \
  -l gateway.networking.k8s.io/gateway-name=maas-default-gateway -o name | head -1)
kubectl logs $GW_POD -n redhat-ods-applications --tail=20 | grep -i "500\|cluster_not_found"
```

If you see `cluster_not_found`, delete ServiceEntries and create a DestinationRule (see 5d).

### 503 with TLS Error

**Symptom:** `upstream connect error ... TLS_error: packet length too long`

**Cause:** Istio mTLS to a backend without a sidecar.

**Fix:** Create a DestinationRule in `redhat-ods-applications` namespace with `tls.mode: DISABLE`
for the backend service host.

### AITenant Not Created

**Cause:** Stale `Config` CR from a previous installation has the annotation
`maas.opendatahub.io/default-aitenant-bootstrapped: "true"`.

**Fix:**

```bash
kubectl annotate config -n redhat-ods-applications --all \
  maas.opendatahub.io/default-aitenant-bootstrapped-
kubectl rollout restart deploy/maas-controller -n redhat-ods-applications
```

### MaaSAuthPolicy Pending / MaaSSubscription Degraded

**Error:** `Gateway API provider (istio / envoy gateway) is not installed`

**Cause:** Kuadrant operator started before Gateway API CRDs were installed.

**Fix:**

```bash
kubectl rollout restart deploy/kuadrant-operator-controller-manager -n kuadrant-operators
```

### Limitador / Authorino ImagePullBackOff

**Cause:** Missing pull secret in `kuadrant-system` namespace.

**Fix:** The RHCL chart should propagate the pull secret automatically. If not:

```bash
kubectl get secret rhai-pull-secret -n kuadrant-operators -o json | \
  jq '.metadata.namespace = "kuadrant-system" | del(.metadata.resourceVersion,.metadata.uid,.metadata.creationTimestamp)' | \
  kubectl apply -f -
```

## Cleanup

To remove all MaaS components:

```bash
# Remove helm releases
helm uninstall rhaii-on-xks
helm uninstall rhcl-operator

# Delete namespaces (optional — releases PVCs, secrets, etc.)
kubectl delete ns \
  redhat-ods-operator redhat-ods-applications redhat-ai-gateway-infra \
  models-as-a-service kuadrant-operators kuadrant-system \
  rhai-cloudmanager-system cert-manager istio-system ai-tenants \
  --ignore-not-found
```

!!! warning "CRDs are retained"
    CRDs with `helm.sh/resource-policy: keep` survive `helm uninstall`. On subsequent
    installs, stale CRs (especially `Config` with bootstrap annotations) can interfere.
    Delete stale CRs before reinstalling, or use a fresh cluster.

## Related

- [MaaS Quickstart (OpenShift)](https://opendatahub-io.github.io/models-as-a-service/dev/quickstart/) — Official automated deployment
- [External Model Setup](https://opendatahub-io.github.io/models-as-a-service/dev/install/external-model-setup/) — Detailed ExternalModel configuration
- [odh-gitops Repository](https://github.com/opendatahub-io/odh-gitops) — Source for Helm charts
- [AI Gateway Payload Processing](https://github.com/opendatahub-io/ai-gateway-payload-processing) — IPP documentation
