# RHOAIENG-53869 Implementation Decision

**Date**: 2026-03-19
**Status**: ✅ **DECISION MADE - Path A Confirmed**
**Estimated Effort**: 2-3 days

---

## Decision Summary

After experimental testing, we have confirmed that **Path A (Simple Fix)** is the correct approach.

### ✅ Path A: Use `strategy: merge`
- **Effort**: 2-3 days
- **Approach**: Refactor MaaS TRLP spec to use `defaults.strategy: merge`
- **Impact**: Minimal code changes, no architectural refactor needed

### ❌ Path B: HTTPRoute Aggregation (NOT NEEDED)
- **Effort**: 10-11 days
- **Reason for rejection**: Merge strategy solves the problem, no need for complex refactor

---

## Test Results

**Experimental testing confirmed**:
- ✅ Multiple TRLPs with `defaults.strategy: merge` can target the same HTTPRoute
- ✅ All policies show `Enforced: True` (no conflicts)
- ✅ Limitador configures limits from ALL policies
- ❌ Without merge strategy, one policy gets `Overridden` (conflict)

**See detailed results**: `RHOAIENG-53869-experimental-test-results.md`

---

## Implementation Plan

### Phase 1: Refactor TRLP Spec (1 day)

**File**: `maas-controller/pkg/controller/maas/maassubscription_controller.go`

**Change** (lines 289-296):

```go
// BEFORE (current implementation)
spec := map[string]interface{}{
    "targetRef": map[string]interface{}{
        "group": "gateway.networking.k8s.io",
        "kind":  "HTTPRoute",
        "name":  httpRouteName,
    },
    "limits": limitsMap,  // ← Top-level limits (no strategy)
}

// AFTER (new implementation)
spec := map[string]interface{}{
    "targetRef": map[string]interface{}{
        "group": "gateway.networking.k8s.io",
        "kind":  "HTTPRoute",
        "name":  httpRouteName,
    },
    "defaults": map[string]interface{}{
        "strategy": "merge",  // ← Enable merge strategy
        "limits":   limitsMap,
    },
}
```

**Impact**:
- Allows multiple MaaSModelRefs to point to the same HTTPRoute
- Each subscription creates a TRLP with unique limit names
- Kuadrant merges all policies without conflicts

---

### Phase 2: Update Tests (1 day)

#### Unit Tests

**File**: `maas-controller/pkg/controller/maas/maassubscription_controller_test.go`

Add test cases:
1. `TestTRLPSpecFormat_WithMergeStrategy`
   - Verify TRLP has `defaults.strategy: merge`
   - Verify `defaults.limits` structure

2. `TestMultipleModels_SameHTTPRoute`
   - Create two MaaSModelRefs → same LLMInferenceService
   - Create subscriptions for each
   - Verify TWO TRLPs are created (one per model)
   - Verify both target the same HTTPRoute
   - Verify both have `defaults.strategy: merge`

#### E2E Tests

**File**: `test/e2e/multiple_models_shared_route_test.go` (new)

Scenario:
1. Deploy LLMInferenceService `shared-model`
2. Create MaaSModelRef `model-a` → `shared-model`
3. Create MaaSModelRef `model-b` → `shared-model`
4. Create subscriptions with different rate limits
5. Verify both TRLPs exist and show `Enforced: True`
6. Test rate limiting for both models

---

### Phase 3: Documentation & Migration (0.5 day)

#### Update Documentation

**File**: `docs/content/user-guide/subscriptions.md`

Add section:
```markdown
## Multiple Models Sharing Routes

When multiple MaaSModelRefs resolve to the same HTTPRoute (e.g., referencing
the same LLMInferenceService), the controller creates separate
TokenRateLimitPolicies for each model using `strategy: merge`. This allows
rate limits to be enforced independently per model while sharing the same route.

**Example**:
- Model A: 1,000 tokens/min
- Model B: 5,000 tokens/min
- Both point to the same LLMInferenceService
- Both rate limits are enforced correctly
```

#### Migration Notes

**Backward Compatibility**:
- Existing TRLPs with top-level `limits` will continue to work
- New TRLPs will use `defaults.strategy: merge`
- No manual migration required (policies will be updated on next reconciliation)

**Potential Issues**:
- If users have custom TRLPs (not managed by MaaS controller), they won't have merge strategy
- Document recommendation to add `strategy: merge` to custom TRLPs

---

### Phase 4: Optional Enhancements (0.5 day)

#### Add Status Condition for Shared Routes

**File**: `maas-controller/pkg/controller/maas/maasmodelref_controller.go`

Add informational condition when models share routes:

```go
apimeta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
    Type:   "HTTPRouteShared",
    Status: metav1.ConditionTrue,
    Reason: "SharedWithOtherModels",
    Message: fmt.Sprintf("HTTPRoute %s/%s is shared with models: %s. "+
        "Rate limits are enforced independently using merge strategy.",
        routeNS, routeName, strings.Join(otherModels, ", ")),
})
```

**Benefit**: Users can see when models share infrastructure without it being an error.

---

## Timeline

| Phase | Task | Effort |
|-------|------|--------|
| 1 | Refactor TRLP spec | 1 day |
| 2 | Update tests | 1 day |
| 3 | Documentation & migration | 0.5 day |
| 4 | Optional enhancements | 0.5 day |
| **Total** | | **2-3 days** |

---

## Success Criteria

- [x] Experimental testing completed
- [x] Merge strategy confirmed to work
- [x] Decision documented
- [ ] TRLP spec refactored to use `defaults.strategy: merge`
- [ ] Multiple MaaSModelRefs can point to same HTTPRoute without conflicts
- [ ] Unit tests pass
- [ ] E2E tests validate the fix
- [ ] Both TRLPs show `Enforced: True` in status
- [ ] Rate limiting works for all models sharing a route
- [ ] Documentation updated

---

## Key Learnings

1. **Kuadrant supports multiple policies per target** when using `strategy: merge`
2. **The `strategy` field only exists in `defaults` and `overrides`**, not in top-level `limits`
3. **MaaS current implementation** uses top-level `limits`, which defaults to `atomic` strategy
4. **Atomic strategy** causes one policy to override others (only one enforced)
5. **Merge strategy** allows policies to compose their limits (all enforced)

---

## Next Steps

1. **Immediate**:
   - Implement Phase 1 (refactor TRLP spec)
   - Test locally with two MaaSModelRefs → same LLMInferenceService

2. **Follow-up**:
   - Add unit tests (Phase 2)
   - Add E2E tests (Phase 2)
   - Update documentation (Phase 3)

3. **Optional**:
   - Add informational status condition (Phase 4)
   - Consider metrics for shared routes

---

## References

- **Experimental Test Results**: `RHOAIENG-53869-experimental-test-results.md`
- **Research Findings**: `RHOAIENG-53869-research-findings.md`
- **Implementation Plan**: `RHOAIENG-53869-approach.md`
- **JIRA**: [RHOAIENG-53869](https://redhat.atlassian.net/browse/RHOAIENG-53869)
- **Related Bug**: [RHOAIENG-53865](https://redhat.atlassian.net/browse/RHOAIENG-53865)
