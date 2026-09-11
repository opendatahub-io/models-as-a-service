---
title: MaaS Subscription Flow Control Design
description: Proposed integration of subscription request priorities with llm-d flow control.
---

# MaaS Subscription Flow Control Design

!!! note "Proposed design"
    This document describes planned behavior and follow-up extensions. It is not documentation of an implemented feature.

**Author:** Pierangelo Di Pilato

**Date:** 2026-09-08

**Scope:** Models-as-a-Service, AI Gateway, and llm-d request scheduling

## Overview

Integrate MaaS with Gateway API Inference Extension flow control. Use the AI tenant as the fairness key and the selected
MaaS subscription as the logical objective key. Users authorized to manage subscriptions configure a subscription's
request priority. When explicitly configured, the MaaS subscription controller reconciles an `InferenceObjective` for
each distinct `InferencePool` backing the models in that subscription. Otherwise, requests use the scheduler's default
priority without a generated objective.

## Motivation

MaaS subscriptions already define model access, token rate limits, and billing metadata. Token rate limits constrain
consumption over time, but do not determine which admitted requests should be dispatched first when inference capacity
is scarce. Flow control adds request queuing, priority, and fairness at the inference scheduler.

Non-MaaS Gateways already inject `x-gateway-inference-fairness-id` and `x-gateway-inference-objective` through
AuthPolicy templates. Their defaults use the cluster issuer for fairness and the caller's service-account namespace, or
an authenticated-user fallback, for the objective. MaaS needs these headers to reflect its tenant and subscription
boundaries. Subscription administrators should not need to discover pools or manually maintain objectives in model
namespaces.

## Goals

* Configure scheduling priority once per subscription and apply it to every eligible model in that subscription.
* Group requests from the same AI tenant for fairness within each priority band.
* Reuse the existing flow-control headers and `InferenceObjective` API.
* Keep generated objectives synchronized with subscriptions and observed model topology.
* Derive flow-control identity from authorized tenant, subscription, and model context.
* Let authorized operators view, set, change, and clear request priority through the subscription UI.

## Non-Goals

* Replace MaaS authorization, subscription selection, token rate limits, or billing.
* Introduce or change request `service_tier` handling. Its interaction with subscription selection, authorization, and
  pricing requires a separate proposal; this integration derives scheduling priority from the selected subscription's
  `requestPriority`, without a `service_tier` override.
* Introduce a new scheduler, per-user fairness, or per-model priority overrides within a subscription.
* Provide a global capacity allocation across independent pools or scheduler instances, or guarantee latency or minimum
  capacity across priority bands.
* Add flow control to external providers or model routes without an `InferencePool`.
* Change the non-MaaS Gateway defaults.

## Proposed Design

### Subscription priority and fairness

Add an optional `MaaSSubscription.spec.requestPriority` field as the desired `InferenceObjective.spec.priority`. Use a
signed `int32`, matching the objective API, and preserve the distinction between an omitted value and an explicit `0`
(for example, using `*int32` in the Go API). Do not apply a CRD default that fills in this field. Higher values receive
higher scheduling priority; negative values represent lower-than-default priority. Validate and preserve this value
consistently across the subscription API and objective reconciliation.

The existing `spec.priority` continues to control automatic subscription selection. `spec.requestPriority` controls
request scheduling after a subscription has been selected. Updating either field does not change the other field's
meaning, and request priority does not influence subscription selection. Explicit subscription selection and API-key
subscription binding continue to follow the existing authorization rules.

Subscriptions without `spec.requestPriority` create no `InferenceObjective`, but both canonical and legacy header pairs
are still injected. The objective header carries the generated name for the subscription/pool pair. With no matching
objective, llm-d applies scheduler priority `0`, regardless of subscription-selection priority. An explicit
`spec.requestPriority: 0` creates an objective with priority `0` under that same name, following the same path as any
other explicit value.

For example, a subscription can have `spec.priority: 100` to make it preferred during automatic selection and
`spec.requestPriority: 10` to schedule its requests at priority `10`.

The documented default objective header value is the generated subscription/pool objective name, even when request
priority is omitted. In that case the resource is intentionally absent and the scheduler defaults to priority `0`. When
request priority is configured, the same header value matches the generated objective in the target pool's namespace.

