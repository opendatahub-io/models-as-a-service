# API Key Administration

This guide covers administrative operations for managing API keys across the MaaS platform.

## Bulk Key Revocation

Platform administrators can bulk revoke API keys by **user**, by **subscription**, or both. A **dry-run** mode is available to preview how many keys would be revoked before committing.

### Revoking All Keys for a User

Send a `POST` request to `/v1/api-keys/bulk-revoke` with the target username:

```bash
curl -sS -X POST "${MAAS_API_URL}/maas-api/v1/api-keys/bulk-revoke" \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -d '{"username": "alice"}'
```

This updates the status of all API keys belonging to the specified user to `revoked` in the database. The next validation request for any of those keys will reject them. Authorino may cache validation results briefly; revocation is effective as soon as the cache expires.

### Revoking All Keys for a Subscription

Revoke every active API key bound to a specific MaaSSubscription. This is useful when decommissioning a subscription or rotating all credentials for a particular access tier:

```bash
curl -sS -X POST "${MAAS_API_URL}/maas-api/v1/api-keys/bulk-revoke" \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -d '{"subscription": "premium-simulator-subscription"}'
```

### Combined Scope (User + Subscription)

Revoke only a specific user's keys that are bound to a particular subscription:

```bash
curl -sS -X POST "${MAAS_API_URL}/maas-api/v1/api-keys/bulk-revoke" \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -d '{"username": "alice", "subscription": "simulator-subscription"}'
```

### Dry-Run Mode

Preview how many keys would be revoked without actually revoking them. The response returns only the count — no key IDs are exposed. Set `dryRun: true` with any scope (username, subscription, or both).

**Dry-run by subscription:**

```bash
curl -sS -X POST "${MAAS_API_URL}/maas-api/v1/api-keys/bulk-revoke" \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -d '{"subscription": "simulator-subscription", "dryRun": true}'
```

**Dry-run by username:**

```bash
curl -sS -X POST "${MAAS_API_URL}/maas-api/v1/api-keys/bulk-revoke" \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -d '{"username": "alice", "dryRun": true}'
```

**Dry-run with combined scope:**

```bash
curl -sS -X POST "${MAAS_API_URL}/maas-api/v1/api-keys/bulk-revoke" \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -d '{"username": "alice", "subscription": "simulator-subscription", "dryRun": true}'
```

**Response (same structure for all dry-run scopes):**

```json
{
  "revokedCount": 12,
  "message": "Dry run: 12 active key(s) would be revoked for subscription simulator-subscription",
  "dryRun": true
}
```

### Request Fields

| Field | Required | Description |
|-------|----------|-------------|
| `username` | Optional (at least one of `username` or `subscription` must be provided) | Target user whose keys should be revoked. Regular users can only specify their own username; specifying another user requires admin privileges. |
| `subscription` | Optional (at least one of `username` or `subscription` must be provided) | MaaSSubscription name whose bound keys should be revoked. Admin-only. |
| `dryRun` | Optional | When `true`, returns the count of keys that would be revoked without performing any mutation. Default: `false`. |

### Authorization

!!! warning "Administrative privilege required"
    Subscription-scoped and cross-user revocation require admin privileges. Regular users can only revoke their own keys via `DELETE /v1/api-keys/{id}` or bulk revoke with their own username.

### Use Cases

- **Security incident response**: Immediately cut off access for a compromised account
- **User offboarding**: Revoke all keys when a user leaves the organization
- **Subscription decommission**: Revoke all keys bound to a subscription before removing it
- **Access tier migration**: Revoke keys for an old subscription after migrating users to a new one
- **Impact assessment**: Use dry-run to preview the blast radius before executing a bulk revoke
- **Policy enforcement**: Revoke keys that violate usage policies

---

## Group Membership Changes

API keys store the user's group membership at creation time. When a user's groups change (role changes, offboarding, etc.), their existing API keys retain the old group membership and permissions until revoked.

### When to Revoke Keys

Revoke all keys for a user immediately when:

- **User leaves the organization** - Offboarding requires immediate revocation
- **Role or group changes** - User moves to a different team or loses access to certain models
- **Security incident** - Compromised credentials or unauthorized access detected

Use the bulk revoke endpoint to revoke all keys for the affected user, then notify them to create new keys with updated permissions.

---

## Ephemeral Key Cleanup

Expired ephemeral keys are automatically deleted from the database by a **CronJob** (`maas-api-key-cleanup`) that runs every 15 minutes. This prevents unbounded accumulation of expired short-lived credentials.

### How It Works

1. The CronJob sends `POST /internal/v1/api-keys/cleanup` to the maas-api Service
2. The endpoint deletes ephemeral keys that expired **more than 30 minutes ago** (grace period)
3. Regular (non-ephemeral) keys are **never** deleted by cleanup — they remain until manually revoked

### Grace Period

A 30-minute grace period after expiration ensures that recently-expired keys are not deleted while in-flight requests may still reference them. Only keys expired for longer than 30 minutes are removed.

### Security

The cleanup endpoint is cluster-internal only:

- It is registered under `/internal/v1/` and is **not exposed** on the external Service or Route
- A `NetworkPolicy` (`maas-api-cleanup-restrict`) restricts cleanup pods to communicate only with `maas-api:8080` and DNS
- No authentication is required on the endpoint itself — access control is enforced at the network layer

### Troubleshooting Cleanup

**Check CronJob status:**

```bash
oc get cronjob maas-api-key-cleanup -n <namespace>
oc get jobs -n <namespace> -l app=maas-api-cleanup --sort-by=.metadata.creationTimestamp
```

**View cleanup logs:**

```bash
# Latest CronJob run
oc logs job/$(oc get jobs -n <namespace> -l app=maas-api-cleanup \
  --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1].metadata.name}') \
  -n <namespace>
```

**Manually trigger cleanup** (from an allowed pod or via oc exec):

```bash
oc exec deploy/maas-api -n <namespace> -- \
  curl -sf -X POST http://localhost:8080/internal/v1/api-keys/cleanup
```

Response: `{"deletedCount": N, "message": "Successfully deleted N expired ephemeral key(s)"}`

---

## Related Documentation

- **[API Key Management](../user-guide/api-key-management.md)**: User guide for creating and managing API keys
- **[Quota and Access Configuration](quota-and-access-configuration.md)**: Subscription setup and access control
