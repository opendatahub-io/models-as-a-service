# Upgrade Guide: RHOAI 3.2 to 3.4 (MaaS Migration)

This guide documents the full manual upgrade procedure from RHOAI 3.2 (tier-based architecture) to RHOAI 3.4 (subscription-driven architecture). It covers the initial 3.2 setup, upgrade steps, manual cleanup of old resources, creation of new subscription CRs, and validation.

## Background

RHOAI 3.4 replaces the tier-based access system with a CRD-driven subscription model. The old tier system used:

- A `tier-to-group-mapping` ConfigMap to define tiers and group membership
- Gateway-level AuthPolicy and TokenRateLimitPolicy with tier-based predicates
- Tier annotations on LLMInferenceService resources

The new system uses:

- **MaaSModelRef** to register models with the MaaS platform
- **MaaSAuthPolicy** to define per-model access control
- **MaaSSubscription** to define per-model rate limits and billing
- **Tenant** for platform-wide configuration (auto-created by maas-controller)

The operator upgrade installs the new CRDs and deploys maas-controller, but **does not clean up old tier resources**. Old Kuadrant policies will coexist with the new gateway defaults and must be removed manually.

## Prerequisites

- Cluster admin access
- RHOAI 3.2 deployed with tier-based MaaS configuration
- Kuadrant/RHCL compatible with 3.4 (Kuadrant v1.4.2+ for ODH, RHCL v1.3+ for RHOAI)
- PostgreSQL instance available for maas-api (API key storage)

## Phase 1: RHOAI 3.2 Setup (Tier-Based System)

This phase documents the resources that exist in a typical 3.2 deployment. If you are setting up a fresh 3.2 environment for testing, create these resources.

### 1.1 Tier-to-Group Mapping ConfigMap

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: tier-to-group-mapping
  namespace: maas-api
  labels:
    app: maas-api
    component: tier-mapping
data:
  tiers: |
    - name: free
      displayName: Free Tier
      level: 0
      groups:
        - system:authenticated
    - name: premium
      displayName: Premium Tier
      level: 1
      groups:
        - premium-users
```

```bash
kubectl apply -f tier-to-group-mapping.yaml
```

### 1.2 Gateway-Level AuthPolicy

```yaml
apiVersion: kuadrant.io/v1
kind: AuthPolicy
metadata:
  name: gateway-auth-policy
  namespace: openshift-ingress
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: Gateway
    name: maas-default-gateway
  rules:
    authentication:
      oidc-token:
        jwt:
          issuerUrl: https://kubernetes.default.svc
    authorization:
      tier-check:
        patternMatching:
          patterns:
            - predicate: auth.identity.tier != ""
```

```bash
kubectl apply -f gateway-auth-policy.yaml
```

### 1.3 Gateway-Level TokenRateLimitPolicy (Tier-Based)

```yaml
apiVersion: kuadrant.io/v1alpha1
kind: TokenRateLimitPolicy
metadata:
  name: gateway-tier-rate-limits
  namespace: openshift-ingress
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: Gateway
    name: maas-default-gateway
  limits:
    free-user-tokens:
      rates:
        - limit: 100
          window: 1m
      when:
        - predicate: auth.identity.tier == "free"
      counters:
        - expression: auth.identity.userid
    premium-user-tokens:
      rates:
        - limit: 50000
          window: 1m
      when:
        - predicate: auth.identity.tier == "premium"
      counters:
        - expression: auth.identity.userid
```

```bash
kubectl apply -f gateway-tier-rate-limits.yaml
```

### 1.4 Model Tier Annotations

```bash
kubectl annotate llminferenceservice my-model -n llm \
  alpha.maas.opendatahub.io/tiers='["free","premium"]' --overwrite
```

### 1.5 Verify 3.2 Setup

```bash
# Verify ConfigMap
kubectl get configmap tier-to-group-mapping -n maas-api

