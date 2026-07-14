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

## DSC Field Reference

| 3.4 | 3.5+ |
|-----|------|
| `kserve.managementState: Managed` | Not required for MaaS |
| `kserve.modelsAsService.managementState: Managed` | `aigateway.modelsAsAService.managementState: Managed` |
