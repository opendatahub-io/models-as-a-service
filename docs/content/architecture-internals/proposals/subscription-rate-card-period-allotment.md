---
title: Usage capture and rate weights (control layer)
description: Proposed MaaS control layer for multi-type usage capture and optional stable-unit weights — cost and go/no-go stay with external metering.
---

# Usage capture and rate weights (control layer)

> [!NOTE]
> **Proposed design**
>
> This document describes a planned control-layer contract. It is not documentation of an implemented feature.
> Field names in the YAML sketches are illustrative and subject to API review.
>
> MaaS declares **what to capture** and optional **weights** (relative to a **stable unit**). **Cost of record** and **go / no-go** for spend cut-off live in an **external metering system** that consumes exported usage and **pushes** entitlement state into the gateway. Spend caps, wallets, and period allotments are **not** MaaS CRD fields.

**Author:** James Land

**Date:** 2026-10-22 (weights revision 2026-10-30)

**Scope:** Models-as-a-Service control plane (`MaaSModelRef`, `MaaSSubscription`); gateway emit / optional weighted QoS; external metering integration (Price Tag, Monetize 360, Cost Management / BYO)

## What

This document defines the **control layer** customers use to declare:

1. **Usage capture** — which usage units to read from inference `usage`, via profiles.
2. **Optional weights** — per-unit multipliers relative to a **stable unit**, for observability and (optionally) weighted rate-limit math.

Those declarations are reconciled into the gateway emit path (and later into any weighted QoS consumer). They are **not** a billing ledger.

**Working assumptions**

- **Capture and default weights** live on **`MaaSModelRef`**.
- **`MaaSSubscription` may override weights** per entitlement; subscriptions do **not** redefine capture.
- **External metering** calculates cost and pushes **go / no-go**; admit uses local mirrored state only (no per-request outbound).
- Some **lag** between true cost and cut-off is inherent; the **meter owns cut-off policy** (and thus **overrun** — e.g. Monetize 360 ~$10 wallet floor). We optimize for the **fastest practical push** into the gateway.

## Why

Operators need:

- **Accurate multi-type usage** — input / output / cache / reasoning (and non-token modes) as first-class emit, matching peer products and FinOps expectations.
- A way to express **relative importance** of those units (**weights** against a **stable unit**) without pretending MaaS is the invoice system.
- A clean seam so **Price Tag**, **Monetize 360**, and (if committed) **Red Hat Cost Management** can rate usage and revoke entitlement without duplicating their ledger in RHOAI.

## Goals

- Declarative **usage capture** on `MaaSModelRef` (built-in / ConfigMap / inline).
- Optional **weights** on ModelRef + subscription override — same numeric space as a **stable unit** (docs term; not a CRD currency field).
- Flexible token-type (and related) emit for external meters — **3.7 highest priority**.
- External meter owns cost + go/no-go; MaaS does not declare spend caps on CRs.
- Keep metering capture separate from QoS `tokenRateLimits` (weights may feed weighted QoS later without merging the policy surfaces).

## Non-Goals (for now)

- Spend caps, wallets, or period allotments as MaaS CR fields.
- Becoming **cost-of-record** or shipping invoices / AR.
- Per-request outbound calls to external meters.
- Prepaid wallet product UX in MaaS.
- Hierarchical org → team → key pools as first-class CRs (BYO / later).

## Definitions

| Term | Meaning |
| --- | --- |
| **Stable unit** | Dimensionless baseline weights are expressed against. Operators choose what “1” means; the platform does not require a currency. **Documentation term — not a CRD field.** |
| **Weight** | Per–usage-unit multiplier relative to the stable unit. Observability (+ optional rate-limit math). **Not** invoice price. |
| **Usage unit** | Named usage dimension from capture (e.g. `prompt`, `output`, `cached`). |
| **Go / no-go** | Entitlement flag pushed by the external meter into local gateway state. |

## Proposed Design

### Control-plane split

| Concern | Object | Role |
| --- | --- | --- |
| Usage shape / capture profile | `MaaSModelRef` | Built-in, ConfigMap, or inline |
| Default weight per usage unit | `MaaSModelRef` | Optional catalog defaults |
| Effective weight for an entitlement | `MaaSSubscription` | Optional override of ModelRef defaults |
| Cost, wallet, period cap, go/no-go | **External meter** | Export CE in; push entitlement state out |
| QoS rate limits | Existing `tokenRateLimits` (etc.) | Separate from metering; may later consume weights |

### Usage capture profiles

`MaaSModelRef` gets its capture definition from one of three places. The expanded form is always the same shape: `usageUnits` plus optional `defaultWeightExpr` / `weightExpr`. A new CRD is intentionally **not** proposed — reuse built-ins, a ConfigMap, or define the profile object **inline on the ModelRef**.

