# Upgrade to 3.6

## Recommended downtime

When upgrading from RHOAI **3.5** to **3.6**, schedule a **short maintenance window**.

MaaS rewrites gateway `AuthPolicy` identity fields and per-model `TokenRateLimitPolicy` predicates so rate limiting matches a short subscription rate-limit ID from maas-api. Until maas-api, Authorino’s subscription-info cache, and the controller-managed policies are all on the new contract, rate-limit matching can briefly fail open. A small planned downtime (or a traffic pause through the gateway) avoids that window.

## Token rate-limit counters reset

On upgrade, TRLP limit map keys change from long names
(`{namespace}-{subscription}-{model}-tokens`) to short keys (`rl-<16-hex-id>`).

Limitador **enforcement** counters for those limits start fresh: in-window token usage against subscription quotas resets. Users may see a one-time “full budget again” effect after upgrade until the next window elapses.

This does **not** rewrite or invalidate historical **Prometheus** or **Loki** usage data. Telemetry labels continue to use the human-readable subscription name (`auth.identity.selected_subscription` / `X-MaaS-Subscription`), not the rate-limit ID. Pre-upgrade series and logs remain queryable under the same subscription labels; only live Limitador quota state is affected.

## What else to know

- `auth.identity.selected_subscription_key` remains for telemetry and debugging.
- Rate limiting matches `auth.identity.selected_subscription_id` (see [Authentication Internals](../architecture-internals/authentication-internals.md)).
- Response header `X-MaaS-Subscription-Rate-Limit-Id` may be injected when select returns `rateLimitId`. Gateway AuthPolicy rejects inbound client values for that header; also ensure Praxis / identity-header strip configs treat `x-maas-*` (prefix or explicit list) so it cannot be spoofed upstream.
