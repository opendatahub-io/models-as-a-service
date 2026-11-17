# External Model Setup (Tech Preview)

!!! warning "Tech Preview"
    ExternalModel support is a tech preview feature. APIs and behavior may change in future releases.

This guide walks through serving an external AI/ML model (e.g., OpenAI, Anthropic) through the MaaS gateway. External models are hosted outside the cluster. MaaS handles authentication, rate limiting, and API key management, and routes inference requests to the external provider.

You describe an external model with two `inference.opendatahub.io/v1alpha1` resources:

- An **ExternalProvider** holds the provider type, its endpoint, and the credential.
- An **ExternalModel** gives the model its client-facing name and binds it to one or more providers.

ai-gateway-controller turns them into routes and Praxis configuration on the tenant's Gateway.

!!! note "Legacy API"
    The `maas.opendatahub.io/v1alpha1` ExternalModel is deprecated, and RHOAI 3.6 rejects new ones. For existing objects, see [Upgrade to 3.6](../migration/upgrade-to-3.6.md#legacy-maasopendatahubio-externalmodels).

## Tenants

An ExternalModel without `spec.gatewayRefs` is served by the tenant whose `MaasTenantConfig` is in the model's namespace. In a namespace that belongs to no tenant, the default tenant's Gateway serves it.

A tenant that still runs the RHOAI 3.5 payload processor after an upgrade behaves differently; see [Tenants still on the legacy payload processor](#tenants-still-on-the-legacy-payload-processor).

## Prerequisites

- MaaS platform deployed per the [Installation Guide](maas-setup.md)
- `kubectl`/`oc` access as cluster-admin
- An API key from the external provider (e.g., OpenAI)

## API translation

!!! important "Enable `spec.apiTranslation` for translated models"
    `MaasTenantConfig.spec.apiTranslation.enabled` is off for new tenants unless you set it. The tenant that serves the model needs it for:

    - Anthropic (`messages`) providers
    - Azure OpenAI providers
    - a binding whose `targetModel` differs from the model's client-facing name

    Without it, ai-gateway-controller does not serve such a binding and reports it with reason `APITranslationDisabled`; a model left with no served binding is `Failed`.

For the default tenant:

```bash
kubectl patch maastenantconfig default-tenant -n models-as-a-service --type=merge \
  -p '{"spec":{"apiTranslation":{"enabled":true}}}'
```

For an AITenant, patch `default-tenant` in its tenant namespace (`ai-tenant-<name>`). An upgrade from a recognized RHOAI 3.5 configuration sets the field to `true` during the migration to Praxis. See the [MaasTenantConfig reference](../reference/crds/tenant.md#tenantapitranslationconfig) for what else the setting permits, and [Upgrade to 3.6](../migration/upgrade-to-3.6.md#ipp-praxis-api-translation-migration) for the migration.

## Architecture

An external model deployment involves the following resources:

| Resource | Created by | Purpose |
|----------|-----------|---------|
| **Secret** | User | Holds the provider API key |
| **ExternalProvider** | User | Defines the provider type, endpoint, and credential |
| **ExternalModel** | User | Defines the client-facing model name and its provider bindings |
| **MaaSModelRef** | User | Registers the model in the MaaS catalog |
| **MaaSAuthPolicy** | User | Defines which groups can access the model |
| **MaaSSubscription** | User | Defines token rate limits for the model |
| **HTTPRoute** | ai-gateway-controller | Routes gateway traffic for the model |
| **Service**, **ServiceEntry**, **DestinationRule** | ai-gateway-controller | Reach the provider endpoint and originate TLS |
| **Kuadrant AuthPolicy** | MaaSAuthPolicy controller | Per-route auth enforcement via Authorino |
| **TokenRateLimitPolicy** | MaaSSubscription controller | Per-route rate limiting via Limitador |

Praxis selects the provider, injects its credential, and rewrites the request for it. It buffers each request body up to 10 MiB for validation and target-model rewriting; larger requests are rejected before they reach the provider.

## Step 1: Create the Model Namespace

This guide uses the `llm` namespace for the model. If it doesn't exist:

```bash
kubectl create namespace llm
```

The model's HTTPRoute lives in this namespace, so the Gateway must admit routes from it. If the Gateway selects namespaces by label, label `llm` as described in [Gateway Patterns](../configuration-and-management/gateway-patterns.md#automatic-infrastructure-and-tenant-namespace-labeling). Tenant namespaces are labeled automatically.

## Step 2: Create the Provider API Key Secret

Store the external provider's API key in a Kubernetes Secret. The Secret must:

- Be in the same namespace as the ExternalProvider
- Use the data key `api-key`

```bash
TMP_KEY_FILE="$(mktemp)"
chmod 600 "${TMP_KEY_FILE}"
cat > "${TMP_KEY_FILE}" <<< "YOUR_OPENAI_API_KEY"

kubectl create secret generic openai-api-key -n llm \
  --from-file=api-key="${TMP_KEY_FILE}"

rm -f "${TMP_KEY_FILE}"
```

## Step 3: Create the ExternalProvider

The ExternalProvider defines the provider connection. Several ExternalModels can use the same provider.

```bash
kubectl apply -f - <<'EOF'
apiVersion: inference.opendatahub.io/v1alpha1
kind: ExternalProvider
metadata:
  name: openai
  namespace: llm
spec:
  provider: openai
  endpoint: api.openai.com
  auth:
    type: apikey
    secretRef:
      name: openai-api-key
EOF
```

| Field | Required | Description |
|-------|----------|-------------|
| `spec.provider` | Yes | Provider type, such as `openai`, `anthropic`, `azure-openai`, or `bedrock-openai`. See [Providers](#providers). |
| `spec.endpoint` | Yes | Provider FQDN, without scheme or path (e.g., `api.openai.com`) |
| `spec.auth.type` | Yes | `apikey` |
| `spec.auth.secretRef.name` | Yes | Secret with the `api-key` data key, in the ExternalProvider namespace |
| `spec.config` | No | Provider-specific key-value settings, such as the [private CA settings](#provider-endpoint-options) |

Verify the provider is ready:

```bash
kubectl get externalproviders.inference.opendatahub.io -n llm
```

## Step 4: Create the ExternalModel and MaaSModelRef

The ExternalModel binds a client-facing model name to the provider. The MaaSModelRef registers it in the MaaS catalog.

```bash
kubectl apply -f - <<'EOF'
apiVersion: inference.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: gpt-4o
  namespace: llm
spec:
  externalProviderRefs:
  - ref:
      name: openai
    targetModel: gpt-4o
    apiFormat: openai-chat
    path: /v1/chat/completions
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

Clients send the model's name, `metadata.name`, in the `model` field of their requests. Set `spec.modelName` when the client-facing name is not a valid resource name, for example `gpt-4.1`.

Each entry in `spec.externalProviderRefs` binds the model to one provider:

| Field | Required | Description |
|-------|----------|-------------|
| `ref.name` | Yes | ExternalProvider name. Leave out `ref.namespace` to use a provider in the model's namespace; a provider in another namespace needs a ReferenceGrant there. |
| `targetModel` | Yes | Model identifier the provider expects. If it differs from the client-facing name, the tenant needs [API translation](#api-translation). |
| `apiFormat` | Yes | `openai-chat` (OpenAI Chat Completions) or `messages` (Anthropic Messages) |
| `path` | Yes | Request path sent to the provider, e.g. `/v1/chat/completions`. `{key}` placeholders are filled from the merged provider and binding `config`, and `{model}` from `targetModel`. |
| `config` | No | Overrides the provider's `spec.config` for this binding |
| `auth` | No | Overrides the provider's credential; its Secret is in the ExternalModel namespace |

Verify the model is ready. Both API groups define an `ExternalModel` kind, so use the full resource name:

```bash
kubectl get externalmodels.inference.opendatahub.io -n llm
kubectl get maasmodelref -n llm
```

Expected output for the MaaSModelRef:

```text
NAME     PHASE   ENDPOINT                                   HTTPROUTE               GATEWAY                AGE
gpt-4o   Ready   https://maas.<cluster-domain>/llm/gpt-4o   external-model-gpt-4o   maas-default-gateway   1m
```

`Ready` reports that ai-gateway-controller reconciled the model, not that requests succeed. Step 6 sends a request.

## Provider endpoint options

ai-gateway-controller connects to the provider over HTTPS on port `443`.

| Setting | Where | Description |
|---------|-------|-------------|
| `inference.opendatahub.io/port` annotation | ExternalProvider | Another HTTPS port (1–65535) |
| `tls.credentialName` in `spec.config` | ExternalProvider | Trusts a provider certificate signed by a private CA: the name of a Secret in the Gateway namespace whose `cacert` key holds the PEM CA chain |
| `tls.caCertificates` in `spec.config` | ExternalProvider | The same, as the absolute path of a CA file already mounted in the Gateway |

Plain HTTP is not supported: `inference.opendatahub.io/tls: "false"` is rejected, because the provider credential would travel in cleartext. Serve internal endpoints, such as a self-hosted vLLM, over TLS.

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

KEY=$(curl -sS -X POST "https://${GW_HOST}/maas-api/v1/api-keys" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"external-model-key","subscription":"gpt-4o-subscription"}' | jq -r '.key')

echo "MaaS API key: ${KEY:0:20}..."
```

### Run Inference

```bash
curl -sS "https://${GW_HOST}/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer ${KEY}" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"say hello"}]}'
```

!!! note "Legacy path-based endpoint"
    You can also use the path-based URL: `https://${GW_HOST}/llm/gpt-4o/v1/chat/completions`. Both routing modes are supported. See [Inference - Path-Based Routing](../user-guide/inference.md#path-based-routing-legacy) for details.

### Verify Auth Enforcement

```bash
# Bogus key - expect 403
curl -sS -w "HTTP: %{http_code}\n" "https://${GW_HOST}/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-oai-FAKE-KEY" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'

# No auth - expect 401
curl -sS -w "HTTP: %{http_code}\n" "https://${GW_HOST}/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}'
```

### Verify Model Listing

```bash
curl -sS "https://${GW_HOST}/v1/models" \
  -H "Authorization: Bearer $KEY" | jq '.data[].id'
```

The model `gpt-4o` should appear in the list.

!!! tip "TLS certificate errors"
    If `curl` returns `curl: (60) SSL certificate problem`, see [Troubleshooting - TLS Certificate Validation](troubleshooting.md#tls-certificate-validation).

## Providers

Clients call every external model with the OpenAI Chat Completions API. ai-gateway-controller serves these provider bindings:

| Provider | `spec.provider` | `apiFormat` | `path` | Credential sent as | Needs API translation |
|----------|-----------------|-------------|--------|--------------------|-----------------------|
| OpenAI | `openai` | `openai-chat` | `/v1/chat/completions` | `Authorization: Bearer` | Only if `targetModel` differs from the client-facing name |
| Anthropic | `anthropic` | `messages` | `/v1/messages` | `x-api-key` | Yes |
| Azure OpenAI | `azure-openai` (or `azure`) | `openai-chat` | `/openai/v1/chat/completions` | `api-key` | Yes |
| AWS Bedrock (Mantle) | `bedrock-openai` | `openai-chat` | `/v1/chat/completions` | `Authorization: Bearer` | Only if `targetModel` differs from the client-facing name |

Any other provider and format combination, Vertex AI included, is not served: the ExternalModel fails reconciliation. The upgrade migration from the RHOAI 3.5 payload processor treats only the OpenAI and Anthropic bindings as compatible; see [Upgrade to 3.6](../migration/upgrade-to-3.6.md#ipp-praxis-api-translation-migration).

### OpenAI

Pass-through: Praxis does not translate the request or the response.

- Set `endpoint: api.openai.com`
- Models: `gpt-4o`, `gpt-4o-mini`, `gpt-4.1`, `gpt-4.1-mini`, `o1`, `o3-mini`, etc.

### Anthropic

Praxis translates OpenAI Chat Completions requests to the Anthropic Messages API and the responses back.

- Set `endpoint: api.anthropic.com`, `apiFormat: messages`, and `path: /v1/messages`
- Models: `claude-sonnet-4-20250514`, `claude-haiku-4-5-20251001`, `claude-opus-4-20250514`

Praxis rejects a Chat Completions request that carries a field it cannot map to Anthropic Messages: `response_format`, `frequency_penalty`, `presence_penalty` and `logprobs` each return `400` with `unsupported Chat Completions field "<name>" cannot be preserved by Anthropic Messages`. `max_tokens`, `user`, `stop`, `temperature`, `top_p`, `tools`, `n` and `seed` are accepted. The RHOAI 3.5 payload processor dropped the unmappable fields silently.

### Azure OpenAI

Uses Azure's OpenAI-compatible endpoint, with the `api-key` header instead of `Authorization: Bearer`.

- Set `endpoint: <resource>.openai.azure.com` (e.g., `my-deployment.openai.azure.com`)
- Set `path: /openai/v1/chat/completions`
- `targetModel` must be the deployment name in your Azure OpenAI resource, not the OpenAI model version

### AWS Bedrock (OpenAI-compatible)

Routes to the OpenAI-compatible Bedrock Mantle endpoint. Pass-through, like OpenAI, with a Bedrock API key (starts with `ABSK`, not AWS access keys) sent as `Authorization: Bearer`.

!!! warning
    Use `bedrock-mantle.<region>.api.aws`, **not** `bedrock-runtime.<region>.amazonaws.com`. Only the Mantle endpoint serves `/v1/chat/completions`; Bedrock Runtime returns `404`.

- Set `endpoint: bedrock-mantle.<region>.api.aws` (e.g., `bedrock-mantle.us-east-2.api.aws`)
- Models vary by region. List available models: `curl -s "https://bedrock-mantle.<region>.api.aws/v1/models" -H "Authorization: Bearer <KEY>"`

## Tenants still on the legacy payload processor

After an upgrade, a tenant keeps the RHOAI 3.5 Inference Payload Processor (IPP) until its migration to Praxis completes; see [Upgrade to 3.6](../migration/upgrade-to-3.6.md#ipp-praxis-api-translation-migration). While a tenant runs IPP:

- Only the default tenant's IPP serves ExternalModels. Other tenants' IPP instances have their ExternalModel controller disabled.
- IPP reads a provider Secret only when it has the label `inference.llm-d.ai/ipp-managed=true`:

    ```bash
    kubectl label secret openai-api-key -n llm inference.llm-d.ai/ipp-managed=true
    ```

- `spec.apiTranslation` does not apply. The `payload-processing-plugins` ConfigMap in the gateway namespace controls translation.
- If the tenant's migration is blocked, its payload processor also acts on other tenants' ExternalModels; see the [known issue](../migration/upgrade-to-3.6.md#known-issue-a-blocked-tenants-payload-processor-is-not-tenant-scoped).

### IPP response translation (opt-in)

The RHOAI 3.5 product-default profile runs `api-translation` on the request path only. To translate responses too:

```bash
GATEWAY_NAMESPACE="${GATEWAY_NAMESPACE:-openshift-ingress}"

# 1. Edit the live ConfigMap — under profiles[0].plugins.response add:
#      - pluginRef: api-translation
kubectl edit configmap payload-processing-plugins -n "${GATEWAY_NAMESPACE}"

# 2. Reload IPP
kubectl rollout restart deployment/payload-processing -n "${GATEWAY_NAMESPACE}"
```

maas-controller stamps the ConfigMap with `opendatahub.io/managed=false` so it keeps your edits. To reset it to product defaults once, or to have the controller own it continuously:

```bash
# One-shot reset to defaults (controller re-applies, then stamps managed=false again)
kubectl annotate configmap payload-processing-plugins -n "${GATEWAY_NAMESPACE}" opendatahub.io/managed-

# Or keep the reconciler owning the ConfigMap continuously:
kubectl annotate configmap payload-processing-plugins -n "${GATEWAY_NAMESPACE}" opendatahub.io/managed=true --overwrite
```

## Cleanup

To remove an external model and all its managed resources:

```bash
# Delete the MaaSModelRef
kubectl delete maasmodelref gpt-4o -n llm

# Delete the ExternalModel; ai-gateway-controller removes its route and Praxis configuration
kubectl delete externalmodels.inference.opendatahub.io gpt-4o -n llm

# Delete the provider once no ExternalModel uses it
kubectl delete externalproviders.inference.opendatahub.io openai -n llm

# Delete auth and subscription
kubectl delete maasauthpolicy gpt-4o-access -n models-as-a-service
kubectl delete maassubscription gpt-4o-subscription -n models-as-a-service

# Delete the provider secret
kubectl delete secret openai-api-key -n llm
```

## Related

- [MaaSModelRef CRD](../reference/crds/maas-model-ref.md)
- [MaasTenantConfig CRD](../reference/crds/tenant.md)
- [Quota and Access Configuration](../configuration-and-management/quota-and-access-configuration.md)
- [Upgrade to 3.6](../migration/upgrade-to-3.6.md)
- [Validation Guide](validation.md)
