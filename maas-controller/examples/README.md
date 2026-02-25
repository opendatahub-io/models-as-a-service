# MaaS examples

Example MaaS CRs for both **regular** and **premium** simulator LLMInferenceServices. Includes policies with and without subscriptions so you can see how access vs. quota behave.

## Contents

| File | Namespace | Description |
|------|-----------|-------------|
| **Models** | | |
| `maas-model.yaml` | llm | Regular simulator (LLMIS `facebook-opt-125m-simulated` in `llm`). |
| `maas-model-premium.yaml` | llm | Premium simulator (LLMIS `premium-simulated-simulated-premium` in `llm`). |
| **Regular (system:authenticated)** | | |
| `maas-auth-policy-regular.yaml` | models-as-a-service | Access for any authenticated user. |
| `maas-subscription-regular.yaml` | models-as-a-service | Token limit 100/min for `system:authenticated`. |
| **Premium (with subscription)** | | |
| `maas-auth-policy-premium.yaml` | models-as-a-service | Access for group `premium-user`. |
| `maas-subscription-premium.yaml` | models-as-a-service | Token limit 1000/min for `premium-user`. |
| **Premium (policy only, no subscription)** | | |
| `maas-auth-policy-premium-no-sub.yaml` | models-as-a-service | Access for group `premium-viewer` with **no** MaaSSubscription. These users pass auth but get no token quota and will receive 429 from the gateway default-deny policy. |

## Layout summary

| Model | Policy | Subscription | Group | Behavior |
|-------|--------|--------------|-------|----------|
| Regular | simulator-access | simulator-subscription | system:authenticated | Access + 100 tokens/min |
| Premium | premium-simulator-access | premium-simulator-subscription | premium-user | Access + 1000 tokens/min |
| Premium | premium-simulator-access-no-sub | *(none)* | premium-viewer | Access only → 429 (no quota) |

## Prerequisites

- MaaS controller installed (`maas-controller/scripts/install-maas-controller.sh` from repo root).
- Gateway-auth-policy disabled (`maas-controller/hack/disable-gateway-auth-policy.sh`).
- Both simulator LLMInferenceServices deployed (see install script below).

## Install

**Option 1 – Use the script** (deploys both simulators and applies all example CRs):

From the repository root:

```bash
maas-controller/scripts/install-examples.sh
```

**Option 2 – Manual**

1. Deploy both simulators (from the repo root):

   ```bash
   kustomize build docs/samples/models/simulator | kubectl apply -f -
   kustomize build docs/samples/models/simulator-premium | kubectl apply -f -
   ```

2. Apply the example MaaS CRs:

   ```bash
   kubectl apply -k maas-controller/examples/
   ```

## Customization

- **Groups**: Replace `premium-user` and `premium-viewer` in the premium policy/subscription files with groups from your identity provider. `system:authenticated` works as-is for the regular example.
- **Model refs**: If your LLMIS names or namespaces differ, update `spec.modelRef` in the model YAMLs and the corresponding `modelRefs` in policies and subscriptions.
