# Tenant

Configures the MaaS platform tenant. The Tenant CRD is a namespace-scoped singleton — the resource name must be `default-tenant` (enforced by CEL validation). It specifies the gateway used to expose model endpoints, API key policies, external OIDC authentication, and telemetry settings.

---

## Spec

### TenantSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| gatewayRef | TenantGatewayRef | No | Reference to the Gateway (Gateway API) used for exposing model endpoints. Defaults to `openshift-ingress/maas-default-gateway`. |
| apiKeys | TenantAPIKeysConfig | No | Configuration for API key management |
| externalOIDC | TenantExternalOIDCConfig | No | External OIDC identity provider settings for the maas-api AuthPolicy |
| telemetry | TenantTelemetryConfig | No | Telemetry and metrics collection configuration |

---

## TenantGatewayRef

`spec.gatewayRef` identifies the Gateway API Gateway resource that the controller uses for model endpoint routing.

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

`spec.externalOIDC` configures an external OIDC identity provider for token-based authentication.

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
  gatewayRef:
    namespace: openshift-ingress
    name: maas-default-gateway
  apiKeys:
    maxExpirationDays: 90
  externalOIDC:
    issuerUrl: "https://keycloak.example.com/realms/maas"
    clientId: maas-api
    ttl: 300
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
