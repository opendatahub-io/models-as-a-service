# External Model Setup (Tech Preview)

!!! warning "Tech Preview"
    ExternalModel support is a tech preview feature. APIs and behavior may change in future releases.

This guide walks through deploying an external AI/ML model (e.g., OpenAI, Anthropic) through the MaaS gateway. External models are hosted outside the cluster — MaaS handles authentication, rate limiting, and API key management while routing inference requests to the external provider.

## Prerequisites

- MaaS platform deployed per the [Installation Guide](maas-setup.md)
- `kubectl`/`oc` access as cluster-admin
- An API key from the external provider (e.g., OpenAI)

## Architecture

An external model deployment involves the following resources:

| Resource | Created by | Purpose |
|----------|-----------|---------|
| **ExternalModel** | User | Defines the external provider, endpoint, and credential reference |
| **MaaSModelRef** | User | Registers the model in the MaaS catalog |
| **MaaSAuthPolicy** | User | Defines which groups can access the model |
| **MaaSSubscription** | User | Defines token rate limits for the model |
| **Service** (ExternalName) | ExternalModel reconciler | Maps an in-cluster DNS name to the external FQDN |
| **ServiceEntry** | ExternalModel reconciler | Registers the external host in the Istio mesh |
| **DestinationRule** | ExternalModel reconciler | Configures TLS origination for the external endpoint |
| **HTTPRoute** | ExternalModel reconciler | Routes gateway traffic to the external provider |
| **Kuadrant AuthPolicy** | MaaSAuthPolicy controller | Per-route auth enforcement via Authorino |
| **TokenRateLimitPolicy** | MaaSSubscription controller | Per-route rate limiting via Limitador |

The Inference Payload Processor (IPP) component (ext-proc) handles API key injection, request translation (OpenAI ↔ provider-native format), and model routing.

## Step 1: Deploy Inference Payload Processor (IPP)

IPP is required for external models — it injects the provider API key and translates between OpenAI-compatible format and the provider's native API.

!!! note
    If MaaS was deployed via the Tenant CR (standard RHOAI path), IPP is already deployed as a subcomponent. Verify with:

    ```bash
    kubectl get pods -n openshift-ingress -l app=payload-processing
    ```

    If the pod is already running, skip to Step 2.

```bash
PROJECT_DIR=$(git rev-parse --show-toplevel)

# RBAC
kubectl apply -f ${PROJECT_DIR}/deployment/base/payload-processing/rbac/serviceaccount.yaml
kubectl apply -f ${PROJECT_DIR}/deployment/base/payload-processing/rbac/clusterrole.yaml
kubectl apply -f ${PROJECT_DIR}/deployment/base/payload-processing/rbac/clusterrolebinding.yaml

# ConfigMap, Service, DestinationRule, EnvoyFilter
kubectl apply -f ${PROJECT_DIR}/deployment/base/payload-processing/manager/plugins-configmap.yaml
kubectl apply -f ${PROJECT_DIR}/deployment/base/payload-processing/manager/service.yaml
kubectl apply -f ${PROJECT_DIR}/deployment/base/payload-processing/manager/destination-rule.yaml
kubectl apply -f ${PROJECT_DIR}/deployment/base/payload-processing/manager/envoy-filter.yaml

# Deployment (substitute the image placeholder)
cat ${PROJECT_DIR}/deployment/base/payload-processing/manager/deployment.yaml | \
  sed 's|image: payload-processing|image: quay.io/opendatahub/odh-ai-gateway-payload-processing:odh-stable|' | \
  kubectl apply -f -

# Verify
kubectl get pods -n openshift-ingress -l app=payload-processing
```

The pod should be `1/1 Running`.

## Step 2: Create the Model Namespace

External models deploy to a model namespace (e.g., `llm`). If it doesn't exist:

```bash
kubectl create namespace llm
kubectl label namespace llm istio-injection=enabled
```

## Step 3: Create the Provider API Key Secret

Store the external provider's API key in a Kubernetes Secret. The Secret must:

- Be in the same namespace as the ExternalModel
- Use the data key `api-key`
- Have the label `inference.networking.k8s.io/bbr-managed=true` so IPP can read it

```bash
TMP_KEY_FILE="$(mktemp)"
chmod 600 "${TMP_KEY_FILE}"
cat > "${TMP_KEY_FILE}" <<< "YOUR_OPENAI_API_KEY"

kubectl create secret generic openai-api-key -n llm \
  --from-file=api-key="${TMP_KEY_FILE}"

rm -f "${TMP_KEY_FILE}"

kubectl label secret openai-api-key -n llm inference.networking.k8s.io/bbr-managed=true
```

