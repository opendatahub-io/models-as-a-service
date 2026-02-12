# MaaS E2E Testing

## Quick Start

### Prerequisites

- **OpenShift Cluster**: Must be logged in as cluster admin
- **Required Tools**: `oc`, `kubectl`, `kustomize`, `jq`
- **Python**: with pip

### Complete End-to-End Testing

Deploys MaaS platform, creates test users, and runs all tests:

```bash
./test/e2e/scripts/prow_run_smoke_test.sh
```

### Individual Test Suites

If MaaS is already deployed and you want to run specific tests:

```bash
# Smoke tests only (API endpoints, model inference)
./test/e2e/smoke.sh

# Observability tests only (metrics, labels, Prometheus scraping)
cd test/e2e
python3 -m venv .venv
source .venv/bin/activate
pip install -r requirements.txt
export MAAS_API_BASE_URL="https://maas.$(oc get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')/maas-api"
export PYTHONPATH="$(pwd):${PYTHONPATH:-}"
pytest tests/test_observability.py -v
```

## Test Suites

### Smoke Tests (`tests/test_smoke.py`)

Verifies core MaaS functionality:
- Health endpoint availability
- Token minting (authentication)
- Model catalog retrieval
- Chat completions endpoint
- Legacy completions endpoint

### Observability Tests (`tests/test_observability.py`)

Verifies the observability stack is correctly deployed and generating metrics:

**Resource Existence:**
- TelemetryPolicy deployed and enforced
- Istio Telemetry resource exists
- Limitador ServiceMonitor exists

**Limitador Metrics:**
- `authorized_hits` - Total tokens consumed
- `authorized_calls` - Total requests allowed
- `limited_calls` - Total requests rate-limited

**Metric Labels:**
- `user` label present on token metrics
- `tier` label present on token metrics
- `model` label present on token metrics

**Prometheus Integration:**
- User workload monitoring available
- Limitador metrics scraped by Prometheus
- Istio latency metrics (`istio_request_duration_milliseconds_bucket`)

**Configuration:** Test expectations are defined in `config/expected_metrics.yaml`

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

## Test Reports

All tests generate reports in `test/e2e/reports/`:

| Report | Description |
|--------|-------------|
| `smoke-${USER}.xml` | Smoke test JUnit XML |
| `smoke-${USER}.html` | Smoke test HTML report |
| `observability-${USER}.xml` | Observability test JUnit XML |
| `observability-${USER}.html` | Observability test HTML report |

## CI/CD Integration

The `prow_run_smoke_test.sh` script is the main entry point for CI. It:

1. Deploys the MaaS platform (`deploy-rhoai-stable.sh`)
2. Installs observability components (`install-observability.sh`)
3. Deploys sample models
4. Runs validation checks
5. Runs smoke tests
6. Runs observability tests
7. Runs token verification tests
8. Verifies model access recovery after token revocation

Exit code is non-zero if any tests fail.
