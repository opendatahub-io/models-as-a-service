# MaasTenantConfig

Configures MaaS-specific tenant settings. `MaasTenantConfig` is a namespace-scoped singleton; the resource name must be `default-tenant` (enforced by CEL validation).

Platform context such as Gateway and external OIDC belongs to [`AITenant`](ai-tenant.md). `MaasTenantConfig` owns MaaS runtime configuration such as API key policy and telemetry settings. The legacy `Tenant` CRD remains installed during the migration window so existing `Tenant/default-tenant` objects can be copied into `MaasTenantConfig/default-tenant`; the migrated legacy objects themselves are temporary.

## Multi-Tenant Deployment

In multi-tenant deployments, each tenant has one `MaasTenantConfig` in its tenant namespace:

| Tenant Type | Config Namespace | Config Name | maas-api Deployment | Created By |
|-------------|------------------|-------------|---------------------|------------|
| Default | `models-as-a-service` | `default-tenant` | `maas-api` (infra namespace) | Default AITenant bootstrap |
| Additional | `ai-tenant-{tenantID}` | `default-tenant` | `maas-api-{tenantID}` (infra namespace) | AITenant reconciler |

Key points:

- All `MaasTenantConfig` resources are named `default-tenant` within their namespace.
- The default `MaasTenantConfig/default-tenant` is created or adopted by `AITenant/models-as-a-service`.
- Additional tenant configs are created by the AITenant reconciler, which provisions the tenant namespace and config object.
- maas-api Deployments are created in the infrastructure namespace (AUTO-derived by default: `odh-ai-gateway-infra` for ODH or `redhat-ai-gateway-infra` for RHOAI). See [Infrastructure Namespace Migration](../../configuration-and-management/infra-namespace-migration.md) for details.
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
| maasApi | MaasAPIConfig | No | Replica count and resource configuration for the maas-api Deployment |
| payloadProcessing | PayloadProcessingConfig | No | Replica count, autoscaling, and resource configuration for the payload-processing Deployment |

---

## MaasAPIConfig

