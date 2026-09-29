# Subscription Cardinality

Cardinality refers to the number of unique label combinations that Limitador and Prometheus must track across rate-limiting counters and metrics. High cardinality increases memory consumption, slows queries, and can destabilize both rate limiting and observability.

This guide explains the cardinality dimensions in MaaS, when they become a problem, and how to keep them under control.

## How Cardinality Arises

The controller generates one Kuadrant **TokenRateLimitPolicy** (TRLP) per model, shared by every MaaSSubscription that references it. The TRLP has one limit per distinct set of `tokenRateLimits` on that model, so subscriptions with identical rates share a limit. Each limit counts per subscription and per user (counters `selected_subscription_key` and `userid`), so sharing a limit never means sharing a budget. Limitador maintains a separate counter for each unique combination of those values. The TelemetryPolicy adds further labels (`user`, `subscription`, `model`, `organization_id`, and optionally `group`) to usage metrics.

Limits and counters scale differently:

```
limits per model = distinct rate sets among the subscriptions on that model
counters         ≈ subscriptions × models × unique_users × rate_limit_windows
```

Model references with `unlimited: true` add no counters. All unlimited subscriptions on a model share one limit without rates, so Limitador only records their usage metrics.

For Prometheus, the cardinality of `authorized_hits`, `authorized_calls`, and `limited_calls` grows with the number of distinct `user` and `subscription` label values.

## Gateway Config Size

Kuadrant renders every TRLP limit into one wasm config object per gateway, `kuadrant-{gateway-name}` in the gateway namespace (an `EnvoyFilter` on Kuadrant 1.5 and later, a `WasmPlugin` on 1.4.x). Each limit becomes two actions, a request-time check and a response-time token report, in every route-match ActionSet, so the object grows roughly with:

```
size ≈ limits × route_matches × listeners × hostnames × 2 actions
```

At 19 route matches, one limit costs about 27 KB per listener. Before RHOAIENG-95277 every subscription and model pair got its own limit, and the object grew until it hit the etcd object size limit (1.5 MiB by default). Now a subscription at an existing rate adds one clause to that limit's predicate: about 80 bytes in the TRLP and about 3 KB per listener in the gateway config. A subscription at a new rate still adds a whole limit.

To keep the object small, reuse a small set of standard rates across subscriptions instead of giving each subscription its own numbers. Spell them the same way too: `100/1m` and `100/60s` are different rate sets.

```bash
# Gateway wasm config size in bytes, as compact JSON. Kuadrant 1.5+ writes an EnvoyFilter:
kubectl get envoyfilter kuadrant-maas-default-gateway -n openshift-ingress -o json | jq -c . | wc -c

# Kuadrant 1.4.x writes a WasmPlugin with the same name instead:
kubectl get wasmplugin kuadrant-maas-default-gateway -n openshift-ingress -o json | jq -c . | wc -c

# Limits per model TRLP
kubectl get tokenratelimitpolicy -A -o json | \
  jq -r '.items[] | "\(.metadata.namespace)/\(.metadata.name): \(.spec.limits | length) limits"'
```

## Users vs Groups

MaaSAuthPolicy and MaaSSubscription both accept `users` and `groups` in their subject/owner fields. The choice directly affects cardinality.

### Prefer groups for human users

Groups create **one** identity entry per group, regardless of how many users belong to that group. Adding a user to a group does not add a new counter or label value — the user inherits the group's subscription.

```yaml
spec:
  owner:
    groups:
      - name: data-science-team    # One counter, many users
    users: []
```

### Reserve users for service accounts

The `users` field creates a **separate counter per entry**. Listing 500 individual human users in `owner.users` creates 500 distinct rate-limit counters per model per window — and 500 distinct `user` label values in Prometheus metrics.

Use `users` only for:

- Kubernetes **ServiceAccounts** used by CI/CD pipelines or automation
- Programmatic identities that need individual rate limits distinct from any group

```yaml
spec:
  owner:
    groups:
      - name: ml-engineers          # Human users go here
    users:
      - system:serviceaccount:ci:pipeline-bot   # Individual tracking justified
```

### Impact summary

| Approach | Counters per model per window | Prometheus label values | Recommended for |
|----------|-------------------------------|------------------------|-----------------|
| 1 group with 500 users | 1 | 1 subscription value | Human users |
| 500 individual users | 500 | 500 user values | Not recommended |
| 5 service accounts | 5 | 5 user values | CI/CD, automation |

## Telemetry Label Cardinality

The MaasTenantConfig CR controls which labels the TelemetryPolicy adds to Limitador usage metrics. Each enabled label multiplies cardinality.

| Field | Default | Cardinality impact | Recommendation |
|-------|---------|-------------------|----------------|
| `captureOrganization` | `true` | Low — typically a handful of orgs | Safe to enable |
| `captureUser` | `false` | **High** — one value per authenticated user | Enable only if you need per-user billing and accept the cost; has GDPR implications |
| `captureGroup` | `false` | **High** — one value per group membership; users in multiple groups multiply counters | Leave disabled unless you specifically need group-level metrics |
| `captureModelUsage` | `true` | Moderate — one value per model | Safe for typical deployments (tens of models) |

To check or change these settings:

```bash
kubectl get maastenantconfig default-tenant -n models-as-a-service \
  -o jsonpath='{.spec.telemetry.metrics}' | jq .
```

To disable a high-cardinality label:

```bash
kubectl patch maastenantconfig default-tenant -n models-as-a-service --type=merge \
  -p '{"spec":{"telemetry":{"metrics":{"captureGroup": false}}}}'
```

