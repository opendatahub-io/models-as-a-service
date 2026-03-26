# Model listing flow

This document describes how the **GET /v1/models** endpoint discovers and returns the list of available models.

The list is **based on MaaSModelRef** custom resources: the API considers MaaSModelRef objects cluster-wide (all namespaces), then filters by access.

## Overview

When a client calls **GET /v1/models** with an **Authorization** header, the MaaS API returns an OpenAI-compatible list of models.

Each entry includes an `id`, **`url`** (the model’s endpoint), a `ready` flag, and related metadata. The list is built from **MaaSModelRef** CRs. The API then validates access by probing each model’s endpoint with the same Authorization header; only models the client can access are included in the response.

!!! note "Model endpoints and routing"
    The returned value includes a **URL** per model; clients use that URL to call the model (e.g. for chat or completions).

    Currently each model is served on a **different endpoint**. **Body Based Routing** is being evaluated to provide a more unified OpenAI API feel (single endpoint with model selection in the request body).

## MaaSModelRef flow

When the [MaaS controller](https://github.com/opendatahub-io/models-as-a-service/tree/main/maas-controller) is installed and the API is configured with a MaaSModelRef lister, the flow is:

1. The MaaS API discovers **MaaSModelRef** custom resources **cluster-wide** (all namespaces) without calling the Kubernetes API on every request.

2. For each MaaSModelRef, it reads **id** (`metadata.name`), **url** (`status.endpoint`), **ready** (`status.phase == "Ready"`), and related metadata. The controller has populated `status.endpoint` and `status.phase` from the underlying LLMInferenceService (for llmisvc) or HTTPRoute/Gateway.

3. **Access validation**: The API probes each model’s `/v1/models` endpoint with the **exact Authorization header** the client sent (passed through as-is). Only models that return **2xx**, **3xx** or **405** are included in the response. This ensures the list only shows models the client is authorized to use.

4. The filtered list is returned to the client.

```mermaid
sequenceDiagram
    participant Client
    participant MaaS API
    participant K8s as Kubernetes API
    participant Model as Model endpoint

    Client->>MaaS API: GET /v1/models (Authorization header)
    MaaS API->>K8s: List MaaSModelRef CRs
    K8s-->>MaaS API: MaaSModelRef list
    loop For each model
        MaaS API->>Model: GET endpoint with same Authorization header
        Model-->>MaaS API: include or exclude by response
    end
    MaaS API->>MaaS API: Map to OpenAI-style entries
    MaaS API-->>Client: JSON data array of models
```

### Benefits

- **List is based on MaaSModelRefs**: Only models registered as a MaaSModelRef appear. The controller reconciles each MaaSModelRef and sets its endpoint and phase; access and quotas are controlled by MaaSAuthPolicy and MaaSSubscription.

- **Access-filtered**: The API probes each model with the client’s Authorization header (passed through as-is), so the returned list only includes models the client can actually use.

- **Consistent with gateway**: The same model names and routes are used for inference; the list matches what the gateway will accept for that client.

If the API is not configured with a MaaSModelRef lister, or if listing fails (e.g. CRD not installed, no RBAC, or server error), the API returns an empty list or an error.

## Subscription Filtering and Aggregation

The `/v1/models` endpoint automatically filters models based on your authentication method and optional headers.

### Authentication-Based Behavior

#### API Key Authentication (Bearer sk-oai-*)
When using an API key, the subscription is automatically determined from the key:
- Returns **only** models from the subscription bound to the API key at mint time

```bash
# API key bound to "premium-subscription"
curl -H "Authorization: Bearer sk-oai-abc123..." \
     https://maas.example.com/maas-api/v1/models

# Returns models from "premium-subscription" only
```

#### User Token Authentication (OpenShift/OIDC tokens)
When using a user token, you have flexible options:

**Default (no X-MaaS-Subscription header)**:
- Returns **all** models from all subscriptions you have access to
- Models are deduplicated and subscription metadata is attached

```bash
# User with access to "basic" and "premium" subscriptions
curl -H "Authorization: Bearer $(oc whoami -t)" \
     https://maas.example.com/maas-api/v1/models

# Returns models from both subscriptions with subscription metadata
```

**With X-MaaS-Subscription header** (optional):
- Returns only models from the specified subscription
- Behaves like an API key request - allows you to scope your query to a specific subscription

```bash
# Filter to only "premium" subscription models
curl -H "Authorization: Bearer $(oc whoami -t)" \
     -H "X-MaaS-Subscription: premium-subscription" \
     https://maas.example.com/maas-api/v1/models

# Returns only "premium-subscription" models
```

!!! tip "User token filtering"
    The `X-MaaS-Subscription` header allows user token requests to filter results to a specific subscription. This is useful when you have access to many subscriptions but only want to see models from one.

### Subscription Metadata

All models in the response include a `subscriptions` array with metadata for each subscription providing access to that model:

```json
{
  "object": "list",
  "data": [
    {
      "id": "llama-2-7b-chat",
      "created": 1672531200,
      "object": "model",
      "owned_by": "model-namespace",
      "url": "https://maas.example.com/llm/llama-2-7b-chat",
      "ready": true,
      "subscriptions": [
        {
          "name": "basic-subscription",
          "displayName": "Basic Tier",
          "description": "Basic subscription with standard rate limits"
        },
        {
          "name": "premium-subscription",
          "displayName": "Premium Tier",
          "description": "Premium subscription with higher rate limits"
        }
      ]
    }
  ]
}
```

### Deduplication Behavior

Models are deduplicated by `(id, url, ownedBy)` key:

- **Same id + same URL + same MaaSModelRef (ownedBy)**: Single entry with subscriptions aggregated into the `subscriptions` array
- **Different id, URL, or MaaSModelRef**: Separate entries

**User token authentication** (multiple subscriptions):
- Model `gpt-3.5` from MaaSModelRef `namespace-a/model-a` at URL `https://example.com/gpt-3.5` is accessible via subscriptions A and B
  - Result: One entry with `subscriptions: [{name: "A"}, {name: "B"}]`
- Model `gpt-3.5` from MaaSModelRef `namespace-b/model-b` at the same URL is only in subscription B
  - Result: Separate entry with `subscriptions: [{name: "B"}]` (different MaaSModelRef)
- Model `gpt-3.5` at URL `https://example.com/gpt-3.5-premium` from `namespace-a/model-a` is only in subscription B
  - Result: Separate entry with `subscriptions: [{name: "B"}]` (different URL)

**API key authentication** (single subscription):
- Deduplication handles edge cases where multiple MaaSModelRef resources point to the same model endpoint
- Each unique MaaSModelRef resource appears as a separate entry

!!! tip "Subscription metadata fields"
    The `displayName` and `description` fields are read from the MaaSSubscription CRD's `spec.displayName` and `spec.description` fields. If these fields are not set in the CRD, they will be empty strings in the response.

## Registering models

To have models appear via the **MaaSModelRef** flow:

1. Install the **MaaS controller** (CRDs, controller deployment, and optionally the default-deny policy). See [maas-controller README](https://github.com/opendatahub-io/models-as-a-service/tree/main/maas-controller).

2. Ensure the underlying **LLMInferenceService** exists and (if applicable) has an HTTPRoute created by KServe.

3. Create a **MaaSModelRef** for each model you want to expose, referencing the LLMIS:

        apiVersion: maas.opendatahub.io/v1alpha1
        kind: MaaSModelRef
        metadata:
          name: my-model-name   # This becomes the model "id" in GET /v1/models
          namespace: llm        # Same namespace as the LLMInferenceService
        spec:
          modelRef:
            kind: LLMInferenceService
            name: my-llm-isvc-name

4. The controller reconciles the MaaSModelRef and sets `status.endpoint` and `status.phase`. The MaaS API will then include this model in GET /v1/models when it lists MaaSModelRef CRs.

You can use the [maas-system samples](https://github.com/opendatahub-io/models-as-a-service/tree/main/docs/samples/maas-system) as a template; the install script deploys LLMInferenceService + MaaSModelRef + MaaSAuthPolicy + MaaSSubscription together so dependencies resolve correctly.

---

## Related documentation

- [MaaS Controller README](https://github.com/opendatahub-io/models-as-a-service/tree/main/maas-controller) — install and MaaSModelRef/MaaSAuthPolicy/MaaSSubscription
- [Model setup](./model-setup.md) — configuring LLMInferenceServices (gateway reference) as backends for MaaSModelRef
- [Architecture](../architecture.md) — overall MaaS architecture
