# Group Membership Known Issues

This document describes known issues and side effects related to group membership in the MaaS subscription-based system. Access control uses **MaaSAuthPolicy** CRDs (group-based), **MaaSSubscription** CRDs for rate limits, and **API keys** (`sk-oai-*` format) for authentication. The maas-controller generates Kuadrant AuthPolicy and TokenRateLimitPolicy per HTTPRoute.

## 1. Group Changes and MaaSAuthPolicy

When a user is removed from a group, their existing API keys retain the **old groups**—the group membership is an immutable snapshot captured at API key creation time.

!!! warning "Immediate revocation required"
    New API key validation uses the **stored** groups, not live group membership. Users whose groups have changed should **revoke old API keys and create new ones** to reflect current access.

**Example scenario:**

```text
T+0h:   User "alice" is in "premium-users" group
T+0h:   Alice creates API key -> Groups ["premium-users"] stored in key metadata
T+1h:   Admin removes Alice from "premium-users" group
T+1h:   Alice's existing API key STILL authorizes based on ["premium-users"]
T+1h:   Alice can still access models until she revokes the key and creates a new one
```

**Workaround:** Revoke the API key and create a new one after group membership changes. The new key will store the current group snapshot.

---

## 2. Multiple Subscriptions

When a user belongs to groups covered by **multiple MaaSSubscription CRs** for the same model, the system cannot automatically determine which subscription to apply.

!!! note "X-MaaS-Subscription header"
    The user (or client) must specify which subscription to use via the **X-MaaS-Subscription** header on each request. Without this header, the request will fail with an error listing the available subscriptions.

**Example:** User is in both `team-a` (subscription `sub-team-a`) and `team-b` (subscription `sub-team-b`). Both subscriptions grant access to model `llama-3`. The client must send:

```bash
curl -X POST "${HOST}/v1/chat/completions" \
  -H "Authorization: Bearer sk-oai-..." \
  -H "X-MaaS-Subscription: sub-team-a" \
  -d '{"model":"llama-3", ...}'
```

---

## 3. API Key Group Staleness

API key metadata stores groups at **creation time**. If groups change (user added/removed from groups), existing API keys continue to authorize based on the **old** groups until revoked.

!!! info "No automatic refresh"
    There is no automatic refresh of group membership for existing API keys. Revocation and re-creation is the only way to update the effective groups for a key.

| Action | Effect on existing API keys |
|--------|-----------------------------|
| User added to new group | Key does **not** gain access to models for that group |
| User removed from group | Key **retains** access until revoked |
| User moved between groups | Key keeps old group snapshot until revoked |

### Recommended Practices

1. **Revoke before removing**: When removing a user from a group, instruct them to revoke their API keys first for immediate access termination.
2. **Communicate changes**: Notify users before group membership changes so they can plan for key rotation.
3. **Document X-MaaS-Subscription**: If users can belong to multiple subscriptions for the same model, document which header value to use for each use case.

### Related Documentation

- [MaaS Controller Overview](./maas-controller-overview.md) - MaaSAuthPolicy and MaaSSubscription setup
- [Token Management](./token-management.md) - API key lifecycle and revocation
- [Migration: Tier to Subscription](../migration/tier-to-subscription.md) - Subscription-based architecture overview
