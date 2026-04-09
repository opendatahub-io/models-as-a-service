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

## MaaSSubscriptionStatus

The controller populates the `status` field with operational state. These fields are read-only.

| Field | Type | Description |
|-------|------|-------------|
| phase | string | Subscription health phase: `Active` (healthy), `Degraded` (some models unhealthy), `Failed` (not operational), or `Pending` (provisioning) |
| conditions | []Condition | Standard Kubernetes conditions (e.g., Ready) |
| modelRefStatuses | []ModelRefStatus | Per-model health status for each referenced MaaSModelRef |

### ModelRefStatus

| Field | Type | Description |
|-------|------|-------------|
| name | string | Name of the MaaSModelRef |
| namespace | string | Namespace of the MaaSModelRef |
| ready | bool | Whether this model is healthy and accessible |
| reason | string | Short reason code if not ready (e.g., `ModelNotFound`, `EndpointUnavailable`) |
| message | string | Human-readable message if not ready |

### Phase Behavior

The `status.phase` field affects subscription behavior:

- **Active**: All models in the subscription are accessible. No per-model health checks required.
- **Degraded**: Some models may be unhealthy. The controller populates `status.modelRefStatuses` with per-model health. Only models with `ready: true` are accessible for inference and model listing.
- **Failed**: Subscription is not operational. All inference requests return **403 Forbidden**. Subscription is excluded from `/v1/models` listing.
- **Pending**: Subscription is being provisioned. All inference requests return **403 Forbidden**. Subscription is excluded from `/v1/models` listing.

!!! note "Health-based filtering"
    The `/v1/models` endpoint and inference authorization only include subscriptions in Active or Degraded phase. For Degraded subscriptions, only models with `ready: true` in `status.modelRefStatuses` are accessible. See [Model Listing Flow](../../configuration-and-management/model-listing-flow.md#subscription-health-filtering) for details.
