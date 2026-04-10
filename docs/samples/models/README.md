# Sample LLMInferenceService Models

This directory contains `LLMInferenceService`s for deploying sample models. Please refer to the [deployment guide](../../content/quickstart.md) for more details on how to test the MaaS Platform with these models.

## Available Models

- **simulator** - Simple simulator for testing
- **simulator-premium** - Premium simulator for testing access policies (configured via MaaSAuthPolicy)
- **facebook-opt-125m-cpu** - Facebook OPT 125M model (CPU-based)
- **qwen3** - Qwen3 model (GPU-based with autoscaling)
- **ibm-granite-2b-gpu** - IBM Granite 2B Instruct model (GPU-based, supports instructions)
- **granite-3-1-8b-rhelai-modelcar** - Granite 3.1 8B Instruct via Red Hat model car OCI + `vllm-cpu-rhel9` (CPU; see comments in `model.yaml`)

## Deployment

### Basic Deployment

Create the `llm` namespace where models are deployed (if it doesn't already exist):

```bash
kubectl create namespace llm
```

Deploy any model using:

```bash
MODEL_NAME=simulator # or simulator-premium, facebook-opt-125m-cpu, qwen3, ibm-granite-2b-gpu, granite-3-1-8b-rhelai-modelcar
kustomize build docs/samples/models/$MODEL_NAME | kubectl apply -f -
```

### Deploying Multiple Models

To deploy both simulator models:

1. **Deploy the standard simulator**:
   ```bash
   kustomize build docs/samples/models/simulator | kubectl apply -f -
   ```

2. **Deploy the premium simulator**:
   ```bash
   kustomize build docs/samples/models/simulator-premium | kubectl apply -f -
   ```

### Distinguishing Between Models

The two simulator models can be distinguished by:

- **Model Name**:
  - Standard: `facebook-opt-125m-simulated` (from kustomization namePrefix)
  - Premium: `premium-simulated-simulated-premium` (from kustomization namePrefix + model name)

- **LLMInferenceService Name**:
  - Standard: `facebook-opt-125m-simulated`
  - Premium: `premium-simulated-simulated-premium`

Subscription-based access is configured via MaaSAuthPolicy and MaaSSubscription (see [docs/samples/maas-system/](../maas-system/)), not via LLMInferenceService annotations.

### Verifying Deployment

After deploying both models:

```bash
# List all LLMInferenceServices
kubectl get llminferenceservices -n llm
```
