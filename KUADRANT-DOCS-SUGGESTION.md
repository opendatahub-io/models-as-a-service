# Kuadrant Documentation Improvement Suggestion

**Date**: 2026-03-23
**Context**: RHOAIENG-53869 - TokenRateLimitPolicy merge strategy implementation
**Kuadrant Version**: v1.3.x, v1.4.x

---

## Summary

During implementation of MaaS multiple-models-per-HTTPRoute support, we discovered that Kuadrant's `strategy: merge` feature is documented but lacks practical guidance and examples. This proposal suggests adding documentation to help users understand when and how to use merge strategy.

---

## Current State of Documentation

The TokenRateLimitPolicy reference docs mention:
- `strategy` field exists in `MergeableTokenRateLimitPolicySpec`
- Values: `atomic` (default) or `merge`
- Field is available in `defaults` and `overrides` sections

**What's missing:**
- No explanation of the problem `strategy: merge` solves
- No examples showing the difference between atomic and merge
- No guidance on when to use merge vs atomic
- No migration path from top-level `limits` to `defaults.limits`

---

## Problem We Encountered

**Scenario**: Multiple TokenRateLimitPolicies targeting the same HTTPRoute

**Without documentation, we had to:**
1. Read Kuadrant operator source code
2. Perform experimental testing with real cluster
3. Trial-and-error to discover that top-level `limits` doesn't support `strategy`

**Our findings:**
- **Default behavior (atomic)**: Only one TRLP enforced, others show `Enforced: False` with reason `Overridden`
- **With `strategy: merge`**: Multiple TRLPs coexist peacefully, all show `Enforced: True`

---

## Suggested Documentation Additions

### 1. Add Section: "Multiple Policies on the Same Target"

**Location**: TokenRateLimitPolicy reference docs, after the basic spec examples

**Content**:

```markdown
## Multiple TokenRateLimitPolicies on the Same Target

### Default Behavior (Atomic Strategy)

By default, only one TokenRateLimitPolicy can be enforced per HTTPRoute target.
If multiple policies target the same route, Kuadrant marks all but one as
`Overridden` (Enforced: False).

**Example - Problem Scenario:**

Two policies targeting the same HTTPRoute:

```yaml
# Policy A
apiVersion: kuadrant.io/v1alpha1
kind: TokenRateLimitPolicy
metadata:
  name: team-a-limits
spec:
  targetRef:
    kind: HTTPRoute
    name: shared-api-route
  limits:  # ← Defaults to atomic strategy
    team-a-rate:
      rates:
      - limit: 1000
        window: 1m
```

```yaml
# Policy B
apiVersion: kuadrant.io/v1alpha1
kind: TokenRateLimitPolicy
metadata:
  name: team-b-limits
spec:
  targetRef:
    kind: HTTPRoute
    name: shared-api-route  # ← Same target
  limits:
    team-b-rate:
      rates:
      - limit: 5000
        window: 1m
```

**Result**: Only one policy is enforced. The other shows:
```
status:
  conditions:
  - type: Enforced
    status: False
    reason: Overridden
```

### Using Merge Strategy

To allow multiple policies on the same target, use `strategy: merge` in the
`defaults` section:

```yaml
# Policy A - Updated
apiVersion: kuadrant.io/v1alpha1
kind: TokenRateLimitPolicy
metadata:
  name: team-a-limits
spec:
  targetRef:
    kind: HTTPRoute
    name: shared-api-route
  defaults:
    strategy: merge  # ← Enables coexistence
    limits:
      team-a-rate:
        rates:
        - limit: 1000
          window: 1m
        when:
        - predicate: auth.identity.team == "team-a"
```

```yaml
# Policy B - Updated
apiVersion: kuadrant.io/v1alpha1
kind: TokenRateLimitPolicy
metadata:
  name: team-b-limits
spec:
  targetRef:
    kind: HTTPRoute
    name: shared-api-route
  defaults:
    strategy: merge  # ← Enables coexistence
    limits:
      team-b-rate:
        rates:
        - limit: 5000
          window: 1m
        when:
        - predicate: auth.identity.team == "team-b"
```

**Result**: Both policies are enforced. Both show `Enforced: True`.

The generated Limitador configuration contains rate limits from both policies.

### When to Use Merge Strategy

**Use `strategy: merge` when:**
- Multiple teams/applications share the same HTTPRoute
- Each needs independent rate limits (different quotas, different predicates)
- You want policies to compose rather than conflict

**Use atomic strategy (default) when:**
- Only one policy should apply to a target
- You want exclusive ownership semantics
- Conflicts should be detected and prevented

### Migration from Top-Level Limits

If you currently use top-level `limits`:

```yaml
# Old format (no strategy support)
spec:
  targetRef: ...
  limits:
    my-limit: ...
```

Migrate to `defaults` to use `strategy`:

```yaml
# New format (strategy supported)
spec:
  targetRef: ...
  defaults:
    strategy: merge  # or atomic
    limits:
      my-limit: ...
```

**Note**: The `strategy` field is only available in `defaults` and `overrides`,
not in top-level `limits`.
```

---

### 2. Add to Troubleshooting Section

```markdown
### Multiple Policies Show "Overridden" Status

**Symptom**: Created multiple TokenRateLimitPolicies targeting the same
HTTPRoute, but only one shows `Enforced: True`.

**Cause**: Default `atomic` strategy allows only one policy per target.

**Solution**: Use `defaults.strategy: merge` in all policies targeting the
same route. See "Multiple Policies on the Same Target" section.
```

---

### 3. Update Examples Section

Add practical example:

```markdown
### Example: Multi-Tenant API with Independent Rate Limits

Scenario: Shared API route with different rate limits per customer tier.

[Include complete YAML example with merge strategy]
```

---

## Implementation Suggestions

**Where to add this:**
1. **Primary location**: Kuadrant TokenRateLimitPolicy reference docs
2. **Link from**: Getting started guide, troubleshooting
3. **Related**: Update architecture docs to explain atomic vs merge semantics

**Format**:
- Add before/after YAML examples
- Include `kubectl` commands to verify status
- Show Limitador config output (what actually gets generated)

**Additional materials**:
- Tutorial: "Managing Rate Limits for Shared Routes"
- Blog post: "Understanding TokenRateLimitPolicy Merge Strategy"

---

## Benefits to Kuadrant Users

1. **Reduced trial-and-error**: Users won't need experimental testing to discover merge strategy
2. **Clearer mental model**: Understanding atomic vs merge helps with policy design
3. **Better multi-tenancy**: Clear guidance for shared infrastructure scenarios
4. **Easier troubleshooting**: Users can diagnose "Overridden" status quickly

---

## References

- **Our implementation**: https://github.com/opendatahub-io/models-as-a-service/pull/585
- **JIRA ticket**: RHOAIENG-53869
- **Current Kuadrant docs**: https://docs.kuadrant.io/1.3.x/kuadrant-operator/doc/reference/tokenratelimitpolicy/

---

## Next Steps

**Option 1**: Open GitHub issue in `Kuadrant/kuadrant-operator` repo with this content
**Option 2**: Fork Kuadrant docs and submit PR with documentation additions
**Option 3**: Share this with Kuadrant team via Slack/email for their input

**Recommended**: Start with GitHub issue to gather feedback, then submit PR.

---

## Contact

If Kuadrant team has questions about our use case or implementation, contact:
- **Assignee**: Egor Lunin (elunin@redhat.com)
- **Team**: Red Hat OpenShift AI - Models as a Service
