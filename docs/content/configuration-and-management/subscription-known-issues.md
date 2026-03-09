# Subscription Known Issues

This document describes known issues and operational considerations for the subscription-based MaaS Platform.

## X-MaaS-Subscription Header

### Multiple Subscriptions Require Header

**Impact:** High

**Description:**

When a user belongs to multiple groups that each have a MaaSSubscription, the client **must** send the `X-MaaS-Subscription` header to specify which subscription's rate limits apply. If the header is omitted, the MaaS API returns an error and the request is denied with **403 Forbidden**.

**Example:**

```text
User in groups: [system:authenticated, premium-users]
Subscriptions: free-subscription (system:authenticated), premium-subscription (premium-users)

Request without header → 403 "must specify X-MaaS-Subscription"
Request with X-MaaS-Subscription: premium-subscription → 200 OK (premium limits apply)
```

**Workaround:**

- Ensure clients send `X-MaaS-Subscription: <subscription-name>` when users can have multiple subscriptions
- Document which subscription names to use for each user segment
- Consider using a single subscription per user segment to avoid header requirement

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

API keys store the user's groups at creation time. If a user's group membership changes after the key was created:

- The key still carries the **old** groups until it is revoked and recreated
- Subscription selection uses the groups from the key validation response (the stored snapshot)
- The user must create a new API key to get updated group membership and subscription access

**Workaround:**

- Revoke and recreate API keys when users change groups
- Use OpenShift tokens for interactive use when group membership changes frequently (tokens reflect live group membership)

## Related Documentation

- [Subscription Overview](subscription-overview.md)
- [Subscription Configuration](subscription-configuration.md)
