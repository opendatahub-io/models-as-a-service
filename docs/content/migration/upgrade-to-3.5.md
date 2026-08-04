# Upgrade Guide: RHOAI 3.4 to 3.5

This guide covers upgrading MaaS from RHOAI 3.4 to 3.5. The main changes are:

- **DSC field moved**: `kserve.modelsAsService` -> `aigateway.modelsAsAService` (KServe is no longer a prerequisite for MaaS)
- **Tenant CR split**: The legacy `Tenant` CR is replaced by `AITenant` (platform bootstrap: gateway, OIDC) + `MaasTenantConfig` (MaaS runtime: API keys, telemetry)
- **Infrastructure namespace separation**: maas-api and database secrets now deploy to a dedicated namespace by default
- **Multi-tenancy**: `AITenant` enables multi-tenant MaaS deployments
- **Body-based routing**: OpenAI-compatible model selection via request body

## Prerequisites

- Cluster admin access
- RHOAI 3.4 deployed with MaaS enabled (`kserve.modelsAsService.managementState: Managed`)
- Existing `Tenant/default-tenant` CR in the `models-as-a-service` namespace

## Phase 1: Pre-Upgrade Backup

```bash
mkdir -p migration-backup-3.5

# Backup Tenant CR (will be migrated to AITenant + MaasTenantConfig)
kubectl get tenant default-tenant -n models-as-a-service -o yaml \
  > migration-backup-3.5/tenant.yaml 2>/dev/null \
  && echo "Backed up Tenant CR" \
  || echo "No Tenant CR found"

# Backup DSC
kubectl get datasciencecluster default-dsc -o yaml \
  > migration-backup-3.5/dsc.yaml 2>/dev/null \
  && echo "Backed up DSC"

# Backup MaaS CRs
kubectl get maasmodelref -A -o yaml > migration-backup-3.5/maasmodelrefs.yaml 2>/dev/null
kubectl get maasauthpolicy -A -o yaml > migration-backup-3.5/maasauthpolicies.yaml 2>/dev/null
kubectl get maassubscription -A -o yaml > migration-backup-3.5/maassubscriptions.yaml 2>/dev/null

echo "Pre-upgrade backup complete"
```

## Phase 2: Upgrade RHOAI Operator to 3.5

Follow the standard RHOAI operator upgrade procedure. The operator upgrade will:

- Install new CRDs: `AITenant`, `MaasTenantConfig`
- Deploy the updated maas-controller
- Create a default `AITenant` CR that bootstraps the tenant namespace, gateway reference, and OIDC config
- Migrate legacy `Tenant` CR settings into `AITenant` + `MaasTenantConfig` automatically

### 2.1 Verify Upgrade

```bash
# Verify new CRDs are installed
kubectl get crd | grep -E "aitenants|maastenantconfigs"

# Verify maas-controller is running (updated version)
kubectl get pods -l control-plane=maas-controller -A

# Verify AITenant exists
kubectl get aitenant -A

# Verify MaasTenantConfig exists
kubectl get maastenantconfig default-tenant -n models-as-a-service
```

## Phase 3: DSC Field Migration

### What Changed

MaaS moved from `kserve.modelsAsService` to `aigateway.modelsAsAService`. KServe is no longer a prerequisite for MaaS.

### Backward Compatibility

**No action required on upgrade.** If your DSC has `kserve.modelsAsService: Managed`, the operator continues to deploy MaaS automatically through 3.6.

The old field is read-only once set (`self == oldSelf`) and will be removed in 3.7.

### Migrating to the New Field

When you are ready, update your DSC:

```yaml
spec:
  components:
    aigateway:
      managementState: Managed
      modelsAsAService:
        managementState: Managed
```

GitOps users: update your manifest and sync. The old `kserve.modelsAsService` field cannot be cleared until 3.7 -- leave it as-is.

### Verify DSC Migration

```bash
oc get datasciencecluster default-dsc -o jsonpath='{.spec.components.aigateway.modelsAsAService}'
# Expected: {"managementState":"Managed"}

oc get aigateway default-aigateway
# Verify AI Gateway operator is running
# Use opendatahub for ODH, redhat-ods-applications for RHOAI
CONTROLLER_NS=$(kubectl get pods -l control-plane=maas-controller -A -o jsonpath='{.items[0].metadata.namespace}')
kubectl get deployment ai-gateway-operator -n "$CONTROLLER_NS"
```

### DSC Field Reference

