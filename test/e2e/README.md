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

Verifies the observability stack is correctly deployed and generating metrics. **Observability tests run once as admin** (cluster-admin) — see [CI/CD Integration](#cicd-integration). This validates the full infrastructure: metrics endpoints, Prometheus scraping, labels, and metric types. Running as edit/view is redundant: the metrics pipeline doesn't depend on who queries it, and port-forward access is an OpenShift RBAC concern, not an observability one.

**How metrics are validated:**

- **Direct endpoint checks (port-forward only):** Tests use **port-forward** from the test process to each component (no exec into pods). A failure isolates "component endpoint" vs "Prometheus scraping":
  - **Limitador:** port-forward → `http://127.0.0.1:18590/metrics`; assert names and `user`/`tier`/`model` labels.
  - **Istio gateway:** port-forward → `http://127.0.0.1:18591/stats/prometheus` (Envoy; not `/metrics`); assert `istio_*` metrics.
  - **vLLM/model:** port-forward → `https://127.0.0.1:18592/metrics` (or http per config); assert at least one vLLM metric.
  - **Authorino:** port-forward → `http://127.0.0.1:18593/server-metrics`; assert `auth_server_authconfig_*`.
- **Prometheus queries (all other components):** Prometheus is queried via **port-forward + REST** (no exec): we port-forward the Prometheus pod to localhost and `GET /api/v1/query` and `/api/v1/metadata`. We check all components, metrics, and labels:
  - **Limitador** (user-workload): `limitador_up`, `authorized_hits`, `authorized_calls`, `limited_calls` and labels `user`, `tier`, `model` (on `authorized_hits`).
  - **Istio gateway** (platform): `istio_request_duration_milliseconds_bucket`, `istio_requests_total` and labels `tier`, `destination_service_name`, `response_code`.
  - **vLLM** (user-workload): e.g. `vllm:e2e_request_latency_seconds_*`, `vllm:request_success_total`, `vllm:num_requests_running`, `vllm:num_requests_waiting`, `vllm:kv_cache_usage_perc`, token histograms, TTFT, ITL, and `model_name` label.
  - **Authorino** (user-workload): `auth_server_authconfig_duration_seconds_*`, `auth_server_authconfig_response_status` and `status` label.
  - **Metric types** (counter/gauge/histogram) are asserted from `expected_metrics.yaml` via Prometheus `/api/v1/metadata`.

**Resource existence:** TelemetryPolicy deployed and enforced; Istio Telemetry; Limitador ServiceMonitor or Kuadrant PodMonitor; RateLimitPolicy/TokenRateLimitPolicy enforced.

**Configuration:** Test expectations are defined in `config/expected_metrics.yaml`.

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

The `prow_run_smoke_test.sh` script is the main entry point for CI. It uses this flow:

1. Deploy MaaS platform
2. Deploy sample models
3. Install observability components (`install-observability.sh`)
4. Set up test users (admin, edit, view)
5. **As admin:** validate deployment, run token verification, run smoke tests, then run observability tests (41 tests)
6. **As edit user:** run smoke tests only
7. **As view user:** run smoke tests only

Exit code is non-zero if any tests fail.

**How the test flow is validated:** The script is not unit-tested. The flow is exercised by running `prow_run_smoke_test.sh` manually or in CI (e.g. Prow); a successful run validates the flow.

Observability runs once as admin (cluster-admin). The test makes a chat request (`make_test_request`) to generate metrics, then validates all component endpoints, Prometheus scraping, labels, and metric types. Edit and view users only run smoke tests — the metrics pipeline doesn't depend on who queries it, and port-forward/Prometheus access is an OpenShift RBAC concern, not an observability one.
