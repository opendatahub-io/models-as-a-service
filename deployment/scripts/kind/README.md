# MaaS Local Development with Kind

Run the full MaaS platform locally using [Kubernetes in Docker (Kind)](https://kind.sigs.k8s.io/).

## 🚀 Quick Start

### One-Command Setup

```bash
# Deploy everything (includes test model by default)
./deployment/scripts/kind/install.sh
```

Wait ~2-3 minutes for all pods to be ready.

> **Note:** Two test models are included by default demonstrating both KServe CRD types:
> - **model-a**: `InferenceService` (standard KServe)
> - **model-b**: `LLMInferenceService` (KServe v0.16.0+)
> 
> Use `--without-models` to skip them.

### Quick Test

```bash
# Get a free-tier token (can access model-a only)
TOKEN=$(kubectl create token free-user -n maas-api --duration=1h --audience=maas-default-gateway-sa)

# List all models (shows both models with permission info)
curl -H "Authorization: Bearer $TOKEN" http://localhost/v1/models

# Chat with model-a
curl -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"model":"model-a","messages":[{"role":"user","content":"Hello"}]}' \
  http://localhost/llm/model-a/v1/chat/completions

# Try to access model-b directly (will get 403 Forbidden with free tier)
curl -H "Authorization: Bearer $TOKEN" http://localhost/llm/model-b/v1/models

# Get premium token to access model-b
TOKEN=$(kubectl create token premium-user -n maas-api --duration=1h --audience=maas-default-gateway-sa)
curl -H "Authorization: Bearer $TOKEN" http://localhost/llm/model-b/v1/models
```

### Run Tests & Demo

```bash
cd deployment/scripts/kind

# Automated tests
./validationtest.sh

# Interactive demo
./demo.sh

# Dedicated test scripts
./tests/test-list-models.sh      # Quick model discovery test
./tests/test-rate-limits.sh      # Request rate limiting test  
./tests/test-token-rate-limits.sh # Token rate limiting test
```

Expected output: All 6 tests pass ✅

> **New**: Dedicated test scripts in `tests/` folder provide focused testing of individual MaaS features with automatic reset handling.

---

## 📋 Prerequisites

### Required Software
- **Docker Desktop** (Mac) or **Docker Engine** (Linux)
  - Mac: Docker Desktop 4.0+ with **8GB RAM** allocated
  - Linux: Docker Engine 20.10+
- **kubectl** 1.28+
- **kind** 0.20+
- **istioctl** 1.20+
- **helm** 3.12+

### Quick Install

Run the automated prerequisite installer:
```bash
../install-prerequisites.sh
```

Or install manually:

#### Mac (Homebrew)
```bash
brew install kubectl kind istioctl helm
```

#### Linux (Ubuntu/Debian)
```bash
# kubectl
curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
sudo install kubectl /usr/local/bin/kubectl

# kind
curl -Lo ./kind https://kind.sigs.k8s.io/dl/v0.20.0/kind-linux-amd64
chmod +x ./kind && sudo mv ./kind /usr/local/bin/kind

# istioctl
curl -L https://istio.io/downloadIstio | sh -
sudo mv istio-*/bin/istioctl /usr/local/bin/

# helm
curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
```

### System Requirements
- **RAM**: 8GB minimum, 16GB recommended
- **Disk**: 20GB free space
- **CPU**: 4 cores recommended

---

## 📦 What You Get

After setup, you'll have:

- ✅ **Gateway API + Istio** - HTTP/HTTPS routing on `localhost`
- ✅ **Kuadrant** - Authentication (Authorino) + Rate Limiting (Limitador)
- ✅ **KServe + Knative** - Model serving infrastructure
- ✅ **OpenAI-compatible API** - `/v1/chat/completions`, `/v1/models`
- ✅ **Test model** - llm-katan (Qwen2.5-0.5B) for development
- ✅ **3-tier access control** - Free/Premium/Enterprise rate limits
- ✅ **MaaS API** - Model discovery and management

**Endpoints:**

All endpoints require authentication. Get a token first:
```bash
# Get a free-tier token (valid for 1 hour)
TOKEN=$(kubectl create token free-user -n maas-api --audience=maas-default-gateway-sa --duration=1h)

# Or use premium/enterprise tokens
TOKEN=$(kubectl create token premium-user -n maas-api --audience=maas-default-gateway-sa --duration=1h)
TOKEN=$(kubectl create token enterprise-user -n maas-api --audience=maas-default-gateway-sa --duration=1h)
```

Then use the token in your requests:
```bash
# List all available models (single endpoint)
curl -H "Authorization: Bearer $TOKEN" http://localhost/v1/models

# Or list specific model endpoints
curl -H "Authorization: Bearer $TOKEN" http://localhost/llm/model-a/v1/models
curl -H "Authorization: Bearer $TOKEN" http://localhost/llm/model-b/v1/models

# Chat with model-a
curl -H "Authorization: Bearer $TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"model":"model-a","messages":[{"role":"user","content":"Hello!"}]}' \
     http://localhost/llm/model-a/v1/chat/completions

# Chat with model-b (requires premium/enterprise token)
curl -H "Authorization: Bearer $TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"model":"model-b","messages":[{"role":"user","content":"Hello!"}]}' \
     http://localhost/llm/model-b/v1/chat/completions

# Health check (no auth required)
curl http://localhost/maas-api/health
```

**Model Access Tiers:**
- `model-a`: Free tier (all users)
- `model-b`: Premium tier (premium/enterprise users only)

---

## 🔧 Manual Setup (Step-by-Step)

If you prefer manual setup or want to understand what the script does.

> **Note:** The automated script (`./install.sh`) does all of this for you.

### 1. Create Kind Cluster
```bash
kind create cluster --config deployment/overlays/kind/kind-config.yaml --name maas-local
```

### 2. Install Gateway API CRDs
```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.1/standard-install.yaml
```

### 3. Install Istio
```bash
istioctl install --set profile=minimal -y
```

### 4. Install Knative Serving
```bash
kubectl apply -f https://github.com/knative/serving/releases/download/knative-v1.10.1/serving-crds.yaml
kubectl apply -f https://github.com/knative/serving/releases/download/knative-v1.10.1/serving-core.yaml
```

### 5. Install Kuadrant
```bash
kubectl create -f https://github.com/Kuadrant/kuadrant-operator/releases/download/v1.0.0/kuadrant-operator.yaml
kubectl wait --for=condition=Available deployment/kuadrant-operator-controller-manager -n kuadrant-system --timeout=300s
```

### 6. Install KServe
```bash
# Use server-side apply to handle large CRD annotations (>256KB)
kubectl apply --server-side -f https://github.com/kserve/kserve/releases/download/v0.16.0/kserve.yaml
kubectl wait --for=condition=Available deployment/kserve-controller-manager -n kserve --timeout=300s
kubectl wait --for condition=established crd/inferenceservices.serving.kserve.io
kubectl wait --for condition=established crd/llminferenceservices.serving.kserve.io
```

### 7. Deploy MaaS Components
```bash
kubectl apply -k deployment/overlays/kind/
```

### 8. (Optional) Deploy Test Model
```bash
kubectl apply -k deployment/overlays/kind/test-models/llm-katan
kubectl wait --for=condition=ready pod -l app=llm-katan -n llm --timeout=600s
```

---

## 🧪 Testing

See [TESTING.md](TESTING.md) for comprehensive testing guide.

**Quick tests:**
```bash
cd deployment/scripts/kind
./test.sh        # 6 automated tests
./demo.sh        # Interactive menu-driven demos
```

For manual curl examples, see [TESTING.md](TESTING.md)

---

## 🏗️ KServe CRD Architecture

This deployment demonstrates **both** KServe Custom Resource Definition types:

### InferenceService vs LLMInferenceService

| Feature | InferenceService | LLMInferenceService |
|---------|------------------|---------------------|
| **API Version** | `serving.kserve.io/v1beta1` | `serving.kserve.io/v1alpha1` |
| **KServe Version** | v0.1.0+ (stable) | v0.16.0+ (alpha) |
| **Use Case** | General ML models | Large Language Models |
| **Deployment** | Standard KServe | LLM-optimized features |
| **Status** | Production-ready | Alpha (evolving) |

### Test Model Examples

**model-a** (InferenceService):
```yaml
apiVersion: serving.kserve.io/v1beta1
kind: InferenceService
metadata:
  name: model-a
spec:
  predictor:
    containers:
    - image: ghcr.io/vllm-project/semantic-router/llm-katan:latest
```

**model-b** (LLMInferenceService):
```yaml
apiVersion: serving.kserve.io/v1alpha1
kind: LLMInferenceService
metadata:
  name: model-b
spec:
  model:
    name: "model-b"
    modelFormat:
      name: "custom"
  predictor:
    containers:
    - image: ghcr.io/vllm-project/semantic-router/llm-katan:latest
```

### API Discovery

The `/v1/models` endpoint discovers **both** CRD types:
- Queries `InferenceService` resources first
- Adds `LLMInferenceService` resources if available
- Returns unified model list for OpenAI compatibility

This allows testing of broader Kubernetes compatibility while showcasing LLM-specific features.

---

## 🐛 Troubleshooting

### Gateway Not Accessible

**Check:**
```bash
kubectl get pods -n istio-system
kubectl get gateway maas-gateway -n istio-system
docker ps | grep maas-local
```

**Fix:**
- Ensure Docker Desktop is running
- Check port 80 is not in use: `lsof -ti:80`
- Restart cluster: `./uninstall.sh && ./install.sh`

### Authentication Failing (401)

**Check:**
```bash
kubectl get pods -n kuadrant-system
kubectl get authpolicy -n istio-system
kubectl get kuadrant -n kuadrant-system
```

**Fix:**
- Ensure Kuadrant instance exists: `kubectl get kuadrant -n kuadrant-system` (should return "kuadrant")
- Wait for Authorino: `kubectl wait --for=condition=ready pod -l app.kubernetes.io/name=authorino -n kuadrant-system`
- Check token audience: Must be `--audience=maas-default-gateway-sa`

### Model Not Loading

**Check:**
```bash
kubectl get pods -n llm
kubectl logs -n llm -l app=llm-katan --tail=100
```

**Fix:**
- Model may be downloading (~60 seconds)
- Increase Docker memory to 8GB+
- Check logs for errors

### Rate Limiting Not Working

**Check:**
```bash
kubectl get ratelimitpolicy -n istio-system
kubectl get limitador -n kuadrant-system
kubectl logs -n kuadrant-system -l app=limitador --tail=50
```

**Fix:**
- Ensure Kuadrant instance is created (see Authentication fix above)
- Check policy is enforced: `kubectl get ratelimitpolicy -n istio-system -o jsonpath='{.items[0].status.conditions[?(@.type=="Enforced")].status}'` (should be "True")

---

## 🧹 Cleanup

```bash
# Delete Kind cluster (removes everything)
kind delete cluster --name maas-local
```

---

## 📚 Documentation

- **[ARCHITECTURE.md](ARCHITECTURE.md)** - System architecture and request flow diagrams
- **[TESTING.md](TESTING.md)** - Comprehensive testing guide with examples
- **Project root**: [../../README.md](../../README.md) - Main MaaS documentation

---

## ⚠️ Known Limitations

- **GPU Models**: GPU passthrough limited on Docker Desktop (Mac). CPU models only.
- **Performance**: Slightly slower than native K8s due to Docker Desktop VM on Mac
- **Multi-node**: Single-node cluster only (sufficient for development)
- **LoadBalancer**: Uses NodePort with Kind port mappings instead of real LoadBalancer

---

## 🔄 Differences from OpenShift/Production

| Feature | OpenShift (Production) | Kind (Local) |
|---------|----------------------|--------------|
| Gateway Controller | OpenShift Gateway API | Istio |
| Routes | OpenShift Routes | HTTPRoutes (Gateway API) |
| Kuadrant | OLM Subscription | Direct YAML install |
| LoadBalancer | OpenShift Router | NodePort + port mappings |
| Auth | OpenShift OAuth | ServiceAccount tokens |
| Models | InferenceService | Plain Deployment (llm-katan) |

---

## 💡 Tips

- **Faster restarts**: Models are cached in Docker layers
- **Save resources**: Delete cluster when not in use
- **Monitor**: `kubectl top pods -A` to check resource usage
- **Logs**: `kubectl logs -n llm -l app=llm-katan -f` to watch model logs
- **Multiple terminals**: Run `./demo.sh` in one terminal, watch logs in another

---

## 🤝 Contributing

This local development setup makes it easy to:
- Test changes before deploying to OpenShift
- Develop new features offline
- Create integration tests
- Validate policies and configurations

See the main [Contributing Guide](../../../CONTRIBUTING.md) for details.
