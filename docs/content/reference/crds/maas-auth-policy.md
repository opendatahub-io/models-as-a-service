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