# Verify gateway policies
kubectl get authpolicy gateway-auth-policy -n openshift-ingress
kubectl get tokenratelimitpolicy gateway-tier-rate-limits -n openshift-ingress

# Verify tier annotations on models
kubectl get llminferenceservice -n llm -o jsonpath='{range .items[*]}{.metadata.name}: {.metadata.annotations.alpha\.maas\.opendatahub\.io/tiers}{"\n"}{end}'

# Test tier lookup endpoint (3.2 only)
HOST="maas.$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"
curl -s -X POST "https://${HOST}/v1/tiers/lookup" \
  -H "Content-Type: application/json" \
  -d '{"groups":["premium-users"]}' | jq .
# Expected: {"tier":"premium","displayName":"Premium Tier"}
```

## Phase 2: Pre-Upgrade Backup

Back up all tier-based resources before upgrading.

```bash
mkdir -p migration-backup

# Backup tier-to-group-mapping ConfigMap
kubectl get configmap tier-to-group-mapping -n maas-api -o yaml \
  > migration-backup/tier-to-group-mapping.yaml 2>/dev/null \
  && echo "Backed up tier-to-group-mapping" \
  || echo "No tier-to-group-mapping found"

# Backup gateway-auth-policy
kubectl get authpolicy gateway-auth-policy -n openshift-ingress -o yaml \
  > migration-backup/gateway-auth-policy.yaml 2>/dev/null \
  && echo "Backed up gateway-auth-policy" \
  || echo "No gateway-auth-policy found"

# Backup gateway TokenRateLimitPolicy
kubectl get tokenratelimitpolicy -n openshift-ingress -o yaml \
  > migration-backup/gateway-rate-limits.yaml 2>/dev/null \
  && echo "Backed up TokenRateLimitPolicy resources" \
  || echo "No TokenRateLimitPolicy found"

# Backup LLMInferenceService resources (with tier annotations)
kubectl get llminferenceservice -n llm -o yaml \
  > migration-backup/llm-models.yaml 2>/dev/null \
  && echo "Backed up LLMInferenceService resources" \
  || echo "No LLMInferenceService found"

# Record current state
echo "=== Pre-upgrade snapshot ===" > migration-backup/pre-upgrade-state.txt
echo "Date: $(date -u)" >> migration-backup/pre-upgrade-state.txt
kubectl get authpolicy -A >> migration-backup/pre-upgrade-state.txt 2>/dev/null
kubectl get tokenratelimitpolicy -A >> migration-backup/pre-upgrade-state.txt 2>/dev/null
kubectl get configmap -n maas-api >> migration-backup/pre-upgrade-state.txt 2>/dev/null
```

## Phase 3: Upgrade RHOAI Operator to 3.4

### 3.1 Upgrade the Operator

Follow the standard RHOAI operator upgrade procedure. The operator upgrade will:

- Install MaaS CRDs (`maas.opendatahub.io/v1alpha1`): MaaSModelRef, MaaSAuthPolicy, MaaSSubscription, ExternalModel
- Deploy maas-controller when `modelsAsService: Managed` is set in the DSC
- Create gateway-level default policies: `gateway-default-auth` and `gateway-default-deny`

**Important:** The `modelsAsService` field defaults to `Removed` if not specified in the DSC. The operator will not deploy maas-controller until you explicitly set `modelsAsService: Managed`. This means the upgrade itself is safe -- MaaS is opt-in.

### 3.2 Enable MaaS

After the operator upgrade completes and the DSC is Ready, enable MaaS:

```bash
kubectl patch datasciencecluster default-dsc --type merge \
  -p '{"spec":{"components":{"kserve":{"modelsAsService":{"managementState":"Managed"}}}}}'

# Wait for MaaS to become ready
kubectl wait datasciencecluster default-dsc \
  --for=jsonpath='{.status.conditions[?(@.type=="Ready")].status}'=True --timeout=300s
```

### 3.3 Verify Upgrade

```bash
# Verify MaaS CRDs are installed
kubectl get crd | grep maas.opendatahub.io

