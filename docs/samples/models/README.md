# Sample LLMInferenceService Models

This directory contains `LLMInferenceService`s for deploying sample models. Please refer to the [deployment guide](../../content/quickstart.md) for more details on how to test the MaaS Platform with these models.

## Available Models

- **simulator** - Simple simulator for testing (accessible to all tiers)
- **simulator-premium** - Premium tier simulator for testing tier-based access control (premium tier only)
- **facebook-opt-125m-cpu** - Facebook OPT 125M model (CPU-based)
- **qwen3** - Qwen3 model (GPU-based with autoscaling)
- **ibm-granite-2b-gpu** - IBM Granite 2B Instruct model (GPU-based, supports instructions)

## Deployment

### Basic Deployment

Deploy any model using:

```bash
MODEL_NAME=simulator # or simulator-premium, facebook-opt-125m-cpu, qwen3, or ibm-granite-2b-gpu
kustomize build docs/samples/models/$MODEL_NAME | kubectl apply -f -
```

### Deploying Multiple Models with Tier Differentiation

To demonstrate tier-based access control, you can deploy both simulator models:

1. **Deploy the standard simulator** (accessible to all tiers):
   ```bash
   kustomize build docs/samples/models/simulator | kubectl apply -f -
   ```

2. **Deploy the premium simulator** (premium tier only):
   ```bash
   kustomize build docs/samples/models/simulator-premium | kubectl apply -f -
   ```

### Distinguishing Between Models

The two simulator models can be distinguished by:

- **Model Name**: 
  - Standard: `facebook-opt-125m-simulated` (from kustomization namePrefix)
  - Premium: `premium-simulated-simulated-premium` (from kustomization namePrefix + model name)

- **Tier Access**:
  - Standard: `alpha.maas.opendatahub.io/tiers: '[]'` (all tiers can access)
  - Premium: `alpha.maas.opendatahub.io/tiers: '["premium"]'` (premium tier only)

- **LLMInferenceService Name**:
  - Standard: `facebook-opt-125m-simulated` 
  - Premium: `premium-simulated-simulated-premium`

When listing models via the MaaS API (`/v1/models`), users will only see models they have access to based on their tier. Premium tier users will see both models, while free tier users will only see the standard simulator.

### Verifying Tier-Based Access

After deploying both models, verify tier-based access:

```bash
# List all LLMInferenceServices
kubectl get llminferenceservices -n llm

# Check tier annotations
kubectl get llminferenceservice facebook-opt-125m-simulated -n llm -o jsonpath='{.metadata.annotations.alpha\.maas\.opendatahub\.io/tiers}'
kubectl get llminferenceservice premium-simulated-simulated-premium -n llm -o jsonpath='{.metadata.annotations.alpha\.maas\.opendatahub\.io/tiers}'
```

The standard simulator should show `[]` (all tiers), while the premium simulator should show `["premium"]` (premium tier only).
