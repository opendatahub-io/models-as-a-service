# Llamastack with Anthropic Claude

This example shows how to deploy Llamastack with Anthropic Claude models integrated into MaaS.

## Prerequisites

- Anthropic account and API key
- Kubernetes cluster with MaaS installed
- `kubectl` configured to access your cluster

## Setup

### 1. Set Environment Variable and Create Secret

First, set your API key as an environment variable:

```bash
export ANTHROPIC_API_KEY="your-actual-anthropic-api-key-here"
```

Then create the secret:

```bash
kubectl create secret generic anthropic-anthropic-api-key \
  --from-literal=api-key="$ANTHROPIC_API_KEY" \
  -n llm
```

### 2. Deploy Llamastack

```bash
cd llamastack-integration
kubectl apply -k deploy/overlays/anthropic
```

### 3. Verify Deployment

Check that the LLMInferenceService is ready:

```bash
kubectl get llminferenceservice -n llm
kubectl get pods -n llm -l provider=anthropic
```

Wait for the pod to be in `Running` state and ready.

## Testing

### 1. Check Health Endpoint

```bash
# Get the service endpoint
ANTHROPIC_SERVICE=$(kubectl get svc -n llm -l provider=anthropic -o jsonpath='{.items[0].metadata.name}')

# Test health endpoint
kubectl port-forward -n llm service/$ANTHROPIC_SERVICE 8443:443 &
curl -k https://localhost:8443/v1/health
```

Expected response:
```json
{"status": "ok"}
```

### 2. Test Model Discovery

Get a token from MaaS API and check that Anthropic models are discoverable:

```bash
# Get MaaS API endpoint
MAAS_API=$(kubectl get route -n llm maas-api -o jsonpath='{.spec.host}')

# Get token (replace with your authentication method)
TOKEN=$(curl -X POST https://$MAAS_API/v1/tokens \
  -H "Authorization: Bearer $(oc whoami -t)" | jq -r .access_token)

# List models - should include Anthropic models
curl -H "Authorization: Bearer $TOKEN" https://$MAAS_API/v1/models
```

Expected to see models like:
- `claude-3-5-sonnet-20241022`
- `claude-3-5-haiku-20241022`
- `claude-3-opus-20240229`

### 3. Test Chat Completion

```bash
# Get gateway endpoint
MAAS_GATEWAY=$(kubectl get route -n openshift-ingress maas-default-gateway -o jsonpath='{.spec.host}')

# Test chat completion
curl -X POST https://$MAAS_GATEWAY/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-3-5-sonnet-20241022",
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
| `claude-3-5-sonnet-20241022` | Anthropic Claude 3.5 Sonnet | 200,000 tokens |
| `claude-3-5-haiku-20241022` | Anthropic Claude 3.5 Haiku | 200,000 tokens |
| `claude-3-opus-20240229` | Anthropic Claude 3 Opus | 200,000 tokens |

## Troubleshooting

### Pod Not Starting

1. Check the logs:
```bash
kubectl logs -n llm -l provider=anthropic
```

2. Verify the API key secret contains actual key (not placeholder):
```bash
kubectl get secret anthropic-anthropic-api-key -n llm -o jsonpath='{.data.api-key}' | base64 -d
```

### Models Not Appearing in MaaS

1. Check that the LLMInferenceService has the correct gateway reference:
```bash
kubectl describe llminferenceservice -n llm anthropic-llamastack
```

2. Verify the service is healthy:
```bash
kubectl get pods -n llm -l provider=anthropic
```

### Authentication Issues

1. Verify your Anthropic API key is valid by testing directly:
```bash
curl -H "x-api-key: YOUR_ANTHROPIC_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  https://api.anthropic.com/v1/messages
```

## Cleanup

To remove the deployment:

```bash
kubectl delete -k deploy/overlays/anthropic
kubectl delete secret anthropic-anthropic-api-key -n llm
```

## Security Notes

- **Never commit API keys to git**: Use environment variables
- **Verify key is set**: `echo $ANTHROPIC_API_KEY` before deploying
- **Rotate keys regularly**: Update both env var and secret
- **Use dedicated keys**: Separate keys per environment (dev/staging/prod)