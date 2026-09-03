# Upgrade Guide: RHOAI 3.3 to 3.4

This guide covers upgrading MaaS from RHOAI 3.3 to 3.4. The main changes are:

- **ModelsAsService CR replaced by Tenant CR** for platform configuration
- **Tier-based access replaced by subscription CRDs** (MaaSModelRef, MaaSAuthPolicy, MaaSSubscription)
- **Gateway default policies** automatically created by maas-controller

## Background

RHOAI 3.4 replaces the tier-based access system with a CRD-driven subscription model. The old tier system (3.3 and earlier) used:

- A `tier-to-group-mapping` ConfigMap to define tiers and group membership
- Gateway-level AuthPolicy and TokenRateLimitPolicy with tier-based predicates
- Tier annotations on LLMInferenceService resources
- A cluster-scoped `ModelsAsService` CR for platform configuration

The new system uses:

- **MaaSModelRef** to register models with the MaaS platform
- **MaaSAuthPolicy** to define per-model access control
- **MaaSSubscription** to define per-model rate limits and billing
- **Tenant** for platform-wide configuration (auto-created by maas-controller, replaces ModelsAsService)

The operator upgrade installs the new CRDs and deploys maas-controller, but **does not clean up old tier resources**. Old Kuadrant policies will coexist with the new gateway defaults and must be removed manually.

## Prerequisites

- Cluster admin access
- RHOAI 3.3 deployed with MaaS enabled (`kserve.modelsAsService.managementState: Managed`)
- Kuadrant/RHCL compatible with 3.4 (Kuadrant v1.4.2+ for ODH, RHCL v1.3+ for RHOAI)
- PostgreSQL instance available for maas-api (API key storage)

## Phase 1: Pre-Upgrade Backup

Back up tier-based resources and the ModelsAsService CR before upgrading.

```bash
mkdir -p migration-backup

# Backup tier-to-group-mapping ConfigMap (if present)
kubectl get configmap tier-to-group-mapping -n maas-api -o yaml \
  > migration-backup/tier-to-group-mapping.yaml 2>/dev/null \
  && echo "Backed up tier-to-group-mapping" \
  || echo "No tier-to-group-mapping found"

# Backup gateway-auth-policy (if present)
kubectl get authpolicy gateway-auth-policy -n openshift-ingress -o yaml \
  > migration-backup/gateway-auth-policy.yaml 2>/dev/null \
  && echo "Backed up gateway-auth-policy" \
  || echo "No gateway-auth-policy found"

# Backup gateway TokenRateLimitPolicy (if present)
kubectl get tokenratelimitpolicy -n openshift-ingress -o yaml \
  > migration-backup/gateway-rate-limits.yaml 2>/dev/null \
  && echo "Backed up TokenRateLimitPolicy resources" \
  || echo "No TokenRateLimitPolicy found"

# Backup LLMInferenceService resources (with tier annotations)
kubectl get llminferenceservice -n llm -o yaml \
  > migration-backup/llm-models.yaml 2>/dev/null \
  && echo "Backed up LLMInferenceService resources" \
  || echo "No LLMInferenceService found"

# Backup ModelsAsService CR (capture custom config before it gets replaced)
if kubectl get modelsasservice default-modelsasservice -o yaml > migration-backup/modelsasservice.yaml 2>/dev/null; then
  if [ -s migration-backup/modelsasservice.yaml ]; then
    echo "Backed up ModelsAsService CR"
  else
    echo "ERROR: ModelsAsService backup is empty" >&2; exit 1
  fi
else
  echo "No ModelsAsService CR found (expected if MaaS was not enabled in 3.3)"
fi

# Record current state
echo "=== Pre-upgrade snapshot ===" > migration-backup/pre-upgrade-state.txt
echo "Date: $(date -u)" >> migration-backup/pre-upgrade-state.txt
kubectl get authpolicy -A >> migration-backup/pre-upgrade-state.txt 2>/dev/null
kubectl get tokenratelimitpolicy -A >> migration-backup/pre-upgrade-state.txt 2>/dev/null
kubectl get configmap -n maas-api >> migration-backup/pre-upgrade-state.txt 2>/dev/null
```

