# Tenant

Configures MaaS-specific tenant settings. The Tenant CRD is a namespace-scoped singleton — the resource name must be `default-tenant` (enforced by CEL validation). For AITenant-managed tenants, platform-derived context such as Gateway and external OIDC is owned by `AITenant`; `Tenant` owns MaaS-specific user configuration such as API key policies and telemetry settings.

## Multi-Tenant Deployment

In multi-tenant deployments, each tenant has its own Tenant CR in a dedicated namespace:

| Tenant Type | Tenant CR Namespace | Tenant CR Name | maas-api Service (in operator namespace) | Created By |
|-------------|---------------------|----------------|------------------------------------------|------------|
| **Default** | `models-as-a-service` | `default-tenant` | `maas-api` | Default AITenant bootstrap |
| **Additional** | `ai-tenant-{tenantID}` | `default-tenant` | `maas-api-{tenantID}` | AITenant reconciler |

**Key points:**
- All Tenant CRs are named `default-tenant` within their respective namespace
- The default `Tenant/default-tenant` is created or adopted by `AITenant/models-as-a-service`
- Additional tenants are created by the AITenant reconciler, which provisions the tenant namespace and Tenant CR
- All maas-api Services deploy to the operator namespace (opendatahub for ODH, redhat-ods-applications for RHOAI), not to tenant namespaces
- Each tenant has an isolated maas-api instance for API key and subscription management
- Tenant CRs for additional tenants have the finalizer `maas.opendatahub.io/tenant-cleanup`
- For AITenant-managed tenants, `Tenant.spec.gatewayRef` and `Tenant.spec.externalOIDC` are ignored. Gateway comes from the owning `AITenant.status.gatewayRef`; OIDC comes from `AITenant.spec.oidc`.

**Naming conventions:**
- `TenantIdentifier`: Used for Kubernetes resource naming (empty string for default, `{tenantID}` for additional tenants)
- `TenantName`: Used for database queries and HTTP headers (always `models-as-a-service` for default, `{tenantID}` for additional tenants)

See [AITenant CRD](ai-tenant.md) for creating additional tenants.

---

## Spec

### TenantSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| gatewayRef | TenantGatewayRef | No | Legacy/unmanaged Tenant reference to the Gateway (Gateway API) used for exposing model endpoints. Ignored for AITenant-managed tenants. |
| apiKeys | TenantAPIKeysConfig | No | Configuration for API key management |
| externalOIDC | TenantExternalOIDCConfig | No | Legacy/unmanaged Tenant external OIDC identity provider settings for the maas-api AuthPolicy. Ignored for AITenant-managed tenants; use `AITenant.spec.oidc`. |
| telemetry | TenantTelemetryConfig | No | Telemetry and metrics collection configuration |

---

## TenantGatewayRef

`spec.gatewayRef` identifies the Gateway API Gateway resource that the controller uses for model endpoint routing only for legacy/unmanaged Tenant resources. For AITenant-managed tenants, this value comes from the owning `AITenant.status.gatewayRef`.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| namespace | string | No | `openshift-ingress` | Namespace of the Gateway resource. Max length: 63 characters. |
| name | string | No | `maas-default-gateway` | Name of the Gateway resource. Max length: 63 characters. |

---

## TenantAPIKeysConfig

`spec.apiKeys` controls API key lifecycle policies.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| maxExpirationDays | int32 | No | Maximum number of days an API key can be valid. Must be at least 1. |

---

## TenantExternalOIDCConfig

`spec.externalOIDC` configures an external OIDC identity provider for token-based authentication only for legacy/unmanaged Tenant resources. For AITenant-managed tenants, configure `AITenant.spec.oidc`.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| issuerUrl | string | Yes | — | OIDC issuer URL. Must start with `https://`. Max length: 2048 characters. |
| clientId | string | Yes | — | OAuth2 client ID. Max length: 256 characters. |
| ttl | int | No | `300` | JWKS cache duration in seconds. Minimum: 30. |

---

## TenantTelemetryConfig

`spec.telemetry` controls what telemetry data the platform collects.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| enabled | bool | No | `true` | Whether telemetry collection is enabled |
| metrics | TenantMetricsConfig | No | — | Fine-grained control over metric dimensions |

### TenantMetricsConfig

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| captureOrganization | bool | No | `true` | Add an "organization" dimension to telemetry metrics |
| captureUser | bool | No | `false` | Add a "user" dimension containing the authenticated user ID. May have GDPR / privacy implications — ensure compliance before enabling. |
| captureGroup | bool | No | `false` | Add a "group" dimension to telemetry metrics |
| captureModelUsage | bool | No | `true` | Capture per-model usage metrics |

---

## Status

### TenantStatus

| Field | Type | Description |
|-------|------|-------------|
| phase | string | High-level lifecycle phase. One of: `Pending`, `Active`, `Degraded`, `Failed` |
| conditions | []Condition | Latest observations. Types: `Ready`, `DependenciesAvailable`, `MaaSPrerequisitesAvailable`, `DeploymentsAvailable`, `Degraded` |

### Print Columns

`kubectl get tenant` displays:

| Column | Source |
|--------|--------|
| Ready | `.status.conditions[?(@.type=="Ready")].status` |
| Reason | `.status.conditions[?(@.type=="Ready")].reason` |
| Age | `.metadata.creationTimestamp` |

---

## Example

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: Tenant
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

## Related Documentation

- [MaaSModelRef CRD](maas-model-ref.md) - Model endpoint references
- [MaaSAuthPolicy CRD](maas-auth-policy.md) - Access control policies
- [MaaSSubscription CRD](maas-subscription.md) - Subscription and rate limiting
