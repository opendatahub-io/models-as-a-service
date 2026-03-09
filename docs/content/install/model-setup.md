# Deploy Sample Models

## What We Deploy

Our sample models are packaged as Kustomize overlays that deploy:

| Resource | Purpose |
|----------|---------|
| **LLMInferenceService** | The LLM workload — the actual inference service (simulator, vLLM, etc.) |
| **MaaSModelRef** | Gives the MaaS system a reference to the model so it appears in the model catalog |
| **MaaSAuthPolicy** | Grants access to the model for specified groups (who can use it) |
| **MaaSSubscription** | Defines rate limits (token quotas) for specific groups |

For more detail on each resource, see [Subscription Overview](../configuration-and-management/subscription-overview.md).

!!! tip "Create llm namespace (optional)"
    Models deploy to the `llm` namespace. If it does not exist, create it first (idempotent—safe to run even if it already exists):

    ```bash
    kubectl create namespace llm --dry-run=client -o yaml | kubectl apply -f -
    ```

## Deploy Sample Models

### Simulator Model (CPU)

A lightweight mock service for testing that generates responses without running an actual language model. This sample deploys the full MaaS stack:

- **LLMInferenceService** — Simulator workload
- **MaaSModelRef** — Registers the model with MaaS
- **MaaSAuthPolicy** — Access for `system:authenticated` (all authenticated users)
- **MaaSSubscription** — Rate limit of 100 tokens/min for `system:authenticated`

```bash
PROJECT_DIR=$(git rev-parse --show-toplevel)
kustomize build ${PROJECT_DIR}/docs/samples/maas-system/free/ | kubectl apply -f -
```

### Simulator Model — Premium (CPU)

Same simulator workload with premium access and higher rate limits:

- **LLMInferenceService** — Simulator workload
- **MaaSModelRef** — Registers the model with MaaS
- **MaaSAuthPolicy** — Access for `premium-user` group only
- **MaaSSubscription** — Rate limit of 1000 tokens/min for `premium-user`

```bash
PROJECT_DIR=$(git rev-parse --show-toplevel)
kustomize build ${PROJECT_DIR}/docs/samples/maas-system/premium/ | kubectl apply -f -
```

### Facebook OPT-125M Model (CPU)

An inference deployment that loads and runs a 125M parameter model without the need for a GPU. This sample deploys the full MaaS stack:

- **LLMInferenceService** — vLLM CPU workload
- **MaaSModelRef** — Registers the model with MaaS
- **MaaSAuthPolicy** — Access for `system:authenticated` (all authenticated users)
- **MaaSSubscription** — Rate limit of 100 tokens/min for `system:authenticated`

```bash
PROJECT_DIR=$(git rev-parse --show-toplevel)
kustomize build ${PROJECT_DIR}/docs/samples/maas-system/facebook-opt-125m-cpu/ | kubectl apply -f -
```

### Qwen3 Model (GPU Required)

⚠️ This model requires GPU nodes with `nvidia.com/gpu` resources available in your cluster. This sample deploys the full MaaS stack:

- **LLMInferenceService** — vLLM GPU workload
- **MaaSModelRef** — Registers the model with MaaS
- **MaaSAuthPolicy** — Access for `system:authenticated` (all authenticated users)
- **MaaSSubscription** — Rate limit of 100 tokens/min for `system:authenticated`

```bash
PROJECT_DIR=$(git rev-parse --show-toplevel)
kustomize build ${PROJECT_DIR}/docs/samples/maas-system/qwen3/ | kubectl apply -f -
```

### Verify Model Deployment

```bash
# Check LLMInferenceService status
kubectl get llminferenceservices -n llm

# Check pods
kubectl get pods -n llm
```

**Validate MaaSModelRef status** — The MaaS controller populates `status.endpoint` and `status.phase` on each MaaSModelRef from the LLMInferenceService. The MaaSModelRef `status.endpoint` should match the URL exposed by the LLMInferenceService (via the gateway). Verify:

```bash
# Check MaaSModelRef status (use opendatahub for ODH, redhat-ods-applications for RHOAI)
kubectl get maasmodelref -n opendatahub -o wide

# Verify status.endpoint is populated and phase is Ready
kubectl get maasmodelref -n opendatahub -o jsonpath='{range .items[*]}{.metadata.name}: phase={.status.phase} endpoint={.status.endpoint}{"\n"}{end}'

# Compare with LLMInferenceService — status.endpoint should match the URL from LLMIS status.addresses or status.url
kubectl get llminferenceservice -n llm -o yaml | grep "url:"
```

The `status.endpoint` on MaaSModelRef is derived from the LLMInferenceService (gateway-external URL, or `status.addresses`, or `status.url`). Both should show the same URL. You can also confirm via the [Validation](validation.md) guide—the `/v1/models` API returns the same URL from MaaSModelRef `status.endpoint`. If phase is not `Ready` or endpoint is empty, the MaaS controller may still be reconciling—wait a minute and recheck.

### Update Existing Models (Optional)

To expose an existing model through MaaS, you must:

1. Ensure the `LLMInferenceService` uses the `maas-default-gateway` gateway
2. Create a **MaaSModelRef** that references the LLMInferenceService
3. Create **MaaSAuthPolicy** and **MaaSSubscription** to define access and rate limits

See [Subscription Configuration](../configuration-and-management/subscription-configuration.md) for step-by-step instructions.

**Gateway reference** — If the model does not yet use the MaaS gateway:

```bash
kubectl patch llminferenceservice my-production-model -n llm --type='json' -p='[
  {
    "op": "add",
    "path": "/spec/gateway/refs/-",
    "value": {
      "name": "maas-default-gateway",
      "namespace": "openshift-ingress"
    }
  }
]'
```

```yaml
apiVersion: serving.kserve.io/v1alpha1
kind: LLMInferenceService
metadata:
  name: my-production-model
spec:
  gateway:
    refs:
      - name: maas-default-gateway
        namespace: openshift-ingress
```

## Next Steps

Proceed to [Validation](validation.md) to test and verify your deployment.
