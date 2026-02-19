# MaaSModel kinds (future)

The MaaS API lists models from **MaaSModel** CRs only. Each MaaSModel defines a **backend reference** (`spec.modelRef`) that identifies the type and location of the model endpoint—similar in spirit to how [Gateway API’s BackendRef](https://gateway-api.sigs.k8s.io/reference/spec/#backendref) defines how a Route forwards requests to a Kubernetes resource (group, kind, name, namespace).

This document describes the current **modelRef** semantics and how new kinds can be supported in the future.

## ModelRef (backend reference)

MaaSModel’s `spec.modelRef` identifies the **referent** (the backend that serves the model):

| Field       | Description |
| ----------- | ----------- |
| **kind**    | The type of backend. Determines which controller reconciles this MaaSModel and how the endpoint is resolved. Example: `llmisvc`, `ExternalModel`. |
| **name**    | Name of the referent resource (e.g. LLMInferenceService name, or external model identifier). |
| **namespace** | *(Optional)* Namespace of the referent. If empty, the MaaSModel’s namespace is used. |

The controller that reconciles MaaSModel uses **kind** to decide how to resolve the backend and populate `status.endpoint` and `status.phase`. Cross-namespace references are supported by specifying `modelRef.namespace`.

## Current behavior

- **Supported kind today:** `llmisvc` (default). The MaaS controller reconciles MaaSModels whose **modelRef** points to an LLMInferenceService (by name and optional namespace). It sets `status.endpoint` from the LLMInferenceService status and `status.phase` from its readiness.
- **API behavior:** The API reads MaaSModels from the informer cache, maps each to an API model (`id`, `url`, `ready`, `kind`, etc.), then **validates access** by probing each model’s `/v1/models` endpoint with the request’s **Authorization header** (passed through as-is). Only models that return 2xx or 405 are included.
- **Kind on the wire:** Each model in the GET /v1/models response can carry a `kind` field (e.g. `llmisvc`) from `spec.modelRef.kind` for clients or tooling.

## Adding a new kind in the future

To support a new backend type (a new **kind** in `spec.modelRef`):

1. **MaaSModel CRD and controller**
   - Add a new allowed value for `spec.modelRef.kind` (e.g. `mybackend`).
   - In the **maas-controller**, extend the reconciler so that when **kind** is the new value it:
     - Resolves the referent (e.g. custom resource or external URL) using **name** and optional **namespace**.
     - Sets `status.endpoint` and `status.phase` (and any other status the API or UI need).

2. **MaaS API**
   - **Listing:** No change required. The API lists all MaaSModels and uses `status.endpoint` and `status.phase`; it does not branch on **kind** for listing.
   - **Access validation:** The API probes `status.endpoint` + `/v1/models` with the request’s Authorization header. If a new kind uses a different path or protocol:
     - **Option A (preferred):** The backend exposes the same path the API expects (e.g. OpenAI-compatible `/v1/models`), so no API change.
     - **Option B:** Extend the API’s access-validation logic to branch on **kind** and use a kind-specific probe (different URL path or client), while keeping the same contract: include a model only if the probe with the user’s token succeeds.

3. **Enrichment (optional)**
   - Extra metadata (e.g. display name) can be set by the controller in status or annotations and mapped into the model response. For a new kind, add a small branch in the MaaSModel → API model conversion if needed.

4. **RBAC**
   - If the new kind’s reconciler or the API needs to read another resource, add the corresponding **list/watch** (and optionally **get**) permissions to the maas-api ClusterRole and/or the controller’s RBAC.

## Summary

- **modelRef** is the backend reference (kind, name, optional namespace), analogous to [Gateway API BackendRef](https://gateway-api.sigs.k8s.io/reference/spec/#backendref).
- **Listing:** Always from MaaSModel cache; no kind-specific listing logic.
- **Access validation:** Same probe (GET endpoint with the request’s Authorization header as-is) for all kinds unless kind-specific probes are added later.
- **New kinds:** Implement in the controller (resolve referent, set status.endpoint and status.phase); extend the API only if the new kind cannot use the same probe path or needs different enrichment.
