# MaaSModel kinds (future)

The MaaS API lists models from **MaaSModel** CRs only. Each MaaSModel has a **kind** (`spec.modelRef.kind`) that identifies the type of backend (e.g. `llmisvc` for LLMInferenceService). This document describes the current behavior and how new kinds can be supported in the future.

## Current behavior

- **Supported kind today:** `llmisvc` (default). The MaaS controller reconciles MaaSModels that reference an LLMInferenceService and sets `status.endpoint` and `status.phase`.
- **API behavior:** The API reads MaaSModels from the informer cache, maps each to an API model (`id`, `url`, `ready`, `kind`, etc.), then **validates access** by probing each model’s `/v1/models` endpoint with the request’s **Authorization header** (passed through as-is). Only models that return 2xx or 405 are included.
- **Kind on the wire:** Each model in the GET /v1/models response can carry a `kind` field (e.g. `llmisvc`) from `spec.modelRef.kind` for future use by clients or tooling.

## Adding a new kind in the future

To support a new backend type (e.g. a different CRD or external service):

1. **MaaSModel CRD and controller**
   - Extend MaaSModel `spec.modelRef.kind` (or equivalent) with a new value (e.g. `mybackend`).
   - In the **maas-controller**, add a reconciler (or extend the existing one) that:
     - Watches MaaSModels with `kind: mybackend`.
     - Resolves the backend (e.g. custom resource, external URL).
     - Sets `status.endpoint` and `status.phase` (and any other status the API or UI need).

2. **MaaS API**
   - **Listing:** No change required. The API already lists all MaaSModels from the cache and uses `status.endpoint` and `status.phase`. It does not branch on `kind` for listing.
   - **Access validation:** The API probes `status.endpoint` + `/v1/models` (or equivalent path) with the user’s token. If a new kind uses a different path or protocol:
     - **Option A (preferred):** The backend still exposes an OpenAI-compatible `/v1/models` (or the same path the API expects), so no API change.
     - **Option B:** Extend the API’s access-validation logic (e.g. in `internal/models/discovery.go`) to branch on `model.Kind` and call a kind-specific probe (different URL path, header, or client). Keep the same contract: “include model only if the probe with the user’s token succeeds.”

3. **Enrichment (optional)**
   - Today, extra metadata (e.g. display name, description) comes from the MaaSModel or the controller. For a new kind, the controller can set status or annotations that the API already maps into the model response.
   - If a kind needs API-side enrichment from another source (e.g. custom CR), add a small branch in the code that converts MaaSModel → API model (e.g. in `maasmodel.maasModelToModel` or a dedicated mapper per kind).

4. **RBAC**
   - If the new kind’s reconciler or the API needs to read another resource (e.g. a custom CR), add the corresponding **list/watch** (and optionally get) permissions to the maas-api ClusterRole and/or the controller’s RBAC.

## Summary

- **Listing:** Always from MaaSModel cache; no kind-specific listing logic.
- **Access validation:** Same probe (GET endpoint with the request’s Authorization header as-is) for all kinds unless we add optional kind-specific probes later.
- **New kinds:** Implement in the controller (status.endpoint + status.phase); extend the API only if the new kind cannot expose the same probe path or needs different enrichment.
