# AITenant Bootstrap

Status: current multi-tenancy implementation for AITenant bootstrap and tenant namespace discovery.

This page captures the MaaS-side implementation of AI Gateway tenancy. The public CRD reference for the bootstrap API is [AITenant](../reference/crds/ai-tenant.md).

## Decision

MaaS uses a two-CR model for tenant bootstrap:

- `AITenant` lives in the controller-configured AI Gateway tenant registry namespace. The default namespace is `ai-tenants`, configurable with `--aitenant-namespace`.
- `Tenant/default-tenant` remains namespace-scoped and is created automatically in the tenant namespace as the temporary MaaS runtime config object.

Each tenant is associated with one Gateway API `Gateway`. The Gateway must already exist and is normally provisioned by a network or cluster administrator. The current controller validates and references Gateways; it does not create, adopt, patch, or delete them.

## Field Ownership

`AITenant` owns tenant bootstrap and cross-service identity configuration:

- tenant namespace reference and namespace creation policy
- tenant Gateway reference
- OIDC issuer/client settings mirrored into the temporary `Tenant/default-tenant`
- tenant-admin RBAC subjects

The temporary `Tenant/default-tenant` object bridges existing MaaS code paths while the long-term MaaS tenant config API is still being settled. Policy reconcilers read `Tenant.spec.gatewayRef` and `Tenant.spec.externalOIDC` from the same namespace as each `MaaSAuthPolicy` or `MaaSSubscription`.

## Reconciliation

When an `AITenant` is reconciled, `maas-controller` creates or updates:

1. the tenant namespace, unless `spec.tenantNamespace.create=false`
2. `Tenant/default-tenant` in the tenant namespace
3. tenant-admin RBAC in the tenant namespace
4. per-object read access for the specific `AITenant` in the infrastructure namespace

`spec.tenantNamespace.name` is immutable in the CRD schema. The controller rejects `AITenant` objects outside the configured infrastructure namespace and prevents tenant namespaces from overlapping the protected application namespace or the infrastructure namespace.

If `spec.gateway.name` is omitted, the Gateway name defaults to the `AITenant` name. The Gateway namespace comes from `--gateway-namespace`.

## Namespace Discovery

The `AITenant` reconciler labels tenant namespaces with:

- `ai-gateway.opendatahub.io/tenant=<aitenant-name>`
- `maas.opendatahub.io/managed-by-aitenant=true`
- `maas.opendatahub.io/tenant-name=<aitenant-name>`
- `maas.opendatahub.io/tenant-namespace=<tenant-namespace>`

When `maas-controller` starts with `--enable-tenant-namespace-discovery=true`, the cache watches `Tenant`, `MaaSSubscription`, and `MaaSAuthPolicy` across all namespaces. The reconcilers still filter at reconcile time: they accept the legacy default namespace or namespaces labeled with either `ai-gateway.opendatahub.io/tenant` or `maas.opendatahub.io/managed-by-aitenant=true`, and ignore unlabeled namespaces.

Generated Kuadrant `AuthPolicy` and `TokenRateLimitPolicy` resources remain model-route centered. Before attaching policy, the reconcilers verify that the model `HTTPRoute` references the Gateway from the tenant namespace's `Tenant/default-tenant.spec.gatewayRef`.

## Tenant MaaS API

The legacy single-tenant install continues to use the configured default maas-api service. The base controller Deployment keeps `--enable-tenant-namespace-discovery=false` and passes `--maas-api-namespace` from the controller pod namespace to preserve that behavior.

When tenant namespace discovery is enabled, tenant-scoped `Tenant/default-tenant` objects can drive dedicated maas-api resource names such as `maas-api-<tenant>`. Generated Authorino callbacks for tenant policies target the tenant-scoped maas-api service name in the configured maas-api namespace.

## Lifecycle

Create and update reconcile the resources above. Delete uses an `AITenant` finalizer to remove resources labeled and annotated as managed by that specific `AITenant`.

Tenant namespace deletion is not performed by the current controller. On `AITenant` deletion, the controller removes the metadata it added to the namespace and leaves the namespace and tenant data in place. Gateway resources are never deleted by `AITenant` reconciliation.

## RBAC

Cluster or MaaS infrastructure administrators create `AITenant` objects. Tenant admins listed in `spec.rbac.admins` receive:

- access to manage `MaaSAuthPolicy` and `MaaSSubscription` objects in their tenant namespace
- read access to `MaaSModelRef` objects in their tenant namespace
- access to read and update the temporary `Tenant/default-tenant` config object
- read access to their specific `AITenant`

## Deferred

The following items remain explicitly deferred to follow-on stories or the broader ADR:

- final name and migration mechanics for the renamed `Tenant` config CR
- mutation webhook behavior for preserving existing default-tenant data during rename
- Gateway creation or lifecycle ownership by a higher-level platform controller
- resource quotas such as maxModels, maxSubscriptions, and maxApiKeys
- long-term migration of tenant bootstrap ownership from `maas-controller` to a platform-level controller
