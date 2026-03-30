# Release Notes

## v3.4.0

### Breaking Changes

#### MaaSSubscription: tokenRateLimits Now Required

**⚠️ BREAKING CHANGE**

The `MaaSSubscription` CRD now requires inline `tokenRateLimits` on each model reference. The unused `tokenRateLimitRef` field has been removed.

**Impact:**
- All `MaaSSubscription` resources must include at least one `tokenRateLimits` entry per model
- Manifests using `tokenRateLimitRef` will be rejected with "unknown field" error
- Subscriptions without `tokenRateLimits` will fail validation with "Required value" error

**Migration:** See [tokenRateLimits Migration Guide](../migration/tokenratelimits-required-3.4.md) for detailed upgrade instructions.

**Rationale:** The controller has always used only inline `tokenRateLimits` - removing the unused `tokenRateLimitRef` field eliminates confusion and ensures UI/API consistency.

---

## v0.1.0

*Initial release.*

<!-- Add release notes for v0.1.0 here -->
