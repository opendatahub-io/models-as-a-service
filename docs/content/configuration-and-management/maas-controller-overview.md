# MaaS Controller Overview

The **MaaS Controller** is the Kubernetes operator that deploys platform pieces (MaaS API, gateway defaults, telemetry) from the `Tenant` CR and reconciles **MaaSModelRef**, **MaaSAuthPolicy**, and **MaaSSubscription** into Gateway API and Kuadrant resources (HTTPRoutes, AuthPolicies, TokenRateLimitPolicies).

This page is a **hub** for operators. Product behavior, access, and quotas are covered elsewhere; controller internals live under **Developer Guide**.

---

## Start here (by task)

| What you need | Where to go |
|---------------|-------------|
| How users get access, subscriptions, and rate limits | [Access and Quota Overview](../concepts/subscription-overview.md) |
| Install, validate, and fix deployment issues | [MaaS Setup](../install/maas-setup.md), [Validation](../install/validation.md), [Troubleshooting](../install/troubleshooting.md) |
| Configure quotas, auth policies, and subscriptions | [Quota and Access Configuration](./quota-and-access-configuration.md) |
| API keys (admin) | [API Key Administration](./api-key-administration.md) |
| Who can create MaaS CRs | [Namespace User Permissions (RBAC)](./namespace-rbac.md) |
| Opt-out and CRD-related annotations | [CRD Annotations](./crd-annotations.md) |
| TLS, Authorino cache, observability | [TLS Configuration](./tls-configuration.md), [Authorino Caching](./authorino-caching.md), [Observability](../advanced-administration/observability.md) |

---

## Controller internals (developers & support)

- [Controller Architecture](../architecture-internals/controller-architecture.md) — components, data model, what the controller creates
- [Reconciliation Flow](../architecture-internals/reconciliation-flow.md) — ownership, watches, status, lifecycle
- [Authentication & gateway identity](../architecture-internals/authentication-internals.md) — subscription selection, rate limits, identity context

---

## Quick checks

```bash
# Controller logs (default namespace may vary with your install)
kubectl logs deployment/maas-controller -n opendatahub --tail=50

# MaaS custom resources
kubectl get maasmodelref -A
kubectl get maasauthpolicy,maassubscription -A
```

For status phases and common failure modes, use [Troubleshooting](../install/troubleshooting.md) and the resources linked above.