| 3.4 | 3.5+ |
|-----|------|
| `kserve.managementState: Managed` | Not required for MaaS |
| `kserve.modelsAsService.managementState: Managed` | `aigateway.modelsAsAService.managementState: Managed` |

## Phase 4: Tenant CR Migration

### What Changed

The 3.4 `Tenant` CR has been split into two resources with distinct responsibilities:

| Aspect | RHOAI 3.4 | RHOAI 3.5 |
|--------|-----------|-----------|
| Platform bootstrap (gateway, OIDC) | `Tenant/default-tenant` | `AITenant` (namespace-scoped) |
| MaaS runtime config (API keys, telemetry) | `Tenant/default-tenant` | `MaasTenantConfig/default-tenant` |
| Legacy `Tenant` CRD | Active | Deprecated (migration grace window) |

### Automatic Migration

The following happens without admin intervention during the upgrade:

1. **AITenant bootstrap**: `maas-controller` creates a default `AITenant` CR that owns the tenant namespace and gateway reference.
2. **OIDC migration**: `Tenant.spec.externalOIDC` and gateway configuration are migrated to the owning `AITenant`.
3. **MaasTenantConfig creation**: `maas-controller` creates `MaasTenantConfig/default-tenant` and copies `Tenant.spec.apiKeys` and `Tenant.spec.telemetry` into it.
4. **Legacy Tenant annotated**: The old `Tenant/default-tenant` is annotated as migrated (not deleted immediately to allow validation).

!!! warning "Gateway Namespace Mismatch"
    If the legacy `Tenant.spec.gatewayRef.namespace` differs from the controller's `--gateway-namespace` flag, the migration will fail with `LegacyTenantMigrationFailed`. Resolve the mismatch before upgrading.

### Field Mapping

| 3.4 Tenant field | 3.5 target | Notes |
|-------------------|------------|-------|
| `spec.gatewayRef.name` | `AITenant.spec.gateway.name` | Platform context |
| `spec.gatewayRef.namespace` | Controller `--gateway-namespace` flag | Not in AITenant spec; reported in `AITenant.status.gatewayRef.namespace` |
| `spec.externalOIDC.issuerUrl` | `AITenant.spec.oidc.issuerUrl` | Platform context |
| `spec.externalOIDC.clientId` | `AITenant.spec.oidc.clientId` | Platform context |
| `spec.externalOIDC.ttl` | `AITenant.spec.oidc.ttl` | Platform context |
| `spec.apiKeys.maxExpirationDays` | `MaasTenantConfig.spec.apiKeys.maxExpirationDays` | MaaS runtime |
| `spec.telemetry.enabled` | `MaasTenantConfig.spec.telemetry.enabled` | MaaS runtime |
| `spec.telemetry.metrics.*` | `MaasTenantConfig.spec.telemetry.metrics.*` | MaaS runtime |

### Verify Tenant Migration

```bash
# AITenant should exist and be Ready
kubectl get aitenant -A
# Expected: default AITenant with Ready=True

# MaasTenantConfig should exist
kubectl get maastenantconfig default-tenant -n models-as-a-service
# Expected: Ready=True

# Legacy Tenant should be annotated as migrated
kubectl get tenant default-tenant -n models-as-a-service -o jsonpath='{.metadata.annotations}' | jq .
# Expected: migration annotation present

# Verify OIDC settings migrated correctly (if you had custom OIDC)
# Replace <aitenant-name> with the name shown by 'kubectl get aitenant -A'
kubectl get aitenant <aitenant-name> -n <namespace> -o jsonpath='{.spec.oidc}'

# Verify API key and telemetry settings migrated
kubectl get maastenantconfig default-tenant -n models-as-a-service -o yaml | yq '.spec'
```

### Post-Migration: Update References

After verifying migration, update any automation or GitOps manifests that reference the old `Tenant` CR:

- Replace `kubectl get/patch tenant` with `kubectl get/patch maastenantconfig` (for API keys, telemetry)
- Replace `kubectl get/patch tenant` with `kubectl get/patch aitenant` (for gateway, OIDC)

## Phase 5: Infrastructure Namespace Separation

### What Changed

In 3.5, MaaS infrastructure resources (maas-api deployment and `maas-db-config` secret) deploy to a dedicated namespace by default, separate from the controller namespace:

- **ODH**: `opendatahub` (controller) -> `odh-ai-gateway-infra` (infrastructure)
- **RHOAI**: `redhat-ods-applications` (controller) -> `redhat-ai-gateway-infra` (infrastructure)

