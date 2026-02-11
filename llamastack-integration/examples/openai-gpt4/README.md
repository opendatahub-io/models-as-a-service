# Llamastack with OpenAI GPT-4

This example shows how to deploy Llamastack with OpenAI models integrated into MaaS.

## Prerequisites

- OpenAI account and API key
- Kubernetes cluster with MaaS installed
- `kubectl` configured to access your cluster

## Setup

### 1. Set Environment Variable and Create Secret

First, set your API key as an environment variable:

```bash
export OPENAI_API_KEY="your-actual-openai-api-key-here"
```

Then create the secret:

```bash
kubectl create secret generic openai-openai-api-key \
  --from-literal=api-key="$OPENAI_API_KEY" \
  -n llm
```

### 2. Deploy Llamastack

```bash
cd llamastack-integration
kubectl apply -k deploy/overlays/openai
```

### 3. Verify Deployment

Check that the LLMInferenceService is ready:

```bash
kubectl get llminferenceservice -n llm
kubectl get pods -n llm -l provider=openai
```

Wait for the pod to be in `Running` state and ready.

## Testing

### 1. Check Health Endpoint

```bash
# Get the service endpoint
OPENAI_SERVICE=$(kubectl get svc -n llm -l provider=openai -o jsonpath='{.items[0].metadata.name}')

# Test health endpoint
kubectl port-forward -n llm service/$OPENAI_SERVICE 8443:443 &
curl -k https://localhost:8443/v1/health
```

Expected response:
```json
{"status": "ok"}
```

### 2. Test Model Discovery

Get a token from MaaS API and check that OpenAI models are discoverable:

```bash
# Get MaaS API endpoint
MAAS_API=$(kubectl get route -n llm maas-api -o jsonpath='{.spec.host}')

# Get token (replace with your authentication method)
TOKEN=$(curl -X POST https://$MAAS_API/v1/tokens \
  -H "Authorization: Bearer $(oc whoami -t)" | jq -r .access_token)

# List models - should include OpenAI models
curl -H "Authorization: Bearer $TOKEN" https://$MAAS_API/v1/models
```

Expected to see models like:
- `gpt-4o`
- `gpt-4o-mini`
- `gpt-3.5-turbo`
- `o1-preview`

### 3. Test Chat Completion

```bash
# Get gateway endpoint
MAAS_GATEWAY=$(kubectl get route -n openshift-ingress maas-default-gateway -o jsonpath='{.spec.host}')

# Test chat completion
curl -X POST https://$MAAS_GATEWAY/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "messages": [
      {"role": "user", "content": "Hello! Tell me about yourself."}
    ],
    "max_tokens": 100
  }'
```

## Available Models

This deployment provides access to:

| Model ID | Description | Context Length |
|----------|-------------|----------------|
| `gpt-4o` | OpenAI GPT-4o | 128,000 tokens |
| `gpt-4o-mini` | OpenAI GPT-4o Mini | 128,000 tokens |
| `gpt-3.5-turbo` | OpenAI GPT-3.5 Turbo | 16,385 tokens |
| `o1-preview` | OpenAI o1 Preview | 128,000 tokens |

## Troubleshooting

### Pod Not Starting

1. Check the logs:
```bash
kubectl logs -n llm -l provider=openai
```

2. Verify the API key secret contains actual key (not placeholder):
```bash
kubectl get secret openai-openai-api-key -n llm -o jsonpath='{.data.api-key}' | base64 -d
```

### Models Not Appearing in MaaS

1. Check that the LLMInferenceService has the correct gateway reference:
```bash
kubectl describe llminferenceservice -n llm openai-llamastack
```

2. Verify the service is healthy:
```bash
kubectl get pods -n llm -l provider=openai
```

### Authentication Issues

1. Verify your OpenAI API key is valid by testing directly:
```bash
curl -H "Authorization: Bearer YOUR_OPENAI_API_KEY" \
  https://api.openai.com/v1/models
```

## Cleanup

To remove the deployment:

```bash
kubectl delete -k deploy/overlays/openai
kubectl delete secret openai-openai-api-key -n llm
```

## Security Notes

- **Never commit API keys to git**: Use environment variables
- **Verify key is set**: `echo $OPENAI_API_KEY` before deploying
- **Rotate keys regularly**: Update both env var and secret
- **Use dedicated keys**: Separate keys per environment (dev/staging/prod)