| Source | How `MaaSModelRef` selects it | Role |
| --- | --- | --- |
| **Built-in** | `usageCapture.profile: openai-chat` | Product-owned defaults for common API shapes |
| **ConfigMap** | `usageCapture.profileRef` → ConfigMap name/key | Shared site-local or vendor-specific shapes |
| **Inline on ModelRef** | `usageCapture.usageUnits` (+ optional `weightExpr`) | Per-model definition without a shared ConfigMap |

**Built-in profiles (illustrative v1 set)**

| Profile | Applies to | Built-in behavior |
| --- | --- | --- |
| `openai-chat` | OpenAI Chat Completions–compatible backends | Raw `prompt` / `cached` / `output` / `reasoning` paths; profile **default `weightExpr`** when cache-aware weights are used (prompt ⊇ cached) |
| `openai-chat-basic` | Same chat `usage` shape without cache-aware weighting | `prompt` ← `prompt_tokens`, `output` ← `completion_tokens`; default weighted usage is Σ count × weight |
| `anthropic-messages` | Anthropic Messages API | Raw `input` / `cache_read` / `cache_creation` / `output` paths; fields disjoint — Σ count × weight is correct |
| `anthropic-messages-basic` | Same Messages `usage` without cache-aware weighting | `input` ← `input_tokens`, `output` ← `output_tokens` |

Day-1 UX remains a built-in profile name with no extra objects. ConfigMap covers shared customs. Inline covers per-model override without a CRD.

**`openai-chat` family.** Assumes Chat Completions–compatible `usage` with at least `prompt_tokens` and `completion_tokens`. Optional detail fields such as `prompt_tokens_details.cached_tokens` and `completion_tokens_details.reasoning_tokens` are read when present and treated as **0** when absent. OpenAI-style `cached_tokens` is a **subset** of `prompt_tokens`, so cache-aware weighting uses the profile `defaultWeightExpr` (not independent Σ on both).

**`anthropic-messages` family.** Assumes Anthropic Messages `usage` with at least `input_tokens` and `output_tokens`. Optional cache fields are additive (not nested inside `input_tokens`). Independent weights are correct without a subtractive expression.

Missing top-level `usage` when metering is enabled is treated as an incomplete emit, not a successful zero-usage request. Embeddings, audio, image, and other API shapes get their own built-in or ConfigMap profiles.

### Weight model

By default, **weighted usage** (for observability / optional QoS) is:

`weighted_usage = Σ (unit_count × unit_weight)`

Most deployments only set `weights.perUnit` (illustrative name). Reported **raw** counts remain whatever capture read from `usage` and are **always emitted** when present — weights must not gate raw emit.

Weights are dimensionless relative to the **stable unit**. The control layer does not name or convert currencies. Invoice cost stays in the external meter.

When weighted usage is not a simple product sum — e.g. OpenAI-style overlap where `cached_tokens` ⊂ `prompt_tokens` — use optional **`weightExpr`** over unit names and weights. Profiles may ship a **`defaultWeightExpr`**. An explicit `weightExpr` on the ModelRef overrides the profile default.

Capture keeps **raw** API counts so showback matches the wire format. Overlap is handled in weighting, not by inventing a reduced “input” counter. Example OpenAI-shaped default:

`(prompt - cached) * weights.input + cached * weights.cached + output * weights.output`

(with missing `cached` treated as 0).

### External metering (cost + go/no-go)

```text
Gateway emit (raw units ± weights)
        → durable plane → CloudEvents
        → external meter (Price Tag / Monetize 360 / Cost Management / BYO)
        → calculates cost / wallet
        → pushes go/no-go into gateway local state
        → admit checks local state only
```

- **3.7 Must:** accurate export so the meter can bind.
- **Talk-back:** push as fast as practical; lag accepted; **cut-off policy / overrun owned by the meter**.
- Gateway may mirror go/no-go state only — not a second wallet ledger.

### Sketches

#### Capture + weights

```yaml
# Sketch — names subject to API review
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: llama-3
  namespace: llm
spec:
  usageCapture:
    profile: openai-chat-basic          # built-in name
  weights:
    perUnit:
      prompt: "1"
      output: "20"
```

#### OpenAI chat with cache-aware weights

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: llama-3
  namespace: llm
spec:
  usageCapture:
    profile: openai-chat
  weights:
    perUnit:
      input: "1"
      cached: "0.1"
      output: "20"
      reasoning: "30"
```

#### ConfigMap-backed custom profile (shared)

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: maas-usage-capture-profiles
  namespace: models-as-a-service          # illustrative
data:
  my-vendor-chat.yaml: |
    usageUnits:
      - name: prompt
        path: usage.prompt_tokens
      - name: output
        path: usage.completion_tokens
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: vendor-model
  namespace: llm
spec:
  usageCapture:
    profileRef:
      name: maas-usage-capture-profiles
      key: my-vendor-chat.yaml
  weights:
    perUnit:
      prompt: "1"
      output: "20"
```

#### Inline profile on `MaaSModelRef`

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: weird-vendor-model
  namespace: llm
spec:
  usageCapture:
    usageUnits:
      - name: prompt
        path: usage.input_tokens
      - name: output
        path: usage.output_tokens
    weightExpr: >
      prompt * weights.prompt + output * weights.output
  weights:
    perUnit:
      prompt: "1"
      output: "20"
