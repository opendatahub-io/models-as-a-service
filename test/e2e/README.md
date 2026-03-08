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

## Running Tests Locally

### Run All Subscription Tests
```bash
export GATEWAY_HOST="maas.apps.your-cluster.example.com"
export MAAS_NAMESPACE="opendatahub"

# Activate virtual environment
cd test/e2e
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt

# Run all tests
pytest tests/test_subscription.py -v

# Run specific test class (e2e subscription flow tests)
pytest tests/test_subscription.py::TestE2ESubscriptionFlow -v

# Run specific test
pytest tests/test_subscription.py::TestE2ESubscriptionFlow::test_e2e_with_both_access_and_subscription_gets_200 -v
```

### Environment Variables

See `tests/test_subscription.py` docstring for all available environment variables. Key ones:

- `GATEWAY_HOST`: Gateway hostname (required)
- `MAAS_NAMESPACE`: MaaS namespace (default: opendatahub)
- `E2E_TEST_TOKEN_SA_NAMESPACE`, `E2E_TEST_TOKEN_SA_NAME`: Service account for token generation
- `E2E_TIMEOUT`: Request timeout in seconds (default: 30)
- `E2E_RECONCILE_WAIT`: Wait time for reconciliation in seconds (default: 8)

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

## CI Integration

These tests run automatically in CI via:
- **Prow**: `./test/e2e/scripts/prow_run_smoke_test.sh` (includes subscription tests)
- **GitHub Actions**: Can be integrated into `.github/workflows/` as needed

The `prow_run_smoke_test.sh` script:
1. Deploys MaaS platform and dependencies
2. Deploys test models (free + premium simulators)
3. Runs subscription controller tests (`test_subscription.py`)
4. Runs deployment validation and token metadata verification
5. Collects artifacts (HTML/XML reports, logs) to `ARTIFACT_DIR`
