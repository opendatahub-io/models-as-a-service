# MaaS E2E Testing

## Quick Start

### Prerequisites

- **OpenShift Cluster**: Must be logged in as cluster admin
- **Required Tools**: `oc`, `kubectl`, `kustomize`, `jq`
- **Python**: with pip

### Complete End-to-End Testing

Deploys MaaS platform, creates test users, and runs smoke and observability tests:

```bash
./test/e2e/scripts/prow_run_e2e_test.sh
```

### Individual Test Suites

If MaaS is already deployed and you want to run specific tests:

```bash
./test/e2e/smoke.sh          # API endpoints, model inference
./test/e2e/observability.sh   # Metrics, Prometheus scraping, labels
```

## Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `SKIP_DEPLOY` | Skip platform deployment | `false` |
| `SKIP_VALIDATION` | Skip deployment validation | `false` |
| `SKIP_SMOKE` | Skip smoke tests | `false` |
| `SKIP_OBSERVABILITY` | Skip observability tests | `false` |
| `SKIP_TOKEN_VERIFICATION` | Skip token metadata verification | `false` |
| `SKIP_AUTH_CHECK` | Skip Authorino auth readiness check | `true` |
| `MAAS_API_IMAGE` | Custom image for MaaS API | (uses default) |
| `INSECURE_HTTP` | Use HTTP instead of HTTPS | `false` |
| `SMOKE_TOKEN_MINT_ATTEMPTS` | Retries for token mint in smoke (transient 5xx) | `3` |

## Debugging

### Token mint failures

Smoke script retries token mint up to `SMOKE_TOKEN_MINT_ATTEMPTS` times (default 3) with 5s delay to tolerate transient API errors.

## Test Reports

All tests generate reports in `test/e2e/reports/`:

| Report | Description |
|--------|-------------|
| `smoke-${USER}.xml` | Smoke test JUnit XML |
| `smoke-${USER}.html` | Smoke test HTML report |
| `observability-${USER}.xml` | Observability test JUnit XML |
| `observability-${USER}.html` | Observability test HTML report |

## CI/CD Integration

The `prow_run_e2e_test.sh` script is the main entry point for CI. It uses this flow:

1. Deploy MaaS platform
2. Deploy sample models
3. Install observability components (`install-observability.sh`)
4. Set up test users (admin, edit, view)
5. **As admin:** validate deployment, run token verification, run smoke tests, then run observability tests (41 tests)
6. **As edit user:** run smoke tests only
7. **As view user:** run smoke tests only

Exit code is non-zero if any tests fail.

**How the test flow is validated:** The script is not unit-tested. The flow is exercised by running `prow_run_e2e_test.sh` manually or in CI (e.g. Prow); a successful run validates the flow.

Observability runs once as admin (cluster-admin). The test makes a chat request (`make_test_request`) to generate metrics, then validates all component endpoints, Prometheus scraping, labels, and metric types. Edit and view users only run smoke tests — the metrics pipeline doesn't depend on who queries it, and port-forward/Prometheus access is an OpenShift RBAC concern, not an observability one.
