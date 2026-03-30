# RHOAIENG-53869: Prevent Multiple MaaSModelRefs from Pointing to the Same HTTPRoute

**Jira Issue:** [RHOAIENG-53869](https://redhat.atlassian.net/browse/RHOAIENG-53869)
**Related Bug:** [RHOAIENG-53865](https://redhat.atlassian.net/browse/RHOAIENG-53865)

## Problem Statement

Multiple `MaaSModelRef` resources can resolve to the same HTTPRoute (e.g., both reference the same LLMInferenceService, or future providers share routes). This produces multiple TokenRateLimitPolicies targeting the same HTTPRoute, which causes Kuadrant reconciliation issues since Kuadrant only allows one TokenRateLimitPolicy per HTTPRoute target by default.

## High-Level Approach

### 1. Understand the Current State

- Trace how MaaSSubscription controller creates TokenRateLimitPolicies
- Identify where/how the aggregation key is determined (currently `modelRef.Name`)
- Map out the complete flow: MaaSModelRef → RouteResolver → HTTPRoute → TokenRateLimitPolicy
- Understand when multiple MaaSModelRefs would legitimately point to the same HTTPRoute

**Files to review:**
- `maas-controller/pkg/controller/maas/maassubscription_controller.go`
- `maas-controller/pkg/controller/maas/providers.go`
- `maas-controller/pkg/controller/maas/providers_llmisvc.go`
- `maas-controller/pkg/controller/maas/providers_external.go`

### 2. Investigate the Merge Strategy Option First

- Read the Kuadrant TokenRateLimitPolicy documentation on `strategy: merge`
  - Reference: https://docs.kuadrant.io/1.3.x/kuadrant-operator/doc/reference/tokenratelimitpolicy/
- Examine RHOAIENG-53865's findings about merge strategy behavior
- Determine if `strategy: merge` applies to top-level `limits` or only `defaults`/`overrides`
- Test whether merge strategy would allow multiple TokenRateLimitPolicies on the same HTTPRoute to coexist
- Document findings: viable or not viable

**Key Question:** Does `strategy: merge` work for our use case (per-route `limits`)?

**Note from RHOAIENG-53865:** "or uses atomic strategy by default; `strategy: merge` may help but MaaS 3.4 consolidates to single TRP per HTTPRoute"

This suggests the preferred long-term solution might be **prevention** rather than relying on merge strategy.

### 3. If Merge Strategy Doesn't Work: Implement Prevention

- Create a validation mechanism that detects HTTPRoute conflicts before they happen
- Decide on validation location: reconciler status conditions vs validating webhook
- Implement `findMaaSModelRefsForHTTPRoute()` helper that uses RouteResolvers
  - Given an HTTPRoute name/namespace, return all MaaSModelRefs whose RouteResolver resolves to that route
  - Must be provider-agnostic (works for LLMInferenceService, ExternalModel, future providers)
- Ensure the check is invoked for all providers via the RouteResolver interface

**Implementation location:**
- Option A: MaaSModelRef reconciler status conditions (softer validation)
- Option B: Validating webhook (stricter validation, prevents creation)

### 4. Refactor TokenRateLimitPolicy Aggregation

- Change aggregation key from `modelRef.Name` to `(httpRouteName, httpRouteNamespace)`
- This ensures one TokenRateLimitPolicy per HTTPRoute regardless of how many MaaSModelRefs point to it
- Update MaaSSubscription controller to aggregate limits from all subscriptions targeting the same route
- Handle ownership/labeling appropriately when multiple MaaSModelRefs share a policy
  - Labels should identify the HTTPRoute, not a specific MaaSModelRef
  - Finalizers/cleanup logic must check if other MaaSModelRefs still reference the same HTTPRoute

**Files to modify:**
- `maas-controller/pkg/controller/maas/maassubscription_controller.go`

### 5. Handle Edge Cases

- **Deletion:** When a MaaSModelRef is deleted, check if others still reference the same HTTPRoute
  - If yes: keep the TokenRateLimitPolicy, update aggregated limits
  - If no: delete the TokenRateLimitPolicy
- **Multiple subscriptions:** Ensure existing multi-subscription-per-model logic still works
- **Cross-namespace scenarios:** Verify behavior when HTTPRoute and MaaSModelRef are in different namespaces
- **Provider transitions:** Handle cases where a MaaSModelRef's provider changes

### 6. Test & Validate

**Unit tests:**
- Test for `findMaaSModelRefsForHTTPRoute()` helper with mocked RouteResolvers
- Test aggregation key logic

**E2E tests:**
- Create two MaaSModelRefs pointing to the same LLMInferenceService
  - Verify the second is rejected (if using prevention) OR verify single TokenRateLimitPolicy is created (if using aggregation)
- Create two MaaSModelRefs for different LLMInferenceServices
  - Verify both get their own TokenRateLimitPolicies
- Test deletion scenarios:
  - Delete one of two MaaSModelRefs pointing to the same route → verify policy remains
  - Delete the last MaaSModelRef pointing to a route → verify policy is cleaned up
- If merge strategy is adopted: create a scenario that previously failed (multiple TRLPs → same route) and verify it succeeds

**Test files:**
- `maas-controller/pkg/controller/maas/maasmodelref_controller_test.go`
- `maas-controller/pkg/controller/maas/maassubscription_controller_test.go`
- `test/e2e/` (add new E2E test)

## Decision Point

**Step 2 determines the approach:**
- **If merge strategy works:** Simpler, less invasive change (just add `strategy: merge` to TokenRateLimitPolicy spec)
- **If merge strategy doesn't work:** More robust solution via prevention + aggregation refactor (aligns with "MaaS 3.4 consolidates to single TRP per HTTPRoute")

## Notes

### TODO: MCP for E2E Testing

Consider creating an MCP (Model Context Protocol) server in the cluster that enables E2E testing of this work:
- Deploy test MaaSModelRefs, MaaSSubscriptions, HTTPRoutes
- Query TokenRateLimitPolicy status
- Verify Kuadrant reconciliation state
- Validate rate limiting behavior

This would allow testing directly in the cluster without manual kubectl commands.

### Key Design Principles

1. **Provider-agnostic:** Solution must work across LLMInferenceService, ExternalModel, and future providers
2. **Use RouteResolver interface:** Don't hardcode provider-specific logic in validation
3. **Single source of truth:** One TokenRateLimitPolicy per HTTPRoute
4. **Clear error messages:** When conflicts occur, users should understand what's wrong and how to fix it

## References

- **Jira:** [RHOAIENG-53869](https://redhat.atlassian.net/browse/RHOAIENG-53869)
- **Related Bug:** [RHOAIENG-53865](https://redhat.atlassian.net/browse/RHOAIENG-53865)
- **Kuadrant Docs:** https://docs.kuadrant.io/1.3.x/kuadrant-operator/doc/reference/tokenratelimitpolicy/
- **Code:** `maas-controller/pkg/controller/maas/`
