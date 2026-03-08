# Sample LLMInferenceService Models

This directory contains `LLMInferenceService`s for deploying sample models. Please refer to the [deployment guide](../../content/quickstart.md) for more details on how to test the MaaS Platform with these models.

> **TODO (ODH model controller):** Update the ODH model controller to remove or modify the existing webhook that validates tier annotations (`alpha.maas.opendatahub.io/tiers`). The webhook currently blocks HTTPRoutes when AuthPolicy is not enforced (e.g., Kuadrant not installed), requiring `security.opendatahub.io/enable-auth=false`. For MaaS-managed models, tier/access control is handled by MaaSAuthPolicy and MaaSSubscription rather than LLMInferenceService annotations. The webhook should not apply automation or block models that are managed by MaaS. See JIRA: [TBD]

## Available Models

- **simulator** - Simple simulator for testing
- **simulator-premium** - Premium simulator for testing tier-based access (configured via MaaSAuthPolicy)
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

Tier-based access is configured via MaaSAuthPolicy and MaaSSubscription (see [docs/samples/maas-system/](../maas-system/)), not via LLMInferenceService annotations.

### Verifying Deployment

After deploying both models:

```bash
# List all LLMInferenceServices
kubectl get llminferenceservices -n llm
```
