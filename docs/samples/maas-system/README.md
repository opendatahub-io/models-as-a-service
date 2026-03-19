# MaaS System Samples

Bundled samples that deploy LLMInferenceService + MaaSModelRef + MaaSAuthPolicy + MaaSSubscription together so dependencies resolve correctly. LLMInferenceServices reference the existing [models/simulator](../models/simulator) and [models/simulator-premium](../models/simulator-premium) samples.

## Tiers

| Tier | Group | Model | Token Limit |
|------|-------|-------|-------------|
| **free** | system:authenticated | facebook-opt-125m-simulated | 100/min |
| **premium** | premium-user | premium-simulated-simulated-premium | 1000/min |

## Usage

To deploy to default namespaces:

```bash
# Create model namespace (models-as-a-service namespace is auto-created by controller)
kubectl create namespace llm --dry-run=client -o yaml | kubectl apply -f -

# Deploy all (LLMIS + MaaS CRs) at once
kustomize build docs/samples/maas-system | kubectl apply -f -

# Verify
kubectl get maasmodelref -n opendatahub
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
kubectl get maasmodelref -n opendatahub
kubectl get maasauthpolicy,maassubscription -n my-namespace
kubectl get llminferenceservice -n llm
```
