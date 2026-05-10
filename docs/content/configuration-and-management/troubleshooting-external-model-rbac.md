# ExternalModel RBAC: Service Creation Failures

## Symptoms

ExternalModel-backed MaaSModelRef objects remain `Pending` with backend-not-ready status. The `external-model-reconciler` logs show:

```text
cannot set blockOwnerDeletion if an ownerReference refers to a resource you can't set finalizers on
```

## Cause

The controller sets a controller `ownerReference` on the Service it creates for each ExternalModel. The API server enforces that the controller ServiceAccount (`maas-controller`) must have `update` permission on the `externalmodels/finalizers` subresource (OwnerReferencesPermissionEnforcement admission controller).

If this permission is missing, Service creation fails and routes never become healthy.

## Resolution

The ClusterRole `maas-controller-role` must include a rule allowing `update` on `externalmodels/finalizers`:

```yaml
apiGroups: ["maas.opendatahub.io"]
resources: ["externalmodels/finalizers"]
verbs: ["update"]
```

Source manifest: `deployment/base/maas-controller/rbac/clusterrole.yaml`

The ClusterRoleBinding `maas-controller-rolebinding` must bind this role to the `maas-controller` ServiceAccount in the controller namespace (typically `opendatahub`).

### Fix: Add the Missing Rule

If the ODH operator manages these resources (via the `modelsAsService` DSC component), upgrade or reconcile the component. Otherwise, patch the ClusterRole directly:

```bash
oc patch clusterrole maas-controller-role --type=json -p='[
  {
    "op": "add",
    "path": "/rules/-",
    "value": {
      "apiGroups": ["maas.opendatahub.io"],
      "resources": ["externalmodels/finalizers"],
      "verbs": ["update"]
    }
  }
]'
```

## Verification

Verify the ServiceAccount has the required permission:

```bash
# Replace NAMESPACE with the ExternalModel namespace (e.g., llm)
# Replace SA_NAMESPACE with the controller namespace (e.g., opendatahub)

oc auth can-i update externalmodels --subresource=finalizers \
  -n NAMESPACE \
  --as=system:serviceaccount:SA_NAMESPACE:maas-controller
```

Expected output: `yes`

!!! warning "Common Pitfall"
    Do not use `oc auth can-i update externalmodels/finalizers` (slash notation). This form often returns `no` even when the permission exists. Always use `--subresource=finalizers` for accurate results.

## Related

- [Namespace user permissions (RBAC)](namespace-rbac.md)
- [Controller Architecture](../architecture-internals/controller-architecture.md)
