# AITenant Tenant Bootstrap

Status: initial implementation for RHOAIENG-63338, updated after the June 2 multi-tenancy sync.

This page captures the MaaS-side implementation shape for multi-tenancy Phase 1. The resource name is `AITenant`; the existing namespace-scoped `Tenant` CR remains as the temporary MaaS config object until the MaaS config rename lands.

## Decision

MaaS uses a two-CR model for tenant bootstrap:

- `AITenant` lives in a MaaS/AI Gateway infrastructure namespace. It is created by a cluster-level or MaaS infrastructure administrator.
- `Tenant/default-tenant` is created automatically in the tenant namespace and carries MaaS runtime config for now.

Each tenant gets a dedicated Gateway API `Gateway` and a dedicated `maas-api` platform instance. Model sharing across tenants is out of scope for the Phase 1 MVP.

## Field Ownership

`AITenant` owns infrastructure and cross-service identity configuration:

- tenant namespace reference and namespace creation policy
- Gateway template: optional name and class
- tenant domain and TLS certificate
- OIDC issuer/client settings
- tenant-admin RBAC subjects

The Gateway namespace is not tenant-configurable. The controller creates tenant Gateways in the configured Gateway namespace, defaulting to `openshift-ingress`.

The MaaS config CR owns MaaS-specific runtime settings:

- API key policy
- telemetry settings
- future MaaS-only configuration

OIDC is currently mirrored from `AITenant.spec.oidc` into `Tenant.spec.externalOIDC` for compatibility with existing post-render and AuthPolicy code. That compatibility path should be removed when the renamed MaaS config CR lands.

## Reconciliation

When an `AITenant` is reconciled, `maas-controller` creates or updates:

1. the tenant namespace, unless `spec.tenantNamespace.create=false`
2. the tenant `Gateway` in the controller-configured Gateway namespace
3. `Tenant/default-tenant` in the tenant namespace
4. tenant-admin RBAC in the tenant namespace
5. per-object RBAC for the specific `AITenant` in the infra namespace

`spec.tenantNamespace.name` is immutable in the CRD schema. The controller rejects `AITenant` objects in the protected ODH application namespace and in the target tenant namespace so bootstrap objects stay in a separate infra namespace.

The existing `models-as-a-service` namespace is allowed as an `AITenant` target namespace for the default tenant. The `AITenant` object itself still must live in a separate infra namespace such as `ai-tenants`.

The Gateway listener is derived automatically from `spec.domain` and `spec.tls`:

- `domain` + `tls`: HTTPS listener on port 443 with TLS termination and the referenced certificate
- `domain` only: HTTP listener on port 80 with the domain as hostname
- neither: default HTTP listener on port 80 without a hostname

## maas-api Placement

For `AITenant`-managed tenants, the Tenant platform reconcile pipeline places `maas-api` workloads in the tenant namespace. That lets each tenant carry its own `maas-db-config` and moves the implementation toward the dedicated-per-tenant `maas-api` model.

Legacy `Tenant` objects that are not labeled or annotated as `AITenant`-managed continue to use the configured `--maas-api-namespace` fallback for compatibility with the existing single-tenant install path.

Full multi-namespace discovery for `Tenant`, `MaaSSubscription`, and `MaaSAuthPolicy` is still handled by the S1 dynamic namespace work. Until that lands, non-default tenant namespaces may not be watched by the controller cache.

## Lifecycle

Create and update reconcile the resources above. Delete uses an `AITenant` finalizer to remove resources labeled and annotated as managed by that specific `AITenant`.

Tenant namespace deletion is opt-in through `spec.tenantNamespace.cleanupOnDelete=true`, and only applies when the namespace was created by the controller for that same `AITenant`. This avoids deleting tenant data when a pre-existing namespace was referenced.

The June 2 sync also identified a future cleanup refinement: deleting an `AITenant` should eventually trigger MaaS key revocation through a one-shot cleanup job before the tenant `maas-api` is removed. This implementation does not add that job because the matching tenant-wide revoke API contract is not present yet.

## RBAC

Cluster or MaaS infrastructure administrators create `AITenant` objects. Tenant admins listed in `spec.rbac.admins` receive:

- access to manage MaaSAuthPolicy and MaaSSubscription objects in their tenant namespace
- access to read and update the temporary `Tenant/default-tenant` config object
- per-object access to their specific `AITenant`

## Deferred

The following items remain explicitly deferred to follow-on stories or the ODH ADR:

- final name and migration mechanics for the renamed `Tenant` config CR
- automatic default `AITenant` bootstrap from existing `models-as-a-service` resources
- dynamic cache/watch expansion for tenant namespaces
- tenant-wide key revocation cleanup job on delete
- resource quotas (maxModels, maxSubscriptions, maxApiKeys)
- long-term migration of `AITenant` ownership from `maas-controller` to a platform-level controller
