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
| window | string | Yes | Time window (e.g., `1m`, `1h`, `24h`). Allowed units: `s` and `m` (1–9999), `h` (1–8784, i.e. 366 days); no leading zeros. **Breaking change:** `d` (days) is no longer accepted; use hours instead (e.g., `24h` not `1d`). Hours above `8784h` are no longer accepted either; see the note below for subscriptions that already hold one. |

!!! note "Subscriptions stored before these limits"
    A subscription can still hold a window or limit that the schema now rejects, for example `9999h` or `1d` (days were dropped), or a model reference with neither `tokenRateLimits` nor `unlimited`. Rate limits for that model cannot be enforced, so the subscription is `Degraded` and inference on that model is denied until the value is fixed; its other models keep working. Callers get a 403 saying the model's token rate limits are invalid in the subscription.

    - A reference with no token budget was served at 100 tokens per minute up to v0.2.x. Set `tokenRateLimits: [{limit: 100, window: 1m}]` to keep that, or `unlimited: true`.
    - This also applies when the model's TokenRateLimitPolicy is managed by hand (`opendatahub.io/managed: "false"`).
    - Edits outside `modelRefs` are still accepted, but any change to `modelRefs` must make every window and limit valid.
    - For a reference with no token budget, API servers before Kubernetes 1.33 also reject the controller's status update, so its phase may still read `Active`. Inference on the affected model is denied regardless.

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
