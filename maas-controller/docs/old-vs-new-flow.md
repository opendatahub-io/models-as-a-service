# MaaS Authentication & Rate Limiting: Old Flow vs New Flow

## Overview

The MaaS system moved from a **tier-based** architecture to a **subscription-driven** architecture. This document compares both approaches.

---

## Old Flow (Tier-Based)

### How it worked

```
User Request
    │
    ▼
Gateway (maas-default-gateway)
    │
    ├── AuthPolicy (gateway-auth-policy) ─── targets Gateway
    │       │
    │       ├── 1. Authenticate via kubernetesTokenReview
    │       │
    │       ├── 2. Call MaaS API /v1/tiers/lookup
    │       │       └── POST { "groups": user.groups }
    │       │       └── MaaS API reads ConfigMap "tier-to-group-mapping"
    │       │       └── Returns: { "tier": "free" | "premium" | "enterprise" }
    │       │
    │       ├── 3. Authorize via SubjectAccessReview on LLMInferenceService
    │       │
    │       └── 4. Set response: auth.identity.tier = matched tier
    │
    ├── TokenRateLimitPolicy (gateway-token-rate-limits) ─── targets Gateway
    │       │
    │       ├── if auth.identity.tier == "free"       → 100 tokens/min
    │       ├── if auth.identity.tier == "premium"    → 50,000 tokens/min
    │       └── if auth.identity.tier == "enterprise" → 100,000 tokens/min
    │
    ▼
Model Endpoint
```

### Key resources

| Resource | Namespace | Purpose |
|----------|-----------|---------|
| AuthPolicy `gateway-auth-policy` | openshift-ingress | Single gateway-level auth: token review + tier lookup + RBAC |
| TokenRateLimitPolicy `gateway-token-rate-limits` | openshift-ingress | Single gateway-level rate limit with per-tier rates |
| ConfigMap `tier-to-group-mapping` | maas-api | Maps groups → tiers (free, premium, enterprise) |
| Deployment `maas-api` | opendatahub | Go API server for tier lookup, token minting, model listing |

### How admins configured it

1. **Define tiers**: Edit ConfigMap `tier-to-group-mapping` with tier names, levels, and group lists
2. **Assign users to tiers**: Add users to Kubernetes groups (e.g. `tier-premium-users`)
3. **Set rate limits**: Manually edit the `gateway-token-rate-limits` TokenRateLimitPolicy YAML
4. **Add models**: Deploy LLMInferenceService and annotate with `alpha.maas.opendatahub.io/tiers: '["free","premium"]'`

### Limitations

- **One tier per user**: If a user matches multiple tiers, only the highest-level one applies
- **Rigid rate limits**: All users in a tier get the same limits for all models
- **ConfigMap-based config**: No schema validation, hard to patch with Kustomize, opaque to GitOps tooling
- **No per-model limits**: Rate limits apply at the gateway level, not per model
- **Manual management**: Admins hand-craft policies and ConfigMaps

---

## New Flow (Subscription-Based)

### How it works

```
User Request
    │
    ▼
Gateway (maas-default-gateway)
    │
    ├── TokenRateLimitPolicy (gateway-default-deny) ─── targets Gateway (defaults)
    │       └── defaults: 0 tokens/min (deny all unsubscribed traffic)
    │       └── OVERRIDDEN by per-route policies below
    │
    ▼
HTTPRoute (per model, created by KServe)
    │
    ├── AuthPolicy (maas-auth-<policy>-model-<model>) ─── targets HTTPRoute
    │       │
    │       ├── 1. Authenticate via kubernetesTokenReview
    │       ├── 2. Authorize: deny if user NOT in allowed groups
    │       └── 3. Pass through groups + userid in response
    │
    ├── TokenRateLimitPolicy (subscription-<sub>-model-<model>) ─── targets HTTPRoute
    │       │
    │       └── Enforce per-model token limits from MaaSSubscription
    │
    ▼
Model Endpoint
```

### Key resources

| Resource | Namespace | Created by | Purpose |
|----------|-----------|------------|---------|
| MaaSModel | opendatahub | User/admin | Registers a model with MaaS |
| MaaSAuthPolicy | opendatahub | User/admin | Defines who (groups) can access which models |
| MaaSSubscription | opendatahub | User/admin | Defines per-model token rate limits for owner groups |
| AuthPolicy (generated) | llm | maas-controller | Per-model auth, one per (MaaSAuthPolicy, model) pair |
| TokenRateLimitPolicy (generated) | llm | maas-controller | Per-model rate limits, one per (MaaSSubscription, model) pair |
| TokenRateLimitPolicy `gateway-default-deny` | openshift-ingress | maas-controller install | Deny all by default (0 tokens) for models without a subscription |

### How admins configure it

1. **Register a model**: Create a `MaaSModel` CR pointing to the LLMInferenceService
2. **Grant access**: Create a `MaaSAuthPolicy` CR listing model refs and allowed groups
3. **Set rate limits**: Create a `MaaSSubscription` CR with per-model token limits and owner groups
4. **Done**: The maas-controller generates all Kuadrant policies automatically

### Dual-gate model

Both gates must pass for a request to succeed:

| Gate | CRD | Question | Failure |
|------|-----|----------|---------|
| Access (AuthPolicy) | MaaSAuthPolicy | Is this user allowed to access this model? | 401/403 |
| Commercial (TokenRateLimitPolicy) | MaaSSubscription | Does this user have a subscription covering this model? | 429 |

A user can have access to a model (via MaaSAuthPolicy) but no subscription → **429**.
A user can have a subscription but no access → **403**.

---

## Side-by-Side Comparison

| Aspect | Old (Tier-Based) | New (Subscription-Based) |
|--------|-----------------|-------------------------|
| **Config format** | ConfigMap (opaque YAML) | Kubernetes CRDs (schema-validated) |
| **Auth scope** | Gateway level (all models share one policy) | Per-HTTPRoute (each model gets its own policy) |
| **Rate limit scope** | Gateway level (tier determines limits for ALL models) | Per-model (each model can have different limits) |
| **User grouping** | Single tier (highest wins) | Multiple subscriptions per user, multiple models per subscription |
| **Access control** | SubjectAccessReview on LLMInferenceService | Group-based via MaaSAuthPolicy (decoupled from subscription) |
| **Rate limit config** | Manual YAML edit | Declarative via MaaSSubscription CR, controller generates policies |
| **Default deny** | No (unauthenticated blocked, but no rate limit for unknown tiers) | Yes (gateway-level `defaults: limit 0` for unsubscribed models) |
| **Managed by** | MaaS API (Go server) + manual policies | maas-controller (Kubernetes operator) |
| **GitOps friendly** | Difficult (ConfigMap + hand-crafted policies) | Yes (CRDs with standard Kustomize patching) |
| **Separation of concerns** | Tier = auth + billing combined | Auth (MaaSAuthPolicy) and billing (MaaSSubscription) separated |

---

## Migration Path

To move from old flow to new flow on an existing cluster:

1. Deploy the full stack (including maas-controller): `./scripts/deploy.sh`
   - Or install just the controller: `kubectl apply -k maas-controller/config/default`
2. For each model:
   - Create a `MaaSModel` CR referencing the LLMInferenceService
   - Create a `MaaSAuthPolicy` CR with the allowed groups
   - Create a `MaaSSubscription` CR with the token rate limits
3. The old `gateway-token-rate-limits` and `tier-to-group-mapping` ConfigMap can be removed
4. The MaaS API is still needed for token minting and model listing (unchanged)
