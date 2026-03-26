# MaaSModelRef Kinds

The MaaS API lists models from **MaaSModelRef** CRs only. Each MaaSModelRef defines a **backend reference** (`spec.modelRef`) that identifies the type and location of the model endpoint—similar in spirit to how [Gateway API's BackendRef](https://gateway-api.sigs.k8s.io/reference/spec/#backendref) defines how a Route forwards requests to a Kubernetes resource (group, kind, name, namespace).

This document describes the current **modelRef** semantics and how new kinds can be supported in the future.

## ModelRef (backend reference)

MaaSModelRef's `spec.modelRef` identifies the **referent** (the backend that serves the model):

| Field       | Description |
| ----------- | ----------- |
| **kind**    | The type of backend. Determines which controller reconciles this MaaSModelRef and how the endpoint is resolved. Valid values: `LLMInferenceService`, `ExternalModel`. The alias `llmisvc` is also accepted for backwards compatibility. |
| **name**    | Name of the referent resource (e.g. LLMInferenceService name, or external model identifier). |

The controller that reconciles MaaSModelRef uses **kind** to decide how to resolve the backend and populate `status.endpoint` and `status.phase`. The referent must be in the same namespace as the MaaSModelRef.

## Endpoint override

MaaSModelRef supports an optional `spec.endpointOverride` field. When set, the controller uses this value for `status.endpoint` instead of the auto-discovered endpoint from the backend (LLMInferenceService status, Gateway, or HTTPRoute hostnames).

This is useful when:
- The controller picks the wrong gateway or hostname for the model endpoint.
- Your environment requires a specific URL that differs from what the backend reports.
- You need to point the model at a custom proxy or load balancer.

Example:

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: my-model
  namespace: opendatahub
spec:
  modelRef:
    kind: LLMInferenceService
    name: my-model
  endpointOverride: "https://correct-hostname.example.com/my-model"
```

The controller still validates the backend (HTTPRoute exists, LLMInferenceService is ready, etc.) — the override only affects the final endpoint URL written to `status.endpoint`. When the field is empty or omitted, the controller uses its normal discovery logic.

## Supported Kinds

### LLMInferenceService

The `LLMInferenceService` kind (also accepts the alias `llmisvc` for backwards compatibility) references models deployed on the cluster via the LLMInferenceService CRD. The controller:
- Sets `status.endpoint` from the LLMInferenceService status
- Sets `status.phase` from LLMInferenceService readiness

### ExternalModel

The `ExternalModel` kind references external AI/ML providers (e.g., OpenAI, Anthropic, Azure OpenAI). When using this kind:
1. Create an [ExternalModel](../reference/crds/external-model.md) CR with provider, endpoint, and credential reference
2. Create a MaaSModelRef that references the ExternalModel by name

The controller:
- Fetches the ExternalModel CR from the same namespace
- Validates the user-supplied HTTPRoute references the correct gateway
- Derives `status.endpoint` from HTTPRoute hostnames or gateway addresses
- Sets `status.phase` based on HTTPRoute acceptance by the gateway

## API Behavior

- The API reads MaaSModelRefs from the informer cache, maps each to an API model (`id`, `url`, `ready`, `kind`, etc.)
- **Access validation**: Probes each model's `/v1/models` endpoint with the request's Authorization header. Only models that return 2xx or 405 are included.
- **Kind on the wire**: Each model in the GET /v1/models response carries a `kind` field from `spec.modelRef.kind`

## Adding a new kind in the future

To support a new backend type (a new **kind** in `spec.modelRef`):

1. **MaaSModelRef CRD and controller**
   - Add a new allowed value for `spec.modelRef.kind` (e.g. `mybackend`).
   - In the **maas-controller**, extend the reconciler so that when **kind** is the new value it:
     - Resolves the referent (e.g. custom resource or external URL) using **name** and optional **namespace**.
     - Sets `status.endpoint` and `status.phase` (and any other status the API or UI need).

2. **MaaS API**
   - **Listing:** No change required. The API lists all MaaSModelRefs and uses `status.endpoint` and `status.phase`; it does not branch on **kind** for listing.
   - **Access validation:** The API probes `status.endpoint` + `/v1/models` with the request's Authorization header. If a new kind uses a different path or protocol:
     - **Option A (preferred):** The backend exposes the same path the API expects (e.g. OpenAI-compatible `/v1/models`), so no API change.
     - **Option B:** Extend the API's access-validation logic to branch on **kind** and use a kind-specific probe (different URL path or client), while keeping the same contract: include a model only if the probe with the user's token succeeds.

3. **Enrichment (optional)**
   - Extra metadata (e.g. display name) can be set by the controller in status or annotations and mapped into the model response. For a new kind, add a small branch in the MaaSModelRef → API model conversion if needed.

4. **RBAC**
   - If the new kind’s reconciler or the API needs to read another resource, add the corresponding **list/watch** (and optionally **get**) permissions to the maas-api ClusterRole and/or the controller’s RBAC.

## Summary

- **modelRef** is the backend reference (kind, name, optional namespace), analogous to [Gateway API BackendRef](https://gateway-api.sigs.k8s.io/reference/spec/#backendref).
- **Listing:** Always from MaaSModelRef cache; no kind-specific listing logic.
- **Access validation:** Same probe (GET endpoint with the request's Authorization header as-is) for all kinds unless kind-specific probes are added later.
- **New kinds:** Implement in the controller (resolve referent, set status.endpoint and status.phase); extend the API only if the new kind cannot use the same probe path or needs different enrichment.