Migration happens automatically -- the controller detects existing resources in the old namespace and copies them to the new infrastructure namespace.

### Verify Infrastructure Separation

```bash
# Check infrastructure namespace (use your tenant namespace)
TENANT_NS=models-as-a-service
INFRA_NS=$(kubectl get maastenantconfig default-tenant -n "$TENANT_NS" -o jsonpath='{.status.infraNamespace}')
echo "Infrastructure namespace: $INFRA_NS"
# Expected: odh-ai-gateway-infra or redhat-ai-gateway-infra

# Verify maas-api is running in the infrastructure namespace
kubectl get pods -n "$INFRA_NS"
```

!!! warning "Source of Truth Change"
    After migration, the `maas-db-config` secret in the **infrastructure namespace** is the source of truth for database credentials. The controller does **not** sync changes back to the old namespace.

    If you rotate database credentials, update the secret in the infrastructure namespace:

    ```bash
    INFRA_NS=$(kubectl get maastenantconfig default-tenant -o jsonpath='{.status.infraNamespace}')
    kubectl edit secret maas-db-config -n $INFRA_NS
    ```

### ROSA and Restricted Clusters

Some clusters (including ROSA) have webhook restrictions that block namespace creation. On such clusters, disable namespace separation:

```bash
# Via environment variable before deploy
export INFRA_NAMESPACE=""
./scripts/deploy.sh
```

See [Infrastructure Namespace Separation](../configuration-and-management/infra-namespace-migration.md) for details.

## Phase 6: Validation

### 6.1 End-to-End Check

```bash
HOST="maas.$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"

# Pick an existing model to test with
MODEL=$(kubectl get maasmodelref -A -o jsonpath='{.items[0].metadata.name}')
echo "Testing with model: $MODEL"

# Verify model access still works
TOKEN=$(oc whoami -t)
curl -s -w "\nHTTP %{http_code}\n" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  "https://${HOST}/v1/chat/completions" \
  -d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hello\"}],\"max_tokens\":10}"
# Expected: HTTP 200
```

### 6.2 Verify All Resources

```bash
# CRDs
kubectl get crd | grep -E "maas|aitenant"

# Platform resources
kubectl get aitenant -A
kubectl get maastenantconfig -A

# MaaS resources (should be unchanged from 3.4)
kubectl get maasmodelref -A
kubectl get maasauthpolicy -A
kubectl get maassubscription -A

# Gateway policies
kubectl get authpolicy -A
kubectl get tokenratelimitpolicy -A

# DSC status
kubectl get datasciencecluster default-dsc -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'
echo ""
# Expected: True
```

## Troubleshooting

### LegacyTenantMigrationFailed

The AITenant controller detected a mismatch between the legacy `Tenant.spec.gatewayRef.namespace` and the controller's configured gateway namespace. Resolve by either:

- Updating the legacy `Tenant.spec.gatewayRef.namespace` to match the controller's `--gateway-namespace` flag before the upgrade
- Correcting the controller's `--gateway-namespace` flag to match the Tenant's gateway namespace

!!! warning
    Do not delete the legacy Tenant to resolve this -- it contains your OIDC, API key, and telemetry settings that need to be migrated.

### maas-api not starting after upgrade

Check if database credentials are in the correct namespace:

```bash
TENANT_NS=models-as-a-service
INFRA_NS=$(kubectl get maastenantconfig default-tenant -n "$TENANT_NS" -o jsonpath='{.status.infraNamespace}')
kubectl get secret maas-db-config -n "$INFRA_NS"
```

If the secret is missing in the infrastructure namespace, restart the maas-controller to trigger the automatic migration (which converts the database connection URL to use FQDN):

```bash
kubectl rollout restart deployment maas-controller -n <controller-namespace>
```

If the controller migration still fails, check the controller logs for the `convertToFQDNConnectionURL` step and resolve any connection URL issues before manually copying the secret.

### Old Tenant CR still present

The legacy `Tenant` CR is kept during the migration grace window. It will be removed in a future release. You can safely ignore it after verifying that `AITenant` and `MaasTenantConfig` are both Ready.

### Body-based routing not working

Body-based routing requires the IPP pipeline components. Verify:

```bash
kubectl get pods -n models-as-a-service | grep -E "model-provider-resolver|maas-headers-guard"
```

If missing, verify the AITenant is reconciled and the gateway reference is correct.
