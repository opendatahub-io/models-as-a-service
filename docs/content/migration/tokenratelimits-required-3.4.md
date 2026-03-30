# MaaSSubscription Breaking Change - tokenRateLimits Required (3.4)

## Overview

**Version:** 3.4
**Type:** Breaking Change
**Component:** MaaSSubscription CRD

Starting in version 3.4, the `MaaSSubscription` CRD requires **inline `tokenRateLimits`** on each model reference. The previously unused `tokenRateLimitRef` field has been **removed** from the API.

## What Changed

### Before (3.3 and earlier)

The `ModelSubscriptionRef` type included both fields, though only `tokenRateLimits` was actually used by the controller:

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
spec:
  modelRefs:
    - name: my-model
      namespace: llm
      # Optional - but this is what the controller used
      tokenRateLimits:
        - limit: 1000
          window: 1m
      # UNUSED by controller, but present in CRD schema
      tokenRateLimitRef: "some-ref"
```

### After (3.4+)

The `tokenRateLimits` field is now **required** with at least one rate limit, and `tokenRateLimitRef` is **removed**:

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
spec:
  modelRefs:
    - name: my-model
      namespace: llm
      # REQUIRED - at least one rate limit
      tokenRateLimits:
        - limit: 1000
          window: 1m
```

## Why This Change

1. **Controller Behavior**: The controller has always used only `tokenRateLimits` - `tokenRateLimitRef` was never implemented
2. **API Clarity**: Removing unused fields prevents confusion and invalid configurations
3. **UI Alignment**: Ensures the console/UI can represent all valid subscription configurations
4. **Product Direction**: MaaS 3.4 requires inline rate limits for all subscriptions

## Migration Guide

### Step 1: Identify Affected Resources

Check if you have any subscriptions without `tokenRateLimits`:

```bash
kubectl get maassubscription -A -o json | \
  jq -r '.items[] | select(.spec.modelRefs[]? | .tokenRateLimits == null) |
  "\(.metadata.namespace)/\(.metadata.name)"'
```

### Step 2: Update Your Subscriptions

For each subscription missing `tokenRateLimits`, add rate limits before upgrading:

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: my-subscription
  namespace: models-as-a-service
spec:
  owner:
    groups:
      - name: system:authenticated
  modelRefs:
    - name: my-model
      namespace: llm
      # ADD THIS - at least one rate limit required
      tokenRateLimits:
        - limit: 1000    # tokens per window
          window: 1m     # 1m, 1h, 24h, etc.
```

### Step 3: Remove tokenRateLimitRef (if present)

If any of your YAML manifests include `tokenRateLimitRef`, remove it:

```yaml
# This will fail validation in 3.4+
tokenRateLimitRef: "some-ref"  # ❌ REMOVE THIS
```

### Step 4: Validate

After updating, validate your subscriptions:

```bash
kubectl apply --dry-run=server -f your-subscription.yaml
```

## Common Rate Limit Patterns

### Free Tier
```yaml
tokenRateLimits:
  - limit: 100
    window: 1m
```

### Standard Tier
```yaml
tokenRateLimits:
  - limit: 1000
    window: 1m
  - limit: 50000
    window: 1h
```

### Premium Tier
```yaml
tokenRateLimits:
  - limit: 10000
    window: 1m
  - limit: 500000
    window: 1h
  - limit: 5000000
    window: 24h
```

## Error Messages

### Missing tokenRateLimits

```
Error: spec.modelRefs[0].tokenRateLimits: Required value
```

**Solution:** Add at least one `tokenRateLimits` entry to each model reference.

### Using tokenRateLimitRef

```
Error: unknown field "spec.modelRefs[0].tokenRateLimitRef"
```

**Solution:** Remove `tokenRateLimitRef` field and use inline `tokenRateLimits` instead.

### Empty tokenRateLimits array

```
Error: spec.modelRefs[0].tokenRateLimits: Invalid value: "array": must have at least 1 items
```

**Solution:** Add at least one rate limit to the `tokenRateLimits` array.

## Rollback

If you need to rollback to 3.3, your subscriptions with inline `tokenRateLimits` will continue to work (this field has always been supported and used by the controller).

## Support

For questions or issues with migration:

- Review the [MaaSSubscription CRD reference](../reference/crds/maas-subscription.md)
- Check existing examples in `docs/samples/maas-system/`
- File an issue at [github.com/opendatahub-io/models-as-a-service](https://github.com/opendatahub-io/models-as-a-service/issues)
