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

### Smoke Tests Only

If MaaS is already deployed and you just want to run tests:
```bash
./test/e2e/smoke.sh
```

### API Key Management Tests

Tests for the API Key Management endpoints (`/v1/api-keys`):

```bash
cd test/e2e
./run_api_key_tests.sh
```

**Environment Variables:**
- `MAAS_API_BASE_URL` - MaaS API URL (auto-discovered from `oc get route maas-api`)
- `TOKEN` - User token (auto-obtained via `oc whoami -t`)
- `ADMIN_OC_TOKEN` - Optional admin token for authorization tests (if not set, admin tests are skipped)

**Test Coverage:**
- ✅ Create, list, revoke API keys
- ✅ Admin authorization (manage other users' keys)
- ✅ Non-admin authorization (403 on other users' keys)
- ✅ Validation endpoint (active and revoked keys)

Results: `test/e2e/reports/api-keys-report.html`
