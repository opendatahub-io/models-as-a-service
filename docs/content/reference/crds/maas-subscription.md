# MaaSSubscription

Defines a subscription plan with per-model token rate limits. Creates Kuadrant TokenRateLimitPolicies enforced by Limitador. Must be created in the `models-as-a-service` namespace.

When a `MaaSSubscription` is deleted, its finalizer waits for a subscription-scoped cleanup Job to soft-delete and invalidate only the API keys bound to that subscription. Keys for other subscriptions in the tenant are not affected. Cleanup failures keep the subscription in `Terminating`, are retried, and emit a Kubernetes Warning event.

## MaaSSubscriptionSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| owner | OwnerSpec | Yes | Who owns this subscription |
| modelRefs | []ModelSubscriptionRef | Yes | Models included with per-model token rate limits (each specifies `name` and `namespace`) |
| tokenMetadata | TokenMetadata | No | Metadata for token attribution and metering |
| priority | int32 | No | Subscription priority when user has multiple (higher = higher priority; default: 0) |
| inferencePriority | int32 | No | Scheduling priority for inference requests made under this subscription (signed; higher = scheduled first, negative = below the scheduler default). Independent of `priority`, which only selects among subscriptions. Omitting the field creates no InferenceObjective and the scheduler applies priority `0`; setting it explicitly to `0` creates an InferenceObjective with priority `0`. |

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
| tokenRateLimits | []TokenRateLimit | Unless `unlimited` | Token-based rate limits for this model (at least one entry when set) |
| unlimited | bool | No | Access to this model without a token budget. Usage is still metered. Mutually exclusive with `tokenRateLimits` |
| billingRate | BillingRate | No | Cost per token |

## TokenRateLimit

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| limit | int64 | Yes | Maximum number of tokens allowed |
| window | string | Yes | Time window (e.g., `1m`, `1h`, `24h`). Allowed units: `s`, `m`, `h` (1–9999). Pattern: `^[1-9]\d{0,3}(s\|m\|h)$`. **Breaking change:** `d` (days) is no longer accepted; use hours instead (e.g., `24h` not `1d`). |

## Status: flowControlStatuses

`status.flowControlStatuses` lists, for each referenced model, the InferencePool and InferenceObjective used for `inferencePriority`. The objective name is published even when `inferencePriority` is unset, so request headers can always carry it.

| Field | Type | Description |
|-------|------|-------------|
| name | string | Name of the MaaSModelRef |
| namespace | string | Namespace of the MaaSModelRef. The InferencePool and InferenceObjective live in this namespace, alongside the LLMInferenceService. |
| inferencePool | LocalObjectReference | InferencePool (`group`, `kind`, `name`) observed in the LLMInferenceService's `status.router.scheduler.inferencePool` |
| objectiveName | string | InferenceObjective name for this subscription and pool, in the pool's namespace. Set whenever the pool is known, including when `inferencePriority` is unset. It does not change when `inferencePriority` changes. |
| ready | bool | `true` when no request-priority reconciliation is left for this model (see reasons below). It does not report whether the model's backend is ready; see `modelRefStatuses`. |
| reason | string | Machine-readable reason for `ready` (see below). New reasons may be added. |
| message | string | Human-readable detail for the reason |

| Reason | ready | Meaning |
|--------|-------|---------|
| `ObjectiveReconciled` | `true` | The InferenceObjective is observed with the priority from `inferencePriority` |
| `PriorityUnset` | `true` | `inferencePriority` is unset: no InferenceObjective is created and the scheduler applies priority `0` |
| `NotApplicable` | `true` | The model is not served through an inference scheduler, so no InferenceObjective is required. ExternalModels always report this; their readiness is in the ExternalModel's `status.phase` and conditions. |
| `Unmanaged` | `true` | The InferenceObjective has `opendatahub.io/managed: "false"`; its priority is owned by the user |
| `PoolPending` | `false` | The model's InferencePool or HTTPRoute has not been observed yet |
| `ObjectivePending` | `false` | The InferenceObjective is not observed with the desired priority yet |
| `Unsupported` | `false` | Request priority cannot be applied, for example because traffic is split across a routing group |
| `NotOnTenantGateway` | `false` | The model's HTTPRoute is not attached to the subscription's tenant Gateway |
| `ObjectiveConflict` | `false` | Another object holds the InferenceObjective name |
| `InferenceObjectiveAPIUnavailable` | `false` | The InferenceObjective API (`llm-d.ai`) is not installed |
| `ReconcileFailed` | `false` | Reconciling request priority for the model failed; it is retried |

When `inferencePriority` is set, the `InferenceObjectivesReady` condition summarizes these entries. It is `True` when every model is ready; otherwise it is `False` with the most severe reason. The condition is removed when `inferencePriority` is unset.

Request priority never fails a subscription. When `inferencePriority` is set and a model reports `ReconcileFailed`, `ObjectiveConflict`, `NotOnTenantGateway`, or `Unsupported`, an otherwise `Active` subscription becomes `Degraded`, which keeps access. Pending states and a missing InferenceObjective API do not change the phase. The status reports controller progress, not confirmation that the inference scheduler has applied the priority.

## Annotations

MaaSSubscription supports standard Kubernetes and OpenShift annotations for use by `kubectl`, the OpenShift console, and other tooling.

| Annotation | Description | Example |
| ---------- | ----------- | ------- |
| `openshift.io/display-name` | Human-readable display name | `"Premium Subscription"` |
| `openshift.io/description` | Free-text description | `"Premium-tier subscription with 1000 tokens/min rate limit"` |

**Example:**

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: premium-subscription
  namespace: models-as-a-service
  annotations:
    openshift.io/display-name: "Premium Subscription"
    openshift.io/description: "Premium-tier subscription with 1000 tokens/min rate limit"
spec:
  # ...
```
