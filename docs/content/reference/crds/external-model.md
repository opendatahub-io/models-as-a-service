# ExternalModel

Defines an external AI/ML model hosted outside the cluster (e.g., OpenAI, Anthropic, Azure OpenAI). The ExternalModel CRD contains provider details, endpoint URL, and credential references that were previously inlined in MaaSModelRef.

## ExternalModelSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| provider | string | Yes | Provider identifier (e.g., `openai`, `anthropic`, `azure`). Max length: 63 characters. |
| endpoint | string | Yes | FQDN of the external provider (no scheme or path), e.g., `api.openai.com`. This is metadata for downstream consumers. Max length: 253 characters. |
| credentialRef | CredentialReference | Yes | Reference to the Secret containing API credentials. Must exist in the same namespace as the ExternalModel. |
| targetModel | string | No | Upstream model name at the external provider (e.g., `gpt-4o`, `claude-sonnet-4-5-20241022`). When omitted, the MaaSModelRef name is used as the model identifier. Max length: 253 characters. |

## CredentialReference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| name | string | Yes | Name of the Secret containing the credentials. Must be in the same namespace as the ExternalModel. Max length: 253 characters. |

## ExternalModelStatus

| Field | Type | Description |
|-------|------|-------------|
| phase | string | One of: `Pending`, `Ready`, `Failed` |
| conditions | []Condition | Latest observations of the external model's state |

## Example

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: gpt4
  namespace: models
spec:
  provider: openai
  endpoint: api.openai.com
  targetModel: gpt-4o
  credentialRef:
    name: openai-credentials
---
apiVersion: v1
kind: Secret
metadata:
  name: openai-credentials
  namespace: models
type: Opaque
stringData:
  api-key: "sk-..."
---
# MaaSModelRef referencing the ExternalModel
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: gpt4-model
  namespace: models
spec:
  modelRef:
    kind: ExternalModel
    name: gpt4
```

## Relationship with MaaSModelRef

ExternalModel is a dedicated CRD for external model configuration. MaaSModelRef references ExternalModel by name using `spec.modelRef.kind: ExternalModel` and `spec.modelRef.name: <external-model-name>`.

This separation allows:
- **Reusability**: One ExternalModel can be referenced by multiple MaaSModelRefs
- **Clean separation**: Provider-specific configuration lives in ExternalModel; MaaSModelRef handles listing and access control
- **Extensibility**: Adding new external providers requires no MaaSModelRef schema changes
