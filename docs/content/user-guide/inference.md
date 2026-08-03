# Inference

This guide explains how to make inference requests to models through the MaaS platform.

!!! note "Prerequisites"
    - You have an API key. See [API Key Management](api-key-management.md) for instructions.
    - You know which model you want to use. See [Model Discovery](model-discovery.md) to list available models.

---

## Endpoint Overview

MaaS provides an **OpenAI-compatible** inference endpoint. Send all requests to a single gateway URL and specify the model in the request body:

```text
POST https://maas.<cluster-domain>/v1/chat/completions
```

The gateway reads the `model` field from the JSON body and routes the request to the correct backend automatically. This is fully compatible with OpenAI SDKs and any client that speaks the OpenAI Chat Completions API.

!!! note "Administrator prerequisite"
    Body-based routing requires the Inference Payload Processing (IPP) component to be deployed. IPP reads the `model` field from the request body and sets the routing header for the gateway. Contact your administrator to confirm IPP is available on your cluster.

!!! info "Legacy path-based endpoints"
    MaaS also supports **path-based endpoints** where the model is encoded in the URL path (e.g. `https://maas.<cluster-domain>/llm/my-model/v1/chat/completions`). These endpoints continue to work, but the body-based endpoint above is recommended for new integrations because it matches the standard OpenAI API contract and works with a single `base_url` for all models. See [Path-Based Routing (Legacy)](#path-based-routing-legacy) for details.

!!! info "Multi-tenant deployments"
    In a multi-tenant setup, non-default tenants use their own gateway URL (e.g. `https://<tenant-gateway>/v1/chat/completions`) with a tenant-scoped API key. See [Multi-Tenant Validation](../install/multi-tenant-validation.md) for details.

---

## Basic Chat Completion

Make a simple chat completion request using your API key:

```bash
# Set up your environment
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
MAAS_API_URL="https://maas.${CLUSTER_DOMAIN}"
API_KEY="sk-oai-..."  # Your API key

# Get the first ready model name
MODEL_NAME=$(curl -s "${MAAS_API_URL}/maas-api/v1/models" \
    -H "Authorization: Bearer ${API_KEY}" | \
    jq -re '[.data[] | select(.ready==true)][0].id') || \
    { echo "No ready models found"; exit 1; }

# Make an inference request
curl -sS \
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
  "${MAAS_API_URL}/v1/chat/completions"
```

!!! tip "Model ID format"
    The `model` value must match the `id` returned by `/maas-api/v1/models`. For on-cluster models (LLMInferenceService), this is a publisher ID like `publishers/llm/models/facebook/opt-125m`. For external models, this is typically the model name (e.g. `gpt-4o`). Always use the `id` from the API response rather than constructing it manually.

**Example response:**

```json
{
  "id": "chatcmpl-123",
  "object": "chat.completion",
  "created": 1677652288,
  "model": "publishers/llm/models/facebook/opt-125m",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "Hello! I'm doing well, thank you for asking. How can I help you today?"
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 10,
    "completion_tokens": 20,
    "total_tokens": 30
  }
}
```

---

## Streaming Chat Completion

Add `"stream": true` and use `--no-buffer` for real-time responses:

```bash
curl -sS --no-buffer \
  -H "Authorization: Bearer ${API_KEY}" \
  -H "Content-Type: application/json" \
  -d "{
        \"model\": \"${MODEL_NAME}\",
        \"messages\": [
          {
            \"role\": \"user\",
            \"content\": \"Tell me a short story\"
          }
        ],
        \"max_tokens\": 200,
        \"stream\": true
      }" \
  "${MAAS_API_URL}/v1/chat/completions"
```

**Example streaming response:**

```text
data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"publishers/llm/models/facebook/opt-125m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"publishers/llm/models/facebook/opt-125m","choices":[{"index":0,"delta":{"content":"Once"},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1677652288,"model":"publishers/llm/models/facebook/opt-125m","choices":[{"index":0,"delta":{"content":" upon"},"finish_reason":null}]}

data: [DONE]
```

Each `data:` line contains a JSON object with a `delta` field containing the incremental content. The stream ends with `data: [DONE]`.

---

## Request Parameters

Common parameters for chat completions:

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `model` | string | Yes | Model identifier (`id`) from `/maas-api/v1/models` |
| `messages` | array | Yes | Array of message objects with `role` and `content` |
| `max_tokens` | integer | No | Maximum tokens to generate (default varies by model) |
| `temperature` | float | No | Sampling temperature (0-2, default 1.0). Higher = more random. |
| `top_p` | float | No | Nucleus sampling (0-1, default 1.0) |
| `stream` | boolean | No | Enable streaming (default false) |
| `stop` | string or array | No | Stop sequences where generation will halt |

**Message roles:**

- `system` - Instructions for the model's behavior
- `user` - User messages
- `assistant` - Model responses (for multi-turn conversations)

---

## Multi-Turn Conversations

Include previous messages for context:

```bash
curl -sS \
  -H "Authorization: Bearer ${API_KEY}" \
  -H "Content-Type: application/json" \
  -d "{
        \"model\": \"${MODEL_NAME}\",
        \"messages\": [
          {
            \"role\": \"system\",
            \"content\": \"You are a helpful assistant that answers concisely.\"
          },
          {
            \"role\": \"user\",
            \"content\": \"What is the capital of France?\"
          },
          {
            \"role\": \"assistant\",
            \"content\": \"The capital of France is Paris.\"
          },
          {
            \"role\": \"user\",
            \"content\": \"What is its population?\"
          }
        ],
        \"max_tokens\": 100
      }" \
  "${MAAS_API_URL}/v1/chat/completions"
```

---

## Path-Based Routing (Legacy)

MaaS also supports per-model URL endpoints where the model is identified by the URL path rather than the request body. These endpoints are available at the `url` field returned by `/maas-api/v1/models`.

```bash
# Get the per-model URL
MODEL_URL=$(curl -s "${MAAS_API_URL}/maas-api/v1/models" \
    -H "Authorization: Bearer ${API_KEY}" | \
    jq -re '[.data[] | select(.ready==true)][0].url') || \
    { echo "No ready models found"; exit 1; }

# Make an inference request using the path-based URL
curl -sS \
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

### Comparison

| | Body-based (recommended) | Path-based (legacy) |
|---|---|---|
| **URL** | Single: `${MAAS_API_URL}/v1/chat/completions` | Per-model: `${MODEL_URL}/v1/chat/completions` |
| **Model selection** | `model` field in request body | URL path determines the model |
| **OpenAI SDK compatible** | Yes, single `base_url` for all models | Requires setting `base_url` per model |
| **Requires IPP** | Yes | No |

!!! tip
    The `model` value must match a model `id` returned by [`/maas-api/v1/models`](model-discovery.md). If the model name does not match, the request will be rejected.

---

## Access and Rate Limits

Your API key is **bound to one subscription** at creation time, which determines:

- Which models you can access
- Token rate limits (e.g., 100 tokens/min, 100000 tokens/24h)
- Metadata for observability

Access requires both a MaaSAuthPolicy (permission) and MaaSSubscription (quota). Contact your administrator for details.

---

## Error Handling

Common HTTP error codes:

| Code | Meaning | Action |
|------|---------|--------|
| 401 | Invalid or malformed API key or authorization header | Verify the key is correctly formatted: `Authorization: Bearer <key>` |
| 403 | Expired/revoked key or insufficient permissions | Create a new API key if expired/revoked, otherwise contact your administrator |
| 429 | Rate limit exceeded | Wait before retrying, or contact your administrator to adjust limits |
| 404 | Model not found | Verify the model ID exists in your subscription via `/maas-api/v1/models` |
| 500 | Internal server error | Check model backend status, contact your administrator if persistent |

!!! tip "TLS certificate errors"
    If `curl` returns `curl: (60) SSL certificate problem`, see [Troubleshooting - TLS Certificate Validation](../install/troubleshooting.md#tls-certificate-validation).

### Handling Rate Limits

When you receive a `429 Too Many Requests` response:

1. **Check the response headers** for rate limit information (if available)
2. **Wait before retrying** - implement exponential backoff
3. **Review your subscription limits** - you may need a higher tier

**Example with exponential backoff:**

```bash
retry_count=0
max_retries=3
backoff=2

while [ $retry_count -lt $max_retries ]; do
  response=$(curl -sS -w "\n%{http_code}" \
    -H "Authorization: Bearer ${API_KEY}" \
    -H "Content-Type: application/json" \
    -d "{\"model\": \"${MODEL_NAME}\", \"messages\": [{\"role\": \"user\", \"content\": \"Hello\"}]}" \
    "${MAAS_API_URL}/v1/chat/completions")
  
  http_code=$(echo "$response" | tail -n1)
  body=$(echo "$response" | head -n-1)
  
  if [ "$http_code" != "429" ]; then
    echo "$body"
    break
  fi
  
  retry_count=$((retry_count + 1))
  sleep_time=$((backoff ** retry_count))
  echo "Rate limited, waiting ${sleep_time}s..." >&2
  sleep $sleep_time
done
```

---

## Related Documentation

- **[Model Discovery](model-discovery.md)** - Find other available models
- **[API Key Management](api-key-management.md)** - Manage your API keys
- **[Access and Quota Overview](../concepts/subscription-overview.md)** - How policies and subscriptions determine access
- **[API Key Authentication](../concepts/api-key-authentication.md)** - Technical deep dive into authentication flows
