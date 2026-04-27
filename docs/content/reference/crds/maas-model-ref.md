# MaaSModelRef

Identifies an AI/ML model on the cluster. Create MaaSModelRef in the **same namespace** as the backend resource.

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

References models deployed on the cluster via the LLMInferenceService CRD (e.g., vLLM, TGI via KServe). The alias `llmisvc` is also accepted for backwards compatibility.

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

For complete setup instructions, see [Model Setup (On Cluster)](../../configuration-and-management/model-setup.md).

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

The controller still validates the backend resource (HTTPRoute exists, LLMInferenceService is ready, etc.)—the override only affects the final endpoint URL written to `status.endpoint`.

---

## Status

### MaaSModelRefStatus

| Field | Type | Description |
|-------|------|-------------|
| phase | string | One of: `Pending`, `Ready`, `Unhealthy`, `Failed` |
| endpoint | string | Endpoint URL for the model (auto-discovered or from `endpointOverride`) |
| httpRouteName | string | Name of the HTTPRoute associated with this model |
| httpRouteNamespace | string | Namespace of the HTTPRoute |
| httpRouteGatewayName | string | Name of the Gateway that the HTTPRoute references |
| httpRouteGatewayNamespace | string | Namespace of the Gateway that the HTTPRoute references |
| httpRouteHostnames | []string | Hostnames configured on the HTTPRoute |
| conditions | []Condition | Latest observations of the model's state |

---

## Related Documentation

- [MaaSModelRef CRD Annotations](../../configuration-and-management/crd-annotations.md) - Display names, descriptions, use cases
- [ExternalModel CRD](external-model.md) - External provider configuration
- [Model Setup (On Cluster)](../../configuration-and-management/model-setup.md) - LLMInferenceService deployment
- [External Model Setup](../../install/external-model-setup.md) - External provider integration