```

Built-in expansion for `profile: openai-chat` (illustrative):

```yaml
usageUnits:
  - name: prompt
    path: usage.prompt_tokens
  - name: cached
    path: usage.prompt_tokens_details.cached_tokens
  - name: output
    path: usage.completion_tokens
  - name: reasoning
    path: usage.completion_tokens_details.reasoning_tokens
defaultWeightExpr: >
  (prompt - cached) * weights.input
  + cached * weights.cached
  + output * weights.output
  + reasoning * weights.reasoning
```

Built-in expansion for `profile: anthropic-messages` (illustrative):

```yaml
usageUnits:
  - name: input
    path: usage.input_tokens
  - name: cache_read
    path: usage.cache_read_input_tokens
  - name: cache_creation
    path: usage.cache_creation_input_tokens
  - name: output
    path: usage.output_tokens
# No defaultWeightExpr required — fields are disjoint
```

#### Anthropic Messages with cache-aware weights

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: claude-sonnet
  namespace: llm
spec:
  usageCapture:
    profile: anthropic-messages
  weights:
    perUnit:
      input: "1"
      cache_read: "0.1"
      cache_creation: "1.25"
      output: "5"
```

#### Explicit `weightExpr`

```yaml
weights:
  perUnit:
    prompt: "1"
    cached: "0.5"
    output: "20"
  weightExpr: >
    (prompt - cached) * weights.prompt
    + cached * weights.cached
    + output * weights.output
```

#### Subscription weight override

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: premium-team
spec:
  modelRefs:
    - name: llama-3
      namespace: llm
      weights:
        perUnit:
          output: "15"                 # entitlement override only
    - name: text-embed
      namespace: llm
      # inherits ModelRef weights (if any)
```

### Working rules

- Raw usage-unit counts are always emitted when present; weights are optional enrichment.
- Default weighted usage is `Σ (unit_count × unit_weight)` unless a profile `defaultWeightExpr` or explicit `weightExpr` applies.
- Subscription weight overrides refer only to usage units defined by the model’s capture.
- Prefer built-in `usageCapture.profile` for common shapes; `profileRef` for shared customs; inline for one-offs.
- When inline capture is present alongside `profile` / `profileRef`, inline **replaces** the referenced expansion for that model (v1).
- No `UsageCaptureProfile` CRD — built-ins + ConfigMap + inline ModelRef are enough.

### Backend dependency (direction only)

| Layer | Owns |
| --- | --- |
| **MaaS control layer (this proposal)** | Capture + optional weights on `MaaSModelRef`; optional weight overrides on `MaaSSubscription`; reconcile into emit / QoS consumers |
| **Gateway / Praxis** | Hot-path emit; durable handoff; local go/no-go state when talk-back is enabled |
| **External metering** | Cost, wallet, cut-off policy / overrun, go/no-go push; CE consumer |

## Consequences

**Positive**

- Accurate multi-type emit without forcing a local billing product.
- Avoids dual ledgers vs Price Tag / Monetize 360 / Cost Management.
- Weights support observability and optional weighted rate limiting without claiming invoice authority.
- Capture stays flexible across providers via profiles.

**Negative / risks**

- Spend cut-off has **lag** / **overrun**; must document that the meter owns cut-off policy clearly for customers.
- Operators must maintain capture paths when providers change `usage` shapes.
- Weighting overlapping units with **only** independent weights (no profile `defaultWeightExpr` / `weightExpr`) silently double-counts — needs docs + validation.
- Existing `billingRate.perToken` overlaps this surface — reconcile or deprecate in API review.
- Push talk-back contract and freshness SLA still open.

## Open Questions

| # | Topic | Notes |
| --- | --- | --- |
| W1 | **Field naming** | `weights` vs reuse/replace `billingRate.perToken` |
| W2 | **Path language** | Dotted path vs JSONPath; arrays / streaming usage |
| W3 | **Missing path on response** | Zero vs incomplete emit |
| W4 | **Override merge** | Partial per-unit override vs replace entire weight map |
| W5 | **Simple total-token mode** | First-class alongside typed units, or one capture entry named `total` |
| W6 | **Weight scale** | Per-token vs per-1K; portal display of stable-unit-weighted usage |
| W7 | **When to require / warn on `weightExpr`** | If `prompt` and `cached` both have weights and no expression |
| W8 | **Profile catalog** | ConfigMap namespace / RBAC / schema; infer profile vs require explicit |
| W9 | **`weightExpr` language** | Minimal ops for v1; ModelRef only vs subscription override |
| E1 | **Push talk-back contract** | Payload, correlation IDs, freshness, fail-open vs fail-closed |
| E2 | **Push latency** | Meter decision → local admit state |
| E3 | **Deny code** | Distinct from QoS 429 when go/no-go is deny |
| E4 | **Integration commitments** | Price Tag + Monetize 360; Cost Management yes/no |
