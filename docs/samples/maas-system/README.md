# MaaS System Samples

Bundled samples that deploy LLMInferenceService + MaaSModelRef + MaaSAuthPolicy + MaaSSubscription together so dependencies resolve correctly. LLMInferenceServices reference the existing [models/simulator](../models/simulator) and [models/simulator-premium](../models/simulator-premium) samples.

## Tiers

| Tier | Group | Model | Token Limit |
|------|-------|-------|-------------|
| **free** | system:authenticated | facebook-opt-125m-simulated | 100/min |
| **premium** | premium-user | premium-simulated-simulated-premium | 1000/min |

## Deploy

```bash
# Create llm namespace if needed
kubectl create namespace llm --dry-run=client -o yaml | kubectl apply -f -

# Deploy all (LLMIS + MaaS CRs) at once
kustomize build docs/samples/maas-system/ | kubectl apply -f -
```

## Verify

```bash
kubectl get maasmodel,maasauthpolicy,maassubscription -n opendatahub
kubectl get llminferenceservice -n llm
```
