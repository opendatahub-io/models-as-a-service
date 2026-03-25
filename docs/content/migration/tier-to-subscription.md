# Migration Guide: Tier-Based to Subscription Model

This guide helps operators migrate from the legacy tier-based architecture (ConfigMap + gateway-auth-policy) to the new subscription-driven architecture (MaaSModelRef + MaaSAuthPolicy + MaaSSubscription).

## Overview

The MaaS platform has evolved from a tier-based system to a subscription model that provides:

- **Per-model access control and rate limits** (instead of gateway-level)
- **CRD-based configuration** (schema-validated, GitOps friendly)
- **Declarative management** via maas-controller
- **Separation of concerns** (auth vs. billing)

For architectural details, see [old-vs-new-flow.md](https://github.com/opendatahub-io/models-as-a-service/blob/main/maas-controller/docs/old-vs-new-flow.md).

## Prerequisites

Before starting the migration:

- **MaaS platform** with maas-controller installed
- **Cluster permissions**: namespace admin for `opendatahub`, `models-as-a-service`, and model namespaces
- **Current configuration backup**:
  - ConfigMap `tier-to-group-mapping`
  - Existing TokenRateLimitPolicy (gateway-level)
  - List of LLMInferenceServices and their tier annotations
  - Existing gateway-auth-policy (if present)

## Pre-Migration Checklist

- [ ] Document current tier definitions and group mappings
- [ ] Document current rate limits per tier
- [ ] List all deployed models and their tier annotations
- [ ] Identify which groups have access to which models
- [ ] Test migration procedure in non-production environment
- [ ] Plan maintenance window (optional, see zero-downtime approach below)
- [ ] Back up current configuration

```bash
# Backup script
mkdir -p migration-backup

# Backup tier-to-group-mapping ConfigMap if it exists
if kubectl get configmap tier-to-group-mapping -n maas-api >/dev/null 2>&1; then
  kubectl get configmap tier-to-group-mapping -n maas-api -o yaml > migration-backup/tier-to-group-mapping.yaml
  echo "Backed up tier-to-group-mapping"
else
  echo "No tier-to-group-mapping ConfigMap found (skipping backup)"
fi

# Only backup gateway-auth-policy if it exists
if kubectl get authpolicy gateway-auth-policy -n openshift-ingress >/dev/null 2>&1; then
  kubectl get authpolicy gateway-auth-policy -n openshift-ingress -o yaml > migration-backup/gateway-auth-policy.yaml
  echo "Backed up gateway-auth-policy"
else
  echo "No gateway-auth-policy found (skipping backup)"
fi

# Backup tokenratelimitpolicy resources if they exist
if kubectl get tokenratelimitpolicy -n openshift-ingress >/dev/null 2>&1; then
  kubectl get tokenratelimitpolicy -n openshift-ingress -o yaml > migration-backup/gateway-rate-limits.yaml
  echo "Backed up tokenratelimitpolicy resources"
else
  echo "No tokenratelimitpolicy resources found (skipping backup)"
fi

# Backup llminferenceservice resources if they exist
if kubectl get llminferenceservice -n llm >/dev/null 2>&1; then
  kubectl get llminferenceservice -n llm -o yaml > migration-backup/llm-models.yaml
  echo "Backed up llminferenceservice resources"
else
  echo "No llminferenceservice resources found (skipping backup)"
fi
```

## Migration Strategies

### Option A: Zero-Downtime (Recommended)

Run both old and new systems in parallel, validate the new system, then switch over.

**Advantages:**
- No service interruption
- Safe rollback if issues arise
- Time to validate new configuration

**Approach:**
1. Install maas-controller (creates gateway defaults)
2. Create new MaaS CRs alongside existing tier configuration
3. Validate new system works correctly
4. Remove old tier-based configuration

### Option B: Full Cutover (Requires Downtime)

Replace old system with new system in one maintenance window.

**Advantages:**
- Simpler process
- Faster migration

**Disadvantages:**
- Service downtime during migration
- Less time for validation

## Step-by-Step Migration (Zero-Downtime)

### Phase 1: Install maas-controller

If maas-controller is not already installed:

```bash
# Deploy maas-controller
kubectl apply -k deployment/base/maas-controller/default

# Verify controller is running
kubectl get pods -n opendatahub -l app=maas-controller

# Check controller logs
kubectl logs -n opendatahub -l app=maas-controller --tail=20

# Verify gateway default policies were created
kubectl get authpolicy gateway-default-auth -n openshift-ingress
kubectl get tokenratelimitpolicy gateway-default-deny -n openshift-ingress
```

**Important:** The maas-controller creates gateway-level default policies (`gateway-default-auth` and `gateway-default-deny`) that deny unconfigured models. These work alongside your existing tier-based policies during migration.

```bash
# Create subscription namespace if it doesn't exist
kubectl create namespace models-as-a-service
```

### Phase 2: Map Tiers to Subscriptions

For each tier in your ConfigMap, create equivalent MaaS CRs for each model.

#### Example: Migrating "premium" tier

**OLD tier configuration** (from tier-to-group-mapping.yaml):
```yaml
- name: premium
  description: Premium tier
  level: 10
  groups:
    - premium-users
    - premium-group
```

**OLD rate limit** (from gateway TokenRateLimitPolicy):
```yaml
spec:
  limits:
    premium-user-tokens:
      rates:
        - limit: 50000
          window: 1m
      when:
        - predicate: auth.identity.tier == "premium"
      counters:
        - expression: auth.identity.userid
```

**OLD model annotation** (on LLMInferenceService):
```yaml
metadata:
  annotations:
    alpha.maas.opendatahub.io/tiers: '["premium","enterprise"]'
```

#### NEW subscription configuration

For **each model** that premium tier can access, create:

**1. MaaSModelRef** (registers model with MaaS):
```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: my-model-name
  namespace: llm  # Must be in same namespace as the LLMInferenceService
spec:
  modelRef:
    kind: LLMInferenceService
    name: my-model-name
```

Apply it:
```bash
kubectl apply -f maasmodelref-my-model.yaml
```

**2. MaaSAuthPolicy** (access control - who can access):
```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: my-model-premium-access
  namespace: models-as-a-service
spec:
  modelRefs:
    - name: my-model-name
      namespace: llm
  subjects:
    groups:
      - name: premium-users
      - name: premium-group
    users: []
```

Apply it:
```bash
kubectl apply -f maasauthpolicy-my-model-premium.yaml
```

**3. MaaSSubscription** (rate limits - billing):
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
      - name: premium-group
    users: []
  modelRefs:
    - name: my-model-name
      namespace: llm
      tokenRateLimits:
        - limit: 50000  # From old TokenRateLimitPolicy
          window: 1m
```

Apply it:
```bash
kubectl apply -f maassubscription-my-model-premium.yaml
```

#### Verify controller generated policies

The maas-controller should automatically create Kuadrant policies:

```bash
# Check MaaSModelRef status
kubectl get maasmodelref my-model-name -n llm -o jsonpath='{.status.phase}'
# Expected: Ready

# Check generated AuthPolicy (one per model)
kubectl get authpolicy -n llm -l maas.opendatahub.io/model=my-model-name

# Check generated TokenRateLimitPolicy (one per model)
kubectl get tokenratelimitpolicy -n llm -l maas.opendatahub.io/model=my-model-name

# View full status
kubectl describe maasmodelref my-model-name -n llm
```

#### Automation Script

To simplify migration, use the provided script:

```bash
# Generate MaaS CRs from existing tier configuration
./scripts/migrate-tier-to-subscription.sh \
  --tier premium \
  --models my-model-1,my-model-2,my-model-3 \
  --groups premium-users \
  --rate-limit 50000 \
  --output migration-crs/

# Review generated CRs
ls migration-crs/

# Apply generated CRs
kubectl apply -f migration-crs/
```

> **Note:** Resources generated by the migration script are automatically labeled with:
> - `migration.maas.opendatahub.io/generated=true` - Identifies script-generated resources
> - `migration.maas.opendatahub.io/from-tier=<tier>` - Tracks which tier they came from
>
> You can use these labels to manage or rollback migration resources:
> ```bash
> # List all script-generated resources
> kubectl get maasmodelref -n llm -l migration.maas.opendatahub.io/generated=true
> kubectl get maasauthpolicy,maassubscription -n models-as-a-service -l migration.maas.opendatahub.io/generated=true
>
> # Delete resources from a specific tier migration
> kubectl delete maasmodelref -n llm -l migration.maas.opendatahub.io/from-tier=premium
> kubectl delete maasauthpolicy,maassubscription -n models-as-a-service -l migration.maas.opendatahub.io/from-tier=premium
> ```

See [Migration Script](#migration-automation-script) section below for details.

### Phase 3: Validate New Configuration

Test each migrated model to ensure the new subscription model works correctly:

```bash
# 1. Check all MaaS CRs are Ready
kubectl get maasmodelref -n llm
kubectl get maasauthpolicy -n models-as-a-service
kubectl get maassubscription -n models-as-a-service

# 2. Check generated Kuadrant policies
kubectl get authpolicy -n llm
kubectl get tokenratelimitpolicy -n llm

# 3. Test inference as a user in the premium group

# ⚠️ SECURITY WARNING: Token Handling
# The examples below store bearer tokens in shell variables, which can leak via:
# - Shell history files (~/.bash_history, ~/.zsh_history)
# - Process listings (ps, /proc)
# - Environment variable dumps
#
# For production or sensitive environments, use one of these safer alternatives:
#
# Option A: Secure token file with restricted permissions
#   mkdir -p ~/.kube/tokens
#   chmod 700 ~/.kube/tokens
#   oc whoami -t > ~/.kube/tokens/current
#   chmod 600 ~/.kube/tokens/current
#   # Use in curl: -H "Authorization: Bearer $(cat ~/.kube/tokens/current)"
#   # Clean up after use: rm -f ~/.kube/tokens/current
#
# Option B: Disable shell history for this session
#   set +o history  # Disable history (bash/zsh)
#   TOKEN=$(oc whoami -t)
#   # ... run commands ...
#   unset TOKEN     # Clear token from environment
#   set -o history  # Re-enable history
#
# For demonstration purposes, the examples use TOKEN variables.
# Always clear sensitive tokens after use with: unset TOKEN

oc login --username=premium-user  # Or use existing token

# Discover gateway host
HOST="maas.$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"

# Safer approach: Use token file with restricted permissions
mkdir -p ~/.kube/tokens && chmod 700 ~/.kube/tokens
oc whoami -t > ~/.kube/tokens/current && chmod 600 ~/.kube/tokens/current

# Test model access
curl -H "Authorization: Bearer $(cat ~/.kube/tokens/current)" \
  "https://${HOST}/llm/my-model-name/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{"model":"my-model-name","messages":[{"role":"user","content":"test"}],"max_tokens":10}'

# Expected: 200 OK with model response

# 4. Test rate limiting
for i in {1..60}; do
  curl -s -o /dev/null -w "%{http_code}\n" \
    -H "Authorization: Bearer $(cat ~/.kube/tokens/current)" \
    "https://${HOST}/llm/my-model-name/v1/chat/completions" \
    -H "Content-Type: application/json" \
    -d '{"model":"my-model-name","messages":[{"role":"user","content":"test"}],"max_tokens":10}'
done | sort | uniq -c
# Expected: Mix of 200 and 429 responses based on rate limit

# 5. Test unauthorized user (should get 403)
oc login --username=unauthorized-user
oc whoami -t > ~/.kube/tokens/current && chmod 600 ~/.kube/tokens/current

curl -v -H "Authorization: Bearer $(cat ~/.kube/tokens/current)" \
  "https://${HOST}/llm/my-model-name/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{"model":"my-model-name","messages":[{"role":"user","content":"test"}],"max_tokens":10}'
# Expected: 403 Forbidden

# Clean up token file after use
rm -f ~/.kube/tokens/current

# 6. Use validation script
./scripts/validate-deployment.sh
```

### Phase 4: Remove Old Configuration

Once new system is validated and working correctly:

#### 4.1 Remove tier annotations from models

```bash
# Remove tier annotations from all models
# Track failures to ensure all annotations are removed
failed_models=()

# Use process substitution to avoid subshell issue with pipe
while read model; do
  if kubectl annotate $model -n llm alpha.maas.opendatahub.io/tiers- --ignore-not-found; then
    echo "✓ Removed tier annotation from $model"
  else
    echo "✗ Failed to remove tier annotation from $model" >&2
    failed_models+=("$model")
  fi
done < <(kubectl get llminferenceservice -n llm -o name)

# Report any failures
if [ ${#failed_models[@]} -gt 0 ]; then
  echo ""
  echo "⚠️  WARNING: Failed to remove tier annotations from the following models:" >&2
  printf '  - %s\n' "${failed_models[@]}" >&2
  echo ""
  echo "Please manually remove annotations from these models:" >&2
  for model in "${failed_models[@]}"; do
    echo "  kubectl annotate $model -n llm alpha.maas.opendatahub.io/tiers-" >&2
  done
  exit 1
else
  echo ""
  echo "✓ Successfully removed tier annotations from all models"
fi
```

#### 4.2 Delete old gateway-auth-policy (if exists)

```bash
# Check if gateway-auth-policy exists
kubectl get authpolicy gateway-auth-policy -n openshift-ingress

# Delete it (gateway-default-auth replaces it)
kubectl delete authpolicy gateway-auth-policy -n openshift-ingress --ignore-not-found
```

#### 4.3 Update or remove gateway-level TokenRateLimitPolicy

The old TokenRateLimitPolicy has tier-based predicates that are no longer needed.

**Option A: Remove tier-based limits**
```bash
# Edit and remove tier-based limit rules
kubectl edit tokenratelimitpolicy <policy-name> -n openshift-ingress

# Remove sections like:
#   premium-user-tokens:
#     when:
#       - predicate: auth.identity.tier == "premium"
```

**Option B: Delete if fully replaced**
```bash
# If gateway-default-deny provides sufficient default, delete old policy
kubectl delete tokenratelimitpolicy <old-policy-name> -n openshift-ingress
```

**Note:** `gateway-default-deny` (created by maas-controller) provides default rate limiting (0 tokens for unconfigured models).

#### 4.4 Handle tier-to-group-mapping ConfigMap

**Option A: Keep ConfigMap** (if MaaS API uses it for other features)
```bash
# Keep ConfigMap but document that tiers are deprecated
kubectl annotate configmap tier-to-group-mapping -n maas-api \
  deprecated="true" \
  deprecated-reason="Migrated to subscription model" \
  --overwrite
```

**Option B: Delete ConfigMap** (if no longer needed)
```bash
# Verify MaaS API doesn't use /v1/tiers/lookup endpoint
# Check maas-api logs for tier lookup calls

# Delete ConfigMap
kubectl delete configmap tier-to-group-mapping -n maas-api
```

### Phase 5: ODH Model Controller Considerations

**Context:** If you have ODH Model Controller deployed (from `github.com/opendatahub-io/odh-model-controller`), it may manage AuthPolicies for LLMInferenceServices.

#### Check if ODH Model Controller is managing AuthPolicies

```bash
# Check for ODH Model Controller deployment
kubectl get deployment odh-model-controller -n opendatahub

# Check for ODH-managed AuthPolicies
kubectl get authpolicy -A -l app.kubernetes.io/managed-by=odh-model-controller
```

#### If ODH Model Controller manages AuthPolicies

**Scenario 1: ODH creates AuthPolicies, maas-controller also creates AuthPolicies**

- Potential conflict: Both controllers may try to manage policies
- **Resolution:** Use annotation to opt out ODH management for MaaS-managed models

```bash
# Opt out ODH management for specific AuthPolicy
kubectl annotate authpolicy <policy-name> -n <namespace> \
  opendatahub.io/managed=false
```

**Scenario 2: Coordinate with ODH team**

- Contact ODH team to understand AuthPolicy management strategy
- Determine if ODH Model Controller's AuthPolicy creation should be disabled for MaaS models
- Consider updating ODH Model Controller configuration

**Scenario 3: No ODH Model Controller or no AuthPolicy management**

- No action needed
- maas-controller is sole owner of AuthPolicies

#### Verify no conflicts

```bash
# Check for duplicate AuthPolicies targeting same HTTPRoute
kubectl get authpolicy -A -o json | \
  jq -r '.items[] | select(.spec.targetRef != null and .spec.targetRef.kind == "HTTPRoute") | "\(.metadata.namespace)/\(.metadata.name) -> \(.spec.targetRef.name // "<missing-target>")"' | \
  sort

# Look for multiple policies targeting the same HTTPRoute
# Expected: One AuthPolicy per HTTPRoute (created by maas-controller)
# If you see "<missing-target>", investigate that AuthPolicy for missing targetRef.name
```

## Migration Automation Script

A migration script is provided to automate CR generation from existing tier configuration.

### Usage

```bash
./scripts/migrate-tier-to-subscription.sh [OPTIONS]
```

### Options

| Flag | Description | Example |
|------|-------------|---------|
| `--tier <name>` | Tier name from ConfigMap | `--tier premium` |
| `--models <list>` | Comma-separated model names | `--models model1,model2` |
| `--groups <list>` | Comma-separated group names (auto-detected if omitted) | `--groups premium-users` |
| `--rate-limit <limit>` | Token rate limit | `--rate-limit 50000` |
| `--window <duration>` | Rate limit window (default: 1m) | `--window 1m` |
| `--output <dir>` | Output directory for CRs (default: migration-crs) | `--output migration-crs/` |
| `--subscription-ns <ns>` | Subscription namespace (default: models-as-a-service) | `--subscription-ns models-as-a-service` |
| `--model-ns <ns>` | Model namespace (default: llm) | `--model-ns llm` |
| `--maas-ns <ns>` | MaaS namespace (default: opendatahub) | `--maas-ns opendatahub` |
| `--dry-run` | Generate files without applying | `--dry-run` |
| `--apply` | Apply generated CRs to cluster | `--apply` |
| `--verbose` | Enable verbose logging | `--verbose` |
| `--help` | Show help message | `--help` |

### Examples

**Example 1: Generate CRs for premium tier**
```bash
./scripts/migrate-tier-to-subscription.sh \
  --tier premium \
  --models model-a,model-b,model-c \
  --groups premium-users \
  --rate-limit 50000 \
  --window 1m \
  --output migration-crs/premium/ \
  --dry-run
```

**Example 2: Generate and apply for all tiers**
```bash
# Free tier
./scripts/migrate-tier-to-subscription.sh \
  --tier free \
  --models simulator,qwen3 \
  --groups system:authenticated \
  --rate-limit 100 \
  --window 1m \
  --output migration-crs/free/ \
  --apply

# Premium tier
./scripts/migrate-tier-to-subscription.sh \
  --tier premium \
  --models simulator,qwen3,llama \
  --groups premium-users \
  --rate-limit 50000 \
  --window 1m \
  --output migration-crs/premium/ \
  --apply

# Enterprise tier
./scripts/migrate-tier-to-subscription.sh \
  --tier enterprise \
  --models simulator,qwen3,llama,gpt \
  --groups enterprise-users \
  --rate-limit 100000 \
  --window 1m \
  --output migration-crs/enterprise/ \
  --apply
```

**Example 3: Extract tier info from ConfigMap and generate CRs**
```bash
# Get tier configuration from ConfigMap
kubectl get configmap tier-to-group-mapping -n maas-api -o yaml

# Run script for each tier with extracted group and limit info
./scripts/migrate-tier-to-subscription.sh \
  --tier premium \
  --groups premium-users,premium-group \
  --models $(kubectl get llminferenceservice -n llm -o json | \
    jq -r '[.items[]
      | . as $item
      | try (
          .metadata.annotations["alpha.maas.opendatahub.io/tiers"] | fromjson
        ) catch (
          (env.DEBUG // "" | if . != "" then "WARN: malformed JSON in \($item.metadata.name)" | debug else empty end) | []
        )
      | if type == "array" and any(. == "premium") then $item.metadata.name else empty end
    ] | join(",")') \
  --rate-limit 50000 \
  --output migration-crs/premium/
```

## Conversion Worksheet

Use this table to plan your migration:

| Old Tier | Groups | Models | Rate Limit (tokens/min) | New MaaSAuthPolicy Name | New MaaSSubscription Name |
|----------|--------|--------|------------------------|------------------------|--------------------------|
| free | system:authenticated | simulator, qwen3 | 100 | free-models-access | free-models-subscription |
| premium | premium-users, premium-group | simulator, qwen3, llama | 50000 | premium-models-access | premium-models-subscription |
| enterprise | enterprise-users, admin-group | all models | 100000 | enterprise-models-access | enterprise-models-subscription |

### Worksheet Template

Download and fill out this worksheet before migration:

```yaml
# migration-plan.yaml
tiers:
  - name: free
    groups:
      - system:authenticated
    models:
      - simulator
      - qwen3
    rateLimit:
      limit: 100
      window: 1m

  - name: premium
    groups:
      - premium-users
      - premium-group
    models:
      - simulator
      - qwen3
      - llama
    rateLimit:
      limit: 50000
      window: 1m

  - name: enterprise
    groups:
      - enterprise-users
      - admin-group
    models:
      - simulator
      - qwen3
      - llama
      - gpt
    rateLimit:
      limit: 100000
      window: 1m
```

## Rollback Plan

If migration fails or issues arise:

### Immediate Rollback

```bash
# 1. List MaaS CRs created during migration (verify before deletion)
echo "=== MaaSModelRef resources ==="
kubectl get maasmodelref -n llm
echo "=== MaaSAuthPolicy resources ==="
kubectl get maasauthpolicy -n models-as-a-service
echo "=== MaaSSubscription resources ==="
kubectl get maassubscription -n models-as-a-service

# 2. Delete specific MaaS CRs created during migration
# Option A: Delete by resource name (if you know the specific names)
kubectl delete maasmodelref my-model-name -n llm
kubectl delete maasauthpolicy my-model-premium-access -n models-as-a-service
kubectl delete maassubscription my-model-premium-subscription -n models-as-a-service

# Option B: Delete all script-generated resources
kubectl delete maasmodelref -n llm -l migration.maas.opendatahub.io/generated=true
kubectl delete maasauthpolicy,maassubscription -n models-as-a-service -l migration.maas.opendatahub.io/generated=true

# Option C: Delete resources from a specific tier migration
kubectl delete maasmodelref -n llm -l migration.maas.opendatahub.io/from-tier=premium
kubectl delete maasauthpolicy,maassubscription -n models-as-a-service -l migration.maas.opendatahub.io/from-tier=premium

# 3. Re-apply old gateway-auth-policy (if it was backed up)
if [ -f migration-backup/gateway-auth-policy.yaml ]; then
  kubectl apply -f migration-backup/gateway-auth-policy.yaml
  echo "Restored gateway-auth-policy"
else
  echo "No gateway-auth-policy backup found (skipping restore)"
fi

# 4. Re-apply old TokenRateLimitPolicy (if backed up)
if [ -f migration-backup/gateway-rate-limits.yaml ]; then
  kubectl apply -f migration-backup/gateway-rate-limits.yaml
  echo "Restored gateway-rate-limits"
else
  echo "No gateway-rate-limits backup found (skipping restore)"
fi

# 5. Re-add tier annotations to models
kubectl annotate llminferenceservice my-model-name -n llm \
  alpha.maas.opendatahub.io/tiers='["premium","enterprise"]' \
  --overwrite

# 6. Re-apply tier-to-group-mapping ConfigMap (if backed up)
if [ -f migration-backup/tier-to-group-mapping.yaml ]; then
  kubectl apply -f migration-backup/tier-to-group-mapping.yaml
  echo "Restored tier-to-group-mapping"
else
  echo "No tier-to-group-mapping backup found (skipping restore)"
fi

# 7. Restart MaaS API to reload tier configuration
kubectl rollout restart deployment/maas-api -n opendatahub
```

### Rollback Validation

```bash
# Test tier-based system is working
# Using secure token file (see Phase 3 security warning for details)
mkdir -p ~/.kube/tokens && chmod 700 ~/.kube/tokens
oc whoami -t > ~/.kube/tokens/current && chmod 600 ~/.kube/tokens/current

HOST="maas.$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"

curl -H "Authorization: Bearer $(cat ~/.kube/tokens/current)" \
  "https://${HOST}/llm/my-model-name/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{"model":"my-model-name","messages":[{"role":"user","content":"test"}],"max_tokens":10}'

# Expected: 200 OK (tier-based system restored)

# Clean up token file
rm -f ~/.kube/tokens/current
```

### Partial Rollback

If only some models have issues, you can rollback specific models:

```bash
# Delete MaaS CRs for specific model only
kubectl delete maasmodelref my-model-name -n llm
kubectl delete maasauthpolicy my-model-premium-access -n models-as-a-service
kubectl delete maassubscription my-model-premium-subscription -n models-as-a-service

# Re-add tier annotation to that model
kubectl annotate llminferenceservice my-model-name -n llm \
  alpha.maas.opendatahub.io/tiers='["premium","enterprise"]' \
  --overwrite
```

## Troubleshooting

### Models return 401 Unauthorized

**Symptom:** Models return 401 after migration

**Possible Causes:**
- No MaaSAuthPolicy exists for the model
- User not authenticated
- gateway-default-auth denying request

**Resolution:**
```bash
# Check if MaaSAuthPolicy exists for the model
kubectl get maasauthpolicy -n models-as-a-service -o json | \
  jq -r '.items[] | select(.spec.modelRefs[]? | .name? == "my-model-name")'

# Check if AuthPolicy was generated
kubectl get authpolicy -n llm -l maas.opendatahub.io/model=my-model-name

# Check AuthPolicy status
kubectl describe authpolicy -n llm <policy-name>

# Verify user is authenticated
oc whoami
```

### Models return 403 Forbidden

**Symptom:** Models return 403 after migration

**Possible Causes:**
- User's groups not in MaaSAuthPolicy subjects
- AuthPolicy not enforced yet

**Resolution:**
```bash
# Check user's groups
oc whoami --show-groups

# Check MaaSAuthPolicy groups
kubectl get maasauthpolicy my-model-premium-access -n models-as-a-service -o yaml

# Verify groups match
kubectl get maasauthpolicy my-model-premium-access -n models-as-a-service -o jsonpath='{.spec.subjects.groups[*].name}'

# Check AuthPolicy enforcement
kubectl get authpolicy -n llm -o jsonpath='{.items[*].status.conditions[?(@.type=="Enforced")].status}'

# Check Authorino logs
kubectl logs -n openshift-ingress -l app.kubernetes.io/name=authorino --tail=50
```

### Models return 429 Too Many Requests

**Symptom:** Models immediately return 429 even on first request

**Possible Causes:**
- No MaaSSubscription exists for the model
- User's groups not in MaaSSubscription owner groups
- TokenRateLimitPolicy not configured correctly

**Resolution:**
```bash
# Check if MaaSSubscription exists for the model
kubectl get maassubscription -n models-as-a-service -o json | \
  jq -r '.items[] | select(.spec.modelRefs[]? | .name? == "my-model-name")'

# Check if TokenRateLimitPolicy was generated
kubectl get tokenratelimitpolicy -n llm -l maas.opendatahub.io/model=my-model-name

# Check TokenRateLimitPolicy status
kubectl describe tokenratelimitpolicy -n llm <policy-name>

# Verify user's groups match subscription owner groups
oc whoami --show-groups
kubectl get maassubscription my-model-premium-subscription -n models-as-a-service -o jsonpath='{.spec.owner.groups[*].name}'

# Check Limitador logs
kubectl logs -n kuadrant-system -l app.kubernetes.io/name=limitador --tail=50
```

### maas-controller not creating policies

**Symptom:** MaaSModelRef shows Ready but no AuthPolicy/TokenRateLimitPolicy created

**Possible Causes:**
- maas-controller not watching correct namespace
- Controller reconciliation failed
- HTTPRoute not found

**Resolution:**
```bash
# Check maas-controller logs
kubectl logs -n opendatahub -l app=maas-controller --tail=100

# Check MaaSModelRef status
kubectl get maasmodelref my-model-name -n llm -o yaml

# Verify HTTPRoute exists
kubectl get httproute -n llm my-model-name

# Check subscription namespace matches controller config
kubectl get deployment maas-controller -n opendatahub -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="MAAS_SUBSCRIPTION_NAMESPACE")].value}'

# Manually trigger reconciliation by updating MaaSAuthPolicy
kubectl annotate maasauthpolicy my-model-premium-access -n models-as-a-service \
  reconcile-trigger="$(date +%s)" --overwrite
```

### MaaSModelRef shows Pending or Failed

**Symptom:** MaaSModelRef status.phase is Pending or Failed

**Possible Causes:**
- LLMInferenceService not ready
- HTTPRoute not created by KServe yet
- Model namespace mismatch

**Resolution:**
```bash
# Check MaaSModelRef status
kubectl describe maasmodelref my-model-name -n llm

# Check LLMInferenceService status
kubectl get llminferenceservice my-model-name -n llm -o yaml

# Check if HTTPRoute exists
kubectl get httproute -n llm my-model-name

# Wait for KServe to create HTTPRoute
kubectl wait --for=condition=Ready llminferenceservice/my-model-name -n llm --timeout=5m

# Check maas-controller logs for errors
kubectl logs -n opendatahub -l app=maas-controller | grep my-model-name
```

### Duplicate AuthPolicies (ODH Model Controller conflict)

**Symptom:** Multiple AuthPolicies targeting the same HTTPRoute

**Possible Causes:**
- Both ODH Model Controller and maas-controller creating AuthPolicies
- Policy ownership conflict

**Resolution:**
```bash
# Check for multiple AuthPolicies on same route
kubectl get authpolicy -n llm -o json | \
  jq -r '.items[] | select(.spec.targetRef.name=="my-model-name") | .metadata.name'

# Check managed-by labels
kubectl get authpolicy -n llm -o json | \
  jq -r '.items[] | "\(.metadata.name): \(.metadata.labels."app.kubernetes.io/managed-by")"'

# Opt out ODH management
kubectl annotate authpolicy <odh-policy-name> -n llm \
  opendatahub.io/managed=false

# Or delete ODH-managed policy (maas-controller will recreate)
kubectl delete authpolicy <odh-policy-name> -n llm
```

### ConfigMap changes not reflected

**Symptom:** Updated tier-to-group-mapping not taking effect

**Note:** After migration, tier-to-group-mapping ConfigMap is no longer used by the subscription model.

**Resolution:**
- Update MaaSAuthPolicy and MaaSSubscription CRs instead of ConfigMap
- ConfigMap is only used if you haven't migrated yet

```bash
# Update MaaSAuthPolicy groups
kubectl edit maasauthpolicy my-model-premium-access -n models-as-a-service

# Update MaaSSubscription owner groups and limits
kubectl edit maassubscription my-model-premium-subscription -n models-as-a-service
```

## Frequently Asked Questions

### Do I need to use API keys with the new subscription model?

**No.** The new subscription model works with OpenShift tokens by default. The `gateway-default-auth` and per-route AuthPolicies use `kubernetesTokenReview` for authentication.

API key support is optional and requires additional MaaS API configuration. The migration guide assumes you continue using OpenShift token authentication.

### Can I have different rate limits for the same model?

**Yes.** Create multiple MaaSSubscriptions for the same model with different owner groups and token limits.

Example:
```yaml
# Basic tier: 100 tokens/min
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: my-model-basic-subscription
  namespace: models-as-a-service
spec:
  owner:
    groups:
      - name: basic-users
  modelRefs:
    - name: my-model
      namespace: llm
      tokenRateLimits:
        - limit: 100
          window: 1m

# Premium tier: 10000 tokens/min
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: my-model-premium-subscription
  namespace: models-as-a-service
spec:
  owner:
    groups:
      - name: premium-users
  modelRefs:
    - name: my-model
      namespace: llm
      tokenRateLimits:
        - limit: 10000
          window: 1m
```

When a user belongs to multiple owner groups, the controller selects the subscription with the **highest token rate limit**. In this example, users in both groups get the premium subscription with 10000 tokens/min (higher than the basic subscription's 100 tokens/min).

### What happens to users during migration?

**With zero-downtime approach:** Users experience no interruption. The old tier-based system remains active until you validate and switch to the new system.

**With full cutover:** Users may experience brief interruption during the maintenance window.

### Do I need to restart MaaS API?

**No.** MaaS API is unchanged. Only the tier lookup endpoint (`/v1/tiers/lookup`) becomes unused after migration.

If you delete the `tier-to-group-mapping` ConfigMap, MaaS API will no longer serve tier information, but this doesn't require a restart.

### Can I migrate one model at a time?

**Yes.** You can migrate models incrementally:

1. Create MaaS CRs for one model
2. Test and validate
3. Remove tier annotation from that model
4. Repeat for next model

This allows gradual migration with minimal risk.

### What if a user is in multiple groups with different subscriptions?

When a user belongs to multiple owner groups with different subscriptions for the same model, the controller selects the subscription with the **highest token rate limit** (the subscription with the highest `limit` value wins).

**Example:** A user in both `basic-users` and `premium-users` groups:
- If `basic-subscription` has 100 tokens/min and `premium-subscription` has 10000 tokens/min, the user gets the premium subscription with 10000 tokens/min (highest limit wins).
- If both subscriptions have the same token rate limit, the controller uses an implementation-defined tie-breaker (not guaranteed to be stable).

> **Note:** The `spec.priority` field exists in the MaaSSubscription CRD but is currently not used by the controller. Selection is based solely on token rate limit.

### Can I still use the tier-to-group-mapping ConfigMap?

**During migration:** Yes, both systems can coexist.

**After migration:** The ConfigMap is no longer used by the subscription model. You can:
- Delete it if not needed
- Keep it if MaaS API uses it for other features (check API documentation)
- Annotate it as deprecated

### How do I know which models a tier has access to?

In the old system, check the `alpha.maas.opendatahub.io/tiers` annotation on each LLMInferenceService:

```bash
kubectl get llminferenceservice -n llm -o json | \
  jq -r '.items[] | "\(.metadata.name): \(.metadata.annotations."alpha.maas.opendatahub.io/tiers")"'
```

In the new system, check MaaSAuthPolicy:

```bash
kubectl get maasauthpolicy -n models-as-a-service -o json | \
  jq -r '.items[] | "\(.metadata.name): \(.spec.modelRefs[])"'
```

### What happens if I don't create a MaaSSubscription for a model?

Users with access (via MaaSAuthPolicy) will get **429 Too Many Requests** immediately because:

1. The per-route AuthPolicy allows them (auth passes)
2. No per-route TokenRateLimitPolicy exists for them
3. gateway-default-deny kicks in with 0 token limit

This is the "dual-gate" model: both auth AND subscription must pass.

### Can I use the subscription model without MaaSAuthPolicy?

**No.** Without MaaSAuthPolicy, no per-route AuthPolicy is created, so `gateway-default-auth` denies all requests (401/403).

You must create both MaaSAuthPolicy (for access) and MaaSSubscription (for rate limits).

### How do I grant access to all authenticated users?

Use the `system:authenticated` group:

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: public-model-access
  namespace: models-as-a-service
spec:
  modelRefs:
    - name: public-model
      namespace: llm
  subjects:
    groups:
      - name: system:authenticated
    users: []
```

This is equivalent to the old tier system's `free` tier with `system:authenticated` group.

## Additional Resources

- [Old vs New Flow Documentation](https://github.com/opendatahub-io/models-as-a-service/blob/main/maas-controller/docs/old-vs-new-flow.md)
- [MaaS Controller README](https://github.com/opendatahub-io/models-as-a-service/blob/main/maas-controller/README.md)
- [Deployment Guide](../quickstart.md)

## Support

For issues or questions:
1. Check the troubleshooting section above
2. Review [MaaS Controller logs](#troubleshooting)
3. Consult the [old-vs-new-flow.md](https://github.com/opendatahub-io/models-as-a-service/blob/main/maas-controller/docs/old-vs-new-flow.md) for architectural details
4. Open an issue on GitHub with migration logs and error messages
