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

Caching reduces load on maas-api by reusing results when the cache key repeats within the TTL window. The `apiKeyValidation` key is the API key itself. The `subscription-info` key covers every input of the subscription select request, so requests only share a cached result when they send maas-api the same request:

- **API keys:** the key ID, the key's username and bound subscription, and the model. A key's groups are fixed when it is created, so they are left out, and the `X-MaaS-Subscription` header does not change the cached result.
- **OpenShift and OIDC tokens:** the identity, groups, requested subscription, and model.

An entry larger than Authorino's per-entry limit is not cached; see [Cache Entry Size](#cache-entry-size).

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
ARGS=$(kubectl get deployment maas-controller -n "$CONTROLLER_NS" -o json |
  jq -c '.spec.template.spec.containers[0].args | map(select(startswith("--metadata-cache-ttl=") | not)) + ["--metadata-cache-ttl=300"]')

kubectl patch deployment maas-controller -n "$CONTROLLER_NS" --type='json' \
  -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args\",\"value\":${ARGS}}]"
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

- **API key revocation:** May take up to the configured metadata cache TTL to propagate
- **API key expiry:** Not delayed; maas-api returns expiry with validation, and the gateway checks it on every request
- **Group membership changes:** API keys keep the groups they were created with, so a change needs a new key. For OpenShift and OIDC tokens the groups are part of the `subscription-info` cache key, so a request with different groups never gets a cached selection
- **Subscription changes:** May take up to the metadata cache TTL to propagate, because a cached selection is reused until it expires
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

### Cache Entry Size

Authorino gives every cached evaluator its own cache of `evaluatorCacheSize` megabytes (default `1`), split into 256 segments. It does not store an entry (cache key plus cached response) larger than a quarter of a segment minus 24 bytes. An entry that does not fit is skipped without an error: the request still succeeds, but maas-api is called on every request.

MaaS wraps each cache key in a one-element list, which makes Authorino store a 32-byte hash of the key instead of the key itself. What counts against the limit is therefore the cached response.

| `evaluatorCacheSize` | Largest cached entry (key + response) |
|---|---|
| 1 (default) | 1000 bytes |
| 2 | 2024 bytes |
| 4 | 4072 bytes |

maas-api leaves display metadata, labels and the model list out of the select response, so a long description or many models do not grow an entry. What grows it is the groups `apiKeyValidation` returns, and the headers (`experimental.maas.opendatahub.io/headers`) and guardrail checks the subscription returns.

| Lookup | Where groups count | Cost per group | Fits at size 1 (default) | At size 2 | At size 4 |
|---|---|---|---|---|---|
| `subscription-info`, any identity | nowhere: the key is stored as a hash, and the response carries no groups | none | any number | any number | any number |
| `apiKeyValidation`, API keys | the response, which lists the key's groups twice | about twice the name length plus 8 bytes | about 6 groups | about 20 groups | about 48 groups |

The group counts assume 33-character group names and were measured on OpenShift with RHCL 1.4.3. Usernames, subscription names, subscription headers and guardrail checks also count, so treat them as rough. The groups that count are the ones the user had when the key was created.

To see how many groups an OpenShift user has and how long their names are:

```bash
oc get groups -o json | jq -r --arg u alice \
  '[.items[] | select(.users // [] | index($u)) | .metadata.name] | "\(length) groups, \(join("") | length) characters"'
```

To raise the limit, patch the Authorino CR in the Kuadrant namespace (`kuadrant-system` by default). Kuadrant does not manage this field, so the change persists:

```bash
oc patch authorino authorino -n kuadrant-system --type merge -p '{"spec":{"evaluatorCacheSize":4}}'
```

The operator restarts the Authorino pods, so caches start empty. Each cached evaluator of every AuthConfig gets its own cache, and MaaS caches two evaluators per AuthConfig. Kuadrant creates one AuthConfig per HTTPRoute rule behind the gateway, so memory per Authorino pod can grow up to the number of AuthConfigs times 2 times `evaluatorCacheSize` MB. Each cache only fills with entries for requests on its own route.

### When to Tune

**Increase metadata cache TTL** if:
- maas-api experiences high load from repeated validation calls
- API key metadata changes infrequently
- Example: `--metadata-cache-ttl=300` (5 minutes) reduces maas-api load by 5x

**Decrease metadata cache TTL** if:
- Faster API key revocation or subscription-change propagation is required (e.g., compliance or security policy requires a shorter staleness window)
- Note: this increases load on maas-api's `/internal/v1/api-keys/validate` endpoint proportionally; monitor request rates before and after

**Increase `evaluatorCacheSize`** if:
- API keys of users in many groups call `/internal/v1/api-keys/validate` on every request even with caching enabled (see [Cache Entry Size](#cache-entry-size))

**Monitor after changes:**
- maas-api load: Reduced `/internal/v1/api-keys/validate` and `/internal/v1/subscriptions/select` call rates. Compare `maas_api_http_requests_total` for those routes with the gateway request rate; a ratio near 1 means lookups are not being cached
- Authorino CPU: Reduced HTTP evaluator work
- Request latency: Cache hits should show lower P99 latency

If `/internal/v1/subscriptions/select` or `/internal/v1/api-keys/validate` calls stay close to the request rate after enabling caching, check [Cache Entry Size](#cache-entry-size): an entry that exceeds Authorino's per-entry limit is not stored.

---

## References

- [AuthPolicy Reference](https://docs.kuadrant.io/latest/kuadrant-operator/doc/reference/authpolicy/)
- [Controller Architecture](../architecture-internals/controller-architecture.md)
