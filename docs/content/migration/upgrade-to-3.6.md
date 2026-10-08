# Upgrade to 3.6

## Recommended downtime

When upgrading from RHOAI **3.5** to **3.6**, schedule a **short maintenance window**.

MaaS rewrites gateway `AuthPolicy` identity fields and per-model `TokenRateLimitPolicy` predicates so **new** subscriptions can rate-limit on a short ID from maas-api. Until maas-api, Authorino’s subscription-info cache, and the controller-managed policies are all on the new contract, rate-limit matching for those new subscriptions can briefly fail open. A small planned downtime (or a traffic pause through the gateway) avoids that window.

Existing subscriptions keep matching on `selected_subscription_key` and do not depend on `rateLimitId` being present.

## Existing subscriptions keep their live quotas

Subscriptions that already exist at upgrade time stay on the pre-1546 identity:

- TRLP **`when`** predicates and Limitador **counters** continue to use `auth.identity.selected_subscription_key`.
- maas-api does **not** return `rateLimitId` for those CRs.
- The controller stamps `maas.opendatahub.io/rate-limit-identity: legacy` so a later status update cannot silently switch them to short IDs.

Subscriptions **created after** the 3.6 controller is running are stamped `…/rate-limit-identity: short`. Those use `selected_subscription_id` (16-hex SHA-256 of `namespace/name@modelNs/model`) for matching and counters, and may expose `X-MaaS-Subscription-Rate-Limit-Id` when select returns `rateLimitId`.

Delete-and-recreate of an old subscription is treated as a **new** CR: it opts into short IDs and starts a **fresh** Limitador counter for that subscription. In-place spec edits do not migrate identity.

## Trade-off: size savings only accrue on new subscriptions

Short IDs exist to keep long `namespace/name@modelNs/model` strings out of Kuadrant WASM / EnvoyFilter predicates (the ~1.5 MiB object-size problem). Grandfathering existing CRs buys quota continuity at the cost of that size win:

| | Existing subscriptions | Subscriptions created after upgrade |
|--|--|--|
| TRLP match / counters | `selected_subscription_key` (long string in each predicate clause) | `selected_subscription_id` (16 hex chars) |
| Live Limitador quota | Unchanged by the identity change | Starts at zero (correct for a new CR) |
| EnvoyFilter / WasmPlugin size | Still pays the long-key clause cost | Compact clauses |

Further costs while a cluster has both generations:

- **A mixed rate group splits into two TRLP limits** (`tokens-{limit}-per-{window}` for legacy members, `tokens-{limit}-per-{window}-id` for short-ID members). Kuadrant copies every limit into every route-match ActionSet, so mixed generations partially undo rate grouping ([RHOAIENG-95277](https://issues.redhat.com/browse/RHOAIENG-95277)) until the legacy members are gone.
- There is **no automatic backfill**. To reclaim the size win for an old subscription you must recreate it (or set the annotation to `short`), which **resets that subscription’s in-window quota**.
- Historical **Prometheus** and **Loki** usage is unaffected either way. Telemetry still labels by subscription name (`auth.identity.selected_subscription` / `X-MaaS-Subscription`).

If the cluster still uses per-subscription TRLP limit names from before rate grouping, those names still change to `tokens-{limit}-per-{window}` on upgrade. That rename is independent of short IDs and can still start fresh Limitador counters for the current window.

## What else to know

- `auth.identity.selected_subscription_key` remains for telemetry and debugging, and is still the rate-limit identity for legacy subscriptions.
- Rate limiting for **new** subscriptions matches `auth.identity.selected_subscription_id` (see [Authentication Internals](../architecture-internals/authentication-internals.md)).
- Response header `X-MaaS-Subscription-Rate-Limit-Id` may be injected when select returns `rateLimitId` (new subscriptions only). Gateway AuthPolicy rejects inbound client values for that header; also ensure Praxis / identity-header strip configs treat `x-maas-*` (prefix or explicit list) so it cannot be spoofed upstream.
