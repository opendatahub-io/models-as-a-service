# Authentication & gateway identity (internals)

This page describes how **identity and subscription context** are established at the gateway and how **rate limiting** uses them. For day-to-day auth behavior from a user perspective, see [Access and Quota Overview](../concepts/subscription-overview.md) and the [User Guide](../user-guide/inference.md).

Implementation references: `maas-controller/pkg/controller/maas/maasauthpolicy_controller.go`, `maas-controller/pkg/controller/maas/maassubscription_controller.go`.

---

## Subscription selection (AuthPolicy) → rate limits (TokenRateLimitPolicy)

Today’s pipeline does **not** drive TokenRateLimitPolicy by splitting group membership in CEL the way older drafts did. The flow is:

1. **AuthPolicy** (controller-generated per model) authenticates the caller and calls **maas-api** at  
   `POST https://maas-api.<namespace>.svc.cluster.local:8443/internal/v1/subscriptions/select`  
   with the caller’s groups, username, requested subscription header (when applicable), and model scope.

2. **maas-api** resolves which **MaaSSubscription** applies (including validation, auto-selection when only one subscription applies, and error paths). Results are exposed to Authorino as **`subscription-info`** metadata.

3. **AuthPolicy `filters.identity`** copies resolved fields onto **`auth.identity`**, including:
   - **`selected_subscription_key`** — model-scoped key of the form  
     `{subscriptionNamespace}/{subscriptionName}@{modelNamespace}/{modelName}`  
     This is the value **rate limiting** keys off.

4. **TokenRateLimitPolicy** (aggregated per model by the MaaSSubscription reconciler) defines **one limit entry per subscription** that applies to that model. Each limit’s **`when`** predicate matches requests where  
   `auth.identity.selected_subscription_key` equals that subscription’s scoped key (and inference paths are distinguished from discovery; `/v1/models` is exempt from token consumption limits where configured).

So enforcement is: **subscription resolved in AuthPolicy → same key matched in TRLP**. Group-based **authorization** still uses groups from TokenReview / API key validation in **MaaSAuthPolicy** rules; **rate limit selection** follows the resolved subscription key, not a separate “group split” expression on TRLP.

**TRLP predicates vs other identity:** TokenRateLimitPolicy **`when`** clauses use **`selected_subscription_key` only**—not `groups_str`, group arrays, or header mirrors. Anything else on `auth.identity` is **not** part of TRLP matching; it exists for **subscription selection** (inputs to maas-api), **Authorino cache/metadata**, and **telemetry** at the gateway/mesh. That matches post–EA2 behavior: limits follow the resolved subscription key; maintainers often describe the remaining decoration as **chiefly telemetry-facing**, aside from selection/caching.

---

## Identity metadata: groups, `groups_str`, and telemetry

The AuthPolicy still exposes **groups** and a comma-separated **`groups_str`** derived from API key validation metadata (`auth.metadata.apiKeyValidation.groups.join(",")` in controller-generated policies during `filters.identity` generation in `maasauthpolicy_controller.go`). They feed **subscription selection** (request body to `/internal/v1/subscriptions/select`), **cache key material** for Authorino, and **telemetry** (e.g. Istio reading injected headers)—but **do not** appear in TRLP predicates (`maassubscription_controller.go` matches **`selected_subscription_key`** only).

If you read older notes about a “string trick” solely for TRLP group matching, treat that as **obsolete** for current TRLP predicates.

---

## Identity headers and defense-in-depth

**Model inference routes** (HTTPRoutes to model workloads):

- Controller-generated AuthPolicies generally **do not** inject most identity headers (`X-MaaS-Username`, `X-MaaS-Group`, `X-MaaS-Key-Id`) upstream to model pods, to reduce leakage via logs or misconfigured proxies.

**`X-MaaS-Subscription`** may be injected where gateway telemetry needs a stable subscription label. Any client-supplied **`X-MaaS-Subscription`** header is **discarded and replaced** with the server-resolved value from Authorino's **AuthPolicy response phase** (Authorino/Kuadrant AuthPolicy is authoritative—the upstream workload sees only what enforcement injected).

**MaaS API routes** use a separate static AuthPolicy that may inject headers required by maas-api middleware (trusted internal service).

---

## Token validation (short)

**OpenShift tokens:** Authorino uses Kubernetes **TokenReview**; groups and username come from the review result.

**API keys:** Authorino calls MaaS API validation; the key carries a bound subscription; validation returns user fields used for identity and subscription resolution.

**Groups for authorization:** Values in **MaaSAuthPolicy** / **MaaSSubscription** must align with groups from TokenReview or API key validation, not only with OpenShift `Group` objects, unless your IdP maps them consistently.

```bash
TOKEN=$(kubectl create token default -n default --duration=1m)
echo '{"apiVersion":"authentication.k8s.io/v1","kind":"TokenReview","spec":{"token":"'$TOKEN'"}}' | \
  kubectl create -o jsonpath='{.status.user.groups}' -f -
```

---

## Related documentation

- [Controller Architecture](./controller-architecture.md)
- [Reconciliation Flow](./reconciliation-flow.md)
- [Controller Overview (hub)](../configuration-and-management/maas-controller-overview.md)
- [Access and Quota Overview](../concepts/subscription-overview.md)
