# MaasTenantConfig

Configures MaaS-specific tenant settings. `MaasTenantConfig` is a namespace-scoped singleton; the resource name must be `default-tenant` (enforced by CEL validation).

Platform context such as Gateway and external OIDC belongs to [`AITenant`](ai-tenant.md). `MaasTenantConfig` owns MaaS runtime configuration such as API key policy and telemetry settings. The legacy `Tenant` CRD remains installed during the migration window so existing `Tenant/default-tenant` objects can be adopted and copied into `MaasTenantConfig/default-tenant`.

## Multi-Tenant Deployment

In multi-tenant deployments, each tenant has one `MaasTenantConfig` in its tenant namespace:

| Tenant Type | Config Namespace | Config Name | maas-api Service (in operator namespace) | Created By |
|-------------|------------------|-------------|------------------------------------------|------------|
| Default | `models-as-a-service` | `default-tenant` | `maas-api` | Default AITenant bootstrap |
| Additional | `ai-tenant-{tenantID}` | `default-tenant` | `maas-api-{tenantID}` | AITenant reconciler |

Key points:

- All `MaasTenantConfig` resources are named `default-tenant` within their namespace.
- The default `MaasTenantConfig/default-tenant` is created or adopted by `AITenant/models-as-a-service`.
- Additional tenant configs are created by the AITenant reconciler, which provisions the tenant namespace and config object.
- All maas-api Services deploy to the operator namespace (opendatahub for ODH, redhat-ods-applications for RHOAI), not to tenant namespaces.
- Each tenant has an isolated maas-api instance for API key and subscription management.
- `MaasTenantConfig` resources for additional tenants have the finalizer `maas.opendatahub.io/tenant-cleanup`.
- For AITenant-managed tenants, Gateway comes from `AITenant.status.gatewayRef`; OIDC comes from `AITenant.spec.oidc`.

See [AITenant CRD](ai-tenant.md) for creating additional tenants.

---

## Spec

### MaasTenantConfigSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| apiKeys | TenantAPIKeysConfig | No | Configuration for API key management |
| telemetry | TenantTelemetryConfig | No | Telemetry and metrics collection configuration |

---

## TenantAPIKeysConfig

`spec.apiKeys` controls API key lifecycle policies.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| maxExpirationDays | int32 | No | Maximum number of days an API key can be valid. Must be at least 1. |

---

## TenantTelemetryConfig

`spec.telemetry` controls what telemetry data the platform collects.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| enabled | bool | No | `true` | Whether telemetry collection is enabled |
| metrics | TenantMetricsConfig | No | - | Fine-grained control over metric dimensions |

### TenantMetricsConfig

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| captureOrganization | bool | No | `true` | Add an "organization" dimension to telemetry metrics |
| captureUser | bool | No | `false` | Add a "user" dimension containing the authenticated user ID. May have privacy implications; ensure compliance before enabling. |
| captureGroup | bool | No | `false` | Add a "group" dimension to telemetry metrics |
| captureModelUsage | bool | No | `true` | Capture per-model usage metrics |

---

## Status

### MaasTenantConfigStatus

| Field | Type | Description |
|-------|------|-------------|
| phase | string | High-level lifecycle phase. One of: `Pending`, `Active`, `Degraded`, `Failed` |
| conditions | []Condition | Latest observations. Types: `Ready`, `DependenciesAvailable`, `MaaSPrerequisitesAvailable`, `DeploymentsAvailable`, `Degraded` |

### Print Columns

`kubectl get maastenantconfig` displays:

| Column | Source |
|--------|--------|
| Ready | `.status.conditions[?(@.type=="Ready")].status` |
| Reason | `.status.conditions[?(@.type=="Ready")].reason` |
| Age | `.metadata.creationTimestamp` |

---

## Example

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaasTenantConfig
metadata:
  name: default-tenant
  namespace: models-as-a-service
spec:
  apiKeys:
    maxExpirationDays: 90
  telemetry:
    enabled: true
    metrics:
      captureOrganization: true
      captureUser: false
      captureGroup: false
      captureModelUsage: true
```

---

## Migration Notes

Existing `Tenant/default-tenant` resources are not deleted immediately. During reconciliation, the controller copies `Tenant.spec.apiKeys` and `Tenant.spec.telemetry` into `MaasTenantConfig/default-tenant` when those fields are not already set. Legacy `Tenant.spec.gatewayRef` and `Tenant.spec.externalOIDC` are migrated to the owning `AITenant` where possible, because Gateway and OIDC are platform context rather than MaaS runtime configuration.

The copy is fill-only: if `MaasTenantConfig/default-tenant` already has `spec.apiKeys` or `spec.telemetry`, the controller does not overwrite those fields from the legacy `Tenant`. Treat `MaasTenantConfig` as the source of truth after it exists.

A namespace that only has the legacy `Tenant/default-tenant` object is unsupported after this migration. Admission compatibility may still allow older tenant-scoped resources during the grace window, but platform workload reconciliation runs from `MaasTenantConfig/default-tenant`; restore the owning `AITenant` bootstrap or create the `MaasTenantConfig` singleton before relying on that namespace.

## Related Documentation

- [AITenant CRD](ai-tenant.md) - Tenant namespace, Gateway, OIDC, and tenant-admin RBAC
- [MaaSModelRef CRD](maas-model-ref.md) - Model endpoint references
- [MaaSAuthPolicy CRD](maas-auth-policy.md) - Access control policies
- [MaaSSubscription CRD](maas-subscription.md) - Subscription and rate limiting
