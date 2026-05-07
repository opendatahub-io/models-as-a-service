# MaaSModelRef

Identifies an AI/ML model for the MaaS platform. The backend may be on-cluster (`LLMInferenceService`) or external (`ExternalModel` for providers like OpenAI, Anthropic, Azure OpenAI that run outside the cluster). Create MaaSModelRef in the **same namespace** as the backend resource.

---

## Spec

### MaaSModelRefSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| modelRef | ModelReference | Yes | Reference to the model backend (kind and name) |
| endpointOverride | string | No | Optional override for the endpoint URL. See [Endpoint Override](#endpoint-override) below. |

### ModelReference

`spec.modelRef` identifies the backend resource that serves the model—similar to [Gateway API BackendRef](https://gateway-api.sigs.k8s.io/reference/spec/#backendref):

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| kind | string | Yes | Backend type. One of: `LLMInferenceService`, `ExternalModel`. See [Supported Kinds](#supported-kinds) below. |
| name | string | Yes | Name of the backend resource. Must be in the same namespace as the MaaSModelRef. Max length: 253 characters. |

---

## Supported Kinds

### LLMInferenceService

References models deployed on the cluster via the LLMInferenceService CRD (e.g., vLLM, TGI via KServe).

The controller:
- Sets `status.endpoint` from the LLMInferenceService status
- Sets `status.phase` based on LLMInferenceService readiness

**Example:**
```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: granite-7b
  namespace: models
spec:
  modelRef:
    kind: LLMInferenceService
    name: granite-7b-instruct
```

For complete setup instructions, see [Model Setup](../../configuration-and-management/model-setup.md).

### ExternalModel

References external AI/ML providers (e.g., OpenAI, Anthropic, Azure OpenAI).

The controller:
- Fetches the ExternalModel CR from the same namespace
- Validates the user-supplied HTTPRoute references the correct gateway
- Derives `status.endpoint` from HTTPRoute hostnames or gateway addresses
- Sets `status.phase` based on HTTPRoute acceptance

**Example:**
```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: gpt-4
  namespace: external
spec:
  modelRef:
    kind: ExternalModel
    name: openai-gpt4
```

For complete setup instructions, see [External Model Setup](../../install/external-model-setup.md).

---

## Endpoint Override

By default, the controller discovers the endpoint URL from the backend (LLMInferenceService status, Gateway, or HTTPRoute hostnames). Use `spec.endpointOverride` to specify a custom URL when:

- The controller picks the wrong gateway or hostname
- Your environment requires a specific URL
- You need to point the model at a custom proxy or load balancer

**Example:**
```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: my-model
  namespace: llm
spec:
  modelRef:
    kind: LLMInferenceService
    name: my-model
  endpointOverride: "https://correct-hostname.example.com/my-model"
```

The override does not bypass backend validation. The controller still checks that the backend is ready (HTTPRoute accepted, LLMInferenceService ready, etc.). The override only determines the final value written to `status.endpoint` **after** the backend becomes ready. While the backend is not ready, the controller clears `status.endpoint` (sets it to empty string) and sets `status.phase` to `Pending`, regardless of the override value.

---

## Status

### MaaSModelRefStatus

| Field | Type | Description |
|-------|------|-------------|
| phase | string | One of: `Pending`, `Ready`, `Failed` |
| endpoint | string | Endpoint URL for the model (auto-discovered or from `endpointOverride`) |
| httpRouteName | string | Name of the HTTPRoute associated with this model |
| httpRouteNamespace | string | Namespace of the HTTPRoute |
| httpRouteGatewayName | string | Name of the Gateway that the HTTPRoute references |
| httpRouteGatewayNamespace | string | Namespace of the Gateway that the HTTPRoute references |
| httpRouteHostnames | []string | Hostnames configured on the HTTPRoute |
| conditions | []Condition | Latest observations of the model's state |

---

## Annotations

MaaSModelRef supports standard Kubernetes and OpenShift annotations. The MaaS API reads these annotations and returns them in the `modelDetails` field of the `GET /v1/models` response.

| Annotation | Description | Returned in API | Example |
| ---------- | ----------- | --------------- | ------- |
| `openshift.io/display-name` | Human-readable model name | `modelDetails.displayName` | `"Llama 2 7B Chat"` |
| `openshift.io/description` | Model description | `modelDetails.description` | `"A large language model optimized for chat"` |
| `opendatahub.io/genai-use-case` | GenAI use case category | `modelDetails.genaiUseCase` | `"chat"` |
| `opendatahub.io/context-window` | Context window size | `modelDetails.contextWindow` | `"4096"` |

### Example with annotations

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: llama-2-7b-chat
  namespace: opendatahub
  annotations:
    openshift.io/display-name: "Llama 2 7B Chat"
    openshift.io/description: "A large language model optimized for chat use cases"
    opendatahub.io/genai-use-case: "chat"
    opendatahub.io/context-window: "4096"
spec:
  modelRef:
    kind: LLMInferenceService
    name: llama-2-7b-chat
```

### API response

When annotations are set, the `GET /v1/models` response includes a `modelDetails` object:

```json
{
  "id": "llama-2-7b-chat",
  "object": "model",
  "created": 1672531200,
  "owned_by": "opendatahub",
  "ready": true,
  "url": "https://...",
  "modelDetails": {
    "displayName": "Llama 2 7B Chat",
    "description": "A large language model optimized for chat use cases",
    "genaiUseCase": "chat",
    "contextWindow": "4096"
  }
}
```

When no annotations are set (or all values are empty), `modelDetails` is omitted from the response.

---

## Related Documentation

- [ExternalModel CRD](external-model.md) - External provider configuration
- [Model Setup](../../configuration-and-management/model-setup.md) - LLMInferenceService deployment
- [External Model Setup](../../install/external-model-setup.md) - External provider integration
