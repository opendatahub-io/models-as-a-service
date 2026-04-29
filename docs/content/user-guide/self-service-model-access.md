# Self-Service Model Access

This guide explains how to create API keys and call models through the MaaS platform. Assumes the platform has been deployed and configured by an administrator.

## Getting Your API Key

!!! tip
    For a detailed explanation of how API key authentication works, including the underlying architecture and security model, see [Understanding Token Management](../configuration-and-management/token-management.md).


Use your OpenShift token to create an API key via the maas-api `/v1/api-keys` endpoint. Keys always expire: omit `expiresIn` to use the operator-configured maximum lifetime, or set a shorter `expiresIn` within that cap.

- Optional `subscription`: MaaSSubscription resource name to bind to this key. If you omit it, the platform picks your **highest-priority** accessible subscription (`spec.priority`).
- The response includes `subscription`: the bound name (same flow whether you set it explicitly or not).

```bash
OC_TOKEN=$(oc whoami -t)
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
MAAS_API_URL="https://maas.${CLUSTER_DOMAIN}"

API_KEY_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer ${OC_TOKEN}" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{"name": "my-api-key", "description": "Key for model access", "expiresIn": "90d", "subscription": "simulator-subscription"}' \
  "${MAAS_API_URL}/maas-api/v1/api-keys")

API_KEY=$(echo $API_KEY_RESPONSE | jq -r .key)
SUBSCRIPTION=$(echo $API_KEY_RESPONSE | jq -r .subscription)

echo "Key prefix: ${API_KEY:0:16}..."
echo "Bound subscription: ${SUBSCRIPTION}"
```

Replace `simulator-subscription` with your MaaSSubscription metadata name, or remove the `subscription` field to bind the **highest-priority** subscription you can access.

!!! warning "API key shown only once"
    The plaintext API key is returned **only at creation time**. We do not store the API key, so there is no way to retrieve it again. Store it securely when it is displayed. If you run into errors, see [Troubleshooting](../install/troubleshooting.md).

### API Key Lifecycle

- **Expiration**: Omit `expiresIn` to use the operator maximum (`API_KEY_MAX_EXPIRATION_DAYS`; see [Token Management](../configuration-and-management/token-management.md)), or set `expiresIn` (e.g., `"90d"`, `"1h"`, `"30d"`) up to that maximum
- **Subscription**: Fixed at creation; mint a new key to change it
- **Revocation**: Revoke via `DELETE /v1/api-keys/{id}` if compromised

## Discovering Models

### List Available Models

Get a list of models available to your subscription:

```bash
curl "${MAAS_API_URL}/v1/models" \
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

!!! note "API key vs OpenShift token behavior"
    - **When authenticating with an API key** (bound to one subscription at creation time), only models from that subscription are returned
    - **When authenticating with an OpenShift token**, models from all accessible subscriptions are returned. Use the `x-maas-subscription` header to filter to a specific subscription; a model may list multiple subscriptions in its `subscriptions` array

## Making Inference Requests

Authenticate with your API key in the `Authorization: Bearer` header. The subscription is already bound to the key.

### Basic Chat Completion

Make a simple chat completion request:

```bash
# First, get the model URL from the models endpoint
MODELS=$(curl "${MAAS_API_URL}/v1/models" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${API_KEY}")
MODEL_URL=$(echo $MODELS | jq -r '.data[0].url')
MODEL_NAME=$(echo $MODELS | jq -r '.data[0].id')

curl -sSk \
  -H "Authorization: Bearer ${API_KEY}" \
  -H "Content-Type: application/json" \
  -d "{
        \"model\": \"${MODEL_NAME}\",
        \"messages\": [
          {
            \"role\": \"user\",
            \"content\": \"Hello, how are you?\"
          }
        ],
        \"max_tokens\": 100
      }" \
  "${MODEL_URL}/v1/chat/completions"
```

### Streaming Chat Completion

For streaming responses, add `"stream": true` to the request and use `--no-buffer` to process the response in real-time:

```bash
curl -sSk --no-buffer \
  -H "Authorization: Bearer ${API_KEY}" \
  -H "Content-Type: application/json" \
  -d "{
        \"model\": \"${MODEL_NAME}\",
        \"messages\": [
          {
            \"role\": \"user\",
            \"content\": \"Hello, how are you?\"
          }
        ],
        \"max_tokens\": 100,
        \"stream\": true
      }" \
  "${MODEL_URL}/v1/chat/completions"
```

## Understanding Your Access Level

Your access to models requires both:

- **Access permission** - An administrator must create a MaaSAuthPolicy granting your group access to specific models
- **Subscription** - Determines which models are available and the token rate limits (e.g., 100 tokens per minute, 100000 tokens per 24 hours)

You must have both a matching MaaSAuthPolicy and MaaSSubscription to use a model. Rate limits are configured per-model in MaaSSubscription. Contact your administrator for details about your access and limits.

## Error Handling

Common HTTP error codes:

| Code | Meaning | Action |
|------|---------|--------|
| 401 | Invalid or malformed API key or authorization header | Verify the key is correctly formatted: `Authorization: Bearer <key>` |
| 403 | Expired/revoked key or insufficient permissions | Create a new API key if expired/revoked, otherwise contact your administrator |
| 429 | Rate limit exceeded | Wait before retrying, or contact your administrator to adjust limits |
| 404 | Model not found | Verify the model ID exists in your subscription via `/v1/models` |
