# MaaSSubscription

Defines a subscription plan with per-model token rate limits. Creates Kuadrant TokenRateLimitPolicies enforced by Limitador. Must be created in the `models-as-a-service` namespace.

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

`status.flowControlStatuses` lists, for each referenced model, the InferencePool and InferenceObjective used for `inferencePriority`. Flow control is optional, so these entries do not affect the subscription phase.

| Field | Type | Description |
|-------|------|-------------|
| name | string | Name of the MaaSModelRef |
| namespace | string | Namespace of the MaaSModelRef |
| inferencePool | InferencePoolReference | InferencePool (`group`, `kind`, `name`, `namespace`) observed in the LLMInferenceService's `status.router.scheduler.inferencePool` |
| objectiveName | string | InferenceObjective name for this subscription and pool, in the pool's namespace. Set whenever the pool is known, including when `inferencePriority` is unset. It does not change when `inferencePriority` changes. |
| state | string | Reconciliation state (see below) |
| message | string | Human-readable detail for the state |

| State | Meaning |
|-------|---------|
| `Pending` | The pool has not been observed yet, or the InferenceObjective is not reconciled yet |
| `NotApplicable` | The model is not served through an inference scheduler (for example, an ExternalModel) |
| `Unsupported` | Flow control cannot be applied, for example because traffic is split across a routing group |
| `Failed` | Reconciling the InferenceObjective failed |
| `Ready` | The InferenceObjective matches `inferencePriority` |
| `NotRequired` | `inferencePriority` is unset: no InferenceObjective is created and the scheduler applies priority `0` |

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
