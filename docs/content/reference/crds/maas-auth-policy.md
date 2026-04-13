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
| phase | string | One of: `Pending`, `Active`, `Failed` |
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
| accepted | string | Whether the AuthPolicy has been accepted (from `status.conditions` type=Accepted) |
| enforced | string | Whether the AuthPolicy is enforced (from `status.conditions` type=Enforced) |
