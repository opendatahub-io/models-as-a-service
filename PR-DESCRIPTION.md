# feat: enable TRLP merge strategy for shared routes

Fixes [RHOAIENG-53869](https://redhat.atlassian.net/browse/RHOAIENG-53869)

## Summary

Enables multiple MaaSModelRefs to point to the same HTTPRoute without causing TokenRateLimitPolicy conflicts. This fixes a critical bug where rate limiting would silently fail for models sharing infrastructure.

## Problem

When multiple `MaaSModelRef` resources resolved to the same HTTPRoute (e.g., both referencing the same LLMInferenceService), the controller created multiple TokenRateLimitPolicies targeting that route. Kuadrant only allows one TRLP per HTTPRoute target by default, causing one policy to be marked as `Overridden` while only the other was `Enforced`.

**Result**: Rate limiting silently failed for some models.

## Root Cause

MaaS used top-level `limits` in TokenRateLimitPolicy specs, which defaults to Kuadrant's `atomic` strategy. With atomic strategy, only one policy per HTTPRoute can be enforced.

## Solution

Refactored TRLP spec to use `defaults.strategy: merge`, allowing multiple TRLPs to target the same HTTPRoute without conflicts.

**Code change** (5 lines):
```diff
 spec := map[string]interface{}{
     "targetRef": map[string]interface{}{
         "group": "gateway.networking.k8s.io",
         "kind":  "HTTPRoute",
         "name":  httpRouteName,
     },
-    "limits": limitsMap,
+    "defaults": map[string]interface{}{
+        "strategy": "merge",
+        "limits":   limitsMap,
+    },
 }
```

## Research & Decision Process

### Phase 0: Research
1. **Documentation Review**: Analyzed Kuadrant TokenRateLimitPolicy docs (v1.3.x, v1.4.x)
2. **Source Code Analysis**: Examined Kuadrant operator implementation
3. **Experimental Testing**: Deployed test TRLPs with and without merge strategy

**Key Finding**: `strategy: merge` allows multiple TRLPs to coexist on the same HTTPRoute.

### Test Results (Experimental)

**WITHOUT merge strategy** (baseline):
```yaml
spec:
  targetRef:
    kind: HTTPRoute
    name: test-route
  limits:  # ← No strategy field
    limit-a: {...}
```
**Result**: ❌ One TRLP shows `Enforced: False` with reason `Overridden`

**WITH merge strategy**:
```yaml
spec:
  targetRef:
    kind: HTTPRoute
    name: test-route
  defaults:
    strategy: merge  # ← Enables coexistence
    limits:
      limit-a: {...}
```
**Result**: ✅ Both TRLPs show `Enforced: True` (no conflicts!)

### Decision: Path A (Simple Fix)

- **Effort**: 3 days (with research and testing)
- **Alternative considered**: HTTPRoute aggregation refactor (10-11 days)
- **Rationale**: Merge strategy solves the problem with minimal changes

## Manual Testing (Real Cluster)

Tested on ROSA cluster with Kuadrant v1.3.1:

### Setup
- LLMInferenceService: `facebook-opt-125m-simulated`
- MaaSModelRef-A and MaaSModelRef-B → Same model → Same HTTPRoute
- Subscription-A: 1,000 tokens/min
- Subscription-B: 5,000 tokens/min

### Results
✅ **Both TRLPs created**:
- `maas-trlp-test-model-ref-a`
- `maas-trlp-test-model-ref-b`

✅ **Both have `defaults.strategy: merge`**:
```bash
$ kubectl get trlp maas-trlp-test-model-ref-a -o jsonpath='{.spec.defaults.strategy}'
merge
$ kubectl get trlp maas-trlp-test-model-ref-b -o jsonpath='{.spec.defaults.strategy}'
merge
```

✅ **Both target the same HTTPRoute**:
```bash
$ kubectl get trlp -n llm -o jsonpath='{.items[*].spec.targetRef.name}'
facebook-opt-125m-simulated-kserve-route facebook-opt-125m-simulated-kserve-route
```

✅ **BOTH show `Enforced: True`** (CRITICAL!):
```json
// TRLP-A
{
  "type": "Enforced",
  "status": "True",
  "message": "TokenRateLimitPolicy has been successfully enforced"
}

// TRLP-B
{
  "type": "Enforced",
  "status": "True",
  "message": "TokenRateLimitPolicy has been successfully enforced"
}
```

✅ **HTTPRoute recognizes both policies**:
```
Object affected by TokenRateLimitPolicy [llm/maas-trlp-test-model-ref-a llm/maas-trlp-test-model-ref-b]
```

✅ **Limitador configured with both rate limits**:
```yaml
limits:
  - name: test-sub-a-test-model-ref-a-tokens
    max_value: 1000
  - name: test-sub-b-test-model-ref-b-tokens
    max_value: 5000
```

### Before vs After Comparison

| Aspect | Before (without merge) | After (with merge) |
|--------|------------------------|-------------------|
| TRLP-A Status | ❌ `Enforced: False` (Overridden) | ✅ `Enforced: True` |
| TRLP-B Status | ✅ `Enforced: True` | ✅ `Enforced: True` |
| HTTPRoute Status | Only one policy recognized | ✅ Both policies recognized |
| Limitador Limits | Only one limit configured | ✅ Both limits configured |
| Actual Behavior | Only one model's rate limit works | ✅ Both models' rate limits work |

## Testing

### Unit Tests ✅
Added 2 new tests in `maassubscription_controller_merge_test.go`:
- `TestMaaSSubscriptionReconciler_TRLPMergeStrategy` - Verifies spec structure
- `TestMaaSSubscriptionReconciler_MultipleModelsSharedRoute` - Validates multiple TRLPs

### E2E Testing ✅
Manual E2E test procedure:
1. Create 2 MaaSModelRefs → same LLMInferenceService
2. Create subscriptions for each with different rate limits
3. Verify both TRLPs created with `defaults.strategy: merge`
4. Verify both show `Enforced: True` (no `Overridden`)
5. Verify Limitador has both rate limits

Full procedure available for future automation.

### Integration Testing ✅
All existing tests pass (no regressions).

## Impact

### Positive
✅ **Fixes critical bug** where rate limiting silently failed
✅ **Minimal code changes** (5 lines)
✅ **Backward compatible** (existing deployments continue to work)
✅ **No breaking changes** required
✅ **Enables legitimate use cases** (multiple user groups → same model)

### Risk Assessment
✅ **Low risk**:
- Simple change (spec structure only)
- All tests pass
- Thoroughly tested in real cluster
- No changes to business logic

### Migration
✅ **Automatic**:
- New TRLPs automatically use merge strategy
- Existing TRLPs updated on next reconciliation
- No manual intervention needed

## Documentation

Updated user-facing documentation in `docs/content/configuration-and-management/maas-controller-overview.md` to explain that multiple models can share HTTPRoutes and that merge strategy enables this.

## Files Changed

- `maas-controller/pkg/controller/maas/maassubscription_controller.go` (+4 -1)
- `maas-controller/pkg/controller/maas/maassubscription_controller_merge_test.go` (+190 new)
- `docs/content/configuration-and-management/maas-controller-overview.md` (+3)

**Total**: 3 files, ~197 lines added, 1 line removed

## Related Issues

- Fixes: [RHOAIENG-53869](https://redhat.atlassian.net/browse/RHOAIENG-53869)
- Related: [RHOAIENG-53865](https://redhat.atlassian.net/browse/RHOAIENG-53865) (UI TokenRateLimitPolicy conflict bug)

## Checklist

- [x] Implementation complete
- [x] Unit tests added and passing
- [x] Manual E2E testing performed in real cluster
- [x] User documentation updated
- [x] All existing tests pass
- [x] Semantic commit messages
- [x] No breaking changes
- [x] Backward compatible

## Reviewer Focus Areas

1. **Spec structure change** (maassubscription_controller.go:289-299)
   - Verify `defaults.strategy: merge` is correct
   - Confirm `limits` moved under `defaults`

2. **Test coverage**
   - Unit tests verify spec format
   - Manual E2E validated real-world scenario

3. **Impact assessment**
   - Backward compatibility preserved
   - No regressions in existing tests