## Phase 2: Upgrade RHOAI Operator to 3.4

### 2.1 Upgrade the Operator

Follow the standard RHOAI operator upgrade procedure. The operator upgrade will:

- Install MaaS CRDs (`maas.opendatahub.io/v1alpha1`): Tenant, MaaSModelRef, MaaSAuthPolicy, MaaSSubscription, ExternalModel
- Deploy maas-controller when `modelsAsService: Managed` is set in the DSC
- Replace the old cluster-scoped `ModelsAsService` CR with a namespace-scoped `Tenant` CR -- see [Phase 2.5](#phase-25-modelsasservice-to-tenant-transition) for details
- Create gateway-level default policies: `gateway-default-auth` and `gateway-default-deny`

**Important:** The `modelsAsService` field defaults to `Removed` if not specified in the DSC. If you already had `modelsAsService: Managed` in 3.3, it carries over. The DSC spec field `kserve.modelsAsService.managementState` is unchanged between 3.3 and 3.4 -- no DSC changes are required.

### 2.2 Verify Upgrade

```bash
# Verify MaaS CRDs are installed
kubectl get crd | grep maas.opendatahub.io

# Verify maas-controller is running
kubectl get pods -l control-plane=maas-controller -A

# Verify new gateway default policies were created
kubectl get authpolicy gateway-default-auth -n redhat-ods-applications
kubectl get tokenratelimitpolicy gateway-default-deny -n redhat-ods-applications
```

### 2.3 Identify Policy Conflicts

After the upgrade, both old and new gateway-level policies may target the same gateway from different namespaces. Audit the state:

```bash
echo "=== AuthPolicies targeting maas-default-gateway ==="
kubectl get authpolicy -A
# You may see BOTH:
#   openshift-ingress         gateway-auth-policy    (OLD - tier-based)
#   redhat-ods-applications   gateway-default-auth   (NEW - maas-controller managed)
#   redhat-ods-applications   maas-api-auth-policy   (NEW - targets maas-api HTTPRoute)

echo ""
echo "=== TokenRateLimitPolicies targeting maas-default-gateway ==="
kubectl get tokenratelimitpolicy -A
# You may see BOTH:
#   openshift-ingress         gateway-tier-rate-limits   (OLD - tier predicates)
#   redhat-ods-applications   gateway-default-deny       (NEW - maas-controller managed)
```

Both old and new policies targeting the same `maas-default-gateway` creates conflicting policy behavior in Kuadrant. This must be resolved by removing the old policies in Phase 3.

## Phase 2.5: ModelsAsService to Tenant Transition

Starting in 3.4, MaaS platform configuration is owned by `maas-controller` via `Tenant/default-tenant` instead of the operator's `ModelsAsService` CR. This section covers what changes automatically and what manual steps may be required.

### What Changed

| Aspect | RHOAI 3.3 | RHOAI 3.4 |
|--------|-----------|-----------|
| CR kind | `ModelsAsService` | `Tenant` |
| API group | `components.platform.opendatahub.io/v1alpha1` | `maas.opendatahub.io/v1alpha1` |
| Scope | Cluster-scoped | Namespace-scoped (`models-as-a-service`) |
| Instance name | `default-modelsasservice` | `default-tenant` |
| Reconciled by | ODH operator (ModelsAsService controller) | maas-controller (TenantReconciler) |
| DSC field | `kserve.modelsAsService.managementState` | Same (unchanged) |

### What the Operator Handles Automatically

The following happen without admin intervention during the upgrade:

1. **Old CR cleanup**: The operator's garbage collection removes the old `ModelsAsService` CR (the operator no longer creates it).
2. **maas-controller deployment**: The operator deploys `maas-controller` (CRDs, RBAC, Deployment) when `modelsAsService: Managed`.
3. **Default tenant creation**: `maas-controller` automatically creates or adopts `Tenant/default-tenant` in the MaaS tenant namespace.
4. **Platform reconciliation**: `maas-controller` deploys maas-api, gateway policies, telemetry, and all other platform resources via the default Tenant CR.

### Manual Steps: Re-applying Custom Configuration

If you had customized the `ModelsAsService` CR spec in 3.3 (e.g., custom gateway, external OIDC, telemetry settings), re-apply those values to the new `Tenant/default-tenant` CR.

**If all fields were at defaults, no manual steps are needed.**

The following table maps old `ModelsAsService` spec fields to new fields:

| Old ModelsAsService field | New field | Default value |
|---------------------------|-----------|---------------|
| `spec.gatewayRef.name` | `Tenant/default-tenant.spec.gatewayRef.name` | `maas-default-gateway` |
| `spec.gatewayRef.namespace` | `Tenant/default-tenant.spec.gatewayRef.namespace` | `openshift-ingress` |
| `spec.externalOIDC.issuerUrl` | `Tenant/default-tenant.spec.externalOIDC.issuerUrl` | (not set) |
| `spec.externalOIDC.clientId` | `Tenant/default-tenant.spec.externalOIDC.clientId` | (not set) |
| `spec.externalOIDC.ttl` | `Tenant/default-tenant.spec.externalOIDC.ttl` | `300` |
| `spec.telemetry.enabled` | `Tenant/default-tenant.spec.telemetry.enabled` | `true` |
| `spec.telemetry.metrics.captureOrganization` | `Tenant/default-tenant.spec.telemetry.metrics.captureOrganization` | `true` |
| `spec.telemetry.metrics.captureUser` | `Tenant/default-tenant.spec.telemetry.metrics.captureUser` | `false` |
| `spec.telemetry.metrics.captureGroup` | `Tenant/default-tenant.spec.telemetry.metrics.captureGroup` | `false` |
| `spec.telemetry.metrics.captureModelUsage` | `Tenant/default-tenant.spec.telemetry.metrics.captureModelUsage` | `true` |
| `spec.apiKeys.maxExpirationDays` | `Tenant/default-tenant.spec.apiKeys.maxExpirationDays` | (not set) |

After `Tenant/default-tenant` exists, update it directly for Gateway, OIDC, API key, and telemetry settings.

To re-apply custom values, patch the Tenant CR after the upgrade:

```bash
# Example: Re-apply external OIDC configuration
kubectl patch tenant default-tenant -n models-as-a-service --type merge \
  -p '{
    "spec": {
      "externalOIDC": {
        "issuerUrl": "https://keycloak.example.com/realms/maas",
        "clientId": "maas-client"
      }
    }
  }'

# Example: Re-apply API key configuration
kubectl patch tenant default-tenant -n models-as-a-service --type merge \
  -p '{
    "spec": {
      "apiKeys": {
        "maxExpirationDays": 90
      }
    }
  }'
```

**Tip:** If you backed up the old `ModelsAsService` CR before the upgrade, compare specs:

```bash
diff <(yq '.spec' migration-backup/modelsasservice.yaml) \
     <(kubectl get tenant default-tenant -n models-as-a-service -o yaml | yq '.spec')
```

### Verify CR Transition

```bash
# Old ModelsAsService CR should be gone
echo "Old ModelsAsService CR (should fail):"
kubectl get modelsasservice default-modelsasservice 2>&1
# Expected: error (not found or resource type not recognized)

# New Tenant CR should exist and be Active/Ready
echo ""
echo "New Tenant CR:"
kubectl get tenant default-tenant -n models-as-a-service
# Expected: Ready=True

# Tenant details
echo ""
echo "Tenant status:"
kubectl get tenant default-tenant -n models-as-a-service -o jsonpath='{.status.phase}'
echo ""
# Expected: Active

# DSC status should show ModelsAsService as Ready
echo ""
echo "DSC ModelsAsService status:"
kubectl get datasciencecluster default-dsc -o jsonpath='{.status.conditions[?(@.type=="modelsasserviceReady")].status}'
echo ""
# Expected: True
```

## Phase 3: Cleanup Old Tier Resources

The operator does not clean up old tier resources automatically. If your 3.3 deployment used the tier-based system, each resource must be removed manually.

!!! note "Skip if no tiers"
    If your 3.3 deployment did not use tier-based access (no `tier-to-group-mapping` ConfigMap, no tier-based gateway policies, no `alpha.maas.opendatahub.io/tiers` annotations on models), skip to [Phase 4](#phase-4-create-subscription-resources).

### 3.1 Delete Old Gateway AuthPolicy

```bash
kubectl delete authpolicy gateway-auth-policy -n openshift-ingress --ignore-not-found
# Verify only the new policy remains
kubectl get authpolicy -A
# Expected: gateway-default-auth in redhat-ods-applications (managed by maas-controller)
```

### 3.2 Delete Old Gateway TokenRateLimitPolicy

```bash
kubectl delete tokenratelimitpolicy gateway-tier-rate-limits -n openshift-ingress --ignore-not-found
# Verify only the new policy remains
kubectl get tokenratelimitpolicy -A
# Expected: gateway-default-deny in redhat-ods-applications (managed by maas-controller)
```

### 3.3 Delete Tier-to-Group Mapping ConfigMap

```bash
kubectl delete configmap tier-to-group-mapping -n maas-api --ignore-not-found
```

### 3.4 Remove Tier Annotations from Models

```bash
# Remove tier annotations from all LLMInferenceServices
for model in $(kubectl get llminferenceservice -n llm -o name); do
  kubectl annotate "$model" -n llm alpha.maas.opendatahub.io/tiers- --ignore-not-found
  echo "Removed tier annotation from $model"
done

# Verify annotations are removed
kubectl get llminferenceservice -n llm -o jsonpath='{range .items[*]}{.metadata.name}: {.metadata.annotations.alpha\.maas\.opendatahub\.io/tiers}{"\n"}{end}'
# Expected: no tier annotations
```

### 3.5 Verify Cleanup

```bash
echo "=== Cleanup verification ==="

# Old resources should be gone
echo "Old ConfigMap (should fail):"
kubectl get configmap tier-to-group-mapping -n maas-api 2>&1

echo "Old AuthPolicy (should fail):"
kubectl get authpolicy gateway-auth-policy -n openshift-ingress 2>&1

echo "Old TokenRateLimitPolicy (should fail):"
kubectl get tokenratelimitpolicy gateway-tier-rate-limits -n openshift-ingress 2>&1

# New resources should exist
echo ""
echo "New gateway-default-auth:"
kubectl get authpolicy gateway-default-auth -n redhat-ods-applications

echo "New gateway-default-deny:"
kubectl get tokenratelimitpolicy gateway-default-deny -n redhat-ods-applications
```

### Cleanup Inventory

| # | Resource | Kind | Namespace | Action | Notes |
|---|----------|------|-----------|--------|-------|
| 1 | `gateway-auth-policy` | AuthPolicy | `openshift-ingress` | Delete | Conflicts with `gateway-default-auth` in `redhat-ods-applications` |
| 2 | `gateway-tier-rate-limits` | TokenRateLimitPolicy | `openshift-ingress` | Delete | Conflicts with `gateway-default-deny` in `redhat-ods-applications` |
| 3 | `tier-to-group-mapping` | ConfigMap | `maas-api` | Delete | Orphaned -- no code reads this in 3.4 |
| 4 | `alpha.maas.opendatahub.io/tiers` | Annotation | `llm` (on each model) | Remove | Orphaned -- annotation is ignored in 3.4 |
| 5 | `/v1/tiers/lookup` | API Endpoint | N/A | Gone in 3.4 (no action) | Clients calling this will get 404 |

## Phase 4: Create Subscription Resources

With old resources cleaned up, create the new CRD-based configuration. For each model, create one MaaSModelRef. For each model/group access pair, create one MaaSAuthPolicy and one MaaSSubscription.

### 4.1 Register Models with MaaS (MaaSModelRef)

Create one MaaSModelRef per model:

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: my-model
  namespace: llm
spec:
  modelRef:
    kind: LLMInferenceService
    name: my-model
```

```bash
kubectl apply -f maasmodelref-my-model.yaml

# Wait for it to become Ready
kubectl wait maasmodelref my-model -n llm --for=jsonpath='{.status.phase}'=Ready --timeout=60s
```

### 4.2 Create Access Policies (MaaSAuthPolicy)

Create one MaaSAuthPolicy per access group:

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: my-model-premium-access
  namespace: models-as-a-service
spec:
  modelRefs:
    - name: my-model
      namespace: llm
  subjects:
    groups:
      - name: premium-users
    users: []
```

```bash
kubectl apply -f maasauthpolicy-premium.yaml

# Verify the controller created the underlying Kuadrant AuthPolicy
kubectl get authpolicy -n llm -l maas.opendatahub.io/model=my-model
```

### 4.3 Create Subscriptions with Rate Limits (MaaSSubscription)

Create one MaaSSubscription per group with rate limits:

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: my-model-premium-subscription
  namespace: models-as-a-service
spec:
  owner:
    groups:
      - name: premium-users
    users: []
  modelRefs:
    - name: my-model
      namespace: llm
      tokenRateLimits:
        - limit: 50000
          window: 1m
```

```bash
kubectl apply -f maassubscription-premium.yaml

# Verify the controller created the underlying Kuadrant TokenRateLimitPolicy
kubectl get tokenratelimitpolicy -n llm -l maas.opendatahub.io/model=my-model
```

### 4.4 Verify New Configuration

```bash
# All MaaS CRs should be Active/Ready
kubectl get maasmodelref -n llm
kubectl get maasauthpolicy -n models-as-a-service
kubectl get maassubscription -n models-as-a-service

# Per-model Kuadrant policies should exist
kubectl get authpolicy -n llm
kubectl get tokenratelimitpolicy -n llm
```

## Phase 5: Validation

!!! note "Body-based routing"
    The curl examples in this section use path-based URLs for continuity with the migration context. For new integrations, use the body-based endpoint (`https://${HOST}/v1/chat/completions` with the model in the request body). See [Inference](../user-guide/inference.md) for details.

### 5.1 Test Authorized Access

```bash
HOST="maas.$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"

# Log in as a user in the premium-users group
oc login --username=premium-user

TOKEN=$(oc whoami -t)

# Test model access
curl -s -w "\nHTTP %{http_code}\n" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  "https://${HOST}/llm/my-model/v1/chat/completions" \
  -d '{"model":"my-model","messages":[{"role":"user","content":"hello"}],"max_tokens":10}'
# Expected: HTTP 200
```

### 5.2 Test Unauthorized Access

```bash
# Log in as a user NOT in any authorized group
oc login --username=unauthorized-user

UNAUTH_TOKEN=$(oc whoami -t)

curl -s -w "\nHTTP %{http_code}\n" \
  -H "Authorization: Bearer $UNAUTH_TOKEN" \
  -H "Content-Type: application/json" \
  "https://${HOST}/llm/my-model/v1/chat/completions" \
  -d '{"model":"my-model","messages":[{"role":"user","content":"hello"}],"max_tokens":10}'
# Expected: HTTP 401 or 403
```

### 5.3 Test Rate Limiting

```bash
# Log back in as the authorized user
oc login --username=premium-user
TOKEN=$(oc whoami -t)

# Send rapid requests to trigger rate limits
for i in $(seq 1 100); do
  code=$(curl -s -o /dev/null -w "%{http_code}" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    "https://${HOST}/llm/my-model/v1/chat/completions" \
    -d '{"model":"my-model","messages":[{"role":"user","content":"hello"}],"max_tokens":10}')
  echo "Request $i: HTTP $code"
done
# Expected: mix of 200 and 429 after exceeding rate limit
```

### 5.4 Test API Key Flow (New in 3.4)

```bash
# Reuse the authorized user's token from the rate-limit test above

API_KEY=$(curl -s -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  "https://${HOST}/maas-api/v1/api-keys" \
  -d '{"name":"test-key"}' | jq -r '.key')

echo "API Key: ${API_KEY}"

# Use API key for model access
curl -s -w "\nHTTP %{http_code}\n" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  "https://${HOST}/llm/my-model/v1/chat/completions" \
  -d '{"model":"my-model","messages":[{"role":"user","content":"hello"}],"max_tokens":10}'
# Expected: HTTP 200
```

## Rollback

If the migration fails, restore the old tier-based configuration from backups:

```bash
# Restore old resources (if they existed)
kubectl apply -f migration-backup/tier-to-group-mapping.yaml 2>/dev/null
kubectl apply -f migration-backup/gateway-auth-policy.yaml 2>/dev/null
kubectl apply -f migration-backup/gateway-rate-limits.yaml 2>/dev/null

# Restore tier annotations
kubectl apply -f migration-backup/llm-models.yaml 2>/dev/null

# Delete MaaS CRs created during migration (use specific names, not --all,
# to avoid deleting unrelated resources if the cluster has other MaaS config)
kubectl delete maasmodelref my-model -n llm --ignore-not-found
kubectl delete maasauthpolicy my-model-premium-access -n models-as-a-service --ignore-not-found
kubectl delete maassubscription my-model-premium-subscription -n models-as-a-service --ignore-not-found
# Repeat for each resource created during Phase 4
```

Note that a full rollback requires reverting the operator to 3.3 as well, since the 3.4 maas-api no longer has the `/v1/tiers/lookup` endpoint.

After reverting the operator, restore the ModelsAsService CR if it was backed up:

```bash
kubectl apply -f migration-backup/modelsasservice.yaml 2>/dev/null \
  && echo "Restored ModelsAsService CR" \
  || echo "No ModelsAsService backup found"
```

## Troubleshooting

### Models return 401/403 after cleanup

The new `gateway-default-auth` denies access to models without a corresponding MaaSAuthPolicy. Verify:

```bash
kubectl get maasauthpolicy -n models-as-a-service
kubectl get authpolicy -n llm -l maas.opendatahub.io/model=<model-name>
```

### Models return 429 immediately

The new `gateway-default-deny` rate-limits to zero for models without a MaaSSubscription. Verify:

```bash
kubectl get maassubscription -n models-as-a-service
kubectl get tokenratelimitpolicy -n llm -l maas.opendatahub.io/model=<model-name>
```

### Duplicate gateway policies after upgrade

If both old and new gateway policies exist targeting the same gateway, Kuadrant behavior is undefined. The old policies are in `openshift-ingress`, the new ones in `redhat-ods-applications`. Delete the old policies (Phase 3).

```bash
kubectl get authpolicy -A
kubectl get tokenratelimitpolicy -A
# Delete old policies in openshift-ingress that target maas-default-gateway
```

### MaaSModelRef stuck in Pending

The model's LLMInferenceService may not have an HTTPRoute yet, or the referenced model does not exist:

```bash
kubectl get llminferenceservice <model-name> -n llm
kubectl get httproute -n llm
kubectl describe maasmodelref <model-name> -n llm
```

### maas-api pod not starting

Verify PostgreSQL secret exists:

```bash
kubectl get secret maas-db-config -n <maas-namespace>
```

If missing, create it before the Tenant reconciler can deploy maas-api:

```bash
kubectl create secret generic maas-db-config \
  -n <maas-namespace> \
  --from-literal=DB_CONNECTION_URL="postgresql://user:pass@host:5432/maasdb?sslmode=require"
```
