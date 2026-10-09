# Authentication & gateway identity (internals)

This page describes how **identity and subscription context** are established at the gateway and how **rate limiting** uses them.

---

## Subscription selection (AuthPolicy) → rate limits (TokenRateLimitPolicy)

Today’s pipeline does **not** drive TokenRateLimitPolicy by splitting group membership in CEL the way older drafts did. The flow is:

1. **AuthPolicy** (controller-generated per model) authenticates the caller and calls **maas-api** at:
   - Default tenant: `POST https://maas-api.opendatahub.svc.cluster.local:8443/internal/v1/subscriptions/select`
   - Additional tenants: `POST https://maas-api-{tenantID}.opendatahub.svc.cluster.local:8443/internal/v1/subscriptions/select`
   
   with the caller’s groups, username, requested subscription header (when applicable), and model scope.

2. **maas-api** resolves which **MaaSSubscription** applies (including validation, auto-selection when only one subscription applies, and error paths). Results are exposed to Authorino as **`subscription-info`** metadata.

3. **AuthPolicy `filters.identity`** copies resolved fields onto **`auth.identity`**, including:
   - **`selected_subscription_key`** — model-scoped key of the form  
     `{subscriptionNamespace}/{subscriptionName}@{modelNamespace}/{modelName}`  
     Kept for telemetry and debugging.
   - **`selected_subscription_id`** — first 16 hex chars of SHA-256 over that key (from
     maas-api `subscription-info.rateLimitId`). Also mirrored as response header
     **`X-MaaS-Subscription-Rate-Limit-Id`** without replacing existing headers.
     This is the value **rate limiting** keys off.