## Step 4: Create the ExternalModel and MaaSModelRef

The ExternalModel defines the provider connection. The MaaSModelRef registers it in the MaaS catalog.

```bash
kubectl apply -f - <<'EOF'
apiVersion: maas.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: gpt-4o
  namespace: llm
spec:
  provider: openai
  endpoint: api.openai.com
  targetModel: gpt-4o
  credentialRef:
    name: openai-api-key
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: gpt-4o
  namespace: llm
spec:
  modelRef:
    kind: ExternalModel
    name: gpt-4o
EOF
```

Verify the model is ready:

```bash
kubectl get maasmodelref -n llm
```

Expected output:

```text
NAME     PHASE   ENDPOINT                                    HTTPROUTE   GATEWAY
gpt-4o   Ready   https://maas.<cluster-domain>/llm/gpt-4o   gpt-4o      maas-default-gateway
```

## Step 5: Configure Access and Rate Limits

Create a MaaSAuthPolicy (who can access) and MaaSSubscription (rate limits) in the `models-as-a-service` namespace:

```bash
kubectl apply -f - <<'EOF'
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: gpt-4o-access
  namespace: models-as-a-service
spec:
  modelRefs:
  - name: gpt-4o
    namespace: llm
  subjects:
    groups:
    - name: "system:authenticated"
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: gpt-4o-subscription
  namespace: models-as-a-service
spec:
  owner:
    groups:
    - name: "system:authenticated"
  modelRefs:
  - name: gpt-4o
    namespace: llm
    tokenRateLimits:
    - limit: 100000
      window: "1h"
EOF
```

## Step 6: Validate

### Mint an API Key

```bash
GW_HOST=$(kubectl get gateway maas-default-gateway -n openshift-ingress \
  -o jsonpath='{.spec.listeners[0].hostname}')
TOKEN=$(oc whoami -t)

KEY=$(curl -sSk -X POST "https://${GW_HOST}/maas-api/v1/api-keys" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"external-model-key","subscription":"gpt-4o-subscription"}' | jq -r '.key')

echo "MaaS API key: ${KEY:0:20}..."
```

### Run Inference

```bash
curl -sSk "https://${GW_HOST}/llm/gpt-4o/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer $KEY" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"say hello"}]}'
```

### Verify Auth Enforcement

```bash
# Bogus key — expect 403
curl -sSk -w "HTTP: %{http_code}\n" "https://${GW_HOST}/llm/gpt-4o/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-oai-FAKE-KEY" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'

# No auth — expect 401
curl -sSk -w "HTTP: %{http_code}\n" "https://${GW_HOST}/llm/gpt-4o/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

### Verify Model Listing

```bash
curl -sSk "https://${GW_HOST}/v1/models" \
  -H "Authorization: Bearer $KEY" | jq '.data[].id'
