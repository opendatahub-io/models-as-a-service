---
title: Subscription rate card and period allotment (control layer)
description: Proposed MaaSSubscription control layer for model rate cards and period spend allotments, with enforcement delegated to a Tokenomics-shaped backend.
---

# Subscription rate card and period allotment (control layer)

> [!NOTE]
> **Proposed design**
>
> This document describes a planned control-layer contract. It is not documentation of an implemented feature.
> Enforcement, counters, renewal, and remaining-quota reads are owned by a gateway / Tokenomics-style backend.
> Field names in the YAML sketch are illustrative and subject to API review.

**Author:** James Land

**Date:** 2026-10-22

**Scope:** Models-as-a-Service control plane (`MaaSSubscription`); AI Gateway / Praxis Tokenomics-shaped enforcement backend

**Related:** [Tokenomics: API + Capabilities (praxis-proxy/enhancements#32)](https://github.com/praxis-proxy/enhancements/pull/32)

## What

Add a **control layer** on `MaaSSubscription` that:

1. Declares an optional **rate card** (`pricing` on each `modelRef`) used for showback / cost attribution when every model on the subscription is priced.
2. Declares an optional **period allotment** (`budgetPolicy`) — a spend cap for a fixed duration window, with renewal behavior (`drain` | `rollover`).
3. Leaves **enforcement and counters** to a pluggable gateway / metering backend (implementation TBD). MaaS owns the declarative contract and reconciliation into that backend; MaaS does **not** become the durable “remaining budget” database in v1.

## Why

Operators need a clear split between:

- **How much does a call cost?** (rate card / showback)
- **How much may this identity spend in a window?** (allotment / FinOps cap)

Without that split, teams conflate prepaid wallets, calendar quotas, and per-model prices. MaaS v1 targets **period allotment** (fixed-duration spend cap with renewal) as the enterprise-default path. Prepaid **wallet** UX (admin top-up / auto-refill) is deferred.

## Goals

- One declarative place for rate card + allotment: `MaaSSubscription`.
- **Observe-only** cost attribution when all models have `pricing`; no enforcement required for showback alone.
- Explicit **`enforcement`**: `hard` | `soft` on the allotment (maps to backend hard deny vs soft over-quota signal). Prefer product default of `hard` when unset only if API review allows a default; otherwise require the field.
- Optional **alerts** (e.g. threshold percentages) — backend-dependent; do not overload `enforcement` with alert thresholds.
- Enforcement key: **`userId` + `subscriptionId`** (service accounts treated as human-equivalent identities).
- Period length required when allotment is set; docs recommend **30d** as the default example (not a silent product default that hides misconfiguration).
- Renewal as a nested object: **`mode`** (`drain` | `rollover`) + **`period`** (wallet mode not in v1; optional `anchor` / `max_carry` as backend allows).
- Cross-subscription pools are **BYO** (operator wires external systems); not a first-class MaaS CR in v1.
- Disable / remove allotment: follow backend-compatible teardown (no orphan counters).

## Non-Goals (v1)

- Prepaid **wallet** product UX (admin top-up, auto-refill, payment-processor-style balance) — deferred to a later design.
- Calendar-month / timezone-aligned periods — parked; prefer fixed duration until the backend calendar story is clear.
- MaaS-owned **remaining balance** DB or portal as source of truth.
- Hierarchical org → team → key pools as first-class CRs.
- Per-user allotment override beyond the subscription policy.
- Raising one user’s cap without changing the subscription (parked).
- Locking the MaaS CRD to a specific store product or a frozen Tokenomics revision. Working assumption is Tokenomics-shaped ([praxis-proxy/enhancements#32](https://github.com/praxis-proxy/enhancements/pull/32)); the contract can and probably will change.

## Proposed Design

### Backend dependency

| Layer | Owns | Examples |
| --- | --- | --- |
| **MaaS control layer (this proposal)** | Declarative rate card + allotment on `MaaSSubscription`; reconcile/publish into the backend; UX/docs for operators | `modelRef.pricing`, `budgetPolicy`, identity keying intent |
| **Enforcement backend (assumed Tokenomics-like)** | Admit/reserve/settle, durable counters, renewal at period boundary, hard/soft outcomes, store-failure matrix, remaining-quota reporting APIs | drain/rollover, UTC anchors, soft over-quota signals, 429 vs 503 |

**Backend assumption (not a lock-in):** Enforcement, counters, renewal, fail behavior, soft-limit signals, and “remaining quota” reads are owned by a **gateway / Tokenomics-style backend**, not by the MaaS CRD. The current working assumption is that this backend will look like [praxis-proxy/enhancements#32 (Tokenomics: API + Capabilities)](https://github.com/praxis-proxy/enhancements/pull/32). That proposal is **still iterating** (further revisions are expected before implementation readiness). MaaS field names and reconcile behavior should stay mappable; do not freeze product semantics on any single draft of #32.

If the shipped backend differs from today’s #32 draft, prefer **adapting the reconcile mapping** over inventing a second ledger in MaaS. Open questions below mark where MaaS cannot decide alone.

### Rate card

- On each `modelRef`, optional `pricing` (unit + amount). **One unit system** for the subscription (e.g. USD per 1K tokens or per request — exact schema in API review).
- **All-or-nothing for observe path:** if any model lacks pricing, do not enable subscription-wide observe-only cost path; reject adding unpriced models when pricing mode is already active (or require completing the card).
- When **all** `modelRef` entries have pricing → **observe-only** showback/cost attribution may run without an allotment.
- Allotment (`budgetPolicy`) is independent and optional; prices without allotment ≠ enforcement.

### Allotment

Optional `budgetPolicy` on the subscription, for example:

```yaml
# Sketch — names subject to API review
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: example
spec:
  modelRefs:
    - name: llama-3
      pricing:
        unit: USD  # illustrative; must match the subscription accounting unit
        amount: "0.002"
  budgetPolicy:
    limit: "200"            # allotment in same unit as pricing (here USD)
    renewal:
      mode: drain           # drain | rollover
      period: 30d           # required when budgetPolicy present
      # anchor: "2026-01-01T00:00:00Z"  # optional; UTC period anchor
      # max_carry: "100"               # optional; rollover only
    enforcement: hard       # hard | soft (required with budgetPolicy, or default hard)
    # alerts:               # optional; backend-dependent
    #   thresholds: [80, 100]
```

Shape intentionally tracks a Tokenomics-style budget (`limit`, nested `renewal`, `enforcement`, optional `alerts`) so reconcile is a mapping, not a redesign. MaaS may omit backend-only fields (`costs` references, named budget catalogs) until needed.

- **`enforcement`** is first-class (`hard` | `soft`); do not encode soft-only behavior as a separate `softLimit` money field.
- Counters / admit path: **lazy seed** on first use (no eager per-user rows for every possible user).
- Reconciliation publishes policy into the chosen enforcement backend; **publish path** details are an open implementation question (see below).

### Identity and period

- Cap key: **`userId` + `subscriptionId`**.
- Period: **fixed duration** (`period` required with policy). Calendar alignment parked.
- Service accounts vs humans: same keying rules (no special SA unlimited path in v1).

### Failure and teardown (direction)

- Store / accounting unavailability: **backend-defined**. Current Tokenomics draft leans **fail-closed for new admission** when accounting is untrustworthy (e.g. pre-dispatch `503`), including soft policies — confirm against the revision we ship against.
- Disable allotment / remove policy: teardown counters per backend-compatible disable path (no orphan counters).

## Consequences

**Positive**

- Clear product story: rate card vs period cap vs (later) wallet.
- Aligns with enterprise FinOps expectations (allotment + renewal).
- Keeps MaaS declarative; avoids inventing a second ledger in the control plane.
- Stays mappable to a Tokenomics-style contract without hard-coding a store product in the CRD.

**Negative / risks**

- Operators who want prepaid wallet UX must wait or BYO.
- Fixed-duration periods may surprise users expecting calendar months.
- Portal “remaining” UX depends on backend APIs (not MaaS DB).
- All-or-nothing pricing can block partial rate cards until complete.
- **Coupling risk:** if #32 (or its successor) changes renewal, tenant model, or failure semantics, MaaS reconcile and docs must track — this control-layer proposal should not over-promise those behaviors.

## Open Questions

Many items below are **backend-dependent**. Working assumption: enforcement backend ≈ [Tokenomics PR #32](https://github.com/praxis-proxy/enhancements/pull/32), which **can and probably will change** before implementation freeze. Resolve jointly with Praxis/Tokenomics owners; do not treat answers below as MaaS-only product calls.

### A. Backend contract (assumed Tokenomics-shaped)

| # | Topic | Why open | Dependency | Notes / current #32 lean (may change) |
| --- | --- | --- | --- | --- |
| A1 | **Which backend revision we ship against** | #32 is explicitly not the last iteration | Backend | Track proposal + follow-on PRs; pin MaaS reconcile to a named contract version when possible |
| A2 | **MaaS as “budget manager” vs thinner publisher** | Tokenomics splits Praxis (enforce) vs budget manager (policies, durable history, managed store) | Backend + MaaS | Does MaaS own managed-store ops, only publish YAML/policy revisions, or call a separate manager? |
| A3 | **Tenant / identity mapping** | Tokenomics tenants, header selection, parent/child allocations | Backend + MaaS | How does `userId` + `subscriptionId` map to tenant + budget selection? Reserved vs shared child allocations needed in v1? |
| A4 | **Publish / reconcile path** | Controller must push subscription policy into the backend | Backend + MaaS | ConfigMap/YAML? API? CR watch? Idempotent revisions? Migration when budgets reassign? |
| A5 | **Period, anchor, timezone** | Fixed duration first; calendar parked | Backend | #32: fixed duration + UTC anchor; calendar-month out of first iteration (`30d` ≠ calendar month). Unpark calendar only when backend supports it |
| A6 | **Renewal details** | drain \| rollover locked at product level; edge cases are backend | Backend | #32: no default; optional `max_carry` on rollover; debt/holds across boundaries. Does MaaS surface `max_carry` / anchor in CRD v1? |
| A7 | **Hard vs soft semantics** | Soft = over-quota signal / continue, not a second product | Backend | Map `budgetPolicy.enforcement` to backend hard/soft. Soft still reserves/settles and does **not** bypass store failures. Optional `alerts` vs soft enforcement — keep distinct |
| A8 | **Fail-open vs fail-closed** | Must be explicit before GA | Backend | #32 matrix: untrustworthy accounting → block new admission (`503`), preserve in-flight responses after dispatch. Confirm product wording (not classic “fail-open”) |
| A9 | **Remaining / reporting limit (current quota)** | Portal and operators need “how much left?” | Backend | Reporting a tenant’s current quota/remaining should be an explicit backend capability. MaaS portal reads backend; no MaaS remaining DB |
| A10 | **Observe-only / reporting without budgets** | Prices without allotment | Backend | #32: reporting without budget engine is an explicit pipeline choice. Map MaaS “all models priced ⇒ observe” onto that path |
| A11 | **Cost / unit schema** | Money vs weighted tokens vs raw tokens | Backend + MaaS | #32: one accounting method/unit per tenant hierarchy; invalid costs fail even under soft. Align `modelRef.pricing` units with backend cost matching |
| A12 | **Disable / teardown** | Remove `budgetPolicy` without orphan state | Backend | Compatible disable path; confirm revision/migration rules when policy deleted |
| A13 | **Store topology (internal K/V vs HA)** | Ops concern, not CRD shape | Backend | Opaque to MaaS CRD; affects HA story and whether MaaS must run a manager |

### B. MaaS control-plane / product

| # | Topic | Why open | Notes |
| --- | --- | --- | --- |
| B1 | **Exact CRD field names** | Sketch only (`limit`, nested `renewal`, `enforcement`, optional `alerts`) | API review; prefer names that map cleanly to backend (`limit`/`renewal.mode`/`enforcement`) without leaking store internals |
| B2 | **All-or-nothing pricing UX** | Reject unpriced model add vs require completing card | Validation rules and status messages on `MaaSSubscription` |
| B3 | **Docs-default period (30d)** | Required field; docs example vs silent default | Keep required; document 30d as recommendation |
| B4 | **Portal UX timing** | Show remaining in v1 portal vs later | Depends on A9 existing; product priority only |
| B5 | **Raise one user’s cap** | Parked; not v1 | Would need backend override primitives (A3) if ever unparked |
| B6 | **Per-user / per-key overrides** | Beyond subscription policy | Not v1; hierarchical Tokenomics child allocations may cover some cases later |
| B7 | **Wallet mode** | Deferred product | Do not block this allotment proposal; design later if needed |
| B8 | **Cross-subscription pools** | BYO in v1 | Document escape hatch; no first-class CR |
| B9 | **Soft-limit notification channel** | Alert vs header-only vs both | Mostly A7; MaaS may only document operator expectations |
| B10 | **HTTP status product story** | 429 vs 402 vs 503 toward end users | Backend emits; MaaS docs must not invent a different code |

### C. Sequencing

1. Review current [#32](https://github.com/praxis-proxy/enhancements/pull/32) with Praxis owners; note which sections are still expected to move.
2. For each **A\*** row, record: *owned by backend* / *owned by MaaS* / *joint*, and the proposal revision referenced.
3. Freeze MaaS CRD sketch only after A3–A7 and A11 have a stable mapping story.
4. File implementation tickets after that freeze — not before.
