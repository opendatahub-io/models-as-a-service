# Model-B Test Simulator for Kind

This directory contains Kubernetes manifests for deploying **model-b** as a test LLMInferenceService in the Kind local development environment.

## What is model-b?

Model-b is a **premium tier** test model using the lightweight LLM inference simulator:
- **Tier**: Premium (accessible only to premium and enterprise users)
- **Simulator**: Fast inference simulator for testing (not real LLM)
- **OpenAI-compatible API**: `/v1/models`, `/v1/chat/completions`
- **Fast startup**: ~10-15 seconds
- **Minimal resources**: 100m CPU, 128Mi memory

Perfect for testing tier-based access control in the MaaS platform!

## Quick Deploy

```bash
# 1. Create namespace (if not exists)
kubectl create namespace llm --dry-run=client -o yaml | kubectl apply -f -

# 2. Deploy model-b
kubectl apply -k deployment/overlays/kind/test-models/model-b/

# 3. Wait for LLMInferenceService to be ready
kubectl wait --for=condition=Ready llminferenceservice/model-b -n llm --timeout=60s

# 4. Check status
kubectl get llminferenceservices -n llm
kubectl get pods -n llm -l serving.kserve.io/inferenceservice=model-b
```

## Test the Model

### Via Gateway (MaaS Platform)

```bash
# Get premium auth token (required for model-b access)
TOKEN=$(kubectl create token premium-user -n maas-api --audience=maas-default-gateway-sa --duration=1h)

# List models
curl -H "Authorization: Bearer $TOKEN" http://localhost/v1/models

# Test model-b (premium tier)
curl -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "model-b",
    "messages": [{"role": "user", "content": "Hello from premium tier!"}],
    "max_tokens": 50
  }' \
  http://localhost/llm/model-b/v1/chat/completions

# Test access control - free user should be blocked
FREE_TOKEN=$(kubectl create token free-user -n maas-api --audience=maas-default-gateway-sa --duration=1h)
curl -H "Authorization: Bearer $FREE_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model": "model-b", "messages": [{"role": "user", "content": "Hello"}]}' \
  http://localhost/llm/model-b/v1/chat/completions
# Expected: 401 Unauthorized
```

### Via Port-Forward (Direct Access)

```bash
# Port-forward to the simulator service
kubectl port-forward -n llm svc/model-b-kserve-workload-svc 8001:8000

# Test health endpoint
curl http://localhost:8001/health

# List models
curl http://localhost:8001/v1/models

# Send chat completion request
curl -X POST http://localhost:8001/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "model-b",
    "messages": [{"role": "user", "content": "Hello! What is 2+2?"}],
    "max_tokens": 50
  }'
```

## Resource Requirements

- **CPU**: 100m (request), 500m (limit)
- **Memory**: 128Mi (request), 256Mi (limit)
- **Startup Time**: 10-15 seconds
- **Access Tier**: Premium (premium/enterprise users only)

## Architecture

This model uses:
- **LLMInferenceService CRD** (KServe v0.16.0+)
- **Inference Simulator**: `ghcr.io/llm-d/llm-d-inference-sim:v0.5.1`
- **PVC approach**: Workaround for storage-initializer requirements
- **HTTPRoute**: Gateway API routing with URL rewriting
- **AuthPolicy**: Restricts access to premium/enterprise tiers only

## Access Control

Model-b is protected by tier-based access control:

| User Type | Access | Token Command |
|-----------|--------|---------------|
| **Free** | ❌ Blocked (401) | `kubectl create token free-user -n maas-api --audience=maas-default-gateway-sa --duration=1h` |
| **Premium** | ✅ Allowed | `kubectl create token premium-user -n maas-api --audience=maas-default-gateway-sa --duration=1h` |
| **Enterprise** | ✅ Allowed | `kubectl create token enterprise-user -n maas-api --audience=maas-default-gateway-sa --duration=1h` |

## Troubleshooting

### LLMInferenceService Not Ready

```bash
# Check LLMInferenceService status
kubectl describe llminferenceservice model-b -n llm

# Check pod status
kubectl get pods -n llm -l serving.kserve.io/inferenceservice=model-b
kubectl describe pod -n llm -l serving.kserve.io/inferenceservice=model-b

# Check logs
kubectl logs -n llm -l serving.kserve.io/inferenceservice=model-b --tail=50
```

### Access Control Issues

```bash
# Check AuthPolicy
kubectl get authpolicy -n istio-system -o yaml

# Check HTTPRoute
kubectl get httproute -n llm model-b-route -o yaml

# Check service endpoints
kubectl get endpoints -n llm model-b-kserve-workload-svc

# Test different user tokens
FREE_TOKEN=$(kubectl create token free-user -n maas-api --audience=maas-default-gateway-sa --duration=1h)
PREMIUM_TOKEN=$(kubectl create token premium-user -n maas-api --audience=maas-default-gateway-sa --duration=1h)

echo "Free token: $FREE_TOKEN"
echo "Premium token: $PREMIUM_TOKEN"
```

## Use Cases for Testing

### 1. Tier-Based Access Control
Test that only premium/enterprise users can access model-b

### 2. Authorization Policies
Validate AuthPolicy enforcement for different user tiers

### 3. Multi-Model Scenarios
Use alongside model-a to test mixed tier deployments

### 4. Rate Limiting by Tier
Test different rate limits for premium tier users

## Cleanup

```bash
kubectl delete -k deployment/overlays/kind/test-models/model-b/
```

## References

- [KServe LLMInferenceService Documentation](https://github.com/kserve/kserve/tree/master/docs)
- [LLM-D Inference Simulator](https://github.com/llm-d/llm-d-inference-sim)
- [Gateway API HTTPRoute](https://gateway-api.sigs.k8s.io/references/spec/#gateway.networking.k8s.io/v1.HTTPRoute)
- [Kuadrant AuthPolicy](https://docs.kuadrant.io/stable/kuadrant-operator/doc/auth/)