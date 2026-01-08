# Model-A Test Simulator for Kind

This directory contains Kubernetes manifests for deploying **model-a** as a test LLMInferenceService in the Kind local development environment.

## What is model-a?

Model-a is a **free tier** test model using the lightweight LLM inference simulator:
- **Tier**: Free (accessible to all user types: free, premium, enterprise)
- **Simulator**: Fast inference simulator for testing (not real LLM)
- **OpenAI-compatible API**: `/v1/models`, `/v1/chat/completions`
- **Fast startup**: ~10-15 seconds
- **Minimal resources**: 100m CPU, 128Mi memory

Perfect for testing the MaaS platform without heavy models!

## Quick Deploy

```bash
# 1. Create namespace (if not exists)
kubectl create namespace llm --dry-run=client -o yaml | kubectl apply -f -

# 2. Deploy model-a
kubectl apply -k deployment/overlays/kind/test-models/model-a/

# 3. Wait for LLMInferenceService to be ready
kubectl wait --for=condition=Ready llminferenceservice/model-a -n llm --timeout=60s

# 4. Check status
kubectl get llminferenceservices -n llm
kubectl get pods -n llm -l serving.kserve.io/inferenceservice=model-a
```

## Test the Model

### Via Gateway (MaaS Platform)

```bash
# Get auth token (any user type can access model-a)
TOKEN=$(kubectl create token free-user -n maas-api --audience=maas-default-gateway-sa --duration=1h)

# List models
curl -H "Authorization: Bearer $TOKEN" http://localhost/v1/models

# Test model-a
curl -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "model-a",
    "messages": [{"role": "user", "content": "Hello, how are you?"}],
    "max_tokens": 50
  }' \
  http://localhost/llm/model-a/v1/chat/completions
```

### Via Port-Forward (Direct Access)

```bash
# Port-forward to the simulator service
kubectl port-forward -n llm svc/model-a-kserve-workload-svc 8000:8000

# Test health endpoint
curl http://localhost:8000/health

# List models
curl http://localhost:8000/v1/models

# Send chat completion request
curl -X POST http://localhost:8000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "model-a",
    "messages": [{"role": "user", "content": "Hello! What is 2+2?"}],
    "max_tokens": 50
  }'
```

## Resource Requirements

- **CPU**: 100m (request), 500m (limit)
- **Memory**: 128Mi (request), 256Mi (limit)
- **Startup Time**: 10-15 seconds
- **Access Tier**: Free (no restrictions)

## Architecture

This model uses:
- **LLMInferenceService CRD** (KServe v0.16.0+)
- **Inference Simulator**: `ghcr.io/llm-d/llm-d-inference-sim:v0.5.1`
- **PVC approach**: Workaround for storage-initializer requirements
- **HTTPRoute**: Gateway API routing with URL rewriting

## Troubleshooting

### LLMInferenceService Not Ready

```bash
# Check LLMInferenceService status
kubectl describe llminferenceservice model-a -n llm

# Check pod status
kubectl get pods -n llm -l serving.kserve.io/inferenceservice=model-a
kubectl describe pod -n llm -l serving.kserve.io/inferenceservice=model-a

# Check logs
kubectl logs -n llm -l serving.kserve.io/inferenceservice=model-a --tail=50
```

### Gateway Access Issues

```bash
# Check HTTPRoute
kubectl get httproute -n llm model-a-route -o yaml

# Check service endpoints
kubectl get endpoints -n llm model-a-kserve-workload-svc

# Test token
TOKEN=$(kubectl create token free-user -n maas-api --audience=maas-default-gateway-sa --duration=1h)
echo "Token: $TOKEN"
```

## Use Cases for Testing

### 1. Free Tier Access Control
Test that all user types (free, premium, enterprise) can access model-a

### 2. Rate Limiting
Test request and token rate limits against free tier model

### 3. Multi-Model Routing
Use alongside model-b to test tier-based routing

### 4. Load Testing
Lightweight enough for concurrent request testing

## Cleanup

```bash
kubectl delete -k deployment/overlays/kind/test-models/model-a/
```

## References

- [KServe LLMInferenceService Documentation](https://github.com/kserve/kserve/tree/master/docs)
- [LLM-D Inference Simulator](https://github.com/llm-d/llm-d-inference-sim)
- [Gateway API HTTPRoute](https://gateway-api.sigs.k8s.io/references/spec/#gateway.networking.k8s.io/v1.HTTPRoute)