| Flow-control input                 | MaaS source                                                              | Meaning                                                                                                |
|------------------------------------|--------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------|
| `x-llm-d-inference-fairness-id`    | Stable, unambiguous identity of the resolved `AITenant`                  | All callers and subscriptions of that tenant share a fairness identity.                                |
| `x-llm-d-inference-objective`      | Generated objective name for the authorized subscription and target pool | Always injected; resolves the configured priority, or scheduler priority `0` when no objective exists. |
| `InferenceObjective.spec.priority` | Explicit `MaaSSubscription.spec.requestPriority`                         | Applies the same request priority to every pool covered by the subscription.                           |

MaaS emits the canonical `x-llm-d-*` names defined by llm-d Router's `FlowFairnessIDKey` and `ObjectiveKey` constants
alongside their legacy aliases to support older deployments. Each alias carries exactly the same trusted value as its
canonical counterpart:

| Canonical header                | Legacy header                     | Value                                      |
|---------------------------------|-----------------------------------|--------------------------------------------|
| `x-llm-d-inference-fairness-id` | `x-gateway-inference-fairness-id` | Resolved AI tenant identity                |
| `x-llm-d-inference-objective`   | `x-gateway-inference-objective`   | Generated subscription/pool objective name |

The scheduler groups flows by `(fairness ID, priority)`. Two subscriptions belonging to one tenant at the same priority
share a flow; creating additional subscriptions does not give that tenant additional fairness shares at that priority.
Different priorities remain separate bands, and higher-priority traffic can delay lower-priority traffic indefinitely.
Fairness applies where requests compete in the same flow-control scheduler; separate tenant Gateways alone do not
establish a shared fairness domain. The fairness ID identifies the competing tenant, while the configured scheduler
policy determines how flows share dispatch opportunities. The default global-strict policy does not provide tenant
fairness; deployments requiring fair sharing must explicitly select an appropriate fairness policy.

### Pool discovery and objective reconciliation

The MaaS subscription controller owns the desired objectives:

1. Resolve the subscription's AI tenant using the existing tenant namespace association and validate that each
   referenced model is exposed through that tenant's Gateway.
2. Follow each `spec.modelRefs[]` entry to its `MaaSModelRef`. For `spec.modelRef.kind: LLMInferenceService`, read the
   referenced service in the model reference's namespace.
3. Discover the active pool through `LLMInferenceService.status.router.scheduler.inferencePool`, including its group,
   kind, name, and namespace. Use this observed reference for both managed pools and explicitly referenced pools; do not
   infer a generated pool name from the service name or assume that the subscription and pool share a namespace.
4. When `spec.requestPriority` is explicitly set, reconcile one objective per distinct `(subscription, pool)` pair in
   the pool's namespace, with `spec.poolRef.name`
   pointing to that pool and `spec.priority` copied from the subscription's `spec.requestPriority`. Multiple model
   references resolving to the same pool share that objective.
5. Reconcile changes to request priority, model membership, tenant association, and observed pool references. Remove
   obsolete objectives when request priority is cleared, a model leaves the subscription, its pool changes, or the
   subscription is deleted. With request priority omitted, no objectives are desired.

An `InferenceObjective` references one pool in its own namespace. Consequently, the bare subscription name cannot serve
as every generated object's name: two models in the same namespace may use different pools, and subscriptions in
different tenant namespaces may share a name. Generate deterministic, DNS-compatible names that include the tenant,
subscription, and pool names, with a collision-resistant hash of their full resource identities. Truncate the readable
name portions as needed to respect Kubernetes naming limits while preserving the hash. The subscription remains the
logical objective; the pool-specific name is its representation in the inference API.

For example, a subscription with `spec.requestPriority: 10` produces an objective for each referenced pool:
The name below is a schematic placeholder; generated names include the pool-specific disambiguation described above.

```yaml
apiVersion: inference.networking.x-k8s.io/v1alpha2
kind: InferenceObjective
metadata:
  # longer than 63 chars names need to be handled through hashing see kmeta.ChildName algorithm
  name: maas-<ai-tenant-name>-<maas-subscription>
  namespace: <inference-pool-namespace>
spec:
  priority: 10
  poolRef:
    name: <inference-pool-name>
```

Extend reconciliation watches to include relevant `LLMInferenceService` status changes, pools, and generated objectives.
Track generated resources using controller ownership metadata that includes the subscription namespace, name, and UID.
Extend the existing subscription cleanup finalizer for objectives in other namespaces, since cross-namespace owner
references cannot provide garbage collection. Reconcile only resources owned by this integration; report naming or
ownership conflicts instead of taking over unrelated objectives.

