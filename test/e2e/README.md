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
- `E2E_TEST_TOKEN_SA_NAMESPACE`, `E2E_TEST_TOKEN_SA_NAME`: Service account for token generation
- `E2E_TIMEOUT`: Request timeout in seconds (default: 30)
- `E2E_RECONCILE_WAIT`: Wait time for reconciliation in seconds (default: 8)

### API Key Management Tests

Tests for the API Key Management endpoints (`/v1/api-keys`):

```bash
cd test/e2e
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt

export GATEWAY_HOST="maas.apps.your-cluster.example.com"
# Or: export MAAS_API_BASE_URL="https://maas.apps.your-cluster.example.com/maas-api"

# Ensure that you are logged into your openshift cluster prior to execution
# export E2E_SKIP_TLS_VERIFY=true # Disables TLS verification
pytest tests/test_api_keys.py -v \
    --html=reports/api-keys-report.html --self-contained-html
```

**Environment Variables:**
- `MAAS_API_BASE_URL` - MaaS API URL (auto-discovered from `oc get route maas-api`)
- `TOKEN` - User token (auto-obtained via `oc whoami -t`)
- `ADMIN_OC_TOKEN` - Optional admin token for authorization tests (if not set, admin tests are skipped)
- `API_KEY_MAX_EXPIRATION_DAYS` - Max expiration policy for expiration tests (default: 90, matching maas-api default)

**Test Coverage:**
- ✅ Create, search, revoke API keys
- ✅ Admin authorization (manage other users' keys)
- ✅ Non-admin authorization (403 on other users' keys)
- ✅ Validation endpoint (active and revoked keys)

Results: `test/e2e/reports/api-keys-report.html`

### Models Endpoint Tests

Tests for the `/v1/models` endpoint that validate subscription-aware model filtering:

```bash
cd test/e2e
source .venv/bin/activate

# Run all /v1/models tests
pytest tests/test_models_endpoint.py -v

# Run specific test scenarios
pytest tests/test_models_endpoint.py::TestModelsEndpoint::test_single_subscription_auto_select -v
pytest tests/test_models_endpoint.py::TestModelsEndpoint::test_multi_subscription_without_header_403 -v
```

**Test Coverage (15 tests):**

*Success Cases (HTTP 200) - 11 tests:*
- ✅ Single subscription auto-select (no header required)
- ✅ Explicit subscription header with multiple subscriptions
- ✅ Empty subscription header value behavior
- ✅ Subscription header case insensitivity (HTTP standard)
- ✅ Models correctly filtered by subscription
- ⚠️  Same modelRef listed twice should deduplicate (xfail - returns 2+ duplicates instead of 1)
- ⚠️  Different modelRefs serving SAME model ID should deduplicate (xfail - returns 3+ duplicates instead of 1)
- ✅ Different modelRefs with different IDs returns 2 entries (uses non-duplicating simulators)
- ⚠️  Empty model list returns [] not null (xfail - currently returns null)
- ✅ Response schema matches OpenAPI specification
- ✅ Model metadata (url, ready, created, owned_by) preserved

*Error Cases (HTTP 403) - 3 tests:*
- ✅ Multiple subscriptions without header → 403 permission_error
- ✅ Invalid subscription header → 403 permission_error
- ✅ Access denied to subscription → 403 permission_error

*Error Cases (HTTP 401) - 1 test:*
- ✅ Unauthenticated request → 401 authentication_error

**What's Being Validated:**
The `/v1/models` endpoint implements subscription-aware model filtering:
- With a **user token**, a single matching subscription can be auto-selected; with multiple subscriptions, the tests expect `X-MaaS-Subscription` when the platform cannot disambiguate.
- With an **API key**, the subscription is fixed at mint time—listing does not rely on sending `X-MaaS-Subscription` for that case.
- Returns proper error responses (403/401) with `permission_error`/`authentication_error` types
- Models are correctly filtered to only show those from the specified subscription
- Response structure matches OpenAPI schema: `{"object": "list", "data": [...]}`
- HTTP header handling follows standards (case-insensitive)
- Model metadata is accurately preserved from source

## CI Integration

These tests run automatically in CI via:
- **Prow**: `./test/e2e/scripts/prow_run_smoke_test.sh` (includes all E2E tests)
- **GitHub Actions**: Can be integrated into `.github/workflows/` as needed

The `prow_run_smoke_test.sh` script:
1. Deploys MaaS platform and dependencies
2. Deploys test models (free + premium simulators)
3. Runs E2E tests:
   - API key management (`test_api_keys.py`)
   - Subscription controller (`test_subscription.py`)
   - Models endpoint (`test_models_endpoint.py`)
   - External OIDC (`test_external_oidc.py`) when `EXTERNAL_OIDC=true`
4. Requires externally provided OIDC settings when `EXTERNAL_OIDC=true`
5. Runs deployment validation and token metadata verification
6. Collects artifacts (HTML/XML reports, logs) to `ARTIFACT_DIR`

When enabling external OIDC coverage, provide a pre-existing OIDC provider and export:

- `OIDC_ISSUER_URL`
- `OIDC_TOKEN_URL`
- `OIDC_CLIENT_ID`
- `OIDC_USERNAME`
- `OIDC_PASSWORD`
