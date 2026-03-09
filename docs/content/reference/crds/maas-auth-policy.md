# MaaSAuthPolicy

Defines who (groups/users) can access which models. Creates Kuadrant AuthPolicies that validate API keys via MaaS API callback and perform subscription selection.

## MaaSAuthPolicySpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| modelRefs | []string | Yes | List of model names this policy grants access to |
| subjects | SubjectSpec | Yes | Who has access (OR logic—any match grants access) |
| meteringMetadata | MeteringMetadata | No | Billing and tracking information |

## SubjectSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| groups | []GroupReference | No | List of Kubernetes group names |
| users | []string | No | List of Kubernetes user names |

At least one of `groups` or `users` must be specified.

## GroupReference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| name | string | Yes | Name of the group |