`spec.maasApi` controls replica count and resource overrides for the tenant's maas-api Deployment.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| replicas | int32 | No | 1 | Sets the Deployment `spec.replicas`. Spec-based replicas take precedence over the `maas.opendatahub.io/maas-api-replicas` annotation. When neither is set, the base manifest default applies. Valid range: 1–100. |
| resources | [ResourceRequirements](https://kubernetes.io/docs/reference/kubernetes-api/core/pod-v1/#resources) | No | requests: 128Mi/100m, limits: 256Mi/500m | Overrides the resource requests and limits for the maas-api container. When set, replaces the entire resource block (full replacement, not merge). Resource claims are not supported. |

### Resource Overrides

Use `resources` to override the default container resource requests and limits for the maas-api container. When set, the entire resource block is replaced (not merged with defaults). When not set, the base manifest defaults are used. Only `requests` and `limits` are accepted; resource claims are not supported.

```yaml
spec:
  maasApi:
    replicas: 2
    resources:
      requests:
        memory: "256Mi"
        cpu: "200m"
      limits:
        memory: "1Gi"
        cpu: "1"
```

---

## PayloadProcessingConfig

`spec.payloadProcessing` controls replica count, autoscaling, and resource limits for the tenant's payload-processing Deployment.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| replicas | int32 | No | 1 | Sets the Deployment `spec.replicas`. When `autoscaling` is also configured, this value becomes the HPA `minReplicas` floor instead. Valid range: 1–100. |
| autoscaling | PayloadProcessingAutoscaling | No | - | Presence of this section enables HPA-based autoscaling for payload-processing pods. |
| resources | [ResourceRequirements](https://kubernetes.io/docs/reference/kubernetes-api/core/pod-v1/#resources) | No | requests: 128Mi/100m, limits: 512Mi/1 | Overrides the resource requests and limits for the payload-processing container. When set, replaces the entire resource block (full replacement, not merge). Resource claims are not supported. |

### PayloadProcessingAutoscaling

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| maxReplicas | int32 | No | 10 | HPA `maxReplicas`. Valid range: 1–100. |
| targetCPUUtilization | int32 | No | 70 | HPA target CPU utilization percentage. Valid range: 1–100. |
| targetMemoryUtilization | int32 | No | 80 | HPA target memory utilization percentage. Valid range: 1–100. |

When autoscaling is enabled:
- `spec.payloadProcessing.replicas` (if set) becomes the HPA `minReplicas` floor instead of directly setting `spec.replicas` on the Deployment.
- The HPA manages the Deployment's replica count based on CPU and memory utilization thresholds.
- Scale-down uses a 300s stabilization window and a 25%/60s policy to prevent flapping.
- Scale-up reacts immediately (0s stabilization) with up to 100%/15s or 4 pods/15s (whichever is higher).

Remove the `autoscaling` section to disable autoscaling. The HPA will be removed and the Deployment will revert to static replica management via `spec.payloadProcessing.replicas`.

### Resource Overrides

Use `resources` to override the default container resource requests and limits for the payload-processing container. When set, the entire resource block is replaced (not merged with defaults). When not set, the base manifest defaults are used. Only `requests` and `limits` are accepted; resource claims are not supported.

If autoscaling is enabled, `resources.requests.cpu` and `resources.requests.memory` must both be specified when overriding resources. Incomplete overrides are rejected and the manifest defaults are preserved. Because `resources` replaces the entire block, a limits-only override removes existing requests. That can cause `FailedGetResourceMetric` for the affected HPA metric; another valid metric may still drive scaling.

```yaml
spec:
  payloadProcessing:
    replicas: 2
    resources:
      requests:
        memory: "256Mi"
        cpu: "200m"
      limits:
        memory: "2Gi"
        cpu: "2"
```

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

## Annotations

Optional metadata annotations that control per-tenant horizontal scaling.

### Replica Count Overrides

| Annotation | Default | Valid Range | Description |
|------------|---------|-------------|-------------|
| `maas.opendatahub.io/maas-api-replicas` | 1 | 1–100 | Overrides the maas-api Deployment replica count. When `spec.maasApi.replicas` is also set, the spec value takes precedence. |
| `maas.opendatahub.io/payload-processing-replicas` | 1 | 1–100 | **Deprecated:** use `spec.payloadProcessing.replicas` instead. Still supported during the migration window but will be removed in a future release. |

When set, the controller patches the corresponding Deployment's `spec.replicas` during reconciliation. Invalid values (non-numeric, zero, negative, or exceeding 100) produce a `Degraded` status condition with a remediation message; the default replica count is preserved.

Remove the annotation to revert to the manifest default.

### Example: Scaling for High Concurrency

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaasTenantConfig
metadata:
  name: default-tenant
  namespace: models-as-a-service
  annotations:
    maas.opendatahub.io/maas-api-replicas: "3"
    maas.opendatahub.io/payload-processing-replicas: "2"
spec:
  apiKeys:
    maxExpirationDays: 90
```

### Example: Autoscaling Payload Processing

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaasTenantConfig
metadata:
  name: default-tenant
  namespace: models-as-a-service
spec:
  payloadProcessing:
    replicas: 2                     # minimum replicas (HPA minReplicas floor)
    autoscaling:
      maxReplicas: 15               # maximum replicas (ceiling)
      targetCPUUtilization: 60      # scale up when avg CPU > 60%
      targetMemoryUtilization: 75   # scale up when avg memory > 75%
  apiKeys:
    maxExpirationDays: 90
```

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

During reconciliation, the controller copies `Tenant.spec.apiKeys` and `Tenant.spec.telemetry` into `MaasTenantConfig/default-tenant` when those fields are not already set. Legacy `Tenant.spec.gatewayRef` and `Tenant.spec.externalOIDC` are migrated to the owning `AITenant` where possible, because Gateway and OIDC are platform context rather than MaaS runtime configuration.

The copy is fill-only: if `MaasTenantConfig/default-tenant` already has `spec.apiKeys` or `spec.telemetry`, the controller does not overwrite those fields from the legacy `Tenant`. Treat `MaasTenantConfig` as the source of truth after it exists.

After migration, the controller marks the legacy singleton as migrated and removes it only after verifying that the expected AITenant-managed `MaasTenantConfig/default-tenant` exists and contains the migrated configuration. If the target is missing, ownership or tenant-namespace metadata does not match, copied configuration is incomplete, or the controller migration markers are absent, the legacy `Tenant/default-tenant` is not deleted. Note that migration markers and annotations may already have been applied to the legacy object even when deletion is skipped.

A namespace that only has the legacy `Tenant/default-tenant` object is unsupported after this migration. Admission compatibility may still allow older tenant-scoped resources during the grace window, but platform workload reconciliation runs from `MaasTenantConfig/default-tenant`; restore the owning `AITenant` bootstrap or create the `MaasTenantConfig` singleton before relying on that namespace.

## Related Documentation

- [AITenant CRD](ai-tenant.md) - Tenant namespace, Gateway, OIDC, and tenant-admin RBAC
- [MaaSModelRef CRD](maas-model-ref.md) - Model endpoint references
- [MaaSAuthPolicy CRD](maas-auth-policy.md) - Access control policies
- [MaaSSubscription CRD](maas-subscription.md) - Subscription and rate limiting
