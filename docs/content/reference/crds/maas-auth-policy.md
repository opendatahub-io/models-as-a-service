# MaaSAuthPolicy

Defines who (groups/users) can access which models. Creates Kuadrant AuthPolicies that validate API keys via MaaS API callback and perform subscription selection. Must be created in the `models-as-a-service` namespace.

## MaaSAuthPolicySpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| modelRefs | []ModelRef | Yes | List of `{name, namespace}` references to MaaSModelRef resources |
| subjects | SubjectSpec | Yes | Who has access (OR logic—any match grants access) |
| meteringMetadata | MeteringMetadata | No | Billing and tracking information |

## SubjectSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| groups | []GroupReference | No | List of Kubernetes group names |
| users | []string | No | List of Kubernetes user names |

At least one of `groups` or `users` must be specified.

## ModelRef (modelRefs item)

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| name | string | Yes | Name of the MaaSModelRef |
| namespace | string | Yes | Namespace where the MaaSModelRef lives |

## GroupReference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| name | string | Yes | Name of the group |

## MeteringMetadata

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| organizationId | string | No | Organization identifier for billing |
| costCenter | string | No | Cost center for billing attribution |
| labels | map[string]string | No | Additional labels for tracking |

## MaaSAuthPolicyStatus

| Field | Type | Description |
|-------|------|-------------|
| phase | string | One of: `Pending`, `Active`, `Degraded`, `Failed`, `Invalid`. `Degraded` means some model references or AuthPolicies are unhealthy. `Invalid` means the spec is missing or structurally invalid. |
| conditions | []Condition | Latest observations of the policy's state |
| authPolicies | []AuthPolicyRefStatus | Underlying Kuadrant AuthPolicies and their state |

## AuthPolicyRefStatus

Reports the status of each underlying Kuadrant AuthPolicy created by this MaaSAuthPolicy.

| Field | Type | Description |
|-------|------|-------------|
| name | string | Name of the AuthPolicy resource |
| namespace | string | Namespace of the AuthPolicy resource |
| model | string | MaaSModelRef name this AuthPolicy targets |
| modelNamespace | string | Namespace of the MaaSModelRef |
| ready | bool | Whether the AuthPolicy resource is valid and healthy |
| reason | ConditionReason | Machine-readable reason code (e.g. `Ready`, `NotReady`, `Unknown`) |
| message | string | Human-readable description of the status |

## Annotations

MaaSAuthPolicy supports standard Kubernetes and OpenShift annotations for use by `kubectl`, the OpenShift console, and other tooling.

| Annotation | Description | Example |
| ---------- | ----------- | ------- |
| `openshift.io/display-name` | Human-readable display name | `"Premium Access Policy"` |
| `openshift.io/description` | Free-text description | `"Grants premium-users group access to premium models"` |

**Example:**

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
