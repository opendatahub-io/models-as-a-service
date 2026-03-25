# Subscription Known Issues

This document describes known issues and operational considerations for the subscription-based MaaS Platform.

## Subscription Selection Caching

### Cache TTL for Subscription Selection

**Impact:** Medium

**Description:**

Authorino caches the result of the MaaS API subscription selection call (e.g., 60 second TTL). If a user's group membership changes:

- Within the cache window, the old subscription selection may still apply
- After cache expiry, the new group membership is used
- Restarting Authorino pods forces immediate cache invalidation (disruptive)

**Workaround:**

- Wait for cache TTL for changes to fully propagate
- For immediate effect, restart Authorino pods (disruptive; use during maintenance windows)

## API Key vs OpenShift Token

### Group Snapshot in API Keys

**Impact:** Medium

**Description:**

API keys store the user's groups and bound subscription name at creation time. If a user's group membership changes after the key was created:

- The key still carries the **old** groups and subscription until it is revoked and recreated
- Subscription metadata for gateway inference uses the stored groups and subscription from validation
- The user must create a new API key to pick up new groups or a different default subscription

**Workaround:**

- Revoke and recreate API keys when users change groups
- Use OpenShift tokens for interactive use when group membership changes frequently (tokens reflect live group membership)

## Related Documentation

- [Understanding Token Management](token-management.md)
- [Access and Quota Overview](subscription-overview.md)
- [Quota and Access Configuration](quota-and-access-configuration.md)
