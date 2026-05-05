# Authorino Caching Configuration

This document describes how to tune Authorino cache TTLs for metadata and authorization evaluators in MaaS.

---

## What is Cached

MaaS-generated AuthPolicy resources enable caching on:

- **Metadata evaluators** (HTTP calls to maas-api):
  - `apiKeyValidation` - validates API keys and returns user identity + groups
  - `subscription-info` - selects the appropriate subscription for the request

- **Authorization evaluators** (OPA policy evaluation):
  - `auth-valid`, `subscription-valid`, `require-group-membership`

Caching reduces load on maas-api and OPA CPU by reusing results when the cache key repeats within the TTL window. Cache keys include user ID, groups, subscription, and model to prevent cross-principal or cross-subscription cache sharing.

---

## Configuration

### Environment Variables

The maas-controller deployment supports the following environment variables:

| Variable | Description | Default | Constraints |
|----------|-------------|---------|-------------|
| `METADATA_CACHE_TTL` | TTL for metadata HTTP caching (seconds) | `60` | Must be ≥ 0 |
| `AUTHZ_CACHE_TTL` | TTL for OPA authorization caching (seconds) | `60` | Must be ≥ 0 |

**Authorization cache TTL capping:** Authorization caches are automatically capped at the metadata cache TTL to prevent stale authorization decisions. If `AUTHZ_CACHE_TTL > METADATA_CACHE_TTL`, the authorization cache uses the metadata TTL instead, and a warning is logged at startup.

### Deployment Configuration

#### Via params.env (ODH Overlay)

Edit `deployment/overlays/odh/params.env`:

```env
metadata-cache-ttl=300  # 5 minutes
authz-cache-ttl=30      # 30 seconds
```

These values are injected into the maas-controller deployment via ConfigMap.

#### Via manager.yaml (Base Deployment)

Edit `deployment/base/maas-controller/manager/manager.yaml` to change hardcoded values and add env vars:

```yaml
args:
  # ... other args ...
  - --metadata-cache-ttl=$(METADATA_CACHE_TTL)  # Change from hardcoded 60
  - --authz-cache-ttl=$(AUTHZ_CACHE_TTL)        # Change from hardcoded 60
env:
  - name: METADATA_CACHE_TTL
    value: "300"  # 5 minutes
  - name: AUTHZ_CACHE_TTL
    value: "30"   # 30 seconds
  # ... other env vars ...
```

> **Note on Customization:** Direct modification of Authorino deployments or settings may interact with operator reconciliation during upgrades. Test changes in non-production environments and document any customizations for support handoff.

---

## Operational Impact

### Stale Data Window

Cache TTL represents the maximum staleness window for access control changes:

- **API key revocation or group membership changes:** May take up to `METADATA_CACHE_TTL` seconds to propagate
- **Subscription selection:** If a user's group membership changes, the cached subscription selection uses the old groups until the TTL expires (up to `METADATA_CACHE_TTL` seconds)
- **Authorization policy changes:** May take up to the effective authorization cache TTL (the minimum of `AUTHZ_CACHE_TTL` and `METADATA_CACHE_TTL`) to propagate

For immediate enforcement after changes:
1. Delete the affected AuthPolicy to clear Authorino's cache (triggers reconciliation)
2. Restart Authorino pods to force cache invalidation (disruptive; use during maintenance windows)
3. Or wait for the TTL to expire

### When to Tune

**Increase metadata cache TTL** if:
- maas-api experiences high load from repeated validation calls
- API key metadata changes infrequently
- Example: `METADATA_CACHE_TTL=300` (5 minutes) reduces maas-api load by 5x

**Decrease authorization cache TTL** if:
- Users are frequently added/removed from groups
- Faster access change propagation is required for compliance
- Example: `AUTHZ_CACHE_TTL=30` (30 seconds) for faster group membership updates

**Monitor after changes:**
- maas-api load: Reduced `/internal/v1/api-keys/validate` and `/internal/v1/subscriptions/select` call rates
- Authorino CPU: Reduced OPA evaluation usage
- Request latency: Cache hits should show lower P99 latency

---

## References

- [AuthPolicy Reference](https://docs.kuadrant.io/latest/kuadrant-operator/doc/reference/authpolicy/)
- [MaaS Controller Overview](./maas-controller-overview.md)
