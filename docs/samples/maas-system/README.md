# MaaS System Samples

Bundled samples that deploy LLMInferenceService + MaaSModelRef + MaaSAuthPolicy + MaaSSubscription together so dependencies resolve correctly. LLMInferenceServices reference the existing [models/simulator](../models/simulator) and [models/simulator-premium](../models/simulator-premium) samples.

## Subscriptions

| Sample | Group | Model | Token Limit |
|--------|-------|-------|-------------|
| **free** | system:authenticated | facebook-opt-125m-simulated | 100/min |
| **premium** | premium-user | premium-simulated-simulated-premium | 1000/min |
| **facebook-opt-125m-cpu** | system:authenticated | facebook-opt-125m-cpu-single-node-no-scheduler-cpu | 100/min |
| **qwen3** | system:authenticated | qwen3-single-node-no-scheduler-nvidia-gpu | 100/min |
| **granite-3-1-8b-rhelai-modelcar** | system:authenticated | granite-3-1-8b-rhelai-modelcar-single-node-cpu (LLMIS in `llm`) | 10000/min |

## Usage

To deploy to default namespaces:

```bash
# Create model namespace (models-as-a-service namespace is auto-created by controller)
kubectl create namespace llm --dry-run=client -o yaml | kubectl apply -f -

# Deploy all (LLMIS + MaaS CRs) at once
kustomize build docs/samples/maas-system/ | kubectl apply -f -

# Or deploy a specific sample
kustomize build docs/samples/maas-system/facebook-opt-125m-cpu/ | kubectl apply -f -
kustomize build docs/samples/maas-system/qwen3/ | kubectl apply -f -
kustomize build docs/samples/maas-system/granite-3-1-8b-rhelai-modelcar/ | kubectl apply -f -

# Verify
kubectl get maasmodelref -n llm
kubectl get maasauthpolicy,maassubscription -n models-as-a-service
kubectl get llminferenceservice -n llm
```

To deploy MaaS CRs to another namespace:

```bash
# Create model namespace (custom subscription namespace is auto-created by controller)
kubectl create namespace llm --dry-run=client -o yaml | kubectl apply -f -

# Note: Configure controller with --maas-subscription-namespace=my-namespace to auto-create custom namespace
# Deploy all (LLMIS + MaaS CRs) at once
kustomize build docs/samples/maas-system | sed "s/namespace: models-as-a-service/namespace: my-namespace/g" | kubectl apply -f -

# Verify
kubectl get maasmodelref -n llm
kubectl get maasauthpolicy,maassubscription -n my-namespace
kubectl get llminferenceservice -n llm
```
