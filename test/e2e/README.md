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

Verifies the observability stack is correctly deployed and generating metrics. **Observability tests run as admin and edit** (see [CI/CD Integration](#cicd-integration)). Admin runs the full suite including infrastructure validation. Edit runs the same suite, verifying that edit-level users can access metrics via port-forward (edit is granted access to platform Prometheus via a Role/RoleBinding in `openshift-monitoring`). View users only run smoke tests -- observability requires Prometheus/port-forward access that view users don't have by OpenShift RBAC design.

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

### Platform Prometheus (edit user) failures

If observability fails for the **edit** user on Istio/platform Prometheus tests in `openshift-monitoring`, the CI script already creates a Role/RoleBinding granting the edit user `get pods` and `create pods/portforward` in that namespace. If the RBAC resources are not present, re-run the full CI or apply them manually.

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
5. **As admin:** validate deployment, run token verification, run smoke tests, then run observability tests
6. **As edit user:** run smoke tests, then run observability tests
7. **As view user:** run smoke tests only (no observability -- view lacks Prometheus/port-forward access by design)

Exit code is non-zero if any tests fail.

**How the test flow is validated:** The script is not unit-tested. The flow is exercised by running `prow_run_smoke_test.sh` manually or in CI (e.g. Prow); a successful run validates the flow.

#### Observability flow for admin and edit users

1. **Admin:** Script logs in as `tester-admin-user` (cluster-admin). Smoke tests run (API + model calls). Then observability tests run: they use the **current** `oc` user, so the test request (`make_test_request`) is made as admin; we then check that Limitador/Prometheus metrics have the expected labels (user, tier, model) for that traffic.
2. **Edit:** Script switches to `tester-edit-user` (edit role) via `oc login --token "$EDIT_TOKEN"`. Smoke tests run as edit user. Then **observability tests run again** with the same shell/env: `oc whoami` is now the edit user, so the observability test's token is the edit user's. The test makes a chat request as edit user and checks that metrics show that user/tier. If only admin traffic were labeled, this would fail.

Each observability run is **per user**: same pytest suite, but the identity (and thus the traffic and expected labels) is whoever is currently logged in. Reports are written to `observability-${USER}.html` / `.xml`, so you get separate reports for admin and edit.
