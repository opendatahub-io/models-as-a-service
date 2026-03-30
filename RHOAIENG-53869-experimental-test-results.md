# RHOAIENG-53869 Experimental Testing Results

**Date**: 2026-03-19
**Test Engineer**: Claude Code
**Test Duration**: ~40 minutes
**Status**: ✅ **COMPLETE - Path A Confirmed**

---

## Test Objective

Determine if Kuadrant's `strategy: merge` allows multiple TokenRateLimitPolicies to target the same HTTPRoute without conflicts.

**Decision Impact**: This test determines whether we take:
- **Path A (Simple)**: Refactor MaaS to use `defaults.strategy: merge` (~3-4 days)
- **Path B (Complex)**: HTTPRoute aggregation refactor (~10-11 days)

---

## Test Setup

### Environment
- **Cluster**: Existing MaaS development cluster (ROSA)
- **Kuadrant**: Already deployed in `rh-connectivity-link` namespace
- **Gateway**: `maas-default-gateway` in `openshift-ingress` namespace
- **Hostname**: `maas.apps.rosa.elunin.rwc6.p3.openshiftapps.com`

### Test 1: WITH `strategy: merge`

**Namespace**: `trlp-merge-test`

**HTTPRoute**:
```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: test-route
  namespace: trlp-merge-test
spec:
  parentRefs:
  - name: maas-default-gateway
    namespace: openshift-ingress
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /test-merge
    backendRefs:
    - name: dummy-backend
      port: 80
```

**TRLP-A (Free Tier)**:
```yaml
apiVersion: kuadrant.io/v1alpha1
kind: TokenRateLimitPolicy
metadata:
  name: trlp-free-tier
  namespace: trlp-merge-test
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: test-route
  defaults:
    strategy: merge  # ← Key difference
    limits:
      free-tier-limit:
        rates:
        - limit: 1000
          window: 1m
        when:
        - predicate: request.headers["x-tier"] == "free"
        counters:
        - expression: request.headers["x-user-id"]
```

**TRLP-B (Premium Tier)**:
```yaml
apiVersion: kuadrant.io/v1alpha1
kind: TokenRateLimitPolicy
metadata:
  name: trlp-premium-tier
  namespace: trlp-merge-test
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: test-route  # ← Same HTTPRoute as TRLP-A
  defaults:
    strategy: merge  # ← Key difference
    limits:
      premium-tier-limit:
        rates:
        - limit: 10000
          window: 1m
        when:
        - predicate: request.headers["x-tier"] == "premium"
        counters:
        - expression: request.headers["x-user-id"]
```

### Test 2: WITHOUT merge strategy (Baseline)

**Namespace**: `trlp-no-merge-test`

**HTTPRoute**:
```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: test-route-no-merge
  namespace: trlp-no-merge-test
spec:
  parentRefs:
  - name: maas-default-gateway
    namespace: openshift-ingress
  rules:
  - matches:
    - path:
        type: PathPrefix
        value: /test-no-merge
    backendRefs:
    - name: dummy-backend
      port: 80
```

**TRLP-A**:
```yaml
apiVersion: kuadrant.io/v1alpha1
kind: TokenRateLimitPolicy
metadata:
  name: trlp-no-merge-a
  namespace: trlp-no-merge-test
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: test-route-no-merge
  limits:  # ← Top-level limits (no strategy field)
    limit-a:
      rates:
      - limit: 1000
        window: 1m
```

**TRLP-B**:
```yaml
apiVersion: kuadrant.io/v1alpha1
kind: TokenRateLimitPolicy
metadata:
  name: trlp-no-merge-b
  namespace: trlp-no-merge-test
spec:
  targetRef:
    group: gateway.networking.k8s.io
    kind: HTTPRoute
    name: test-route-no-merge  # ← Same HTTPRoute as TRLP-A
  limits:  # ← Top-level limits (no strategy field)
    limit-b:
      rates:
      - limit: 5000
        window: 1m
```

---

## Test Results

