# Subscription and Policy Cardinality

MaaSAuthPolicy and MaaSSubscription support both `groups` and `users` in their subject/owner configuration. Using `users` for many individual human users can cause cardinality issues in the rate-limiting and policy enforcement layer (Limitador, Authorino), which may impact performance and scalability.

**Recommendation:** Prefer `groups` for human users. Reserve the `users` field for Service Accounts and other programmatic identities where the number of distinct users remains small.

!!! note "See also"
    For configuration guidance, see [Quota and Access Configuration](../configuration-and-management/quota-and-access-configuration.md).

## When to Use `users` vs `groups`

| Scenario | Recommended Field | Rationale |
|---------|------------------|-----------|
| Human users (SSO/LDAP) | `groups` | Users inherit access via group membership. Adding/removing users doesn't change the policy CR count. |
| Service accounts (CI/CD pipelines) | `users` | Service accounts often don't belong to meaningful groups. The number of SA identities is typically small and bounded. |
| Individual API consumers (< 50) | `users` | Acceptable when the total number of distinct users across all policies is small. |
| Large user populations (100+) | `groups` | Each user entry creates a distinct auth evaluation path. At scale, this increases Authorino evaluation time and Kuadrant policy size. |

## Cardinality Impact

Each MaaSSubscription generates one aggregated TokenRateLimitPolicy per model. The rate-limit key includes the user identity, so each unique user who accesses a model creates a separate counter in Limitador. This is the primary source of metric cardinality growth.

The `users` field in the CR itself does not directly cause high cardinality — but it correlates with it: policies that list many individual users tend to have many distinct users hitting the rate limiter.

For detailed cardinality projections, storage impact, and mitigation strategies, see [Telemetry Defaults and Cardinality Guidance](telemetry-defaults-and-cardinality.md).

## Monitoring Cardinality

Query the total active series from Limitador metrics:

```promql
count({__name__=~"authorized_hits|authorized_calls|limited_calls"})
```

If this exceeds 100k series, review the [mitigation strategies](telemetry-defaults-and-cardinality.md#mitigation-strategies).
