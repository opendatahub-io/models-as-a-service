# Tenant RBAC

`AITenant` creates the Roles required for tenant administration, but it does not bind users, groups, or ServiceAccounts to those Roles. Platform administrators grant tenant access by creating standard Kubernetes `RoleBinding` resources.

These `RoleBinding` resources are fully user-managed. Deleting an `AITenant` deletes the tenant namespace, so tenant-namespace RoleBindings are removed with that namespace. User-managed RoleBindings in the `AITenant` infrastructure namespace are not tenant-namespace content; review or delete them before deleting or recreating a tenant with the same name.

## Controller-Created Roles

For each `AITenant`, the controller creates two Roles.

| Role | Namespace | Purpose |
|------|-----------|---------|
| `aitenant-<tenant-name>-tenant-admin` | Tenant namespace | Manage tenant-scoped MaaS resources. |
| `aitenant-<tenant-name>-object-admin` | AITenant infrastructure namespace, usually `ai-tenants` | Read the `AITenant` object. |

For long tenant names, the controller truncates the Role name and appends a hash so the name fits the Kubernetes 63-character limit. To find the exact Role name, list Roles by tenant label:

```bash
kubectl get roles -A -l ai-gateway.opendatahub.io/tenant=<tenant-name>
```

## Tenant-Admin Permissions

The tenant-admin Role in the tenant namespace grants:

- `get`, `list`, `watch`, `create`, `update`, `patch`, and `delete` on `MaaSAuthPolicy`
- `get`, `list`, `watch`, `create`, `update`, `patch`, and `delete` on `MaaSSubscription`
- `get`, `update`, and `patch` on `Tenant/default-tenant`
- `get`, `list`, and `watch` on `MaaSModelRef`

The object-admin Role grants `get` on the specific `AITenant` object in the AITenant infrastructure namespace. Bind it when tenant administrators or dashboards need to read tenant bootstrap status.

## SubjectAccessReview

Kubernetes authorization checks use the username, groups, and ServiceAccount identity in the request. OIDC-backed users and groups do not need matching OpenShift `User` objects before they can be referenced in a RoleBinding. If the authenticated request identity matches a RoleBinding subject, `SubjectAccessReview` can authorize it.

## Default Tenant

The default tenant usually uses:

- AITenant name: `models-as-a-service`
- Tenant namespace: `models-as-a-service`
- AITenant infrastructure namespace: `ai-tenants`

Grant a group access to manage default tenant MaaS resources:

```bash
kubectl create rolebinding models-as-a-service-tenant-admin \
  --role=aitenant-models-as-a-service-tenant-admin \
  --group=rhoai-admins \
  -n models-as-a-service
```

Grant the same group read access to the default `AITenant` object:

```bash
kubectl create rolebinding models-as-a-service-aitenant-reader \
  --role=aitenant-models-as-a-service-object-admin \
  --group=rhoai-admins \
  -n ai-tenants
```

## Additional Tenants

For an additional tenant named `red-team`, the tenant namespace is usually `ai-tenant-red-team`.

Grant a user tenant-admin access:

```bash
kubectl create rolebinding red-team-tenant-admin \
  --role=aitenant-red-team-tenant-admin \
  --user=alice@example.com \
  -n ai-tenant-red-team
```

Grant a group tenant-admin access:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: red-team-tenant-admins
  namespace: ai-tenant-red-team
subjects:
  - kind: Group
    apiGroup: rbac.authorization.k8s.io
    name: red-team-admins
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: aitenant-red-team-tenant-admin
```

Grant a ServiceAccount tenant-admin access:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: red-team-admin-serviceaccount
  namespace: ai-tenant-red-team
subjects:
  - kind: ServiceAccount
    name: tenant-admin
    namespace: platform-automation
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: aitenant-red-team-tenant-admin
```

Grant read access to the `AITenant` object:

```bash
kubectl create rolebinding red-team-aitenant-reader \
  --role=aitenant-red-team-object-admin \
  --user=alice@example.com \
  -n ai-tenants
```

## Verify Access

Check tenant namespace permissions:

```bash
kubectl auth can-i create maassubscriptions.maas.opendatahub.io \
  --as=alice@example.com \
  -n ai-tenant-red-team
```

Check `AITenant` read access:

```bash
kubectl auth can-i get aitenants.maas.opendatahub.io/red-team \
  --as=alice@example.com \
  -n ai-tenants
```
