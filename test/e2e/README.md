# MaaS E2E Testing

## Quick Start

### Prerequisites

- **OpenShift Cluster**: Must be logged in as cluster admin
- **Required Tools**: `oc`, `kubectl`, `kustomize`, `jq`
- **Python**: with pip

### Complete End-to-End Testing

Deploys MaaS platform, creates test users, and runs smoke tests:

```bash
./test/e2e/scripts/prow_run_smoke_test.sh
```

This script will:
1. Deploy the MaaS platform on OpenShift (using ODH operator by default)
2. Deploy a simulator model for testing
3. Validate the deployment
4. Create test users (admin, edit, view)
5. Run token metadata verification
6. Run smoke tests for each user

### Smoke Tests Only

If MaaS is already deployed and you just want to run tests:
```bash
./test/e2e/smoke.sh
```

## CI/CD Pipeline Usage

The script supports testing pipeline-built images via environment variables:

```bash
# Test PR-built images
OPERATOR_CATALOG=quay.io/opendatahub/opendatahub-operator-catalog:pr-123 \
MAAS_API_IMAGE=quay.io/opendatahub/maas-api:pr-456 \
./test/e2e/scripts/prow_run_smoke_test.sh
```

Supported variables: `OPERATOR_CATALOG`, `MAAS_API_IMAGE`, `OPERATOR_IMAGE`, `SKIP_VALIDATION`, `SKIP_SMOKE`

See the script header comments for complete documentation.