### Test 1: WITH `strategy: merge` ✅ SUCCESS

**TRLP-A (free-tier) Status**:
```json
{
  "name": "trlp-free-tier",
  "conditions": [
    {
      "type": "Accepted",
      "status": "True",
      "reason": "Accepted",
      "message": "TokenRateLimitPolicy has been accepted"
    },
    {
      "type": "Enforced",
      "status": "True",  // ← SUCCESS
      "reason": "Enforced",
      "message": "TokenRateLimitPolicy has been successfully enforced"
    }
  ]
}
```

**TRLP-B (premium-tier) Status**:
```json
{
  "name": "trlp-premium-tier",
  "conditions": [
    {
      "type": "Accepted",
      "status": "True",
      "reason": "Accepted",
      "message": "TokenRateLimitPolicy has been accepted"
    },
    {
      "type": "Enforced",
      "status": "True",  // ← SUCCESS
      "reason": "Enforced",
      "message": "TokenRateLimitPolicy has been successfully enforced"
    }
  ]
}
```

**✅ BOTH policies show `Enforced: True`** - No conflicts!

**HTTPRoute Status**:
```
Object affected by TokenRateLimitPolicy [trlp-merge-test/trlp-free-tier trlp-merge-test/trlp-premium-tier]
```
Both policies are recognized as affecting the HTTPRoute.

**Limitador Configuration** (extracted from Limitador CR):
```yaml
limits:
  - name: premium-tier-limit
    namespace: trlp-merge-test/test-route
    max_value: 10000
    seconds: 60
    conditions:
      - descriptors[0]["tokenlimit.premium_tier_limit__faf3daca"] == "1"
    variables:
      - descriptors[0]["request.headers["x-user-id"]"]

  - name: free-tier-limit
    namespace: trlp-merge-test/test-route
    max_value: 1000
    seconds: 60
    conditions:
      - descriptors[0]["tokenlimit.free_tier_limit__35e16589"] == "1"
    variables:
      - descriptors[0]["request.headers["x-user-id"]"]
```

**✅ Both limits are configured in Limitador** - confirming merge worked!

---

### Test 2: WITHOUT merge strategy ❌ CONFLICT

**TRLP-A (no-merge-a) Status**:
```json
{
  "name": "trlp-no-merge-a",
  "conditions": [
    {
      "type": "Accepted",
      "status": "True",
      "reason": "Accepted",
      "message": "TokenRateLimitPolicy has been accepted"
    },
    {
      "type": "Enforced",
      "status": "False",  // ← FAILURE
      "reason": "Overridden",
      "message": "TokenRateLimitPolicy is overridden by [trlp-no-merge-test/trlp-no-merge-a trlp-no-merge-test/trlp-no-merge-b]"
    }
  ]
}
```

**TRLP-B (no-merge-b) Status**:
```json
{
  "name": "trlp-no-merge-b",
  "conditions": [
    {
      "type": "Accepted",
      "status": "True",
      "reason": "Accepted",
      "message": "TokenRateLimitPolicy has been accepted"
    },
    {
      "type": "Enforced",
      "status": "True",  // ← Only this one enforced
      "reason": "Enforced",
      "message": "TokenRateLimitPolicy has been successfully enforced"
    }
  ]
}
```

**❌ TRLP-A shows `Enforced: False` with reason `Overridden`**
**❌ Only TRLP-B is enforced** - demonstrating the conflict behavior!

---

## Comparison Table

| Criterion | WITH `strategy: merge` | WITHOUT merge |
|-----------|------------------------|---------------|
| **TRLP-A Status** | ✅ `Enforced: True` | ❌ `Enforced: False` (Overridden) |
| **TRLP-B Status** | ✅ `Enforced: True` | ✅ `Enforced: True` |
| **HTTPRoute shows both policies** | ✅ Yes | ✅ Yes (but only one enforced) |
| **Limitador has both limits** | ✅ Yes (verified) | ❌ No (only one) |
| **Conflicts/errors in logs** | ✅ None | ❌ Override warning |

