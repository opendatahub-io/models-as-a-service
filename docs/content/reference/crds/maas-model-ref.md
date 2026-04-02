# MaaSModelRef

Identifies an AI/ML model on the cluster. Create MaaSModelRef in the **same namespace** as the backend (`LLMInferenceService`, `ExternalModel`, etc.). The MaaS API lists models from MaaSModelRef resources cluster-wide (using `status.endpoint` and `status.phase`).

## MaaSModelRefSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| modelRef | ModelReference | Yes | Reference to the model endpoint |
| endpointOverride | string | No | Overrides the endpoint URL that the controller would otherwise discover from the backend (e.g. LLMInferenceService status or Gateway/HTTPRoute) |

## ModelReference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| kind | string | Yes | One of: `LLMInferenceService`, `ExternalModel` |
| name | string | Yes | Name of the model resource (e.g. LLMInferenceService name, ExternalModel name). Must be in the same namespace as the MaaSModelRef. Max length: 253 characters. |

For `kind: ExternalModel`, the MaaSModelRef references an [ExternalModel](external-model.md) CR that contains the provider configuration.

## MaaSModelRefStatus

| Field | Type | Description |
|-------|------|-------------|
| phase | string | One of: `Pending`, `Ready`, `Unhealthy`, `Failed` |
| endpoint | string | Endpoint URL for the model |
| httpRouteName | string | Name of the HTTPRoute associated with this model |
| httpRouteNamespace | string | Namespace of the HTTPRoute |
| httpRouteGatewayName | string | Name of the Gateway that the HTTPRoute references |
| httpRouteGatewayNamespace | string | Namespace of the Gateway that the HTTPRoute references |
| httpRouteHostnames | []string | Hostnames configured on the HTTPRoute |
| conditions | []Condition | Latest observations of the model's state |
