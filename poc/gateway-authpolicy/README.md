# PoC: Gateway-Level AuthPolicy for MaaS Inference

**Goal**: Validate that a single Gateway-scoped `AuthPolicy` can replace N per-model HTTPRoute-scoped policies, preserve model-level authorization, and add hostname-based tenant isolation for multi-gateway deployments.

**Links**: RHOAIENG-62570 · RHAISTRAT-1741 · RHAISTRAT-1540 (BBR dependency)

---

## What This PoC Tests

| Scenario | What it proves |
|---|---|
| Path-based route auth works | Gateway policy replaces per-model HTTPRoute policy end-to-end |
| Bad key → 401 | API key validation still enforced at Gateway scope |
| No subscription → 403 | Subscription check still model-aware via CEL path extraction |
| Hostname isolation (Coke/Pepsi) | `overrides` block enforces tenant boundary — per-route policies can't bypass |
| Body route with header stub | Gateway policy resolves model from `X-Gateway-Model-Name` (simulates Approach C) |

---

## Architecture

```
Request
  │
  ▼
Gateway (maas-default-gateway, openshift-ingress)
  │
  ├─► overrides.authorization.tenant-gateway-isolation   ← enforced on ALL routes
  │     Hostname → tenant validation (stub in Phase 1)
  │
  └─► defaults.authentication + metadata + authorization  ← applies unless per-route overrides
        API key validation     (apiKeyValidation)
        Subscription check     (subscription-info)     ← model from path or X-Gateway-Model-Name header
        OPA auth-valid         (auth-valid)
        OPA subscription-valid (subscription-valid)
        Response filters       (identity, X-MaaS-Subscription, strip Authorization)

  [Optional per-route policy — 02-per-model-group-policy.yaml]
        OPA require-group-membership   ← only needed if model has group/user restrictions
```

### Model identity resolution

| Route type | How model is identified | Status |
|---|---|---|
| Path-based (`/llm/granite-3b/v1/...`) | CEL: `path.split("/").filter(x, x!="")[0] + "/" + [1]` | ✅ Works today |
| Body-based (`/v1/chat/completions`) | `X-Gateway-Model-Name` header | ✅ Works as stub; needs Approach C (BBR WASM body-peek) for production |

### Policy chaining (Kuadrant defaults/overrides)

```
Gateway policy (defaults)     + per-route group policy (defaults)
      ↓                                   ↓
Full auth + subscription       Adds group membership check for this model only
(model-agnostic)               (tiny, model-specific OPA rule)
```

Kuadrant computes the "effective policy" by merging both. The Gateway `overrides` block (tenant check) wins over everything.

---

## Prerequisites

- OCP cluster with Kuadrant operator installed
- MaaS deployed with at least one `MaaSModelRef` and `LLMInferenceService`
- A valid API key (`sk-oai-...`) with an active subscription for the test model
- `kubectl` access to `openshift-ingress` namespace

---

## Setup

### 1. Fill in `overlay/params.env`

```bash
# Edit overlay/params.env with your cluster values:
app-namespace=maas-api                          # namespace where maas-api is deployed
gateway-namespace=openshift-ingress
gateway-name=maas-default-gateway
cluster-audience=https://kubernetes.default.svc # HyperShift/ROSA: use your OIDC URL
metadata-cache-ttl=300
authz-cache-ttl=300
```

Also update the TTL patch values in `overlay/kustomization.yaml` (the four `ttl:` lines)
to match `metadata-cache-ttl` and `authz-cache-ttl` in `params.env`.

### 1b. Preview the rendered output

```bash
kubectl kustomize overlay/
# Review the rendered AuthPolicy before applying
```

### 2. Pick one test model and suspend its existing per-model AuthPolicy

```bash
MODEL_NS=llm
MODEL_NAME=granite-3b

# Option A: delete the per-model policy (safest for a fresh PoC)
kubectl delete authpolicy -n ${MODEL_NS} maas-auth-${MODEL_NAME}

# Option B: opt the policy out of management (maas-controller skips opted-out policies)
kubectl annotate authpolicy -n ${MODEL_NS} maas-auth-${MODEL_NAME} \
  opendatahub.io/managed=false
```

### 3. Apply the Gateway policy

```bash
# Via kustomize (recommended — handles all substitutions)
kubectl apply -k overlay/

# Or raw (after manually filling placeholders in 01-gateway-authpolicy.yaml)
kubectl apply -f 01-gateway-authpolicy.yaml
```

### 4. Check policy status

```bash
kubectl get authpolicy -n openshift-ingress maas-gateway-auth-poc -o yaml
# Look for status.conditions: Accepted=True, Enforced=True
```

### 5. (Optional) Apply the per-model group policy

If the test model has group/user restrictions, apply the group policy too:

```bash
sed \
  -e "s/MODEL_NAMESPACE/${MODEL_NS}/g" \
  -e "s/MODEL_NAME/${MODEL_NAME}/g" \
  -e 's/ALLOWED_GROUPS/["your-group"]/g' \
  -e 's/ALLOWED_USERS/[]/g' \
  02-per-model-group-policy.yaml | kubectl apply -f -
```

---

## Run Tests

```bash
chmod +x test.sh

export GATEWAY_HOST=maas.apps.your-cluster.example.com
export MODEL_NS=llm
export MODEL_NAME=granite-3b
export VALID_KEY=sk-oai-xxxxxxxxxxxx
export INVALID_KEY=sk-oai-invalid

# Optional extras
export NO_SUB_KEY=sk-oai-key-with-no-subscription
export PEPSI_HOST=pepsi.apps.your-cluster.example.com  # second gateway hostname
export COKE_KEY=sk-oai-coke-tenant-key

./test.sh
```

---

## Cleanup / Revert

```bash
# Remove Gateway policy
kubectl delete -k overlay/
# or: kubectl delete -f 01-gateway-authpolicy.yaml

# Restore per-model policy
kubectl apply -f <original maas-auth-MODEL_NAME policy>
# OR remove the opt-out annotation:
kubectl annotate authpolicy -n ${MODEL_NS} maas-auth-${MODEL_NAME} \
  opendatahub.io/managed-  # removes the annotation, controller resumes management
```

---

## Known Limitations (by design for PoC)

| Limitation | Impact | Required fix |
|---|---|---|
| Tenant hostname check is a stub (`allow { true }`) | Scenario 4 won't enforce isolation yet | Wire `/internal/v1/tenants/validate` into `overrides` block |
| Body routes (`/v1/chat/completions`) fall back to path extraction if `X-Gateway-Model-Name` absent | Model identity wrong for real body requests | Requires Approach C (Alex Snaps WASM body-peek, RHAISTRAT-1540) |
| `require-group-membership` is a separate per-route policy | Still N objects if all models have group restrictions | Acceptable for PoC; production could externalize to maas-api endpoint |
| TRLP not in scope | Rate limiting still uses per-model TRLPs unchanged | TRLP migration is a separate, harder story (per-subscription limits) |

---

## LOE Assessment (to be filled after PoC runs)

| Approach | Optimistic | Realistic | Risk |
|---|---|---|---|
| Gateway AuthPolicy (path routes) | | | |
| + Hostname tenant isolation | | | |
| + BBR body-route support (Approach C) | | | |
| Full productization (controller refactor) | | | |

**Recommendation**: [ ] Proceed / [ ] Pivot / [ ] Stop

**Rationale**: _fill after PoC_
