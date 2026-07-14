# ExternalModel

Defines an external AI/ML model hosted outside the cluster (e.g., OpenAI, Anthropic, Azure OpenAI). The ExternalModel CRD contains provider details, endpoint URL, and credential references that were previously inlined in MaaSModelRef.

## ExternalModelSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| provider | string | Yes | Provider identifier. Allowed values: `openai`, `anthropic`, `azure-openai`, `vertex`, `bedrock-openai`. Max length: 63 characters. |
| endpoint | string | Yes | FQDN of the external provider (no scheme or path), e.g., `api.openai.com`. This is metadata for downstream consumers. Max length: 253 characters. |
| credentialRef | CredentialReference | Yes | Reference to the Secret containing API credentials. Must exist in the same namespace as the ExternalModel. |
| targetModel | string | Yes | Upstream model name at the external provider (e.g., `gpt-4o`, `claude-sonnet-4-5-20241022`). Max length: 253 characters. |

## CredentialReference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| name | string | Yes | Name of the Secret containing the credentials. Must be in the same namespace as the ExternalModel. Max length: 253 characters. |

## ExternalModelStatus

| Field | Type | Description |
|-------|------|-------------|
| phase | string | One of: `Pending`, `Ready`, `Failed` |
| conditions | []Condition | Latest observations of the external model's state |

## Annotations

Optional metadata annotations that control networking behavior for the external model.

| Annotation | Required | Default | Description |
|------------|----------|---------|-------------|
| `maas.opendatahub.io/tls` | No | `true` | Controls TLS origination to the external endpoint. When `true`, the Istio sidecar performs TLS handshake with the provider and a DestinationRule is created. Set to `false` for internal or non-TLS endpoints (e.g., a vLLM instance on an internal network). TLS should remain enabled when `credentialRef` is set, unless the network path is trusted and isolated, to avoid sending API keys in cleartext. |
| `maas.opendatahub.io/port` | No | `443` | Overrides the default port used for the external endpoint. Valid range: 1–65535; values outside this range are rejected during reconciliation. |

When `maas.opendatahub.io/tls` is set to `false`:

- The ServiceEntry protocol changes from HTTPS to HTTP
- The DestinationRule for TLS origination is not created. Any existing controller-managed DestinationRule is deleted; DestinationRules annotated with `opendatahub.io/managed: "false"` are preserved.
- The default port remains 443 but can be overridden with `maas.opendatahub.io/port`

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

### Non-TLS Internal Endpoint

!!! warning "Cleartext credential traffic"
    Disabling TLS while `credentialRef` is set means the provider API key is sent in cleartext. Only use non-TLS mode on a trusted, isolated network.

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: internal-vllm
  namespace: models
  annotations:
    maas.opendatahub.io/tls: "false"
    maas.opendatahub.io/port: "8000"
spec:
  provider: openai
  endpoint: vllm.internal.example.com
  targetModel: my-model
  credentialRef:
    name: vllm-credentials
```

## Relationship with MaaSModelRef

ExternalModel is a dedicated CRD for external model configuration. MaaSModelRef references ExternalModel by name using `spec.modelRef.kind: ExternalModel` and `spec.modelRef.name: <external-model-name>`.

This separation allows:
- **Reusability**: One ExternalModel can be referenced by multiple MaaSModelRefs
- **Clean separation**: Provider-specific configuration lives in ExternalModel; MaaSModelRef handles listing and access control
- **Extensibility**: Adding new external providers requires no MaaSModelRef schema changes
