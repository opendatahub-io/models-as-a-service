# Llamastack with Google Gemini Pro

This example shows how to deploy Llamastack with Google Gemini models integrated into MaaS.

## Prerequisites

- Google Cloud account with Vertex AI API enabled
- Gemini API key
- Kubernetes cluster with MaaS installed
- `kubectl` configured to access your cluster

## Setup

### 1. Create API Key Secret

Replace `YOUR_GEMINI_API_KEY` with your actual Google Gemini API key:

```bash
kubectl create secret generic gemini-api-key \
  --from-literal=api-key="YOUR_GEMINI_API_KEY" \
  -n llm
```

### 2. Deploy Llamastack

```bash
cd llamastack-integration
kubectl apply -k deploy/overlays/gemini
```

### 3. Verify Deployment

Check that the LLMInferenceService is ready:

```bash
kubectl get llminferenceservice -n llm
kubectl get pods -n llm -l provider=gemini
```

Wait for the pod to be in `Running` state and ready.

## Testing

### 1. Check Health Endpoint

```bash
# Get the service endpoint
GEMINI_SERVICE=$(kubectl get svc -n llm -l provider=gemini -o jsonpath='{.items[0].metadata.name}')

# Test health endpoint
kubectl port-forward -n llm service/$GEMINI_SERVICE 8443:443 &
curl -k https://localhost:8443/v1/health
```

Expected response:
```json
{"status": "ok"}
```

### 2. Test Model Discovery

Get a token from MaaS API and check that Gemini models are discoverable:

```bash
# Get MaaS API endpoint
MAAS_API=$(kubectl get route -n llm maas-api -o jsonpath='{.spec.host}')

# Get token (replace with your authentication method)
TOKEN=$(curl -X POST https://$MAAS_API/v1/tokens \
  -H "Authorization: Bearer $(oc whoami -t)" | jq -r .access_token)

# List models - should include Gemini models
curl -H "Authorization: Bearer $TOKEN" https://$MAAS_API/v1/models
```

Expected to see models like:
- `gemini-1.5-pro`
- `gemini-1.5-flash`
- `gemini-2.0-flash-exp`

### 3. Test Chat Completion

```bash
# Get gateway endpoint
MAAS_GATEWAY=$(kubectl get route -n openshift-ingress maas-default-gateway -o jsonpath='{.spec.host}')

# Test chat completion
curl -X POST https://$MAAS_GATEWAY/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gemini-1.5-pro",
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
| `gemini-1.5-pro` | Google Gemini 1.5 Pro | 2,097,152 tokens |
| `gemini-1.5-flash` | Google Gemini 1.5 Flash | 1,048,576 tokens |
| `gemini-2.0-flash-exp` | Google Gemini 2.0 Flash (Experimental) | 1,048,576 tokens |

## Troubleshooting

### Pod Not Starting

1. Check the logs:
```bash
kubectl logs -n llm -l provider=gemini
```

2. Verify the API key secret:
```bash
kubectl get secret gemini-api-key -n llm -o yaml
```

### Models Not Appearing in MaaS

1. Check that the LLMInferenceService has the correct gateway reference:
```bash
kubectl describe llminferenceservice -n llm gemini-llamastack
```

2. Verify the service is healthy:
```bash
kubectl get pods -n llm -l provider=gemini
```

### Authentication Issues

1. Verify your Gemini API key is valid by testing directly:
```bash
curl -H "Authorization: Bearer YOUR_GEMINI_API_KEY" \
  https://generativelanguage.googleapis.com/v1beta/models
```

## Cleanup

To remove the deployment:

```bash
kubectl delete -k deploy/overlays/gemini
kubectl delete secret gemini-api-key -n llm
```