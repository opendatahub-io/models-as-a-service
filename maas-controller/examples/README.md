# MaaS examples

This directory contains example MaaS CRs that match a typical deployment using the **simulator** LLMInferenceService from the docs samples.

## Contents

| File | Description |
|------|-------------|
| `maas-model.yaml` | Registers the simulator model (LLMIS `facebook-opt-125m-simulated` in namespace `llm`) with MaaS. |
| `maas-auth-policy.yaml` | Grants access to that model for users in group `free-user`. |
| `maas-subscription.yaml` | Applies token rate limits (100 tokens/min) for that model for owners in group `free-user`. |

## Prerequisites

- MaaS controller installed (`./scripts/install-maas-controller.sh`).
- Gateway-auth-policy disabled (`./hack/disable-gateway-auth-policy.sh`).
- Simulator LLMInferenceService deployed (see install script below).

## Install

**Option 1 – Use the script** (applies examples + deploys the simulator LLMInferenceService from `docs/samples/models`):

```bash
./scripts/install-examples.sh
```

**Option 2 – Manual**

1. Deploy the simulator LLMInferenceService (from the repo root, e.g. `models-as-a-service`):

   ```bash
   kustomize build docs/samples/models/simulator | kubectl apply -f -
   ```

2. Apply the example MaaS CRs:

   ```bash
   kubectl apply -k examples/
   ```

## Customization

- **Model name/namespace**: If your LLMInferenceService has a different name or namespace, edit `maas-model.yaml` `spec.modelRef` and keep `modelRefs` in the auth policy and subscription in sync.
- **Groups**: Adjust `subjects.groups` in `maas-auth-policy.yaml` and `owner.groups` in `maas-subscription.yaml` to match your cluster groups.
