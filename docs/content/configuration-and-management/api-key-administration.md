# API Key Administration

This guide covers administrative operations for managing API keys across the MaaS platform.

## Bulk Key Revocation

Platform administrators can revoke API keys for any user, which is useful for security incidents or offboarding.

### Revoking All Keys for a User

Send a `POST` request to `/v1/api-keys/bulk-revoke` with the target username:

```bash
curl -sS -X POST "${MAAS_API_URL}/maas-api/v1/api-keys/bulk-revoke" \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -d '{"username": "alice"}'
```

This updates the status of all API keys belonging to the specified user to `revoked` in the database. The next validation request for any of those keys will reject them. Authorino may cache validation results briefly; revocation is effective as soon as the cache expires.

!!! warning "Administrative privilege required"
    Only administrators with appropriate permissions can revoke other users' keys. Regular users can only revoke their own keys via `DELETE /v1/api-keys/{id}`.

### Use Cases

- **Security incident response**: Immediately cut off access for a compromised account
- **User offboarding**: Revoke all keys when a user leaves the organization
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

Expired ephemeral keys are automatically deleted from the database by a **background cleanup loop** inside the maas-api process. By default it runs every 15 minutes (`CLEANUP_INTERVAL_MINUTES=15`). This prevents unbounded accumulation of expired short-lived credentials.

When upgrading from releases that used the external `maas-api-key-cleanup` CronJob, the Tenant reconciler automatically removes that CronJob and its `maas-api-cleanup-restrict` NetworkPolicy on the next platform reconcile.

### How It Works

1. On startup, maas-api starts a goroutine that periodically invokes the cleanup logic
2. Cleanup deletes ephemeral keys that expired **more than 30 minutes ago** (grace period)
3. Regular (non-ephemeral) keys are **never** deleted by cleanup — they remain until manually revoked

### Grace Period

A 30-minute grace period after expiration ensures that recently-expired keys are not deleted while in-flight requests may still reference them. Only keys expired for longer than 30 minutes are removed.

### Security

The cleanup logic runs in-process within maas-api. The `/internal/v1/api-keys/cleanup` HTTP endpoint remains available for manual triggers but is cluster-internal only:

- It is registered under `/internal/v1/` and is **not exposed** on the external Service or Route
- No authentication is required on the endpoint itself — access control is enforced at the network layer (cluster-internal only)

### Troubleshooting Cleanup

**Check cleanup interval configuration:**

```bash
oc get deploy maas-api -n <namespace> -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="CLEANUP_INTERVAL_MINUTES")].value}{"\n"}'
```

**View cleanup activity in maas-api logs:**

```bash
oc logs deploy/maas-api -n <namespace> | grep -i cleanup
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
