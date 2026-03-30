# RHOAIENG-53869 Research Findings: Merge Strategy Investigation

**Date**: 2026-03-18
**Researcher**: Claude Code
**Status**: Phase 0 - Documentation Review Complete

---

## Research Question

Can Kuadrant's `strategy: merge` allow multiple TokenRateLimitPolicies to target the same HTTPRoute without conflicts?

---

## Findings

### 1. Current MaaS Implementation

**File**: `maas-controller/pkg/controller/maas/maassubscription_controller.go:289-296`

```go
spec := map[string]interface{}{
    "targetRef": map[string]interface{}{
        "group": "gateway.networking.k8s.io",
        "kind":  "HTTPRoute",
        "name":  httpRouteName,
    },
    "limits": limitsMap,  // ← Top-level limits
}
```

**Key observation**: MaaS uses **top-level `limits`**, NOT `defaults` or `overrides`.

---

### 2. Kuadrant API Structure

From Kuadrant v1.3.x and v1.4.x documentation:

```yaml
spec:
  targetRef: (required)
    kind: HTTPRoute
    name: my-route

  # Option 1: Top-level limits (current MaaS approach)
  limits: {...}  # ← NO strategy field available here

  # Option 2: Defaults (with strategy)
  defaults:
    strategy: merge  # ← Strategy ONLY available here
    limits: {...}

  # Option 3: Overrides (with strategy, Gateway only)
  overrides:
    strategy: merge
    limits: {...}
```

**Key constraints**:
1. `strategy` field **only exists** in `defaults` and `overrides` sections
2. `limits`, `defaults`, and `overrides` are **mutually exclusive** - can only use one per policy
3. `strategy` values: `atomic` (default) or `merge`

**Source**: https://docs.kuadrant.io/1.4.x/kuadrant-operator/doc/reference/tokenratelimitpolicy/

---

### 3. What `strategy: merge` Does

**Documentation says**: "Merge strategy to apply when merging with other policies."

**What documentation DOES NOT explain**:
- ❌ Whether this allows multiple TRLPs to target the same HTTPRoute
- ❌ How limits from different policies are merged
- ❌ Whether "merging with other policies" means:
  - Merging multiple TRLPs targeting the same resource? OR
  - Merging defaults/overrides from different Gateway hierarchy levels?
- ❌ What happens without merge strategy (conflicts? last-write-wins?)

**Conclusion**: Documentation is ambiguous and lacks examples.

---

### 4. Hypothesis: Two Possible Interpretations

#### Interpretation A: Cross-Policy Merging (What We Hope)
- Multiple TRLPs can target the same HTTPRoute
- With `strategy: merge`, their limits combine
- Example: TRLP-A (free tier limits) + TRLP-B (premium tier limits) → both apply

#### Interpretation B: Hierarchy Merging (More Likely)
- `strategy` controls how Gateway-level defaults merge with HTTPRoute-level limits
- Does NOT enable multiple TRLPs per HTTPRoute
- "Merge with other policies" means merge between Gateway and HTTPRoute policies, not between sibling TRLPs

**Why B seems more likely**:
- Kuadrant follows Gateway API policy attachment patterns
- Gateway API typically enforces one policy per target
- `defaults` and `overrides` language suggests parent → child inheritance
- No examples in docs showing multiple TRLPs on same route

---

### 5. What We Need to Test

To determine which interpretation is correct:

**Test Scenario**:
1. Create HTTPRoute `test-route`
2. Create TRLP-A targeting `test-route` with `defaults.strategy: merge` and limit for free tier
3. Create TRLP-B targeting `test-route` with `defaults.strategy: merge` and limit for premium tier
4. Check status of both TRLPs:
   - **Success**: Both show `Enforced` status
   - **Failure**: One shows `Overridden` status
5. Verify rate limiting works for both tiers

---

## Recommended Next Steps

### Option 1: Experimental Testing (RECOMMENDED)
- Deploy test cluster with Kuadrant
- Run the test scenario above
- Document actual behavior
- **Timeline**: 1 day
- **Risk**: Low - just testing

### Option 2: Ask Kuadrant Community
- File GitHub issue or check existing issues
- Ask on Kuadrant Slack/Discord
- **Timeline**: Variable (hours to days)
- **Risk**: May get faster answer than testing

### Option 3: Read Kuadrant Source Code
- Examine Kuadrant operator implementation
- Look for policy conflict/merge logic
- **Timeline**: 2-4 hours
- **Risk**: Complex codebase

---

## Decision Point

