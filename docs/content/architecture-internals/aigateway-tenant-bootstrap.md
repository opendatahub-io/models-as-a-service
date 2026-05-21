# AIGateway Tenant Bootstrap

Status: initial implementation for RHOAIENG-63338.

This document captures the MaaS-side working design for multi-tenancy Phase 1. The upstream Open Data Hub ADR is expected to finalize naming and long-term ownership; until then, the implementation treats `AIGateway` as the bootstrap CR and keeps the existing `Tenant` CR as the temporary MaaS config object.

## Decision

MaaS uses a two-CR model for tenant bootstrap:

- `AIGateway` lives in a MaaS/AI Gateway infrastructure namespace. It is created by a cluster-level or MaaS infrastructure administrator.
- `Tenant` remains namespace-scoped and is created automatically in the tenant namespace as the MaaS config object until the ADR names its replacement.

Each tenant gets a dedicated Gateway API `Gateway`. Model sharing across tenants is out of scope for the Phase 1 MVP.

## Field Ownership

`AIGateway` owns infrastructure and cross-service identity configuration:

- tenant namespace reference and namespace creation policy
- Gateway template: name, namespace, class, listeners, TLS certificate references
- OIDC issuer/client settings
- tenant-admin RBAC subjects

The MaaS config CR owns MaaS-specific runtime settings:

- API key policy
- telemetry settings
- future MaaS-only configuration

In this initial code, OIDC is mirrored from `AIGateway.spec.oidc` into `Tenant.spec.externalOIDC` only for compatibility with existing post-render and AuthPolicy code. That compatibility path should be removed when the renamed MaaS config CR lands.

## Reconciliation

When an `AIGateway` is reconciled, `maas-controller` creates or updates:

1. the tenant namespace, unless `spec.tenantNamespace.create=false`
2. the tenant `Gateway`
3. `Tenant/default-tenant` in the tenant namespace
4. tenant-admin RBAC in the tenant namespace
5. per-object RBAC for the specific `AIGateway` in the infra namespace

`spec.tenantNamespace.name` and `spec.gateway.namespace` are immutable in the CRD schema. The controller also rejects `AIGateway` objects in the protected ODH application namespace and in the legacy tenant namespace so bootstrap objects stay in a separate infra namespace.

## Lifecycle

Create and update reconcile the resources above. Delete uses an `AIGateway` finalizer to remove resources labeled and annotated as managed by that specific `AIGateway`.

Tenant namespace deletion is opt-in through `spec.tenantNamespace.cleanupOnDelete=true`, and only applies when the namespace was created by the controller for that same `AIGateway`. This avoids deleting tenant data when a pre-existing namespace was referenced.

## RBAC

Cluster or MaaS infrastructure administrators create `AIGateway` objects. Tenant admins listed in `spec.rbac.admins` receive:

- access to manage MaaSAuthPolicy and MaaSSubscription objects in their tenant namespace
- access to read and update the temporary `Tenant/default-tenant` config object
- per-object access to their specific `AIGateway`

## Deferred

The following items remain explicitly deferred to follow-on stories or the ODH ADR:

- final name and migration mechanics for the renamed `Tenant` config CR
- mutation webhook behavior for preserving existing default-tenant data during rename
- dynamic cache/watch expansion for MaaSSubscription and MaaSAuthPolicy across all AIGateway tenant namespaces
- long-term migration of `AIGateway` ownership from `maas-controller` to a platform-level controller