```

The model `gpt-4o` should appear in the list.

## Supported Providers

The `spec.provider` field determines how IPP translates requests and injects credentials. Each provider has different authentication headers and API formats — IPP handles the translation automatically.

| Provider | `spec.provider` | Translation | Auth Header |
|----------|----------------|-------------|-------------|
| OpenAI | `openai` | Pass-through | `Authorization: Bearer` |
| Anthropic | `anthropic` | OpenAI ↔ Messages API | `x-api-key` |
| Azure OpenAI | `azure-openai` | Path rewrite + field stripping | `api-key` |
| Vertex AI | `vertex-openai` | Path rewrite + field stripping | `Authorization: Bearer` (OAuth) |
| AWS Bedrock | `bedrock-openai` | Pass-through (Mantle) | `Authorization: Bearer` |

### OpenAI

Pass-through — no body translation needed. Auth uses `Authorization: Bearer`.

- Set `endpoint: api.openai.com`
- Models: `gpt-4o`, `gpt-4o-mini`, `gpt-4.1`, `gpt-4.1-mini`, `o1`, `o3-mini`, etc.

See the [OpenAI provider guide](https://github.com/opendatahub-io/ai-gateway-payload-processing/blob/main/docs/providers/openai.md).

### Anthropic

Translates OpenAI Chat Completions to Anthropic Messages API format. Auth uses `x-api-key` header (not `Authorization: Bearer`).

- System messages extracted to top-level `system` field
- `tools[]` converted to Anthropic format with `input_schema`
- `tool_choice` mapped: `auto` → `{"type":"auto"}`, `required` → `{"type":"any"}`
- `stream` field forwarded for streaming support
- `anthropic-version: 2023-06-01` header added automatically
- Response `stop_reason` mapped back to OpenAI `finish_reason`
- Unsupported parameters silently dropped: `frequency_penalty`, `presence_penalty`, `logprobs`, `n`, `response_format`, `seed`
- Set `endpoint: api.anthropic.com`
- Models: `claude-sonnet-4-20250514`, `claude-haiku-4-5-20251001`, `claude-opus-4-20250514`

See the [Anthropic provider guide](https://github.com/opendatahub-io/ai-gateway-payload-processing/blob/main/docs/providers/anthropic.md).

### Azure OpenAI

Uses Azure's OpenAI-compatible endpoint with a different auth header and path. Auth uses `api-key` header (not `Authorization: Bearer`).

- Path rewritten to `/openai/v1/chat/completions`
- Response strips Azure-specific fields: `content_filter_results`, `prompt_filter_results`
- `targetModel` must match the deployment name in your Azure OpenAI resource
- Set `endpoint: <resource>.openai.azure.com` (e.g., `my-deployment.openai.azure.com`)

See the [Azure OpenAI provider guide](https://github.com/opendatahub-io/ai-gateway-payload-processing/blob/main/docs/providers/azure-openai.md).

### AWS Bedrock (OpenAI-compatible)

Routes to AWS Bedrock's OpenAI-compatible Mantle endpoint. Pass-through — no body translation. Auth uses `Authorization: Bearer` with a Bedrock API Key (starts with `ABSK`, not AWS access keys).

!!! warning
    Use `bedrock-mantle.<region>.api.aws`, **not** `bedrock-runtime.<region>.amazonaws.com`. The IPP translator uses `/v1/chat/completions` which is only available on the Bedrock Mantle endpoint. Using `bedrock-runtime` will result in `404` errors.

- Set `endpoint: bedrock-mantle.<region>.api.aws` (e.g., `bedrock-mantle.us-east-2.api.aws`)
- Models vary by region. List available models: `curl -sk "https://bedrock-mantle.<region>.api.aws/v1/models" -H "Authorization: Bearer <KEY>"`

See the [Bedrock provider guide](https://github.com/opendatahub-io/ai-gateway-payload-processing/blob/main/docs/providers/bedrock-openai.md).

### Vertex AI (OpenAI-compatible)

Routes to Google Vertex AI's OpenAI-compatible endpoint. Auth uses OAuth2 Bearer tokens.

!!! warning
    Unlike other providers, `vertex-openai` requires IPP plugin configuration with your GCP project, location, and endpoint. Without this configuration, the translator will not be registered and requests will fail.

- Requires plugin-level config: `project`, `location`, `endpoint` (set in Helm values or ConfigMap)
- OAuth2 tokens **expire every hour** — the Secret must be refreshed with `gcloud auth print-access-token`
- `targetModel` must use `publisher/model` format (e.g., `google/gemini-2.5-flash`, not `gemini-2.5-flash`)
- Response strips `usage.extra_properties`
- Set `endpoint: <region>-aiplatform.googleapis.com` (e.g., `us-central1-aiplatform.googleapis.com`)
- Models: `google/gemini-2.5-flash`, `google/gemini-2.5-pro`, `google/gemini-2.0-flash`

See the [Vertex AI provider guide](https://github.com/opendatahub-io/ai-gateway-payload-processing/blob/main/docs/providers/vertex-openai.md).

For detailed per-provider configuration, examples, and troubleshooting, see the [IPP provider guides](https://github.com/opendatahub-io/ai-gateway-payload-processing/tree/main/docs/providers).

## Cleanup

To remove an external model and all its managed resources:

```bash
# Delete the ExternalModel — OwnerReferences clean up Service, ServiceEntry, DR, HTTPRoute
kubectl delete externalmodel gpt-4o -n llm

# Delete the MaaSModelRef
kubectl delete maasmodelref gpt-4o -n llm

# Delete auth and subscription
kubectl delete maasauthpolicy gpt-4o-access -n models-as-a-service
kubectl delete maassubscription gpt-4o-subscription -n models-as-a-service

# Delete the provider secret
kubectl delete secret openai-api-key -n llm
```

## Related

- [ExternalModel CRD Reference](../reference/crds/external-model.md)
- [MaaS Model Kinds](../configuration-and-management/maas-model-kinds.md)
- [Quota and Access Configuration](../configuration-and-management/quota-and-access-configuration.md)
- [IPP Provider Guides](https://github.com/opendatahub-io/ai-gateway-payload-processing/tree/main/docs/providers)
- [Validation Guide](validation.md)