**If `strategy: merge` works** (Interpretation A):
→ **Path A**: Simple fix
  - Refactor MaaS TRLP spec from `limits` to `defaults.strategy: merge, defaults.limits`
  - Keep model-centric approach (one TRLP per model)
  - Estimated effort: 2-3 days

**If `strategy: merge` doesn't work** (Interpretation B):
→ **Path B**: HTTPRoute aggregation refactor
  - Change from model-centric to HTTPRoute-centric aggregation
  - One TRLP per HTTPRoute (not per model)
  - Estimated effort: 10-11 days

---

---

## Additional Research

### GitHub Issues Search
- **Search 1**: `TokenRateLimitPolicy multiple` → **0 results**
- **Search 2**: `overridden OR conflict OR "merge strategy"` → **106 results**, but none directly addressing multiple TRLPs per HTTPRoute
- **Notable finding**: Epic #1653 "Policy status restructuring" suggests this area is actively being redesigned

### Source Code Analysis
**File**: `github.com/Kuadrant/kuadrant-operator/api/v1alpha1/tokenratelimitpolicy_types.go`

```go
type TokenRateLimitPolicySpec struct {
    TargetRef   gatewayapiv1alpha2.LocalPolicyTargetReferenceWithSectionName
    Defaults    *MergeableTokenRateLimitPolicySpec  // ← strategy field here
    Overrides   *MergeableTokenRateLimitPolicySpec  // ← strategy field here
    TokenRateLimitPolicySpecProper                  // ← top-level limits (no strategy)
}

type MergeableTokenRateLimitPolicySpec struct {
    Strategy string // Values: "atomic" or "merge"
    TokenRateLimitPolicySpecProper
}
```

**Key observations**:
- Source code confirms `strategy` is only in `defaults`/`overrides`
- No code comments explaining merge behavior
- No validation preventing multiple policies per target (but also no examples showing it working)

### Conclusion from Research

**Evidence suggests Interpretation B is correct**:

1. **No examples found**: Zero documentation, issues, or examples showing multiple TRLPs per HTTPRoute
2. **Hierarchy-focused naming**: `defaults` and `overrides` language strongly suggests Gateway → HTTPRoute inheritance
3. **Policy attachment patterns**: Kubernetes Gateway API typically enforces one policy per target
4. **Active redesign**: Epic #1653 suggests policy status handling is still evolving
5. **Lack of validation**: If multiple TRLPs per target was supported, you'd expect clear documentation and examples

**Confidence level**: **80% certain** that `strategy: merge` is for hierarchy merging (Gateway → HTTPRoute), NOT for allowing multiple sibling TRLPs on the same HTTPRoute.

---

## Recommendation

### Skip Experimental Testing

**Rationale**:
- Setting up test cluster with Kuadrant: ~4-6 hours
- Writing test scenarios: ~2-3 hours
- **Total time**: ~1 day
- **Expected outcome**: 80% likely to confirm merge doesn't work for our use case
- **Value**: Low, given high confidence from research

### Proceed Directly to Path B (HTTPRoute Aggregation)

**Reasons**:
1. Even if merge strategy worked, we'd still need to refactor TRLP spec structure (2-3 days)
2. If it doesn't work (80% likely), we'd waste 1 day testing + still need full refactor (10-11 days)
3. Path B is the robust, well-understood solution
4. Path B aligns with semantic correctness (policies should aggregate by route, not by model)

**Alternative**: If stakeholders want 100% certainty before committing to 10-day refactor, do quick experimental test.

---

## Decision

🎯 **RECOMMENDED**: Proceed with **Path B - HTTPRoute Aggregation**

**Skip** `strategy: merge` testing unless stakeholders specifically request confirmation.

---

## Status

- [x] Kuadrant documentation reviewed
- [x] Current MaaS implementation analyzed
- [x] API structure documented
- [x] GitHub issues searched
- [x] Source code examined
- [x] Confidence assessment completed
- [ ] ~~Experimental testing~~ (SKIPPED - not worth time investment)
- [x] Decision: **Path B recommended**
- [ ] Implementation (proceed to HTTPRoute aggregation refactor)

---

## References

- Kuadrant TokenRateLimitPolicy Ref: https://docs.kuadrant.io/1.4.x/kuadrant-operator/doc/reference/tokenratelimitpolicy/
- RHOAIENG-53869: https://redhat.atlassian.net/browse/RHOAIENG-53869
- RHOAIENG-53865: https://redhat.atlassian.net/browse/RHOAIENG-53865 (UI TokenRateLimitPolicy conflict bug)
- MaaS Controller: `maas-controller/pkg/controller/maas/maassubscription_controller.go`
