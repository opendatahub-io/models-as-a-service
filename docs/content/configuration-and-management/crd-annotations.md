# CRD Annotations Reference

This page documents the standard annotations supported on MaaS custom resources.

## Common annotations (all CRDs)

These annotations are supported on **MaaSModelRef**, **MaaSAuthPolicy**, and **MaaSSubscription**. They follow OpenShift conventions and are recognized by the OpenShift console, `kubectl`, and other tooling.

| Annotation | Description | Example |
| ---------- | ----------- | ------- |
| `openshift.io/display-name` | Human-readable display name | `"Llama 2 7B Chat"` |
| `openshift.io/description` | Free-text description of the resource | `"A general-purpose LLM for chat"` |

## MaaSModelRef annotations

In addition to the common annotations above, the MaaS API reads these annotations from **MaaSModelRef** and returns them in the `modelDetails` field of the `GET /v1/models` response.

| Annotation | Description | Returned in API | Example |
| ---------- | ----------- | --------------- | ------- |
| `openshift.io/display-name` | Human-readable model name | `modelDetails.displayName` | `"Llama 2 7B Chat"` |
| `openshift.io/description` | Model description | `modelDetails.description` | `"A large language model optimized for chat"` |
| `opendatahub.io/genai-use-case` | GenAI use case category | `modelDetails.genaiUseCase` | `"chat"` |
| `opendatahub.io/context-window` | Context window size | `modelDetails.contextWindow` | `"4096"` |

### Example MaaSModelRef with annotations

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

## MaaSAuthPolicy and MaaSSubscription annotations

The common annotations (`openshift.io/display-name`, `openshift.io/description`) can be set on MaaSAuthPolicy and MaaSSubscription resources for use by `kubectl`, the OpenShift console, and other tooling. They are **not** returned in the `GET /v1/models` API response.

### Example

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: premium-access
  namespace: models-as-a-service
  annotations:
    openshift.io/display-name: "Premium Access Policy"
    openshift.io/description: "Grants premium-users group access to premium models"
spec:
  # ...
```
