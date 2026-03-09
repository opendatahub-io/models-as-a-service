# Subscription Concepts

This section provides reference information about how the subscription system works.

## CRD Relationships

The MaaS controller reconciles three custom resources to produce Kuadrant policies:

| You Create | Controller Generates | Purpose |
|------------|---------------------|---------|
| **MaaSModelRef** | (validates HTTPRoute) | Registers model; controller sets status.endpoint |
| **MaaSAuthPolicy** | Kuadrant **AuthPolicy** (per model) | Who can access which models |
| **MaaSSubscription** | Kuadrant **TokenRateLimitPolicy** (per model) | Per-model token rate limits |

Relationships are **many-to-many**: multiple MaaSAuthPolicies and MaaSSubscriptions can reference the same model. The controller aggregates them into a single Kuadrant policy per model.

## MaaSAuthPolicy — Access Control

Defines **who** (OIDC subjects/groups/users) can access **which models**.

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: data-science-access
  namespace: opendatahub
spec:
  modelRefs:
    - granite-3b-instruct
    - gpt-4-turbo
  subjects:
    groups:
      - name: data-science-team
    users:
      - name: service-account-a
```

- **modelRefs**: List of model names (MaaSModelRef `metadata.name`) this policy grants access to
- **subjects**: Groups and/or users; **OR logic** — any match grants access
- **meteringMetadata**: Optional billing and tracking labels

## MaaSSubscription — Commercial Entitlements

Defines per-model **token rate limits** for owner groups.

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: premium-subscription
  namespace: opendatahub
spec:
  owner:
    groups:
      - name: premium-users
    users: []
  modelRefs:
    - name: granite-3b-instruct
      tokenRateLimits:
        - limit: 100000
          window: 24h
    - name: gpt-4-turbo
      tokenRateLimits:
        - limit: 2000
          window: 2m
  tokenMetadata:
    organizationId: "acme-corp"
    costCenter: "ai-r-and-d"
```

- **owner**: Groups/users who "own" this subscription — they get rate limits for the listed models
- **modelRefs**: Models included with per-model token limits
- **tokenMetadata**: Optional metering and billing attribution
- **priority**: When a user has multiple subscriptions, higher priority wins (default: 0)

## Subscription Selection

When a user belongs to **multiple subscriptions** (via multiple group memberships):

1. **Single subscription** — No header needed; the subscription is used automatically
2. **Multiple subscriptions** — The `X-MaaS-Subscription` header is **required** to specify which subscription to use

The MaaS API's `/v1/subscriptions/select` endpoint (called by Authorino) validates the user's groups and the header value:

- If the header is missing and the user has multiple subscriptions → **403** (must specify)
- If the header specifies a subscription the user does not have access to → **403**
- On success, the selected subscription's rate limits are applied

## Request Flow

1. **Client** sends inference request with API key (or OpenShift token) and optional `X-MaaS-Subscription` header
2. **Gateway** routes to Authorino (AuthPolicy)
3. **Authorino** validates API key via MaaS API callback (`/internal/v1/api-keys/validate`)
4. **Authorino** calls MaaS API subscription selection (`/v1/subscriptions/select`) with user groups and header
5. **MaaS API** returns selected subscription or error
6. **Authorino** caches result (e.g., 60s TTL)
7. **Limitador** enforces TokenRateLimitPolicy for the selected subscription
8. Request reaches model endpoint

## Authentication Methods

The MaaS API supports two authentication methods for inference:

| Method | Auth Flow | Use Case |
|--------|-----------|----------|
| **API Key** | `sk-oai-*` format; validated via MaaS API callback | Programmatic access, long-lived credentials |
| **OpenShift Token** | Kubernetes TokenReview | Interactive use, CLI |

Both flows support subscription selection. API keys store the user's groups at creation time; OpenShift tokens provide live group membership.

## Observability

Limitador metrics include labels for subscription, user, and model:

- `authorized_hits`, `authorized_calls`, `limited_calls`
- Labels: `user`, `subscription`, `model` (or equivalent)

See [Observability](../advanced-administration/observability.md) for dashboard and metric details.
