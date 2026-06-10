# AITenant

Bootstraps a MaaS tenant from an infrastructure namespace. `AITenant` creates or labels the tenant namespace, validates an existing tenant Gateway, creates the temporary `Tenant/default-tenant` MaaS config object, and grants tenant-admin RBAC.

`AITenant` resources must be created in the controller-configured infrastructure namespace, which defaults to `redhat-ai-gateway-infra`. The controller creates this namespace if it does not already exist. Set the controller `--aitenant-namespace` flag to use a different infrastructure namespace.

---

## Spec

### AITenantSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| tenantNamespace | AITenantTenantNamespace | Yes | Tenant namespace where tenant administrators manage MaaS objects. |
| gateway | AITenantGatewayRef | No | Existing Gateway to reference. If omitted, the Gateway name defaults to the `AITenant` name. |
| oidc | TenantExternalOIDCConfig | No | OIDC settings mirrored into the temporary `Tenant/default-tenant` config object while the MaaS config CR rename is pending. |
| rbac | AITenantRBACConfig | No | Tenant-admin subjects that receive RBAC in the tenant namespace and read access to this `AITenant`. |

---

## AITenantTenantNamespace

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| name | string | Yes | — | Namespace for tenant-scoped MaaS objects. Immutable after creation. |
| create | bool | No | `true` | Whether the controller creates the namespace if it does not exist. |

The controller does not delete the tenant namespace when an `AITenant` is deleted. During deletion, it removes the labels and annotations it added to that namespace. Gateway resources are never deleted or modified by `AITenant` reconciliation.

---

## AITenantGatewayRef

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| name | string | No | `metadata.name` | Name of the Gateway in the controller-configured Gateway namespace. |

The Gateway namespace is controller configuration, not an `AITenant` spec field. The Gateway must already exist, normally after network or cluster administrator approval. The controller only reads the Gateway and reports the resolved reference in `status.gatewayRef`; it does not create, label, annotate, reconcile, adopt, or delete Gateway resources.

---

## AITenantRBACConfig

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| admins | []AITenantRBACSubject | No | Subjects granted tenant-admin RBAC. Max 128 entries. |

### AITenantRBACSubject

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| kind | string | Yes | One of `User`, `Group`, or `ServiceAccount`. |
| name | string | Yes | Subject name. |
| namespace | string | No | Required only for `ServiceAccount` subjects. |

---

## Status

### AITenantStatus

| Field | Type | Description |
|-------|------|-------------|
| phase | string | High-level lifecycle phase. One of `Pending`, `Active`, or `Failed`. |
| tenantNamespace | string | Reconciled tenant namespace. |
| gatewayRef | TenantGatewayRef | Resolved reference to the tenant Gateway. |
| conditions | []Condition | Latest observations. |

---

## Example

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: AITenant
metadata:
  name: red-team
  namespace: redhat-ai-gateway-infra
spec:
  tenantNamespace:
    name: red-team-maas
  gateway:
    name: red-team
  oidc:
    issuerUrl: "https://keycloak.example.com/realms/red-team"
    clientId: red-team-maas
  rbac:
    admins:
      - kind: Group
        name: red-team-admins
```

---

## Related Documentation

- [Tenant CRD](tenant.md) - Temporary MaaS runtime config object
- [MaaSAuthPolicy CRD](maas-auth-policy.md) - Access control policies
- [MaaSSubscription CRD](maas-subscription.md) - Subscription and rate limiting
