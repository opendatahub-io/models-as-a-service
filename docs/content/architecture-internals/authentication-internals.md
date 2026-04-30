# Authentication Internals

This document explains how authentication and identity flow through the MaaS system, including the "string trick" for passing user groups from AuthPolicy to TokenRateLimitPolicy. For operational guidance on authentication, see [Controller Overview](../configuration-and-management/maas-controller-overview.md).

---

## The "String Trick" (AuthPolicy → TokenRateLimitPolicy)

Kuadrant's TokenRateLimitPolicy CEL predicates do not always support array fields the same way as the AuthPolicy response. To pass **user groups** from AuthPolicy to TokenRateLimitPolicy in a reliable way, the controller uses a **comma-separated string**:

### How It Works

1. **AuthPolicy (controller-generated)**  
   - In the `filters.identity` section, the controller adds a property **`groups_str`** with a CEL expression that takes **all** user groups from API key validation (unfiltered) and **joins them with a comma**:  
     `auth.metadata.apiKeyValidation.groups.join(",")`  
   - So the identity object has both `groups` (array from `auth.metadata.apiKeyValidation.groups`) and **`groups_str`** (string, e.g. `"system:authenticated,free-user,premium-user"`).  
   - Groups are passed unfiltered so that TRLP predicates can match against subscription groups, which may differ from auth policy groups.
   - This is implemented in `maasauthpolicy_controller.go` in the controller-generated AuthPolicy response.

2. **TokenRateLimitPolicy (controller-generated)**  
   - For each subscription owner group, the controller generates a CEL predicate that **splits** `groups_str` and checks membership, e.g.  
     `auth.identity.groups_str.split(",").exists(g, g == "free-user")`.

So: **AuthPolicy** turns the user-groups array into a **comma-separated string**; **TokenRateLimitPolicy** turns that string back into a logical list and uses it for rate-limit matching. That's the "string trick."

### Why This Pattern?

**Problem:** Kuadrant's CEL predicates in TokenRateLimitPolicy cannot reliably evaluate array membership using AuthPolicy's `groups` array field in all cases due to CEL evaluation context differences.

**Solution:** By serializing groups to a comma-delimited string in AuthPolicy and deserializing in TokenRateLimitPolicy, we ensure consistent group matching regardless of CEL context limitations.

**Example CEL expressions:**

```cel
// In AuthPolicy filters.identity (controller-generated)
auth.identity.groups_str = auth.metadata.apiKeyValidation.groups.join(",")

// In TokenRateLimitPolicy predicate
auth.identity.groups_str.split(",").exists(g, g == "premium-users")
```

---

## Identity Headers and Defense-in-Depth

**For model inference routes** (HTTPRoutes targeting model workloads):

The controller-generated AuthPolicies do **not** inject most identity-related HTTP headers (`X-MaaS-Username`, `X-MaaS-Group`, `X-MaaS-Key-Id`) into requests forwarded to upstream model pods. This is a defense-in-depth security measure to prevent accidental disclosure of user identity, group membership, and key identifiers in:

- Model runtime logs
- Upstream debug dumps
- Misconfigured proxies or sidecars

### Exception: X-MaaS-Subscription

**`X-MaaS-Subscription` IS injected** for Istio Telemetry to enable per-subscription latency tracking. Istio runs in the Envoy gateway and cannot access Authorino's `auth.identity` context—it can only read request headers. The injected subscription value is server-controlled (resolved by Authorino from validated subscriptions), not client-provided.

### Gateway-Level Identity Access

All identity information remains available to **gateway-level features** through Authorino's `auth.identity` and `auth.metadata` contexts, which are consumed by:

- **TokenRateLimitPolicy (TRLP)**: Uses `selected_subscription_key`, `userid`, `groups`, and `subscription_info` from `filters.identity` (access `subscription_info.labels` for tier-based rate limiting)
- **Gateway telemetry/metrics**: Accesses identity fields with `metrics: true` enabled on `filters.identity`
- **Authorization policies**: OPA/Rego rules evaluate `auth.identity` and `auth.metadata` directly

### MaaS API Routes (Different Security Model)

The static AuthPolicy for maas-api (`deployment/base/maas-api/policies/auth-policy.yaml`) still injects `X-MaaS-Username` and `X-MaaS-Group` headers, as maas-api's `ExtractUserInfo` middleware requires them. This is separate from model inference routes and follows a different security model (maas-api is a trusted internal service).

### Security Motivation

Model workloads (vLLM, Llama.cpp, etc.) do not require strong identity claims in cleartext headers. By keeping identity at the gateway layer, we reduce the attack surface and limit the blast radius of potential log leaks or upstream vulnerabilities.

**Key principle:** Identity claims are security-sensitive. Minimize their exposure beyond the enforcement point (the gateway).

---

## Token Validation Flow

### OpenShift User Tokens

1. **Client** sends request with `Authorization: Bearer <openshift-token>`
2. **Gateway** forwards to Authorino
3. **Authorino** calls Kubernetes TokenReview API:
   ```json
   {
     "apiVersion": "authentication.k8s.io/v1",
     "kind": "TokenReview",
     "spec": {"token": "<openshift-token>"}
   }
   ```
4. **Kubernetes API** validates token and returns:
   ```json
   {
     "status": {
       "authenticated": true,
       "user": {
         "username": "user@example.com",
         "uid": "...",
         "groups": ["system:authenticated", "free-users", ...]
       }
     }
   }
   ```
5. **Authorino** builds identity context with `userid`, `groups`, and `groups_str`
6. **TokenRateLimitPolicy** matches groups against subscription predicates

### API Keys

1. **Client** sends request with `Authorization: Bearer sk-oai-...`
2. **Gateway** forwards to Authorino
3. **Authorino** calls MaaS API `/api/v1/api-keys/validate`:
   ```http
   POST /api/v1/api-keys/validate
   Authorization: Bearer sk-oai-...
   ```
4. **MaaS API** validates key (checks hash, expiration, revocation) and returns:
   ```json
   {
     "valid": true,
     "username": "user@example.com",
     "groups": ["system:authenticated", "premium-users"],
     "subscription": "premium-subscription",
     "keyId": "uuid-here"
   }
   ```
5. **Authorino** builds identity context with `userid`, `groups`, `groups_str`, `selected_subscription_key`
6. **TokenRateLimitPolicy** uses the pre-selected subscription (API keys are bound at mint time)

### Group Membership Validation

**Important:** Auth groups in MaaSAuthPolicy and MaaSSubscription must match groups returned by the identity provider (TokenReview API or API key validation), **not** OpenShift Group objects created via `oc adm groups`.

To check your token's actual groups:

```bash
TOKEN=$(kubectl create token default -n default --duration=1m)
echo '{"apiVersion":"authentication.k8s.io/v1","kind":"TokenReview","spec":{"token":"'$TOKEN'"}}' | \
  kubectl create -o jsonpath='{.status.user.groups}' -f -
```

Common groups: `dedicated-admins`, `system:authenticated`, `system:authenticated:oauth`.

---

## Related Documentation

- [Controller Architecture](./controller-architecture.md) - High-level controller design
- [Reconciliation Flow](./reconciliation-flow.md) - How policies are generated and attached
- [Controller Overview](../configuration-and-management/maas-controller-overview.md) - Operational authentication guidance