4. **TokenRateLimitPolicy** (aggregated per model by the MaaSSubscription reconciler) defines **one limit entry per distinct rate set** among the subscriptions that apply to that model. Each limit’s **`when`** predicate matches requests where
   `auth.identity.selected_subscription_id` equals the short ID of any subscription in that group (and inference paths are distinguished from discovery; `/v1/models` is exempt from token consumption limits where configured). Its counters are `selected_subscription_id` and `userid`, so subscriptions sharing a limit keep separate budgets; see [Reconciliation Flow](./reconciliation-flow.md#maassubscription-reconciler).

So enforcement is: **subscription resolved in AuthPolicy → same short ID matched in TRLP**. Group-based **authorization** is resolved during subscription selection: **maas-api** evaluates **MaaSAuthPolicy** rules for the caller and returns `accessAllowed` in **`subscription-info`** metadata. The gateway authorization rule simply reads that boolean rather than embedding per-model allowlists, keeping the AuthPolicy CR fixed-size at any subscription count. **Rate limit selection** follows the resolved subscription ID, not a separate “group split” expression on TRLP.

**TRLP predicates vs other identity:** TokenRateLimitPolicy **`when`** clauses use **`selected_subscription_id` only**—not `selected_subscription_key`, `groups_str`, group arrays, or header mirrors. Anything else on `auth.identity` is **not** part of TRLP matching; it exists for **subscription selection** (inputs to maas-api), **Authorino cache/metadata**, and **telemetry** at the gateway/mesh.

---

## Identity metadata: groups, `groups_str`, and telemetry

Behavior is split between **`filters.identity`** (what gets serialized onto **`auth.identity`** for downstream consumers) and **`celGroups`** (what the controller uses for subscription selection and cache keys). Both are defined in **`maasauthpolicy_controller.go`**.

### API key validation path

After Authorino runs API key validation, **`auth.metadata.apiKeyValidation`** is populated. In the generated AuthPolicy **success response** → **`filters.identity`** → **`json.properties`**, the controller sets:

- **`groups`** → `auth.metadata.apiKeyValidation.groups`
- **`groups_str`** → `auth.metadata.apiKeyValidation.groups.join(",")`

So for **API keys**, `groups_str` is the comma-joined snapshot from validation metadata.

### Kubernetes `TokenReview` (OpenShift bearer token) path

TokenReview returns groups on **`status.user.groups`**. Authorino surfaces that as **`auth.identity.user.groups`**.

The controller **does not** define a separate `groups_str` expression that reads TokenReview groups. The **`filters.identity`** fields above still reference **`auth.metadata.apiKeyValidation.groups`** only; without API key metadata those expressions do **not** mirror TokenReview—treat **`groups_str`** (and the sibling **`groups`** property in that same JSON block) as **empty / irrelevant for pure bearer-token auth**.

Subscription selection and caching use the **`celGroups`** CEL constant in the same file: **`auth.metadata.apiKeyValidation.groups`** when API key metadata exists; otherwise **`auth.identity.groups`** if present, else **`auth.identity.user.groups`**. For TokenReview, inspect **`auth.identity.user.groups`**, not `groups_str` from `filters.identity`.

### OIDC path

When a JWT carries a groups claim, Authorino may populate **`auth.identity.groups`**. **`celGroups`** uses that branch before falling back to **`auth.identity.user.groups`**.

### TokenRateLimitPolicy (`maassubscription_controller.go`)

TRLP **`when`** predicates match **`auth.identity.selected_subscription_id` only**. Using **`groups_str`** or the long **`selected_subscription_key`** for rate limits is **obsolete**.

### Troubleshooting: which field to inspect

| Concern | API key flow | TokenReview (kube) token | OIDC |
|--------|----------------|---------------------------|------|
| Raw groups after auth | **`auth.metadata.apiKeyValidation.groups`** | **`auth.identity.user.groups`** (from TokenReview **`status.user.groups`**) | **`auth.identity.groups`** if present, else **`auth.identity.user.groups`** |
| Serialized **`groups_str` in `filters.identity`** | From **`apiKeyValidation.groups.join(",")`** | Not populated from TokenReview (still tied to apiKeyValidation expression) | Same—only apiKeyValidation drives that JSON property |
| Subscription selection + Authorino cache inputs | **`celGroups`**, **`celUsername`**, **`celSubscription`** + **`auth.metadata["subscription-info"]`** after select | Same **`celGroups`** branch → **`auth.identity.user.groups`** | **`celGroups`** → **`auth.identity.groups`** when set |
| Rate limiting | **`auth.identity.selected_subscription_id`** after successful select | Same | Same |
| Telemetry / debug subscription@model | **`auth.identity.selected_subscription_key`** | Same | Same |

If you read older notes about a “string trick” solely for TRLP group matching, treat that as **obsolete**.

---

## Identity headers and defense-in-depth

**Model inference routes** (HTTPRoutes to model workloads):

- Controller-generated AuthPolicies generally **do not** inject most identity headers (`X-MaaS-Username`, `X-MaaS-Group`, `X-MaaS-Key-Id`) upstream to model pods, to reduce leakage via logs or misconfigured proxies.

For **API keys**, AuthPolicy injects **`X-MaaS-Subscription`** only when validation returns a nonempty bound subscription. For **user tokens**, AuthPolicy preserves a client-supplied subscription header and uses it for subscription selection; it does not inject another copy. A user-token request without that header does not gain one during authentication, so telemetry that reads the header may have an empty subscription label.

AuthPolicy always sets the llm-d flow-control headers, replacing any client-supplied values, so clients cannot select an InferenceObjective or fairness ID:

- **`x-llm-d-inference-objective`** and **`x-gateway-inference-objective`**: the `objective` returned by maas-api subscription selection, which is the requested model's `objectiveName` from the subscription's [`flowControlStatuses`](../reference/crds/maas-subscription.md#status-flowcontrolstatuses). Empty when the model has none (for example, an ExternalModel) or when another object holds the name, so the scheduler applies priority `0`.
- **`x-llm-d-inference-fairness-id`** and **`x-gateway-inference-fairness-id`**: the `fairnessId` returned by maas-api, which is the tenant name of the maas-api instance.

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
- [Access and Quota Overview](../concepts/subscription-overview.md)
- [MaaS Setup](../install/maas-setup.md)