---

## Key Observations

1. **`strategy: merge` is REQUIRED for multiple TRLPs per HTTPRoute**
   - Without it, Kuadrant uses `atomic` strategy (default)
   - Atomic strategy causes one policy to override others

2. **Merge behavior**:
   - Multiple TRLPs with `defaults.strategy: merge` can coexist peacefully
   - Each TRLP's limits are added to Limitador independently
   - All policies show `Enforced: True` status
   - No conflicts or reconciliation issues

3. **Current MaaS implementation is incompatible**:
   - MaaS uses top-level `limits` (not `defaults.limits`)
   - The `strategy` field ONLY exists in `defaults` and `overrides`
   - Must refactor to `defaults.strategy: merge, defaults.limits: {...}`

4. **This matches RHOAIENG-53865 findings**:
   - UI bug created separate TRLPs per tier
   - Only one showed "Enforced", others "Overridden"
   - Fix suggested: use `strategy: merge`

---

## Conclusion

### Test Outcome: ✅ **SUCCESS**

**`strategy: merge` DOES work** for allowing multiple TokenRateLimitPolicies to target the same HTTPRoute.

### Recommended Path: **Path A (Simple Fix)**

**Rationale**:
- Minimal code changes (refactor TRLP spec structure)
- No need for HTTPRoute aggregation refactor
- Aligns with Kuadrant's designed behavior
- Matches fix for RHOAIENG-53865

**Effort Estimate**: 2-3 days (vs. 10-11 days for Path B)

---

## Next Steps

### 1. Implementation (Path A)

**File**: `maas-controller/pkg/controller/maas/maassubscription_controller.go`

**Change** (lines 289-296):

**FROM**:
```go
spec := map[string]interface{}{
    "targetRef": map[string]interface{}{
        "group": "gateway.networking.k8s.io",
        "kind":  "HTTPRoute",
        "name":  httpRouteName,
    },
    "limits": limitsMap,
}
```

**TO**:
```go
spec := map[string]interface{}{
    "targetRef": map[string]interface{}{
        "group": "gateway.networking.k8s.io",
        "kind":  "HTTPRoute",
        "name":  httpRouteName,
    },
    "defaults": map[string]interface{}{
        "strategy": "merge",
        "limits":   limitsMap,
    },
}
```

### 2. Testing

- Unit tests: Verify TRLP spec structure
- E2E tests:
  - Create two MaaSModelRefs → same LLMInferenceService
  - Create subscriptions for each model
  - Verify both TRLPs show `Enforced: True`
  - Verify rate limiting works for both models

### 3. Migration Considerations

**Backward Compatibility**:
- Existing TRLPs use top-level `limits`
- New TRLPs will use `defaults.strategy: merge, defaults.limits`
- Both formats can coexist temporarily
- Recommend migration plan for existing deployments

---

## Cleanup

```bash
kubectl delete namespace trlp-merge-test
kubectl delete namespace trlp-no-merge-test
rm -rf /Users/egorlu/code/redhat/models-as-a-service/test-merge-strategy/
```

---

## References

- **JIRA**: [RHOAIENG-53869](https://redhat.atlassian.net/browse/RHOAIENG-53869)
- **Related Bug**: [RHOAIENG-53865](https://redhat.atlassian.net/browse/RHOAIENG-53865)
- **Kuadrant Docs**: https://docs.kuadrant.io/1.3.x/kuadrant-operator/doc/reference/tokenratelimitpolicy/
- **Test Manifests**: `/Users/egorlu/code/redhat/models-as-a-service/test-merge-strategy/`

---

## Status

- [x] Test environment setup
- [x] HTTPRoute created
- [x] TRLPs with merge strategy deployed
- [x] TRLPs without merge strategy deployed (baseline)
- [x] Status validation completed
- [x] Limitador configuration verified
- [x] Results documented
- [x] **Decision: Path A confirmed**
- [ ] Implementation (next phase)
