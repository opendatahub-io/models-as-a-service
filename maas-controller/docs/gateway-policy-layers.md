# Gateway policies (inference, HTTPRoute)

For **inference** with **MaaS API keys** (`Authorization: Bearer sk-oai-…`), MaaS attaches **Kuadrant `AuthPolicy`** and **`TokenRateLimitPolicy`** to the model’s **`HTTPRoute`** (not the Gateway). **AuthPolicy** decides **allow/deny** and builds **`auth.identity`**; **TokenRateLimitPolicy** enforces **token quotas** using that identity (especially **`selected_subscription_key`**). OpenShift / OIDC paths on **`/v1/models`** are **out of scope** here—see [Authentication & gateway identity (internals)](../../docs/content/architecture-internals/authentication-internals.md).

**Rule:** Kuadrant allows **one `AuthPolicy` and one `TokenRateLimitPolicy` per `HTTPRoute` target**. **`MaaSAuthPolicy`** CRs aggregate into a single **`AuthPolicy`**; **`MaaSSubscription`** CRs aggregate into a single **`TokenRateLimitPolicy`** (name patterns such as `maas-auth-<modelName>` and `maas-trlp-<modelName>` in the model namespace). Implementation: **`maasauthpolicy_controller.go`**, **`maassubscription_controller.go`**.

---

## AuthPolicy on the HTTPRoute

**MaaSAuthPolicy** CRs merge into **one** **`AuthPolicy`**; **`spec.targetRef`** is the model **`HTTPRoute`**.

- The controller **lists** every **`MaaSAuthPolicy`** that references the model (across the configured subscription namespace).
- It **aggregates** subjects (groups/users) and authorization-related policy into **one** spec (Kuadrant allows a single **`AuthPolicy`** per route).
- It **writes** the **`AuthPolicy`** in the **model namespace** so **`targetRef`** resolves to the same **HTTPRoute** clients use for inference.

---

## TokenRateLimitPolicy on the HTTPRoute

**MaaSSubscription** CRs that reference the model merge into **one** **`TokenRateLimitPolicy`** on the same **`HTTPRoute`**.

- Limits run **after** **AuthPolicy** succeeds and **`auth.identity`** (including **`selected_subscription_key`**) is available.
- The controller **aggregates** each subscription’s **`tokenRateLimits`** for that model into **separate named limits** on one **`TokenRateLimitPolicy`** object.
- It **writes** the **`TokenRateLimitPolicy`** in the **model namespace** with **`targetRef`** set to the same **HTTPRoute** as the **AuthPolicy**.

---

## Inference flow (API key)