# Verify maas-controller is running
kubectl get pods -l control-plane=maas-controller -A

# Verify new gateway default policies were created
kubectl get authpolicy gateway-default-auth -n redhat-ods-applications
kubectl get tokenratelimitpolicy gateway-default-deny -n redhat-ods-applications
```

### 3.4 Identify Policy Conflicts

After enabling MaaS, both old and new gateway-level policies target the same gateway from different namespaces. Audit the state:

```bash
echo "=== AuthPolicies targeting maas-default-gateway ==="
kubectl get authpolicy -A
# You will see BOTH:
#   openshift-ingress         gateway-auth-policy    (OLD - tier-based)
#   redhat-ods-applications   gateway-default-auth   (NEW - maas-controller managed)
#   redhat-ods-applications   maas-api-auth-policy   (NEW - targets maas-api HTTPRoute)

echo ""
echo "=== TokenRateLimitPolicies targeting maas-default-gateway ==="
kubectl get tokenratelimitpolicy -A
# You will see BOTH:
#   openshift-ingress         gateway-tier-rate-limits   (OLD - tier predicates)
#   redhat-ods-applications   gateway-default-deny       (NEW - maas-controller managed)
```

Both old and new policies target the same `maas-default-gateway` from different namespaces. This creates conflicting policy behavior in Kuadrant and must be resolved by removing the old policies.

## Phase 4: Manual Cleanup of Old Tier Resources

This is the critical phase. The operator does not clean up old resources automatically. Each resource must be removed manually.

### 4.1 Delete Old Gateway AuthPolicy

```bash
kubectl delete authpolicy gateway-auth-policy -n openshift-ingress
# Verify only the new policy remains
kubectl get authpolicy -A
# Expected: gateway-default-auth in redhat-ods-applications (managed by maas-controller)
```

### 4.2 Delete Old Gateway TokenRateLimitPolicy

```bash
kubectl delete tokenratelimitpolicy gateway-tier-rate-limits -n openshift-ingress
# Verify only the new policy remains
kubectl get tokenratelimitpolicy -A
# Expected: gateway-default-deny in redhat-ods-applications (managed by maas-controller)
```

### 4.3 Delete Tier-to-Group Mapping ConfigMap

```bash
kubectl delete configmap tier-to-group-mapping -n maas-api
```

### 4.4 Remove Tier Annotations from Models

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

### 4.5 Verify Cleanup Complete

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

# Tier lookup endpoint should return 404
HOST="maas.$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"
echo ""
echo "Tier lookup endpoint (should 404):"
curl -s -o /dev/null -w "%{http_code}" -X POST "https://${HOST}/v1/tiers/lookup" \
  -H "Content-Type: application/json" \
  -d '{"groups":["premium-users"]}'
echo ""
```

### Complete Cleanup Inventory

| # | Resource | Kind | Namespace | Action | Conflict |
|---|----------|------|-----------|--------|----------|
| 1 | `gateway-auth-policy` | AuthPolicy | `openshift-ingress` | Delete | Conflicts with `gateway-default-auth` in `redhat-ods-applications` -- both target `maas-default-gateway` |
| 2 | `gateway-tier-rate-limits` | TokenRateLimitPolicy | `openshift-ingress` | Delete | Conflicts with `gateway-default-deny` in `redhat-ods-applications` -- both target `maas-default-gateway` |
| 3 | `tier-to-group-mapping` | ConfigMap | `maas-api` | Delete | Orphaned -- no code reads this in 3.4 |
| 4 | `alpha.maas.opendatahub.io/tiers` | Annotation | `llm` (on each model) | Remove | Orphaned -- annotation is ignored in 3.4 |
| 5 | `/v1/tiers/lookup` | API Endpoint | N/A | Gone in 3.4 (no action) | Clients calling this will get 404 |

## Phase 5: Create New Subscription Resources

