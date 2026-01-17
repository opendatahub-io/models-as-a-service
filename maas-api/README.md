# maas-api Development

## Environment Setup

### Prerequisites

- kubectl
- jq
- kustomize 5.7
- OCP 4.19.9+ (for GW API)

### Setup

### Core Infrastructure

First, we need to deploy the core infrastructure. That includes:

- Kuadrant
- Cert Manager

> [!IMPORTANT]
> If you are running RHOAI, both Kuadrant and Cert Manager should be already installed.

```shell
PROJECT_DIR=$(git rev-parse --show-toplevel) 
for ns in opendatahub kuadrant-system llm maas-api; do kubectl create ns $ns || true; done
"${PROJECT_DIR}/scripts/install-dependencies.sh" --kuadrant
```

#### Enabling GW API

> [!IMPORTANT]
> For enabling Gateway API on OCP 4.19.9+, only GatewayClass creation is needed.

```shell
PROJECT_DIR=$(git rev-parse --show-toplevel)
kustomize build ${PROJECT_DIR}/deployment/base/networking | kubectl apply --server-side=true --force-conflicts -f -
```

### Deploying Opendatahub KServe

```shell
PROJECT_DIR=$(git rev-parse --show-toplevel)
kustomize build ${PROJECT_DIR}/deployment/components/odh/kserve | kubectl apply --server-side=true --force-conflicts -f -
```

> [!NOTE]
> If it fails the first time, simply re-run. CRDs or Webhooks might not be established timely.
> This approach is aligned with how odh-operator would process (requeue reconciliation).

### Deploying MaaS API for development

```shell
make deploy-dev
```

This will:

- Deploy MaaS API component with Service Account Token provider in debug mode

#### Patch Kuadrant deployment

> [!IMPORTANT]
> See https://docs.redhat.com/en/documentation/red_hat_connectivity_link/1.1/html/release_notes_for_connectivity_link_1.1/prodname-relnotes_rhcl#connectivity_link_known_issues

If you installed Kuadrant using Helm chats (i.e. by calling `./install-dependencies.sh --kuadrant` like in the example above),
you need to patch the Kuadrant deployment to add the correct environment variable.

```shell
kubectl -n kuadrant-system patch deployment kuadrant-operator-controller-manager \
  --type='json' \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/env/-","value":{"name":"ISTIO_GATEWAY_CONTROLLER_NAMES","value":"openshift.io/gateway-controller/v1"}}]'
```

If you installed Kuadrant using OLM, you have to patch `ClusterServiceVersion` instead, to add the correct environment variable.

```shell
kubectl patch csv kuadrant-operator.v0.0.0 -n kuadrant-system --type='json' -p='[
  {
    "op": "add",
    "path": "/spec/install/spec/deployments/0/spec/template/spec/containers/0/env/-",
    "value": {
      "name": "ISTIO_GATEWAY_CONTROLLER_NAMES",
      "value": "openshift.io/gateway-controller/v1"
    }
  }
]'
```

#### Apply Gateway Policies

```shell
PROJECT_DIR=$(git rev-parse --show-toplevel)
kustomize build ${PROJECT_DIR}/deployment/base/policies | kubectl apply --server-side=true --force-conflicts -f -
```

#### Ensure the correct audience is set for AuthPolicy

Patch `AuthPolicy` with the correct audience for Openshift Identities:

```shell
AUD="$(kubectl create token default --duration=10m \
  | cut -d. -f2 \
  | jq -Rr '@base64d | fromjson | .aud[0]' 2>/dev/null)"

echo "Patching AuthPolicy with audience: $AUD"

kubectl patch authpolicy maas-api-auth-policy -n maas-api \
  --type='json' \
  -p "$(jq -nc --arg aud "$AUD" '[{
    op:"replace",
    path:"/spec/rules/authentication/openshift-identities/kubernetesTokenReview/audiences/0",
    value:$aud
  }]')"
```

#### Update Limitador image to expose metrics

Update the Limitador deployment to use the latest image that exposes metrics:

```shell
NS=kuadrant-system
kubectl -n $NS patch limitador limitador --type merge \
  -p '{"spec":{"image":"quay.io/kuadrant/limitador:1a28eac1b42c63658a291056a62b5d940596fd4c","version":""}}'
```

### Testing

> [!IMPORTANT] 
> You can also use automated script `scripts/verify-models-and-limits.sh` 

#### Deploying the demo model

```shell
PROJECT_DIR=$(git rev-parse --show-toplevel)
kustomize build ${PROJECT_DIR}/docs/samples/models/simulator | kubectl apply --server-side=true --force-conflicts -f -
```

#### Getting a token

MaaS API supports two types of tokens:

