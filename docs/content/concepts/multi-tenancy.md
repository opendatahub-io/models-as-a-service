# Multi-Tenancy

!!! warning "Tech Preview"
    Multi-tenancy support is in Tech Preview. API and CR schemas may change in future releases.

MaaS supports multiple isolated tenants within a single cluster. Each tenant operates independently with dedicated API infrastructure, separate namespaces, and no shared state beyond the PostgreSQL database.

## Tenant Model

Multi-tenancy separates **platform context** from **MaaS runtime configuration** to allow independent lifecycle management:

**AITenant** (platform context):
- Lives in `ai-tenants` namespace (cluster tenant registry)
- Defines Gateway, OIDC, tenant namespace lifecycle
- Each tenant can use a dedicated OIDC provider (`spec.oidc`); tokens from one tenant's IdP are not valid on another tenant's gateway
- Owned by platform administrators
- Bootstraps the tenant environment

**MaasTenantConfig** (runtime configuration):
- Lives in tenant namespace as singleton `default-tenant`
- Defines API key policies, telemetry settings
- Owned by tenant administrators
- Configures MaaS-specific operational behavior

When you create an AITenant, the controller provisions the tenant namespace and creates the MaasTenantConfig automatically. When you delete an AITenant, tenant-scoped MaaS state and controller-managed platform resources are cleaned up (including `MaasTenantConfig`, MaaS CRs, per-tenant maas-api resources, and AITenant RBAC). The tenant namespace itself is kept so non-MaaS user objects in that namespace are preserved.

## Isolation Model

**Namespace isolation:**
- Each tenant has a dedicated namespace (`ai-tenant-{name}`)
- MaaS CRs (MaaSModelRef, MaaSAuthPolicy, MaaSSubscription) are namespace-scoped
- No cross-tenant resource visibility

**API isolation:**
- Each tenant has a separate maas-api Deployment
- Dedicated Gateway per tenant (Gateways cannot be shared)
- Dedicated IPP (payload-processing) Deployments per tenant in the gateway namespace, wired to that tenant's Gateway via a per-tenant EnvoyFilter
- API keys and subscriptions are tenant-scoped

**Database isolation:**
- Shared PostgreSQL database with schema-level isolation via `tenant_id` column
- No cross-tenant API key or subscription access

**Access control:**
- Kubernetes RBAC enforces tenant namespace boundaries
- Tenant-admin and object-admin roles grant scoped permissions
- See [Tenant RBAC](../configuration-and-management/tenant-rbac.md)

## Default Tenant

Existing single-tenant deployments automatically become the **default tenant** on upgrade. The controller bootstraps `AITenant/models-as-a-service` on startup, migrates legacy `Tenant/default-tenant` to `MaasTenantConfig/default-tenant`, and preserves all existing API keys, subscriptions, and models. No user action required.

## Creating Additional Tenants

Each additional tenant requires a dedicated Gateway. Create the Gateway first, then reference it in the AITenant CR. The controller provisions the tenant namespace, deploys an isolated maas-api instance, and configures access control.

See [Multi-Tenant Setup](../install/multi-tenant-setup.md) for procedures.

## Related Documentation

- [Multi-Tenant Setup](../install/multi-tenant-setup.md) - Creating and configuring tenants
- [External OIDC Configuration](../advanced-administration/external-oidc.md) - Per-tenant OIDC configuration and verification
- [Multi-Tenant Validation](../install/multi-tenant-validation.md) - Verification procedures
- [Tenant RBAC](../configuration-and-management/tenant-rbac.md) - Access control
- [AITenant CRD](../reference/crds/ai-tenant.md) - Platform context reference
- [MaasTenantConfig CRD](../reference/crds/tenant.md) - Runtime configuration reference
- [Controller Architecture](../architecture-internals/controller-architecture.md) - Namespace architecture details
