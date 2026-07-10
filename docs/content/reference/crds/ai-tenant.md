# AITenant

Bootstraps a MaaS tenant from an infrastructure namespace. `AITenant` creates or labels the derived tenant namespace, validates an existing tenant Gateway, owns tenant platform context such as Gateway and OIDC configuration, creates the `MaasTenantConfig/default-tenant` MaaS config object, and creates tenant-admin Roles.

`AITenant` resources must be created in the controller-configured infrastructure namespace, which defaults to `ai-tenants`. The controller creates this namespace if it does not already exist. Set the controller `--aitenant-namespace` flag to use a different infrastructure namespace.

Creates outside the configured infrastructure namespace are rejected by the validating admission webhook before the object is persisted.

The controller automatically creates `AITenant/models-as-a-service` for the default tenant once per `Config/default` lifecycle. That AITenant targets the existing default Gateway and creates or adopts `MaasTenantConfig/default-tenant` in the MaaS subscription namespace. For migration compatibility, the default tenant keeps legacy resource names such as `maas-api`, `maas-api-route`, and `maas-api-auth-policy`; non-default tenants use suffixed names. If an administrator deletes the default AITenant after bootstrap, the controller does not recreate it until the `Config/default` anchor is recreated.

---

## Spec

### AITenantSpec

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| gateway | AITenantGatewayRef | No | Existing Gateway to reference. If omitted, the Gateway name defaults to the `AITenant` name. |
| oidc | TenantExternalOIDCConfig | No | OIDC settings for this tenant's AI Gateway platform context. AITenant-managed tenants do not mirror this into `MaasTenantConfig`. |
| rbac | AITenantRBACConfig | No | Deprecated compatibility field. Accepted but ignored; create standard Kubernetes RoleBindings instead. |

`spec.rbac` remains in the served schema only for upgrade compatibility with existing manifests. New manifests should omit it. The controller does not create, update, or delete user-managed RoleBindings from this field.

---

## Tenant Namespace

For non-default tenants, the controller derives the tenant namespace from the `AITenant` name as `ai-tenant-<aitenant-name>`. `AITenant` names are limited to 41 characters so per-tenant platform resources stay within Kubernetes 63-character name limits. The default `AITenant/models-as-a-service` keeps the configured MaaS tenant namespace, usually `models-as-a-service`, for migration compatibility.

Deleting an `AITenant` performs a hard cleanup of the tenant's MaaS state. The controller revokes active API keys through the tenant maas-api instance, deletes the bridge `Tenant/default-tenant`, deletes tenant-scoped MaaS resources such as `MaaSSubscription` and `MaaSAuthPolicy`, removes per-tenant maas-api platform resources, and deletes the tenant namespace. The shared Gateway object is never deleted or modified by `AITenant` reconciliation; tenant-scoped gateway and claim resources are cleaned up during deletion.

If namespace deletion is blocked by remaining content or finalizers, the `AITenant` stays in `Terminating` phase with `Ready=False` and reason `DeletionBlocked` until the namespace can finish deleting.

---

## Namespace Discovery

`AITenant` labels tenant namespaces with `ai-gateway.opendatahub.io/tenant=<aitenant-name>` and `maas.opendatahub.io/managed-by-aitenant=true`. When `maas-controller` runs with `--enable-tenant-namespace-discovery=true`, `MaaSAuthPolicy` and `MaaSSubscription` resources in those namespaces are reconciled against the owning `AITenant` platform context (`status.gatewayRef` and `spec.oidc`).

---

## Ownership Semantics

`AITenant` owns derived platform context for AITenant-managed tenants:

- Gateway context: `spec.gateway` intent and resolved `status.gatewayRef`
- External OIDC context: `spec.oidc`
- Tenant namespace metadata and tenant-admin Roles

The `MaasTenantConfig/default-tenant` object in each tenant namespace owns MaaS-specific user configuration, such as API key and telemetry settings. For backward compatibility, old `Tenant.spec.gatewayRef` and `Tenant.spec.externalOIDC` values may remain on existing legacy objects, but AITenant-managed reconciliation migrates or ignores them.

---

## AITenantGatewayRef

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| name | string | No | `metadata.name` | Name of the Gateway in the controller-configured Gateway namespace. |

The Gateway namespace is controller configuration, not an `AITenant` spec field. The Gateway must already exist, normally after network or cluster administrator approval. The controller only reads the Gateway and reports the resolved reference in `status.gatewayRef`; it does not create, label, annotate, reconcile, adopt, or delete Gateway resources.

---

## Tenant Administration

The controller creates tenant-admin Roles but does not create RoleBindings from `spec.rbac` or any other `AITenant` field. Platform administrators grant tenant access by creating standard Kubernetes `RoleBinding` resources that reference those Roles. See [Tenant RBAC](../../configuration-and-management/tenant-rbac.md) for Role names, permissions, lifecycle cleanup notes, and examples for users, groups, and ServiceAccounts.

---

## Status

### AITenantStatus

| Field | Type | Description |
|-------|------|-------------|
| phase | string | High-level lifecycle phase. One of `Pending`, `Active`, `Failed`, or `Terminating`. |
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
  namespace: ai-tenants
spec:
  gateway:
    name: red-team
  oidc:
    issuerUrl: "https://keycloak.example.com/realms/red-team"
    clientId: red-team-maas
```

---

## Related Documentation

- [MaasTenantConfig CRD](tenant.md) - MaaS runtime config object
- [Multi-Tenant Setup](../../install/multi-tenant-setup.md) - End-to-end guide for deploying additional tenants
- [Multi-Tenant Validation](../../install/multi-tenant-validation.md) - Validation steps for additional tenants
- [MaaSAuthPolicy CRD](maas-auth-policy.md) - Access control policies
- [MaaSSubscription CRD](maas-subscription.md) - Subscription and rate limiting
- [Tenant RBAC](../../configuration-and-management/tenant-rbac.md) - Granting tenant administration access with RoleBindings
