# Subscription limitations and known issues

This document describes known issues and operational considerations for the subscription-based MaaS Platform.

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

## Token rate limits when multiple model references share one HTTPRoute

**Impact:** High

**Description:**

When more than one **MaaSModelRef** resolves to the **same** **HTTPRoute**, the controller creates multiple **TokenRateLimitPolicy** resources targeting that route. Kuadrant then **enforces only one** of them in practice, so **per-subscription token limits may not all apply** even though CRs look valid.

The **MaaS controller** treats a TRLP as healthy for **MaaSSubscription** status using the Kuadrant **`Accepted`** condition on each `TokenRateLimitPolicy`. Kuadrant also publishes runtime conditions such as **`Enforced`**; when multiple TRLPs conflict on one route you may see **`Enforced`** = True on one policy and **`Overridden`** (or similar) on others—check **`status.conditions`** (and **`reason`** / **`message`**) on each TRLP.

**Detection:**

List TRLPs that target an HTTPRoute, then inspect **`Accepted`** (controller readiness) and **`Enforced`** (gateway application):

```bash
# List TRLPs that target an HTTPRoute (namespace/name → route name)
kubectl get tokenratelimitpolicy -A -o json | jq -r '.items[] | select(.spec.targetRef.kind=="HTTPRoute") | "\(.metadata.namespace)/\(.metadata.name) → \(.spec.targetRef.name)"' | sort

# Accepted + Enforced condition status per TRLP (needs jq; if this fails, use kubectl describe on each TRLP)
kubectl get tokenratelimitpolicy -A -o json | jq -r '
  .items[] | select(.spec.targetRef.kind == "HTTPRoute")
  | . as $i
  | (($i.status.conditions // []) | map(select(.type == "Accepted")) | .[0]) as $a
  | (($i.status.conditions // []) | map(select(.type == "Enforced")) | .[0]) as $e
  | [
      $i.metadata.namespace,
      $i.metadata.name,
      $i.spec.targetRef.name,
      (($a // {}) | .status // "?"),
      (($e // {}) | .status // "?"),
      (($e // {}) | .reason // "")
    ] | @tsv'
```

**How to recognize it:** Several TRLPs share the same `spec.targetRef.name`. Compare **`Accepted`** (what the MaaS controller uses for subscription readiness) and **`Enforced`** / **`reason`** (for example **`Overridden`**) on each policy—one route may show one TRLP fully effective and others superseded.

**Workarounds:**

1. **Dedicated routes per model** — Deploy each model with its own HTTPRoute to ensure independent rate limiting
2. **Shared subscription design** — If models share an **HTTPRoute**, use **one** **MaaSSubscription** that lists every **MaaSModelRef** on that route so you are not applying **different** subscription limits to the same route. The controller may still create **one TRLP per model ref**; **prefer (1)** when each subscription must enforce limits independently until **Tracking** below ships.
3. **Route consolidation by tier** — **Yes:** if **multiple** **MaaSModelRef** resources still target the **same** **HTTPRoute**, you still get **multiple TRLPs**; grouping models by tier on shared routes does **not** change that by itself. Treat “premium” vs “free” as an operational label only. This pattern is **only** appropriate when **every** model on that shared route is meant to share **one** **MaaSSubscription** and a consistent **MaaSAuthPolicy** access story—**not** when different teams or subscriptions each register their own model refs on one route. If you need **separate** subscriptions with **separate** limits on the same route, use **dedicated routes per model** (1).

**Status in v3.4:**

This limitation **remains in Models-as-a-Service v3.4**. The fix requiring merge strategy support for TokenRateLimitPolicy is not included. Plan your model deployment topology accordingly.

**Tracking:** [opendatahub-io/models-as-a-service#585](https://github.com/opendatahub-io/models-as-a-service/pull/585) proposes the controller change for coexisting token rate limit policies on a shared route.

## Related Documentation

- [Understanding Token Management](token-management.md)
- [Access and Quota Overview](../concepts/subscription-overview.md)
- [Quota and Access Configuration](quota-and-access-configuration.md)
- [MaaS Controller Overview](maas-controller-overview.md)