### Request classification

For every authorized inference request targeting a pool, the MaaS-generated AuthPolicy always injects all four headers:
the canonical fairness and objective headers and their legacy aliases, after authentication, subscription selection, and
model authorization. Their values come exclusively from the resolved tenant and subscription/pool identity. The
objective header carries the generated resource name, not the numeric request priority, whether or not an objective
resource is required. Clients cannot choose either scheduling identity by supplying their own headers.

Extend the trusted subscription-selection metadata returned by maas-api to expose the resolved objective identity for
the requested model. The controller must publish the model-to-objective-name mapping in subscription status even when
request priority is unset and no objective resource is created, so maas-api can consume it through its existing informer
cache. This keeps naming and pool discovery in the control plane and avoids a Kubernetes lookup on every inference
request. The AuthPolicy consumes this metadata for both path-based and body-based model routing.

All four headers must be set with replacement semantics, removing all client-supplied values, including duplicate or
differently cased occurrences, before injecting trusted values. MaaS emits exactly one value per header, with identical
values within each canonical/legacy pair. A requested subscription is only a selection input; it becomes an objective
after MaaS validates access, model membership, and tenant association. Requests without a valid subscription must not
obtain a scheduling identity through a fallback to arbitrary client headers. Existing authentication, authorization, and
rate-limit enforcement remain in the request path.

### Operator UI and live updates

The subscription UI must expose a distinct **Request priority** control to authorized operators. Operators can view,
set, change, or clear the value without recreating the subscription. An unset field displays **Scheduler default (0)**;
the UI must preserve that unset state rather than saving an explicit `0`. Subscription-selection priority remains a
separate setting with a separate description.

Expose the configured request priority and per-model objective references and reconciliation state through the API used
by the UI. Subscription details must let operators inspect generated objective names and their pool associations without
inspecting raw traffic. Show pending or failed reconciliation separately from the configured value; when request
priority is unset, indicate that no objective is required. Reconciliation status reports controller progress, not
confirmation that every EPP instance has observed the update.

Changing an explicit request priority updates existing objectives in place. Objective names must not depend on the
numeric priority, so header values remain stable while EPP observes the updated objective priority. Setting a previously
unset value creates objectives; clearing the field removes generated objectives while retaining the mappings and all
four injected headers. EPP then falls back to priority `0`. Subsequent requests use the new behavior after controller,
informer, and EPP propagation, using the same subscription and credentials without a restart or subscription recreation.

### Availability and compatibility

Creating objectives and injecting headers requires an installed, compatible `InferenceObjective` API and flow control
enabled in the target inference scheduler. MaaS manages subscription classification and objective lifecycle; KServe and
the inference serving stack retain ownership of pools, scheduler deployment, and scheduling configuration.

