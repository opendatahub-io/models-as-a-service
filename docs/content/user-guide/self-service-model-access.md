# Self-Service Model Access

This guide is for **end users** who want to use AI models through the MaaS platform.

## 🎯 What is MaaS?

The Models-as-a-Service (MaaS) platform provides access to AI models through a simple API. Your organization's administrator has set up the platform and configured access for your team.

## Getting Your API Key

!!! tip
    For a detailed explanation of how API key authentication works, including the underlying architecture and security model, see [Understanding Token Management](../configuration-and-management/token-management.md).

### Step 1: Get Your OpenShift Authentication Token

First, you need your OpenShift token to prove your identity to the maas-api.

```bash
# Log in to your OpenShift cluster if you haven't already
oc login ...

# Get your current OpenShift authentication token
OC_TOKEN=$(oc whoami -t)
```

### Step 2: Create an API Key

Use your OpenShift token to create an API key via the maas-api `/v1/api-keys` endpoint. You can create permanent keys (omit `expiresIn`) or expiring keys.

- Optional `subscription`: MaaSSubscription resource name to bind to this key. If you omit it, the platform picks your **highest-priority** accessible subscription (`spec.priority`).
- The response includes `subscription`: the bound name (same flow whether you set it explicitly or not).

```bash
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

- **Permanent keys**: Omit `expiresIn` in the request body
- **Expiring keys**: Set `expiresIn` (e.g., `"90d"`, `"1h"`, `"30d"`)
- **Subscription**: Fixed at creation; mint a new key to change it
- **Revocation**: Revoke via `DELETE /v1/api-keys/{id}` if compromised

## Discovering Models

### List Available Models

Get a list of models available to your subscription:

```bash
MODELS=$(curl "${MAAS_API_URL}/v1/models" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${API_KEY}")

echo $MODELS | jq .
```

Example response:

```json
{
  "data": [
    {
      "id": "simulator",
      "name": "Simulator Model",
      "url": "https://gateway.your-domain.com/simulator/v1/chat/completions",
      "subscription": "free"
    },
    {
      "id": "qwen3",
      "name": "Qwen3 Model",
      "url": "https://gateway.your-domain.com/qwen3/v1/chat/completions",
      "subscription": "premium"
    }
  ]
}
```

### Get Model Details

Get detailed information about a specific model:

```bash
MODEL_ID="simulator"
MODEL_INFO=$(curl "${MAAS_API_URL}/v1/models" \
    -H "Authorization: Bearer ${API_KEY}" | \
    jq --arg model "$MODEL_ID" '.data[] | select(.id == $model)')

echo $MODEL_INFO | jq .
```

## Making Inference Requests

Use **only** your API key in `Authorization: Bearer`. The subscription is fixed when the key was created.

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

Your access is determined by your **subscription**, which controls:

- **Available models** - Which AI models you can use
- **Request limits** - How many requests per minute
- **Token limits** - Maximum tokens per request
- **Features** - Advanced capabilities available

Rate limits are configured per-model in MaaSAuthPolicy and MaaSSubscription. Contact your administrator for your subscription's limits.

## Error Handling

### Common Error Responses

**401 Unauthorized**

```json
{
  "error": {
    "message": "Invalid authentication token",
    "type": "invalid_request_error",
    "code": "invalid_api_key"
  }
}
```

**403 Forbidden**

```json
{
  "error": {
    "message": "Insufficient permissions for this model",
    "type": "permission_error",
    "code": "access_denied"
  }
}
```

**429 Too Many Requests**

```json
{
  "error": {
    "message": "Rate limit exceeded",
    "type": "rate_limit_error",
    "code": "rate_limit_exceeded"
  }
}
```

## Monitoring Usage

Check your current usage through response headers:

```bash
# Make a request and check headers
curl -I -sSk \
  -H "Authorization: Bearer ${API_KEY}" \
  -H "Content-Type: application/json" \
  -d '{"model": "simulator", "messages": [{"role": "user", "content": "test"}]}' \
  "${MODEL_URL}/v1/chat/completions" | grep -i "x-ratelimit"
```

## ⚠️ Common Issues

### Authentication Errors

**Problem**: `401 Unauthorized`

**Solution**: Check your API key and ensure it's correctly formatted:

```bash
# Correct format
-H "Authorization: Bearer YOUR_API_KEY"

# Wrong format
-H "Authorization: YOUR_API_KEY"
```

### Rate Limit Exceeded

**Problem**: `429 Too Many Requests`

**Solution**: Wait before making more requests, or contact your administrator to adjust your subscription limits.

### Model Not Available

**Problem**: `404 Model Not Found`

**Solution**: Check which models are available in your subscription:

```bash
curl -X GET "${MAAS_API_URL}/v1/models" \
  -H "Authorization: Bearer ${API_KEY}"
```