## Cardinality Limits and Observed Impact

### Limitador

Limitador stores one counter per unique combination of (namespace, label values, window). When counters grow large:

- **Memory usage increases** — each counter consumes memory in Limitador pods. With Redis persistence, this also increases Redis memory.
- **Lookup latency rises** — counter lookups become slower as the keyspace grows.
- **Pod restarts lose state** — without Redis persistence, all counters reset on pod restart (see [Limitador Persistence](limitador-persistence.md)).

There is no hard-coded limit on counter count in Limitador, but practical limits depend on your pod memory allocation and whether you use in-memory or Redis storage.

### Prometheus

High cardinality in `authorized_hits`, `authorized_calls`, and `limited_calls` metrics affects:

- **Prometheus memory and storage** — each unique label combination creates a new time series.
- **Query performance** — queries like `sum by (user) (rate(authorized_hits[5m]))` become slow when thousands of user values exist.
- **Dashboard responsiveness** — Perses panels using high-cardinality metrics may time out.

Gateway latency metrics (`istio_request_duration_milliseconds_bucket`) are labeled by **subscription only** (not by user) specifically to keep cardinality bounded. See [Metrics & Dashboards](../observability/metrics-and-dashboards.md#per-subscription-latency-tracking).

## Monitoring Cardinality

### Check current counter counts

Query Limitador's counter count through Prometheus:

```promql
# Number of active rate-limit counters
limitador_up
```

### Check Prometheus cardinality

```promql
# Count unique user label values on usage metrics
count(count by (user) (authorized_calls))

# Count unique subscription label values
count(count by (subscription) (authorized_calls))

# Total time series for MaaS usage metrics
count({__name__=~"authorized_hits|authorized_calls|limited_calls"})
```

### Check from the CLI

```bash
# List all subscriptions and their owner counts
kubectl get maassubscription -n models-as-a-service -o json | \
  jq -r '.items[] | "\(.metadata.name): groups=\(.spec.owner.groups // [] | length), users=\(.spec.owner.users // [] | length), models=\(.spec.modelRefs | length)"'

# Count total TokenRateLimitPolicies (one per model)
kubectl get tokenratelimitpolicy -A --no-headers | wc -l
```

## Troubleshooting

### Symptom: Limitador memory usage is growing

**Likely cause:** Large number of individual users in `owner.users` across multiple subscriptions.

**Diagnosis:**

```bash
# Find subscriptions with many individual users
kubectl get maassubscription -n models-as-a-service -o json | \
  jq -r '.items[] | select((.spec.owner.users // []) | length > 10) | "\(.metadata.name): \(.spec.owner.users | length) users"'
```

**Fix:** Migrate individual users to groups. Create a Kubernetes group, add users to it, and reference the group in the subscription's `owner.groups` instead of listing users individually.

### Symptom: Prometheus queries for usage metrics are slow

**Likely cause:** `captureUser: true` or `captureGroup: true` in the Tenant telemetry config with many users/groups.

**Diagnosis:**

```promql
# Check cardinality of the user label
count(count by (user) (authorized_hits))
```

If this returns hundreds or more, the `user` label is driving high cardinality.

**Fix:** Disable `captureUser` or `captureGroup` if per-user or per-group metrics are not required for billing. Per-user token data remains available through the MaaS API even with metrics labels disabled.

### Symptom: Rate limiting is not applied consistently

**Likely cause:** Multiple TokenRateLimitPolicies targeting the same HTTPRoute (see the [shared route warning](../configuration-and-management/quota-and-access-configuration.md#3-define-subscriptions-maassubscription) in the quota configuration guide).

**Diagnosis:**

```bash
# Find TRLPs sharing the same HTTPRoute
kubectl get tokenratelimitpolicy -A -o json | \
  jq -r '.items[] | select(.spec.targetRef.kind=="HTTPRoute") | "\(.metadata.namespace)/\(.metadata.name) → \(.spec.targetRef.name)"' | \
  sort | uniq -d -f2
```

**Fix:** Use dedicated HTTPRoutes per model to ensure independent rate limiting.

## Best Practices

1. **Use groups, not individual users** — assign human users to Kubernetes groups and reference groups in `owner.groups`. Reserve `owner.users` for service accounts.
2. **Keep telemetry labels minimal** — leave `captureUser` and `captureGroup` disabled unless you have a specific billing or compliance requirement.
3. **Monitor counter growth** — periodically check the number of TokenRateLimitPolicies and Prometheus time series for MaaS metrics.
4. **Use Redis for Limitador** in production — persistent storage prevents counter resets on pod restarts and provides better visibility into counter counts. See [Limitador Persistence](limitador-persistence.md).
5. **Use dedicated routes per model** — avoids TRLP conflicts and simplifies cardinality accounting.
6. **Reuse a small set of rates** - every distinct rate set on a model adds a limit to the gateway wasm config, while another subscription at an existing rate adds only a predicate clause. See [Gateway Config Size](#gateway-config-size).

## Related Documentation

- [Quota and Access Configuration](../configuration-and-management/quota-and-access-configuration.md) — step-by-step subscription setup including the `users` field warning
- [Observability](../observability/index.md) — metrics collection, TelemetryPolicy labels, and dashboard configuration
- [Limitador Persistence](limitador-persistence.md) — configuring Redis for persistent rate-limit counters
- [MaaSSubscription CRD Reference](../reference/crds/maas-subscription.md) — full CRD field reference
