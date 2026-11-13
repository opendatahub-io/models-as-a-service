# Authorino Caching Configuration

This document describes how to tune the Authorino cache TTL for metadata evaluators in MaaS.

---

## What is Cached

MaaS-generated AuthPolicy resources cache the metadata evaluators that call maas-api:

- `apiKeyValidation` - validates API keys and returns user identity + groups
- `subscription-info` - selects the appropriate subscription for the request

Authorization evaluators are not cached. They recompute from the cached metadata on
every request; caching their results could retain an allow decision after newer
metadata contains a denial.

Caching reduces load on maas-api by reusing results when the cache key repeats within the TTL window. Cache keys are scoped to the individual API key (using the full unique key string, equivalent to keying on the token JTI), plus groups, subscription, and model, to prevent cross-principal or cross-subscription cache sharing.

---

## Configuration

### CLI Flags

The maas-controller deployment configures the cache TTL with a command-line flag:

- `--metadata-cache-ttl`: TTL for metadata HTTP caching in seconds. Default: `60`. Must be `>= 0`; `0` disables caching.
- `--authz-cache-ttl`: Deprecated and ignored. It remains accepted so existing deployments continue to start.


### Deployment Configuration

#### Via controller Deployment args (running cluster)

The cache TTL is configured as a command-line flag on the maas-controller Deployment. To change it on a running cluster, patch the Deployment args and restart:

```bash
CONTROLLER_NS=opendatahub

kubectl patch deployment maas-controller -n "$CONTROLLER_NS" --type='json' -p='[
  {"op":"replace","path":"/spec/template/spec/containers/0/args/6","value":"--metadata-cache-ttl=300"}
]'
```

#### Via manager.yaml (base deployment)

Edit `deployment/base/maas-controller/manager/manager.yaml` to change hardcoded values:

```yaml
args:
  # ... other args ...
  - --metadata-cache-ttl=300  # 5 minutes
```

> **Note on Customization:** Direct modification of Authorino deployments or settings may interact with operator reconciliation during upgrades. Test changes in non-production environments and document any customizations for support handoff.

---

## Operational Impact

### Stale Data Window

Cache TTL represents the maximum staleness window for access control changes:

- **API key revocation or group membership changes:** May take up to the configured metadata cache TTL to propagate
- **API key expiry:** Not delayed; maas-api returns expiry with validation, and the gateway checks it on every request
- **Subscription selection:** If a user's group membership changes, the cached subscription selection uses the old groups until the metadata cache TTL expires
- **Authorization policy changes:** May take up to the metadata cache TTL to propagate because the affected metadata can remain cached

#### Security implication: revocation lag

> **Known accepted trade-off (ASVS V3.3.1 / CWE-613):** A revoked API key can continue to produce successful inference responses for up to `--metadata-cache-ttl` seconds (default: **60 seconds**) after the revocation is recorded in maas-api. This is a deliberate trade-off — reducing the cache TTL was evaluated and rejected for performance reasons.
>
> The `apiKeyValidation` cache entry is scoped to the individual key's unique raw value (functionally equivalent to keying on JTI), so cache hits are always per-key. Authorino does not expose an active cache invalidation API, so there is no mechanism to evict a specific key's cache entry without the workarounds below.

For **immediate enforcement** after a sensitive revocation (e.g., a compromised key):

1. **Delete the affected AuthPolicy** — Authorino drops all cache entries for the policy and re-evaluates on the next request. The maas-controller will reconcile it back within seconds, so this causes only a brief auth disruption for that tenant.
   ```bash
   kubectl delete authpolicy <policy-name> -n <tenant-namespace>
   ```
2. **Restart Authorino pods** — forces a full cache flush across all tenants. Use only during maintenance windows.
   ```bash
   kubectl rollout restart deployment/authorino -n <authorino-namespace>
   ```
3. **Wait for TTL to expire** — acceptable for non-urgent revocations; the default window is 60 seconds.

### When to Tune

**Increase metadata cache TTL** if:
- maas-api experiences high load from repeated validation calls
- API key metadata changes infrequently
- Example: `--metadata-cache-ttl=300` (5 minutes) reduces maas-api load by 5x

**Decrease metadata cache TTL** if:
- Faster API key revocation or group-membership propagation is required (e.g., compliance or security policy requires a shorter staleness window)
- Note: this increases load on maas-api's `/internal/v1/api-keys/validate` endpoint proportionally; monitor request rates before and after

**Monitor after changes:**
- maas-api load: Reduced `/internal/v1/api-keys/validate` and `/internal/v1/subscriptions/select` call rates
- Authorino CPU: Reduced HTTP evaluator work
- Request latency: Cache hits should show lower P99 latency

---

## References

- [AuthPolicy Reference](https://docs.kuadrant.io/latest/kuadrant-operator/doc/reference/authpolicy/)
- [Controller Architecture](../architecture-internals/controller-architecture.md)