**Order:** **AuthPolicy** — authenticate → **metadata** (see [appendix](#appendix-metadata-maas-api-api-key-path)) → OPA → response with **`auth.identity`** → **TokenRateLimitPolicy** / Limitador → model backend if both allow.

- **Gateway / Envoy** matches the **HTTPRoute**, then calls **Authorino** to evaluate the attached **AuthPolicy**.
- **Authorino** runs metadata (**maas-api**), OPA rules, and the **response** filters so **`auth.identity`** is populated for downstream consumers.
- **Limitador** (via **TRLP**) applies **token quotas** using **`auth.identity`**; failures here are typically **429**.
- **401/403** come from **AuthPolicy** (authn/authz); **429** from rate limiting when auth already passed.

---

## Layers inside the `AuthPolicy` spec

1. **Route targeting**
   - **`spec.targetRef`** names the model **`HTTPRoute`** (group `gateway.networking.k8s.io`, kind **`HTTPRoute`**).
   - The **`AuthPolicy`** spec is built from **every `MaaSAuthPolicy`** that references that model; subjects and rules are **merged** so satisfying the combined policy is sufficient.
   - An annotation (for example **`maas.opendatahub.io/auth-policies`**) lists contributing **`MaaSAuthPolicy`** names for operators.

2. **Authentication (API key)**
   - Runs only when **`Authorization`** matches **`^Bearer sk-oai-.*`** (**`when`** on the **api-keys** block).
   - Treats the header as the credential (plain selector); **does not** by itself prove the key is valid—that happens in **metadata**.

3. **Metadata**
   - **`apiKeyValidation`**: **POST** **`/internal/v1/api-keys/validate`** with the key material; populates **`auth.metadata.apiKeyValidation`** (see [appendix](#appendix-metadata-maas-api-api-key-path)).
   - **`subscription-info`**: **POST** **`/internal/v1/subscriptions/select`** using groups/username/subscription/model from the request + validation output; populates **`auth.metadata["subscription-info"]`**.
   - Each metadata step may use an **Authorino HTTP cache** (TTL from controller **`--metadata-cache-ttl`**, **`MetadataCacheTTL`**).

4. **Authorization (OPA)** — three evaluators (all must allow for the API key inference path). Each may use an **authorization cache** (TTL **`--authz-cache-ttl`** / **`AuthzCacheTTL`**; controller requires it not exceed metadata TTL).

   - **`auth-valid`** (identity present / key valid for this path)
     - **API key path:** **`auth.metadata.apiKeyValidation.valid == true`** (maas-api already decided invalid vs valid in metadata).
     - *(Other `allow` arms exist for Kubernetes and OIDC identities; they are not used on the pure API key inference path documented here.)*

   - **`require-group-membership`** (only emitted if the merged **`MaaSAuthPolicy`** subjects include at least one **group** or **user**)
     - Builds **`username`** from **`apiKeyValidation.username`** when API key metadata is present (else other identity branches out of scope here).
     - Builds **`groups`** from **`apiKeyValidation.groups`** when non-empty for API keys.
     - **Allows** if **`username`** equals any entry in **`allowed_users`** (from merged **`MaaSAuthPolicy`** `spec.subjects.users`).
     - **Allows** if **any** resolved **`groups`** element equals any entry in **`allowed_groups`** (from merged **`MaaSAuthPolicy`** `spec.subjects.groups`).

   - **`subscription-valid`** (subscription selection outcome is usable)
     - **`subscription-info.name`** must be **non-empty** (a subscription was selected).
     - **`subscription-info.error`** must be **empty** (no selector/validation error string from maas-api).
     - **`subscription-info.phase`** must be **`Active`** or **`Degraded`** (explicit allowlist; empty / **Pending** / **Failed** / unknown → deny).
     - **`subscription-info.deletionTimestamp`** must be **empty** (subscription not terminating).

5. **Response phase** (success path)
   - **`Authorization`** header set to **empty** so the **model backend never receives** the raw API key or bearer token.
   - **`X-MaaS-Subscription`**: for API keys, set from **`apiKeyValidation.subscription`** (bound subscription name); empty for non–API-key paths in the same policy object.
   - **`filters.identity` → JSON** (serialized to **`auth.identity`** for the mesh / **TRLP**), including for the API key path: **`groups`**, **`groups_str`**, **`userid`** (from **`apiKeyValidation.username`** in the generated policy), **`keyId`**, **`selected_subscription`**, **`selected_subscription_key`** (model-scoped **`namespace/name@modelNamespace/modelName`**), **`subscription_info`**, and subscription error fields when present.
   - **Denial responses:** **`unauthenticated`** → **401** with fixed message; **`unauthorized`** → **403** with body/headers derived from **`subscription-info`** when available.

---

## Token rate limit layers (`TokenRateLimitPolicy`)

Same **`HTTPRoute`** as the **AuthPolicy**; implementation **`maas-controller/pkg/controller/maas/maassubscription_controller.go`**. The API key inference path assumes **AuthPolicy** has already populated **`auth.identity`** (especially **`selected_subscription_key`** and **`userid`**).

1. **Gateway default (optional context)**
   - A **gateway-attached** **TokenRateLimitPolicy** can define a **zero-token** budget on model paths as a **fallback** if something reached the mesh without a matching per-route limit.
   - For a route that has a **per-route TRLP**, Kuadrant’s **atomic** merge means the **per-route** object **replaces** those defaults for that **HTTPRoute**.

2. **Per-route aggregated limits**
   - **Exactly one** **`TokenRateLimitPolicy`** per model **HTTPRoute** (name pattern such as **`maas-trlp-<modelName>`** in the model namespace).
   - Contains **one named limit block per `MaaSSubscription`** that references the model, each with **`rates`** from that subscription’s **`tokenRateLimits`** (or a **default** if none are set).

3. **Which limit applies**
   - Each limit’s **`when`** uses a predicate on **`auth.identity.selected_subscription_key`** equal to that subscription’s **model-scoped** key: **`{subNamespace}/{subName}@{modelNamespace}/{modelName}`**.
   - Only the limit whose **`when`** matches the resolved key runs for quota accounting on that request.

4. **Counters**
   - **`counters`** use **`auth.identity.userid`** so quotas are **per end-user** within the matched subscription bucket (value comes from the **AuthPolicy** identity JSON the mesh sees after auth).

5. **Listing vs inference**
   - Generated predicates typically **exclude** paths ending in **`/v1/models`** from consuming **inference token** budget (discovery vs chat/completions).
   - **Chat / completions** (and similar inference paths) **increment** usage against the matched subscription’s **`rates`**.

If no per-route limit matches a resolved subscription, the gateway zero-budget rule may still yield **429**; normal auth failures remain **401/403** from **AuthPolicy**.

---

## Appendix: Metadata (maas-api, API key path)

Implementation references: **`maas-api/internal/api_keys/handler.go`** (`ValidateAPIKeyHandler`), **`maas-api/internal/api_keys/types.go`** (`ValidateAPIKeyRequest`, `ValidationResult`), **`maas-api/internal/subscription/handler.go`** (`SelectSubscription`), **`maas-api/internal/subscription/types.go`** (`SelectRequest`, `SelectResponse`). URLs are built in **`maas-controller/pkg/controller/maas/maasauthpolicy_controller.go`** as `https://maas-api.<namespace>.svc.cluster.local:8443/internal/v1/...`.

### `POST /internal/v1/api-keys/validate`

| | |
|--|--|
| **When it runs** | Only for **`Authorization`** values matching the MaaS API key pattern (`Bearer sk-oai-…`); see **`apiKeyValidation`** **`when`** in the generated policy. |
| **HTTP** | **POST**, **`Content-Type: application/json`**. |
| **Request JSON** | `{"key": "<plaintext key>"}` — the **`key`** field is the bearer secret **without** the `Bearer ` prefix (CEL: `request.headers.authorization.replace("Bearer ", "")`). |
| **Success response** | HTTP **200**, body **`ValidationResult`**: `valid: true` plus identity fields populated from the key row in storage. |
| **Invalid key** | HTTP **200** with `valid: false` and **`reason`** (Authorino reads metadata; 4xx is not the primary “bad key” signal). |

**`ValidationResult` fields** (subset used by downstream OPA / subscription select):

| Field | Meaning |
|-------|---------|
| **`valid`** | `true` only if format, hash lookup, lifecycle, and **non-empty bound subscription** all pass (maas-api fails closed if subscription was never bound). |
| **`userId`** | Stable identifier for this key (same backing id as **`keyId`** in the current service implementation). |
| **`username`** | Owner username from key metadata. |
| **`keyId`** | API key id (UUID string). |
| **`groups`** | Group snapshot **stored at key creation**; used for **`MaaSAuthPolicy`** matching and for subscription selection. |
| **`subscription`** | **MaaSSubscription name** bound at mint time; passed through as **`requestedSubscription`** on the select call. |
| **`reason`** | When **`valid`** is `false`: e.g. invalid format, key not found, revoked/expired, or missing subscription. |

**Transport errors:** unexpected failures in the service layer can yield HTTP **500** with `{"error":"validation failed"}` — that is distinct from **`valid: false`**.

### `POST /internal/v1/subscriptions/select`

| | |
|--|--|
| **When it runs** | After **`apiKeyValidation`** (lower **priority** number runs first in the metadata list). |
| **HTTP** | **POST**, **`Content-Type: application/json`**. |
| **Request JSON** (`SelectRequest`) | Built from CEL so the **API key path** prefers **`auth.metadata.apiKeyValidation`**: **`groups`**, **`username`**, **`requestedSubscription`** from the bound subscription on the key, and **`requestedModel`** set to **`"<modelNamespace>/<modelName>"`** for the **HTTPRoute** being enforced. |

**`SelectResponse` — HTTP status**

| Behavior | Notes |
|----------|--------|
| **Almost always HTTP 200** | Including business failures — Authorino expects **`error`** / **`message`** in the body. |
| **Invalid JSON body** | Still **200** with `error: "bad_request"` and a message describing the bind failure. |

**`SelectResponse` — success fields** (when **`error`** is empty / omitted)

Includes at least **`name`**, **`namespace`**, **`phase`**, **`ready`**, optional display fields (**`displayName`**, **`description`**, **`priority`**), **`deletionTimestamp`** when the subscription is terminating, optional **`organizationId`**, **`costCenter`**, **`labels`**, and **`modelRefs`**. The generated AuthPolicy’s **`subscription-valid`** OPA rule expects a resolved subscription **`name`**, empty **`error`**, **`phase`** in **Active** or **Degraded**, and no **`deletionTimestamp`**.

**`SelectResponse` — failure `error` codes** (non-exhaustive; see **`SelectSubscription`** in **`handler.go`**)

| `error` | Typical meaning |
|---------|-----------------|
| **`bad_request`** | Malformed request body. |
| **`not_found`** | No subscription for the user, or named subscription missing. |
| **`access_denied`** | User cannot use the requested subscription. |
| **`multiple_subscriptions`** | More than one candidate and no unambiguous choice. |
| **`model_not_in_subscription`** | Subscription does not include **`requestedModel`**. |
| **`model_unhealthy`** | Model / subscription not in an acceptable state; **`message`** / optional **`phase`** carry detail. |
| **`internal_error`** | Unexpected selector failure. |