llm-d Router supports the canonical `x-llm-d-inference-objective` and `x-llm-d-inference-fairness-id` headers defined in
its
[header constants](https://github.com/llm-d/llm-d-router/blob/main/pkg/epp/metadata/consts.go). MaaS emits both
canonical names and their legacy aliases so existing deployments that consume the older names receive the same
scheduling identities. Newer routers prefer canonical values, while older routers consume the legacy values. Both pairs
must be derived from the same trusted metadata and injected together, including when request priority is unset. Header
compatibility does not replace the requirement for the objective API and scheduler flow-control support.

Report per-model flow-control reconciliation state in subscription status. A missing observed pool for a service that is
still reconciling is pending and retried; an external model or a route deliberately lacking an inference scheduler is
not applicable. When request priority is unset, absence of an objective is expected and must not be reported as an
objective reconciliation failure. Objective creation or update failures for explicit request priorities must be visible
without preventing reconciliation of the subscription's other models.

The existing scheduler defaults to priority `0` when an objective cannot be resolved. Controller and informer
propagation are asynchronous, so updates may temporarily use the old priority or that default. This integration does not
promise atomic changes across all pools. Missing objectives must not bypass MaaS access control or token rate limits,
and MaaS must not report successful flow-control reconciliation while required objectives are missing.

Deployments without the optional inference APIs must continue to support existing MaaS functionality. Enabling flow
control should explicitly report unsupported or pending models rather than imply that priority is enforced for every
backend.

### llm-d Scheduler Policy Controls

The llm-d Router/Scheduler implementation and configuration are the authority for the behavior described here.
`requestPriority` selects a band; the EPP's `flowControl` configuration determines dispatch eligibility, fairness,
request ordering, and overload handling. Model owners own these controls. The initial MaaS API exposes request
classification without adding subscription-level configuration for scheduler plugins.

| Control                                                | Scheduling effect                                                                                     | Implication for MaaS                                                                                      |
|--------------------------------------------------------|-------------------------------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------|
| `priority-holdback-policy`                             | Gives lower-priority bands lower saturation ceilings. A gated band's requests remain queued.          | Preserves headroom for higher tiers before full saturation; it does not reserve a fixed GPU share.        |
| `soft-reflective-ceiling-policy`                       | Alternates lower-band dispatch eligibility as saturation rises, using rank and band count.            | Provides gradual throttling across dispatch cycles, without guaranteeing service to lower bands.          |
| `round-robin-fairness-policy`                          | Alternates dispatch opportunities among active flows within a band.                                   | With tenant fairness IDs, fairness is measured in turns, not tokens or GPU time.                          |
| `program-aware-fairness`                               | Uses accumulated weighted token consumption, decay, and queue wait to favor underserved flows.        | Can account for differences in request cost between tenants; usage accounting depends on completion data. |
| `global-strict-fairness-policy`                        | Selects the best request across flows according to the ordering policy; this is the default.          | Tenant IDs alone do not ensure fair sharing or protection from high-volume tenants.                       |
| `edf-ordering-policy` / `slo-deadline-ordering-policy` | Orders requests by queue-expiry deadline or a supplied TTFT deadline.                                 | Urgency affects request order within the selected band and fairness policy.                               |
| Band `maxRequests` / `maxBytes` and queue TTLs         | Bound buffering and waiting; separate defaults can be configured for negative-priority bands.         | High-priority traffic is still subject to resource limits and expiry.                                     |
| `enableEviction`                                       | Enables demand-driven termination of eligible lower-priority in-flight requests; disabled by default. | Negative-priority requests can be interrupted to reclaim capacity for higher-priority demand.             |

#### Priority holdback: What `domain` Controls

`domain` is a parameter of the `priority-holdback-policy` plugin. It determines how numerical priorities are mapped to
dispatch ceilings between `minCeiling` and `maxCeiling`. It is configured by the pool administrator in the scheduler,
separately from a subscription's `requestPriority`.

A ceiling is a saturation threshold: when measured pool saturation reaches or exceeds a band's ceiling, dispatch from
that band pauses and its requests remain queued. For example, a ceiling of `0.65` allows dispatch below saturation
`0.65`. The value refers to the configured saturation detector's signal; it is not a reservation of 65% of GPU capacity.

* **`domain: rank`** uses only the ordering of priority levels. It spaces ceilings evenly from highest to lowest rank,
  regardless of numerical gaps. This fits priorities used as tier labels, such as premium, standard, and batch.
* **`domain: value`** uses the numerical distance between priority levels. A priority close to the lowest value receives
  a ceiling close to `minCeiling`. This fits configurations where numerical spacing is intended to affect holdback.

For three registered priorities `100`, `10`, and `0`, with `minCeiling: 0.4` and `maxCeiling: 0.9`:

| Request priority | Position | Ceiling with `domain: rank` | Ceiling with `domain: value` |
|------------------|----------|-----------------------------|------------------------------|
| `100`            | Highest  | `0.90`                      | `0.90`                       |
| `10`             | Middle   | `0.65`                      | `0.45`                       |
| `0`              | Lowest   | `0.40`                      | `0.40`                       |

With `rank`, the middle band gets the midpoint between `0.40` and `0.90`: `0.65`. With `value`, priority `10` is only
10% of the way from `0` to `100`, so its ceiling is `0.40 + 0.10 * (0.90 - 0.40) = 0.45`.

At saturation `0.50`, priority `10` is eligible under `rank` but held in queue under `value`. Priority `100` is eligible
in both cases, and priority `0` is held in both. Eligibility still respects dispatch order: priority `10` gets a turn
only when higher-priority work does not take that opportunity.

Both modes give the highest band `maxCeiling` and, with multiple bands, the lowest band `minCeiling`. With a single
registered priority, its ceiling is `maxCeiling`. Ceilings are computed from the priority levels known to the scheduler;
adding or removing bands can change the mapping. A subscription's priority number therefore does not define a fixed
capacity entitlement. Equally spaced priorities, such as `10`, `0`, and `-10`, produce the same ceilings in both modes.

Dispatch still examines higher-priority bands first and stops when a band's ceiling blocks dispatch. Opening a lower
band's ceiling does not guarantee a turn if higher-priority demand continues. These policies do not provide minimum
capacity guarantees across priority bands. The saturation detector also matters: utilization-based detection consumes
backend telemetry, while concurrency-based detection uses in-flight request or token accounting. Missing or stale
telemetry can halt dispatch with the utilization detector.

With in-flight eviction enabled, the existing sheddable filter admits only negative-priority victims, and reclamation
requires victim priority to be below the blocked demand's priority. Victims are ordered by lowest priority, then newest
dispatch time. Setting a negative `requestPriority` therefore permits interruption under that configuration; it does not
merely place requests later in the queue. This behavior must be explained to subscription operators.

If SLO deadline ordering is enabled, `x-llm-d-slo-ttft-ms` and its legacy alias can influence scheduling order. A
follow-up must define trusted derivation or validation and bounds for these values before exposing subscription SLO
controls. Protecting objective and fairness headers does not govern additional scheduling inputs. Tenant
request-priority ranges complement scheduler policies by restricting which bands a subscription may select.

## Priority-Tier Pricing and Chargeback

The existing per-model `billingRate.perToken` on a subscription can incorporate the price of its priority tier. For
example, an online subscription with higher `requestPriority` can carry a higher per-token rate than a batch
subscription. This requires no additional priority-specific pricing field. MaaS does not automatically adjust the
billing rate when request priority changes; administrators or a separate pricing policy must manage that relationship.

For charges based on the configured rate, token usage and the applicable rate are sufficient; recording request
priority is not a prerequisite. Historical attribution must preserve the applicable rate or an immutable pricing-policy
version alongside usage. Looking up a subscription's current rate cannot reliably price earlier requests after a rate
change. Durable rate attribution and charge calculation are separate billing concerns outside the initial flow-control
implementation.

An optional follow-up could record the request priority observed by MaaS and the objective identity for auditing and
troubleshooting. These values must come from trusted request-time metadata, and the existing subscription-selection
`priority` must not be confused with `requestPriority`. Objective names remain stable across priority changes, so they
alone cannot reconstruct historical scheduling priority.

If a future charging contract depends on the priority actually applied by EPP rather than the configured billing rate,
it would additionally require scheduler-side priority reporting correlated with usage. Gateway metadata alone cannot
establish applied priority during propagation delays or scheduler fallback. This is an optional extension, not a
requirement for pricing through `billingRate.perToken`.

The optional Envoy/OTel usage-log path can discard events when the collector is unavailable, and missing token usage
currently falls back to zero. Before using it as an authoritative billing record, billing work must address durable
delivery, duplicate handling, and incomplete usage, including interrupted streams. It must also define pricing changes,
retry attribution, and charging rules for rejected requests and preempted negative-priority work. Pricing and billing
enforcement remain outside the initial flow-control implementation.

## Security and Privacy Considerations

Only users authorized to edit subscriptions may set their request priority. Since priorities can compete across tenants
sharing a pool, platform administrators must govern which priority values tenant administrators can assign; fairness
within a band does not constrain a tenant that assigns itself a higher band.

> **Current scope:** Sharing inference pools across AI tenants is not currently supported, so cross-tenant priority
> escalation within a shared pool does not affect the supported deployment model. The initial implementation can
> proceed without tenant-level priority ranges. When cross-tenant pool sharing is introduced, the
> [tenant request-priority range follow-up](#follow-up-tenant-request-priority-ranges) should define and enforce these
> boundaries. Subscription priorities still govern contention within each tenant.

The controller requires read access to services and pools and permission to manage objectives in model namespaces.
Preserve tenant-to-model Gateway validation before writing these resources, and carry the required RBAC changes into
both the AI Gateway and ODH parent operators. Restrict direct objective edits to the platform's authorized
administrators. Internal headers and status mappings must not contain API keys or other credentials.

## Follow-up: Tenant Request-Priority Ranges

A future extension could let platform administrators configure an allowed request-priority range on `AITenant`, for
example `spec.requestPriorityRange.min` and `spec.requestPriorityRange.max` (illustrative field names). Tenant
administrators could then choose subscription request priorities within inclusive bounds set by the platform. This would
constrain priority escalation on shared pools while preserving subscription-level control and the existing objective
mapping. Managing the bounds must require platform-level authorization so tenant administrators cannot raise their own
limits.

The [allowed-values alternative](#allowed-priority-values-instead-of-ranges) preserves the same batch-versus-online
subscription flexibility while restricting choices to a discrete set. The follow-up should evaluate both approaches.

The follow-up should define validation of subscription creation and updates, reconciliation when tenant bounds change,
and UI display of the allowed range. It must also decide how to handle existing subscriptions outside a newly narrowed
range and the scheduler's effective priority `0` for subscriptions that omit `requestPriority`, particularly when `0`
falls outside the range. Rejecting invalid values should provide actionable feedback rather than silently clamping them.
These policy and migration choices are deferred; tenant-level ranges are not required for this design's initial
implementation because sharing inference pools across AI tenants is not currently supported. Priority governance
should be addressed as part of the follow-up design for enabling cross-tenant pool sharing.

### Allowed Priority Values Instead of Ranges

For future cross-tenant pool sharing, platform administrators could grant each `AITenant` a discrete set of allowed
`requestPriority` values instead of a continuous range. Tenant administrators would retain subscription-level control:
for example, a tenant allowed `-10`, `0`, and `10` could assign `-10` to a batch subscription and `10` to an online
subscription. This does not impose one fixed priority on all of a tenant's subscriptions.

A discrete set limits the priority bands tenants can introduce, making holdback behavior easier to govern, particularly
with `domain: rank`. It offers less numerical flexibility than ranges and requires maintaining tenant entitlements and
validating subscription updates. The initial numeric subscription API can remain unchanged; named tiers could be added
separately if needed. As with ranges, the follow-up must define how entitlement changes affect existing subscriptions
and how omitted request priority, with effective scheduler priority `0`, is handled.

## Alternatives

| Alternative                                          | Tradeoff                                                                                                                                                                                                                                                                                                                                                         |
|------------------------------------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| Manually create objectives in each model namespace   | Reuses the API but requires subscription administrators to discover pools and maintain cross-namespace resources as subscriptions change.                                                                                                                                                                                                                        |
| Use the subscription as the fairness key             | Gives tenants with more subscriptions more independent fairness shares at the same priority.                                                                                                                                                                                                                                                                     |
| Use the AI tenant as both fairness and objective key | Cannot express different subscription priorities within one tenant.                                                                                                                                                                                                                                                                                              |
| Use only token rate limits                           | Controls consumption over a time window but does not prioritize queued requests competing for inference capacity.                                                                                                                                                                                                                                                |
| Reuse `spec.priority` for request scheduling         | Avoids a new field, but couples subscription selection with scheduling and changes the effect of existing nonzero priorities. Separate `spec.requestPriority` keeps these decisions independent.                                                                                                                                                                 |
| Reference an operator-defined priority tier          | A named tier such as `premium` could map to a centrally governed numeric priority, limiting tenant self-escalation. It introduces another configuration resource and makes tier changes affect all referencing subscriptions. Consider if centrally managed tiers become a requirement.                                                                          |
| Share objectives by pool and numeric priority        | Subscriptions with the same request priority could reuse one objective per pool while retaining tenant fairness headers. This reduces object count but requires shared ownership and cleanup, and priority changes must switch the subscription's objective mapping. Per-subscription objectives keep ownership direct and names stable across priority changes. |

## Risks

* **Resource growth:** Objective count grows with the number of distinct subscription/pool pairs. Watches and cleanup
  must avoid full-cluster reconciliation for every change.
* **Topology mismatch:** Body-based routing and pool changes must use the same resolved backend for classification and
  forwarding. Routes that can select multiple pools need an explicit mapping strategy before claiming support.
* **Scheduling expectations:** Priority controls dispatch under contention; it is not a latency guarantee, and
  lower-priority traffic can starve.

Implementation validation must cover multiple pools in one namespace, equal subscription names across tenants, multiple
models sharing a pool, request-priority and pool changes, subscription deletion, missing optional APIs, forged headers,
and both path-based and body-based model resolution. End-to-end tests must demonstrate tenant fairness within one
priority band using an explicitly configured fairness policy, and priority ordering under contention with flow control
enabled. Test the selected pool policy configuration, including dispatch gating and negative-priority interruption when
eviction is enabled. Verify that omitted `spec.requestPriority` creates no objective, overwrites all four headers with
trusted values, and uses scheduler priority `0` while retaining tenant fairness. Test conflicting canonical/legacy
values, spoofed values, duplicate headers, and case variants, with request priority both set and unset. Explicit `0`
must create an objective and inject its name. Verify negative values, independence from subscription selection, and
set/change/clear transitions using the same subscription and credentials. Test UI viewing and editing, default display,
and per-model reconciliation details, along with the identical values within each canonical/legacy pair on both newer
and older supported routers. After propagation, verify subsequent requests carry the correct objective name and tenant
fairness identity, including when request priority is unset, and EPP applies the corresponding priority.

## Stakeholder Impacts

| Group             | Key Contacts | Date | Impacted?                                                                     |
|-------------------|--------------|------|-------------------------------------------------------------------------------|
| MaaS / AI Gateway | TBD          | TBD  | Yes — objective reconciliation, subscription metadata, and AuthPolicy headers |
| Dashboard         | TBD          | TBD  | Yes — request priority in MaaSSubscription pages                              |
| Documentation     | TBD          | TBD  | Yes — subscription request priority semantics documentation                   |

## References

* [MaaS architecture context](https://github.com/opendatahub-io/architecture-context/blob/main/architecture/rhoai.next/models-as-a-service.md)
* [MaaS subscription API](https://github.com/opendatahub-io/models-as-a-service/blob/main/maas-controller/api/maas/v1alpha1/maassubscription_types.go)
* [MaaS subscription controller](https://github.com/opendatahub-io/models-as-a-service/blob/main/maas-controller/pkg/controller/maas/maassubscription_controller.go)
* [MaaS subscription selection](https://github.com/opendatahub-io/models-as-a-service/blob/main/maas-api/internal/subscription/selector.go)
* [MaaS AuthPolicy generation](https://github.com/opendatahub-io/models-as-a-service/blob/main/maas-controller/pkg/controller/maas/maasauthpolicy_controller.go)
* [LLMInferenceService API and observed scheduler status](https://github.com/opendatahub-io/kserve/blob/main/pkg/apis/serving/v1alpha2/llm_inference_service_types.go)
* [InferenceObjective schema bundled with KServe](https://github.com/opendatahub-io/kserve/blob/main/config/llmisvc/gateway-inference-extension.yaml)
* [llm-d EPP HTTP headers reference](https://llm-d.ai/docs/0.8/api-reference/epp-http-headers)
* [llm-d Router header constants](https://github.com/llm-d/llm-d-router/blob/main/pkg/epp/metadata/consts.go)
* [llm-d flow-control configuration API](https://github.com/llm-d/llm-d-router/blob/main/apix/config/v1alpha1/endpointpickerconfig_types.go)
* [llm-d dispatch and priority gating](https://github.com/llm-d/llm-d-router/blob/main/pkg/epp/flowcontrol/controller/internal/processor.go)
* [llm-d priority holdback policy](https://github.com/llm-d/llm-d-router/blob/main/pkg/epp/framework/plugins/flowcontrol/usagelimits/priorityholdback/priority_holdback.go)
* [llm-d soft reflective ceiling policy](https://github.com/llm-d/llm-d-router/blob/main/pkg/epp/framework/plugins/flowcontrol/usagelimits/softreflectiveceiling/policy.go)
* [llm-d fairness policies](https://github.com/llm-d/llm-d-router/tree/main/pkg/epp/framework/plugins/flowcontrol/fairness)
* [llm-d SLO deadline ordering](https://github.com/llm-d/llm-d-router/blob/main/pkg/epp/framework/plugins/flowcontrol/ordering/slodeadline/slo_deadline.go)
* [llm-d in-flight reclamation](https://github.com/llm-d/llm-d-router/blob/main/pkg/epp/flowcontrol/controller/internal/reclamation.go)
* [MaaS subscription-selection metadata](https://github.com/opendatahub-io/models-as-a-service/blob/main/maas-api/internal/subscription/types.go)
* [MaaS telemetry-label generation](https://github.com/opendatahub-io/models-as-a-service/blob/main/maas-controller/pkg/platform/tenantreconcile/postrender.go)
* [MaaS structured usage logs](https://github.com/opendatahub-io/models-as-a-service/blob/main/deployment/components/observability/usage-logs/envoy-otel-access-log.yaml)
* [llm-d request-priority resolution](https://github.com/llm-d/llm-d-router/blob/main/pkg/epp/requestcontrol/director.go)
