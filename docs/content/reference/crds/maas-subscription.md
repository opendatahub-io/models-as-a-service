# MaaSSubscription

Defines a subscription plan with per-model token rate limits. Creates Kuadrant TokenRateLimitPolicies enforced by Limitador. Must be created in the `models-as-a-service` namespace.

## MaaSSubscriptionSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| owner | OwnerSpec | Yes | Who owns this subscription |
| modelRefs | []ModelSubscriptionRef | Yes | Models included with per-model token rate limits (each specifies `name` and `namespace`) |
| tokenMetadata | TokenMetadata | No | Metadata for token attribution and metering |
| priority | int32 | No | Subscription priority when user has multiple (higher = higher priority; default: 0) |

## OwnerSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| groups | []GroupReference | No | Kubernetes group names that own this subscription |
| users | []string | No | Kubernetes user names that own this subscription |

## ModelSubscriptionRef

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| name | string | Yes | Name of the MaaSModelRef |
| namespace | string | Yes | Namespace where the MaaSModelRef lives |
| tokenRateLimits | []TokenRateLimit | Yes | Token-based rate limits for this model (at least one required) |
| billingRate | BillingRate | No | Cost per token |

## TokenRateLimit

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| limit | int64 | Yes | Maximum number of tokens allowed |
| window | string | Yes | Time window (e.g., `1m`, `1h`, `24h`). Pattern: `^(\d+)(s|m|h|d)$` |
