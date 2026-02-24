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
# JWT uses base64url encoding; convert to standard base64 before decoding
AUD="$(kubectl create token default --duration=10m \
  | cut -d. -f2 \
  | tr '_-' '/+' | awk '{while(length($0)%4)$0=$0"=";print}' \
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

echo $TOKEN_RESPONSE | jq -r .token | cut -d. -f2 | jq -Rr '@base64d | fromjson'

TOKEN=$(echo $TOKEN_RESPONSE | jq -r .token)
```

> [!NOTE]
> This is a self-service endpoint that issues ephemeral tokens. Openshift Identity (`$(oc whoami -t)`) is used as a refresh token.

##### API Keys (Named Tokens)

**V1 Legacy API Keys (ServiceAccount-backed):**

To create a legacy named API key (ServiceAccount-backed, will be deprecated):

```shell
HOST="$(kubectl get gateway -l app.kubernetes.io/instance=maas-default-gateway -n openshift-ingress -o jsonpath='{.items[0].status.addresses[0].value}')"

# Create a legacy named API key (ServiceAccount-backed)
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
```

**V2 API Keys (Hash-based, Recommended):**

The V2 API uses hash-based API keys with OpenAI-compatible format (`sk-oai-*`). These keys support both permanent and expiring modes.

```shell
HOST="$(kubectl get gateway -l app.kubernetes.io/instance=maas-default-gateway -n openshift-ingress -o jsonpath='{.items[0].status.addresses[0].value}')"

# Create a permanent API key (no expiration)
API_KEY_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{
    "name": "my-permanent-key",
    "description": "Production API key for my application"
  }' \
  "${HOST}/maas-api/v2/api-keys")

echo $API_KEY_RESPONSE | jq -r .
API_KEY=$(echo $API_KEY_RESPONSE | jq -r .key)

# Create an expiring API key (90 days)
API_KEY_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{
    "name": "my-expiring-key",
    "description": "90-day test key",
    "expiresIn": "90d"
  }' \
  "${HOST}/maas-api/v2/api-keys")

echo $API_KEY_RESPONSE | jq -r .
API_KEY=$(echo $API_KEY_RESPONSE | jq -r .key)
```

> [!IMPORTANT]
> The plaintext API key is shown ONLY ONCE at creation time. Store it securely - it cannot be retrieved again.

**Managing API Keys:**

List, get, and delete operations use V2 endpoints and work for both V1 and V2 keys:

```shell
# List all your API keys (both v1 and v2)
curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  "${HOST}/maas-api/v2/api-keys" | jq .

# Get specific API key by ID
API_KEY_ID="<id-from-list>"
curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  "${HOST}/maas-api/v2/api-keys/${API_KEY_ID}" | jq .

# Revoke specific API key
curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -X DELETE \
  "${HOST}/maas-api/v2/api-keys/${API_KEY_ID}"

# Revoke all tokens (ephemeral and API keys)
curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -X DELETE \
  "${HOST}/maas-api/v1/tokens"
```

> [!NOTE]
> V2 API keys use hash-based storage (only SHA-256 hash stored, never plaintext). They are OpenAI-compatible (sk-oai-* format) and support optional expiration. V1 legacy keys are ServiceAccount-backed and will be deprecated in a future release. API keys are stored in the configured database (see [Storage Configuration](#storage-configuration)) with metadata including creation date, expiration date, and status.

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
