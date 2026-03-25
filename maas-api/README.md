# maas-api Development

## Environment Setup

### Prerequisites

- kubectl
- jq
- kustomize 5.7
- OCP 4.19.9+ (for GW API)
- **PostgreSQL database** (required for API key management)

!!! warning "Database Required"
    The maas-api **requires** a PostgreSQL database and will fail to start without it.
    You must create a Secret named `maas-db-config` with the `DB_CONNECTION_URL` key before deploying.

    For development, the `scripts/deploy.sh` script creates this automatically.
    For production ODH/RHOAI deployments, see [Database Prerequisites](../docs/content/install/prerequisites.md#database-prerequisite).

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
kustomize build ${PROJECT_DIR}/deployment/base/maas-controller/policies | kubectl apply --server-side=true --force-conflicts -f -
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
2.  **API Keys** - Named, long-lived tokens for applications (stored in PostgreSQL database). Suitable for services or applications that need persistent access with metadata tracking.

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
> ServiceAccount-based tokens have been removed. All authentication now uses API keys (`sk-oai-*` format) with hash-based storage.

##### API Keys

The API uses hash-based API keys with OpenAI-compatible format (`sk-oai-*`). Keys expire after a configurable duration (default: 90 days via `API_KEY_MAX_EXPIRATION_DAYS`).

```shell
HOST="$(kubectl get gateway -l app.kubernetes.io/instance=maas-default-gateway -n openshift-ingress -o jsonpath='{.items[0].status.addresses[0].value}')"

# Create an API key (defaults to API_KEY_MAX_EXPIRATION_DAYS, typically 90 days)
API_KEY_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{
    "name": "my-api-key",
    "description": "Production API key for my application",
    "subscription": "simulator-subscription"
  }' \
  "${HOST}/maas-api/v1/api-keys")

echo $API_KEY_RESPONSE | jq -r .
API_KEY=$(echo $API_KEY_RESPONSE | jq -r .key)

# Create an API key with custom expiration (30 days)
API_KEY_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{
    "name": "my-short-lived-key",
    "description": "30-day test key",
    "expiresIn": "30d",
    "subscription": "simulator-subscription"
  }' \
  "${HOST}/maas-api/v1/api-keys")

echo $API_KEY_RESPONSE | jq -r .
API_KEY=$(echo $API_KEY_RESPONSE | jq -r .key)
```

> [!NOTE]
> Replace `simulator-subscription` with your `MaaSSubscription` metadata name. To rely on **auto-selection** instead, remove the `subscription` field; maas-api then picks the accessible subscription with the highest `spec.priority`.

> [!IMPORTANT]
> The plaintext API key is shown ONLY ONCE at creation time. Store it securely - it cannot be retrieved again.

**Managing API Keys:**

```shell
# Search your API keys
curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{}' \
  "${HOST}/maas-api/v1/api-keys/search" | jq .

# Get specific API key by ID
API_KEY_ID="<id-from-search>"
curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  "${HOST}/maas-api/v1/api-keys/${API_KEY_ID}" | jq .

# Revoke specific API key
curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -X DELETE \
  "${HOST}/maas-api/v1/api-keys/${API_KEY_ID}"
```

> [!NOTE]
> API keys use hash-based storage (only SHA-256 hash stored, never plaintext). They are OpenAI-compatible (sk-oai-* format) and support optional expiration. API keys are stored in the configured database (see [Storage Configuration](#storage-configuration)) with metadata including creation date, expiration date, and status.

##### Ephemeral API Keys

Ephemeral keys are short-lived programmatic keys designed for temporary access scenarios. They differ from regular API keys in several ways:

| Feature | Regular API Keys | Ephemeral API Keys |
|---------|------------------|-------------------|
| Default expiration | 90 days | 1 hour |
| Maximum expiration | 90 days (configurable) | 1 hour (enforced) |
| Name | Required | Optional (auto-generated if not provided) |
| Shown in list/search | Yes | No (excluded by default) |
| Use case | Long-term application access | Short-term programmatic access |

```shell
# Create an ephemeral key (1-hour default expiration, name auto-generated)
API_KEY_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{"ephemeral": true}' \
  "${HOST}/maas-api/v1/api-keys")

echo $API_KEY_RESPONSE | jq -r .
API_KEY=$(echo $API_KEY_RESPONSE | jq -r .key)

# Create an ephemeral key with custom name and expiration (max 1hr)
API_KEY_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{
    "ephemeral": true,
    "name": "playground-session",
    "expiresIn": "30m"
  }' \
  "${HOST}/maas-api/v1/api-keys")
```

To include ephemeral keys in search results, use the `includeEphemeral` filter:

```shell
# Search including ephemeral keys
curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{"filters": {"includeEphemeral": true}}' \
  "${HOST}/maas-api/v1/api-keys/search" | jq .
```

### Database Configuration

maas-api uses PostgreSQL for persistent storage of API key metadata. The database connection is configured via a Kubernetes Secret.

!!! note "Automatic Setup"
    When using `scripts/deploy.sh` for development, PostgreSQL is deployed automatically with the secret created.

For production deployments, see the [Database Prerequisites](../docs/content/install/prerequisites.md#database-prerequisite) guide.

#### Listing models with subscription filtering

The `/v1/models` endpoint supports subscription filtering and aggregation. Use an **OpenShift token** or an **API key** in `Authorization: Bearer`. With a **user token**, optional `X-MaaS-Subscription` filters to one subscription when you have access to several. With an **API key**, the subscription is fixed at key mint time—no client `X-MaaS-Subscription` is needed for listing.

    HOST="$(kubectl get gateway -l app.kubernetes.io/instance=maas-default-gateway -n openshift-ingress -o jsonpath='{.items[0].status.addresses[0].value}')"

    # List models from all accessible subscriptions
    curl ${HOST}/v1/models \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer $TOKEN" | jq .

    # List models from a specific subscription
    curl ${HOST}/v1/models \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer $TOKEN" \
        -H "X-MaaS-Subscription: my-subscription" | jq .

    # List models from the subscription bound to an API key
    curl ${HOST}/v1/models \
        -H "Content-Type: application/json" \
        -H "Authorization: Bearer $API_KEY" | jq .

**Subscription Aggregation**: When the same model (same ID and URL) is accessible via multiple subscriptions, it appears once in the response with an array of all subscriptions providing access:

    {
      "object": "list",
      "data": [
        {
          "id": "model-name",
          "url": "https://...",
          "subscriptions": [
            {"name": "subscription-a", "displayName": "Subscription A"},
            {"name": "subscription-b", "displayName": "Subscription B"}
          ]
        }
      ]
    }

#### Calling the model and hitting the rate limit

Inference requires an API key (mint with `POST /v1/api-keys` using your OpenShift token). Send **only** `Authorization: Bearer <api-key>`; subscription is taken from the key at mint time.

Using model discovery (maas-api URL matches the [validation guide](../docs/content/install/validation.md); model `url` values come from the list response):

```shell
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
MAAS_API="https://maas.${CLUSTER_DOMAIN}/maas-api"
API_KEY=$(curl -sSk -H "Authorization: Bearer $(oc whoami -t)" -H "Content-Type: application/json" \
  -X POST -d '{"name":"rate-limit-demo","subscription":"simulator-subscription"}' \
  "${MAAS_API}/v1/api-keys" | jq -r .key)

MODELS=$(curl -sSk "${MAAS_API}/v1/models"  \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${API_KEY}" | jq . -r)

echo $MODELS | jq .
MODEL_URL=$(echo $MODELS | jq -r '.data[0].url')
MODEL_NAME=$(echo $MODELS | jq -r '.data[0].id')

for i in {1..16}
do
curl -sSk -o /dev/null -w "%{http_code}\n" \
  -H "Authorization: Bearer ${API_KEY}" \
  -d "{
        \"model\": \"${MODEL_NAME}\",
        \"prompt\": \"Not really understood prompt\",
        \"max_prompts\": 40
    }" \
  "${MODEL_URL}/v1/chat/completions";
done
```