With old resources cleaned up, create the new CRD-based configuration. For each model and each access tier, create three resources.

### 5.1 Register Models with MaaS (MaaSModelRef)

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

### 5.2 Create Access Policies (MaaSAuthPolicy)

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

### 5.3 Create Subscriptions with Rate Limits (MaaSSubscription)

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

### 5.4 Verify New Configuration

```bash
# All MaaS CRs should be Active/Ready
kubectl get maasmodelref -n llm
kubectl get maasauthpolicy -n models-as-a-service
kubectl get maassubscription -n models-as-a-service

# Per-model Kuadrant policies should exist
kubectl get authpolicy -n llm
kubectl get tokenratelimitpolicy -n llm
```

## Phase 6: Validation

### 6.1 Test Authorized Access

```bash
HOST="maas.$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"

# Log in as a user in the premium-users group
oc login --username=premium-user

TOKEN=$(oc whoami -t)

# Test model access
curl -s -w "\nHTTP %{http_code}\n" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  "https://${HOST}/llm/my-model/v1/chat/completions" \
  -d '{"model":"my-model","messages":[{"role":"user","content":"hello"}],"max_tokens":10}'
# Expected: HTTP 200
```

### 6.2 Test Unauthorized Access

```bash
# Log in as a user NOT in any authorized group
oc login --username=unauthorized-user

TOKEN=$(oc whoami -t)

curl -s -w "\nHTTP %{http_code}\n" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  "https://${HOST}/llm/my-model/v1/chat/completions" \
  -d '{"model":"my-model","messages":[{"role":"user","content":"hello"}],"max_tokens":10}'
# Expected: HTTP 401 or 403
```

### 6.3 Test Rate Limiting

```bash
# Send rapid requests to trigger rate limits
for i in $(seq 1 100); do
  code=$(curl -s -o /dev/null -w "%{http_code}" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "Content-Type: application/json" \
    "https://${HOST}/llm/my-model/v1/chat/completions" \
    -d '{"model":"my-model","messages":[{"role":"user","content":"hello"}],"max_tokens":10}')
  echo "Request $i: HTTP $code"
done
# Expected: mix of 200 and 429 after exceeding rate limit
```

### 6.4 Test API Key Flow (New in 3.4)

```bash
# Create an API key via maas-api
TOKEN=$(oc whoami -t)

API_KEY=$(curl -s -X POST \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  "https://${HOST}/maas-api/v1/api-keys" \
  -d '{"name":"test-key"}' | jq -r '.key')

echo "API Key: ${API_KEY}"

# Use API key for model access
curl -s -w "\nHTTP %{http_code}\n" \
  -H "Authorization: Bearer ${API_KEY}" \
  -H "Content-Type: application/json" \
  "https://${HOST}/llm/my-model/v1/chat/completions" \
  -d '{"model":"my-model","messages":[{"role":"user","content":"hello"}],"max_tokens":10}'
# Expected: HTTP 200
```

## Rollback

If the migration fails, restore the old tier-based configuration from backups:

```bash
# Restore old resources
kubectl apply -f migration-backup/tier-to-group-mapping.yaml
kubectl apply -f migration-backup/gateway-auth-policy.yaml
kubectl apply -f migration-backup/gateway-rate-limits.yaml

# Restore tier annotations
kubectl apply -f migration-backup/llm-models.yaml

# Delete new MaaS CRs (controller will clean up generated policies)
kubectl delete maasmodelref --all -n llm
kubectl delete maasauthpolicy --all -n models-as-a-service
kubectl delete maassubscription --all -n models-as-a-service
```

Note that rollback requires reverting the operator to 3.2 as well, since the 3.4 maas-api no longer has the `/v1/tiers/lookup` endpoint.

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

If both old and new gateway policies exist targeting the same gateway, Kuadrant behavior is undefined. The old policies are in `openshift-ingress`, the new ones in `redhat-ods-applications`. Delete the old policies (Phase 4).

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
