# ADR 0001: Standalone subscription mode for maas-api

- **Status:** Proposed
- **Date:** 2026-11-12
- **Owners:** maas-api maintainers
- **Related:** `opendatahub-io/models-as-a-service#1548`

## Context

`maas-api` currently assumes the full MaaS control plane:

- `maas-controller` reconciles `MaaSSubscription` objects.
- Kuadrant/Authorino authenticates requests at the gateway.
- Subscription selection binds every API key to a reconciled subscription.
- The MaaS informers must sync before the HTTP server starts.

Some deployments need only the API-key lifecycle and validation part of
`maas-api`. Two concrete consumers are:

- **PriceTag / EnMaaS**, which runs `maas-api` beside its own gateway and
  deliberately does not deploy `maas-controller`, Kuadrant, or subscription
  CRs.
- **Secure Single Server**, which runs Praxis, MaaS, and metering on one RHEL
  host without Kubernetes or a local LLM.

Those deployments currently pin a fork because the public `maas-api` rejects
key creation when no controller has populated `MaaSSubscription.status.phase`.
Operators must either fabricate controller-owned status or install the full MaaS
platform.

## Decision

Add an explicit, opt-in `MAAS_SUBSCRIPTION_MODE`:

| Value | Behavior |
| --- | --- |
| `enforced` (default) | Existing behavior. API keys require a reconciled subscription. All MaaS informers are required. |
| `standalone` | API keys are created and validated without a subscription. Subscription informers are not started. Subscription endpoints return `503`; subscription-scoped key operations return `400`. |

The mode is a deployment contract, not a new product tier. The default remains
`enforced`, so existing deployments are unchanged.

One `DisabledSelector` implements the existing subscription selection and
administration contracts. `cmd/main.go` passes either the live `Selector` or the
disabled implementation to every handler, so request packages do not infer
policy from a nil pointer or from control-plane configuration.

In standalone mode:

- `maas-api` still requires Kubernetes API access for its database secret and
  TLS profile discovery. It does not require the MaaS subscription CRDs.
- If the `MaaSModelRef` CRD is served at startup, only that informer starts and
  `/v1/models` lists ready models. If the CRD is absent, `/v1/models` returns an
  empty list. Restart `maas-api` after installing or removing the CRD. The
  gateway owns the model catalog in either case.
- API-key hashing, expiry, revocation, tenant isolation, and group validation
  remain active.
- The deployment is responsible for protecting the trusted identity headers
  that `maas-api` consumes.

## Consequences

### Positive

- Removes the fork dependency for standalone consumers.
- Makes the “API-key-only” deployment contract explicit and testable.
- Keeps the enforced path unchanged and fail-closed.

### Negative

- Adds a second startup path and a second authorization shape to maintain.
- Standalone deployments lose subscription governance and MaaS model catalog
  filtering by design.
- The mode is **unsupported** as a RHOAI product configuration.

### Neutral

- A future product decision may promote the mode to supported status. That
  requires a separate ADR, E2E coverage in the supported release matrix, and
  product sign-off.

## Alternatives considered

1. **Keep the fork.** Rejected: it externalizes a deployment dependency and
   blocks downstream releases on a private image.
2. **Require operators to fabricate `MaaSSubscription.status`.** Rejected: it
   makes controller-owned status a user contract and hides the real deployment
   shape.
3. **Install the full MaaS stack in standalone deployments.** Rejected: it
   defeats the purpose of a minimal API-key service and adds Kuadrant,
   controller, and CRD overhead.
4. **Infer the mode from missing CRDs.** Rejected: implicit behavior is hard to
   audit and can mask a broken enforced deployment. The mode must be explicit.

## Unsupported scope

Standalone mode is not a supported RHOAI configuration. It does not provide:

- subscription-based access control or token budgets;
- subscription-filtered MaaS model discovery;
- controller-managed credential or route reconciliation.

Consumers must provide those capabilities in their own gateway layer.