1.  **Ephemeral Tokens** - Stateless tokens that provide better security posture as they can be easily refreshed by the caller using OpenShift Identity. These tokens can live as long as API keys (up to the configured expiration), making them suitable for both temporary and long-term access scenarios.
2.  **API Keys** - Named, long-lived tokens for applications (stored in SQLite database). Suitable for services or applications that need persistent access with metadata tracking.

##### Ephemeral Tokens

To get a short-lived ephemeral token:

```shell
HOST="$(kubectl get gateway -l app.kubernetes.io/instance=maas-default-gateway -n openshift-ingress -o jsonpath='{.items[0].status.addresses[0].value}')"

TOKEN_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{
    "expiration": "4h"
  }' \
  "${HOST}/maas-api/v1/tokens")

echo $TOKEN_RESPONSE | jq -r .

echo $TOKEN_RESPONSE | jq -r .token | cut -d. -f2 | base64 -d 2>/dev/null | jq .

TOKEN=$(echo $TOKEN_RESPONSE | jq -r .token)
```

> [!NOTE]
> This is a self-service endpoint that issues ephemeral tokens. Openshift Identity (`$(oc whoami -t)`) is used as a refresh token.

##### API Keys (Named Tokens)

To create a named API key that can be tracked and managed:

```shell
HOST="$(kubectl get gateway -l app.kubernetes.io/instance=maas-default-gateway -n openshift-ingress -o jsonpath='{.items[0].status.addresses[0].value}')"

# Create a named API key
API_KEY_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{
    "expiration": "720h",
    "name": "my-application-key"
  }' \
  "${HOST}/maas-api/v1/api-keys")

echo $API_KEY_RESPONSE | jq -r .
TOKEN=$(echo $API_KEY_RESPONSE | jq -r .token)

# List all your API keys
curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  "${HOST}/maas-api/v1/api-keys" | jq .

# Get specific API key by ID
API_KEY_ID="<id-from-list>"
curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  "${HOST}/maas-api/v1/api-keys/${API_KEY_ID}" | jq .

# Revoke all tokens (ephemeral and API keys)
curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -X DELETE \
  "${HOST}/maas-api/v1/tokens"
```

> [!NOTE]
> API keys are stored in the configured database (see [Storage Configuration](#storage-configuration)) with metadata including creation date, expiration date, and status. They can be listed and inspected individually. To revoke tokens, use `DELETE /v1/tokens` which revokes all tokens (ephemeral and API keys) by recreating the Service Account and marking API key metadata as expired.

### Storage Configuration

maas-api supports three storage modes, controlled by the `--storage` flag:

| Mode | Flag | Use Case | Persistence |
|------|------|----------|-------------|
| **In-memory** (default) | `--storage=in-memory` | Development/testing | ❌ Data lost on restart |
| **Disk** | `--storage=disk` | Single replica, demos | ✅ Survives restarts |
| **External** | `--storage=external` | Production, HA | ✅ Full persistence |

#### Quick Start

```bash
# In-memory (default - no configuration needed)

# Disk storage (persistent, single replica)
kustomize build deployment/overlays/tls-backend-disk | kubectl apply -f -

# External database - see docs/samples/database/external for configuration
```

#### Configuration Flags and Environment Variables

| Flag | Environment Variable | Default | Description |
|------|---------------------|---------|-------------|
| `--storage` | `STORAGE_MODE` | `in-memory` | Storage mode: `in-memory`, `disk`, or `external` |
| `--db-connection-url` | `DB_CONNECTION_URL` | - | Database URL (required for `--storage=external`) |
| `--data-path` | `DATA_PATH` | `/data/maas-api.db` | Path for disk storage |
| - | `DB_MAX_OPEN_CONNS` | 25 | Max open connections (external mode only) |
| - | `DB_MAX_IDLE_CONNS` | 5 | Max idle connections (external mode only) |
| - | `DB_CONN_MAX_LIFETIME_SECONDS` | 300 | Connection max lifetime in seconds (external mode only) |

For detailed external database setup instructions, see [docs/samples/database/external](../docs/samples/database/external/README.md).

#### Calling the model and hitting the rate limit

Using model discovery:

```shell
HOST="$(kubectl get gateway -l app.kubernetes.io/instance=maas-default-gateway -n openshift-ingress -o jsonpath='{.items[0].status.addresses[0].value}')"

MODELS=$(curl ${HOST}/v1/models  \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $TOKEN" | jq . -r)

echo $MODELS | jq .
MODEL_URL=$(echo $MODELS | jq -r '.data[0].url')
MODEL_NAME=$(echo $MODELS | jq -r '.data[0].id')

for i in {1..16}
do
curl -sSk -o /dev/null -w "%{http_code}\n" \
  -H "Authorization: Bearer $TOKEN" \
  -d "{
        \"model\": \"${MODEL_NAME}\",
        \"prompt\": \"Not really understood prompt\",
        \"max_prompts\": 40
    }" \
  "${MODEL_URL}/v1/chat/completions";
done
```
