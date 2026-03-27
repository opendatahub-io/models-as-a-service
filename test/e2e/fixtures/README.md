# E2E Test Fixtures

This directory contains kustomizations for end-to-end testing that combine public samples with test-only fixtures.

## Contents

### Public Samples (from `docs/samples/maas-system/`)
- **free**: `system:authenticated` group, 100 tokens/min
- **premium**: `premium-user` group, 1000 tokens/min

### Test-Only Fixtures
- **unconfigured**: Model with no MaaSAuthPolicy or MaaSSubscription (validates that gateway denies access with 403)
- **distinct**: First distinct model serving `test/e2e-distinct-model` (validates multiple distinct models in subscriptions)
- **distinct-2**: Second distinct model serving `test/e2e-distinct-model-2` (validates multiple distinct models in subscriptions)

## Usage

### For E2E Tests (CI)

```bash
# Deploy all fixtures (public samples + test-only)
kustomize build test/e2e/fixtures | kubectl apply -f -
```

### For Manual Testing

To deploy only the public samples without test fixtures, use:

```bash
# Public samples only (free + premium)
kustomize build docs/samples/maas-system | kubectl apply -f -
```

## Note

⚠️ **Do not use this kustomization for production or sample installations.** It includes test-only models that are designed to validate edge cases and should not be deployed in normal usage scenarios. For sample installations, use `docs/samples/maas-system/` instead.
