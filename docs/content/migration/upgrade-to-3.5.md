# Upgrade to 3.5

## What Changed

MaaS moved from `kserve.modelsAsService` to `aigateway.modelsAsAService`. KServe is no longer a prerequisite for MaaS.

## Backward Compatibility

**No action required on upgrade.** If your DSC has `kserve.modelsAsService: Managed`, the operator continues to deploy MaaS automatically through 3.6.

The old field is read-only once set (`self == oldSelf`) and will be removed in 3.7.

## Migrating to the New Field

When you are ready, update your DSC:

```yaml
spec:
  components:
    aigateway:
      managementState: Managed
      modelsAsAService:
        managementState: Managed
```

GitOps users: update your manifest and sync. The old `kserve.modelsAsService` field cannot be cleared until 3.7 — leave it as-is.

## Verify

```bash
oc get aigateway default-aigateway
oc get deployment -n opendatahub ai-gateway-operator
oc get datasciencecluster default-dsc -o jsonpath='{.spec.components.aigateway.modelsAsAService}'
```

## Database Credential Management

!!! warning "Source of Truth Change"
    With infrastructure namespace separation (enabled by default), the `maas-db-config`
    secret in the infrastructure namespace is the source of truth for database credentials.
    The controller performs a one-time copy during migration but does **not** sync
    subsequent changes.

    If you rotate database credentials, update the secret in the infrastructure namespace
    (e.g., `odh-ai-gateway-infra` or `redhat-ai-gateway-infra`). The `MaasTenantConfig`
    status now includes an `infraNamespace` field showing where the active secret lives:

    ```bash
    kubectl get maastenantconfig default-tenant -o jsonpath='{.status.infraNamespace}'
    ```

    See [Infrastructure Namespace Separation](../configuration-and-management/infra-namespace-migration.md)
    for details.

## Dashboard Feature Flags

The `maasAuthPolicies` field in `OdhDashboardConfig` is **deprecated** and frozen via a CEL
transition rule. Clusters upgrading from 3.4 that already have it set will retain the value
without error, but the field is no longer used. New installs that attempt to set it will
receive a validation error.

Replace it with the new MaaS dashboard flags:

```bash
kubectl patch odhdashboardconfig odh-dashboard-config \
  -n redhat-ods-applications --type=merge \
  -p '{"spec":{"dashboardConfig":{"modelAsService":true,"vLLMDeploymentOnMaaS":true}}}'
```

| 3.4 (deprecated) | 3.5+ replacement | Effect |
|---|---|---|
| `maasAuthPolicies: true` | `modelAsService: true` | Enables MaaS UI in the Dashboard |
| *(not available)* | `vLLMDeploymentOnMaaS: true` | Enables vLLM deployment via the MaaS Dashboard UI |

!!! note
    The deprecated `maasAuthPolicies` value cannot be cleared until the field is removed in a
    future version. Leave it as-is; it has no effect on the dashboard once `modelAsService` is set.

## DSC Field Reference

| 3.4 | 3.5+ |
|-----|------|
| `kserve.managementState: Managed` | Not required for MaaS |
| `kserve.modelsAsService.managementState: Managed` | `aigateway.modelsAsAService.managementState: Managed` |
