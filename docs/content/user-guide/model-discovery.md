# Model Discovery

This guide explains how to discover which models are available to you through the MaaS platform.

!!! note "Prerequisites"
    You need an API key to list models. See [API Key Management](api-key-management.md) for instructions on creating one.

---

## Listing Available Models

Get a list of models available to your subscription:

```bash
# Set up your environment
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
MAAS_API_URL="https://maas.${CLUSTER_DOMAIN}"
API_KEY="sk-oai-..."  # Your API key from api-key-management

# List models
curl "${MAAS_API_URL}/maas-api/v1/models" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${API_KEY}" | jq .
```

**Example response:**

```json
{
  "object": "list",
  "data": [
    {
      "id": "llama-2-7b-chat",
      "created": 1672531200,
      "object": "model",
      "owned_by": "llm/llama-2-7b-chat",
      "kind": "LLMInferenceService",
      "url": "https://maas.your-domain.com/llm/llama-2-7b-chat",
      "ready": true,
      "modelDetails": {
        "description": "Llama 2 7B optimized for chat",
        "displayName": "Llama 2 7B Chat"
      },
      "subscriptions": [
        {
          "name": "premium-subscription",
          "displayName": "Premium Tier",
          "description": "Premium-tier subscription with 1000 tokens/min rate limit"
        }
      ]
    },
    {
      "id": "mixtral-8x7b-instruct",
      "created": 1672531200,
      "object": "model",
      "owned_by": "llm/mixtral-8x7b-instruct",
      "kind": "LLMInferenceService",
      "url": "https://maas.your-domain.com/llm/mixtral-8x7b-instruct",
      "ready": true,
      "modelDetails": {
        "description": "Mixtral 8x7B instruction-tuned model",
        "displayName": "Mixtral 8x7B Instruct"
      },
      "subscriptions": [
        {
          "name": "premium-subscription",
          "displayName": "Premium Tier",
          "description": "Premium-tier subscription with 1000 tokens/min rate limit"
        }
      ]
    }
  ]
}
```

---

## Understanding the Response

Each model in the `data` array contains:

| Field | Description |
|-------|-------------|
| `id` | Model identifier used in inference requests |
| `kind` | Backend type: `LLMInferenceService` or `ExternalModel` |
| `url` | Path-based endpoint URL for the model (legacy). For body-based routing, use the gateway base URL with `/v1/chat/completions` instead (see [Inference](inference.md)). |
| `ready` | Whether the model is currently available (`true`/`false`) |
| `modelDetails.displayName` | Human-friendly model name |
| `modelDetails.description` | Model description |
| `subscriptions` | List of subscriptions that provide access to this model |

---

## Access Control

The models you see depend on:

1. **Your API key's subscription** - Bound at key creation time
2. **MaaSAuthPolicy** - Defines which groups can access which models
3. **Model readiness** - Only models with `ready: true` are available for inference

If a model doesn't appear in the list, check:

- Your API key is bound to the correct subscription
- Your groups are included in the MaaSAuthPolicy for that model
- The model backend is ready (check with your administrator)

!!! info "Learn more about access control"
    For details on how policies and subscriptions work together, see [Access and Quota Overview](../concepts/subscription-overview.md).

---

## Using Model Information

### Make an Inference Request

Use the model `id` with the gateway's body-based endpoint:

```bash
MODEL_NAME=$(curl -s "${MAAS_API_URL}/maas-api/v1/models" \
    -H "Authorization: Bearer ${API_KEY}" | \
    jq -re '[.data[] | select(.ready==true)][0].id') || \
    { echo "No ready models found"; exit 1; }

curl -sS \
  -H "Authorization: Bearer ${API_KEY}" \
  -H "Content-Type: application/json" \
  -d "{\"model\": \"${MODEL_NAME}\", \"messages\": [{\"role\": \"user\", \"content\": \"Hello\"}]}" \
  "${MAAS_API_URL}/v1/chat/completions"
```

!!! tip "Model ID format"
    For on-cluster models (LLMInferenceService), the `id` is a publisher ID like `publishers/llm/models/facebook/opt-125m`. For external models, it is typically the model name (e.g. `gpt-4o`). Always use the `id` from the API response.

See [Inference](inference.md) for full examples including streaming and multi-turn conversations.

### Get the Path-Based Model URL (Legacy)

The `url` field provides a per-model endpoint for path-based routing:

```bash
MODEL_URL=$(curl -s "${MAAS_API_URL}/maas-api/v1/models" \
    -H "Authorization: Bearer ${API_KEY}" | \
    jq -re '[.data[] | select(.ready==true)][0].url') || \
    { echo "No ready models found"; exit 1; }

echo "Model URL: ${MODEL_URL}"
```

See [Inference - Path-Based Routing](inference.md#path-based-routing-legacy) for details.

### Check Model Readiness

Filter for only ready models:

```bash
curl "${MAAS_API_URL}/maas-api/v1/models" \
    -H "Authorization: Bearer ${API_KEY}" | \
    jq '.data[] | select(.ready==true)'
```

### List Models by Subscription

See which subscription provides access to each model:

```bash
curl "${MAAS_API_URL}/maas-api/v1/models" \
    -H "Authorization: Bearer ${API_KEY}" | \
    jq '.data[] | {id, subscriptions: (.subscriptions // [] | map(.name))}'
```

---

## Next Steps

- **[Inference](inference.md)** - Make inference requests with your API key

## Related Documentation

- **[API Key Management](api-key-management.md)** - Manage your API keys
- **[Model Access Control](../concepts/model-access-control.md)** - Technical deep dive into how model discovery and access decisions work
- **[Access and Quota Overview](../concepts/subscription-overview.md)** - How policies and subscriptions determine access
- **[Quota and Access Configuration](../configuration-and-management/quota-and-access-configuration.md)** - For administrators: configuring model access
