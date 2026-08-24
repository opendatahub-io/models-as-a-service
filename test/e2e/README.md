# MaaS E2E Testing

**Ownership:** Deep MaaS behavior is tested here (controller, CRDs, gateway policies, maas-api). DSC toggling MaaS, `ModelsAsServiceReady`, Tenant presence/absence vs DSC, and thin operator smoke belong in the operator repo.

## Quick start

Full deploy and pytest (same path CI uses):

```bash
./test/e2e/scripts/prow_run_smoke_test.sh
```

Existing cluster (skip deploy):

```bash
SKIP_DEPLOYMENT=true ./test/e2e/scripts/prow_run_smoke_test.sh
```

Parallel pytest on an existing cluster (default 7 workers):

```bash
SKIP_DEPLOYMENT=true ./test/e2e/run-tests-quick.sh
```

Smoke helper only:

```bash
./test/e2e/smoke.sh
```

## Local prerequisites

- OpenShift access (`oc` logged in)
- From repo root: `cd test/e2e`, create venv, `pip install -r requirements.txt`
- Most HTTP tests need `GATEWAY_HOST` (and often routes/API reachable). Full env list: `tests/test_helper.py` docstring.

## Pytest modules

```bash
cd test/e2e && source .venv/bin/activate   # after setup above
pytest tests/<file>.py -v
```

| File | Focus |
|------|--------|
| `test_subscription.py` | Subscription / inference flows |
| `test_api_keys.py` | `/v1/api-keys` |
| `test_models_endpoint.py` | `/v1/models` |
| `test_negative_security.py` | Security / negative paths |
| `test_namespace_scoping.py` | Namespace wiring |
| `test_external_models.py` | External model refs |
| `test_tenant.py` | `default-tenant` (subscription namespace): Ready/phase, optional payload-processing (gateway namespace), user CRs not owned by Tenant |
| `test_aitenant_lifecycle.py` | `AITenant` bootstrap create/delete; reserved namespace rejection |
| `test_tenant_namespace_discovery.py` | Multi-tenant namespace discovery (S1), webhooks (S6); smoke enables `ENABLE_TENANT_NAMESPACE_DISCOVERY=true` by default |
| `test_gateway_scoped_authpolicy.py` | Gateway-scoped `maas-gateway-auth` (S10 / #912); runs in default CI |
| `test_multi_tenant_integration.py` | Multi-tenant lifecycle and coexistence scenarios; smoke enables tenant namespace discovery by default |
| `test_multi_tenant_maas_api.py` | Per-tenant `maas-api` infrastructure (S24); gated by `ENABLE_S24_E2E=true` |
| `test_tenant_auth_isolation.py` | Tenant-scoped API-key/OIDC isolation (S4); gated by `ENABLE_S4_E2E=true` and tenant API URLs |
| `test_tenant_subscription_isolation.py` | Tenant-scoped subscription listing/selection (S4); gated by `ENABLE_S4_E2E=true` and tenant API URLs |
| `test_tenant_rate_limit_isolation.py` | Tenant-scoped rate-limit isolation (S4); gated by `ENABLE_S4_E2E=true` and tenant API URLs |
| `test_config_tenant.py` | Cluster `Config/default`: anchor present, owner refs on Tenant and `maas-controller` Deployment (skips if Config CRD missing) |

Modules outside the explicit smoke list (for example `test_subscription_list_endpoints.py`) can be run directly or via `smoke.sh`, which executes all tests under `tests/`.

**Skips:** `test_tenant.py` and `test_config_tenant.py` skip the whole module when the needed CRD or object is absent (partial cluster or older bundle). Neither module deletes Config or exercises DSC disable; that stays in operator or manual teardown.

## CI

CI runs `./test/e2e/scripts/prow_run_smoke_test.sh`, which follows a **three-phase model**:

| Phase | Script | Responsibility |
|-------|--------|----------------|
| 1. Deploy | `prow_run_smoke_test.sh` | Install RHCL, deploy MaaS, configure gateway |
| 2. Validate | `prow_run_smoke_test.sh` | Wait for gateway reachability, verify auth chain |
| 3. Test | `scripts/run_e2e_tests.sh` | pytest only: two-pass xdist, artifact paths |

`prow_run_smoke_test.sh` is the thin orchestrator — it delegates pytest execution to `run_e2e_tests.sh`. Test-only PRs touch the runner and test files, not deploy helpers.

`run_e2e_tests.sh` can also be called directly on an existing cluster (env vars must be exported) or via `run-tests-quick.sh` which sets up the env and delegates to the same runner.

Deployment uses **Red Hat Connectivity Link (RHCL)** from the cluster `redhat-operators` catalog (`POLICY_ENGINE=rhcl` by default, channel head unless `RHCL_STARTING_CSV` is set) into **`kuadrant-system`**. Reports are written to `ARTIFACT_DIR` when set.

Multi-tenancy discovery tests run by default in `prow_run_smoke_test.sh`, which sets `ENABLE_TENANT_NAMESPACE_DISCOVERY=true` unless explicitly overridden and patches maas-controller before pytest. If set to `false`, `test_tenant_namespace_discovery.py` and `test_multi_tenant_integration.py` skip. When discovery is enabled, `test_namespace_scoping.py` skips (dormant-mode assumptions).

The dormant-mode regression inside `test_tenant_namespace_discovery.py` mutates controller flags and only runs when `ENABLE_TENANT_DISCOVERY_DORMANT_E2E=true`.

The S24/S4 suites are in the smoke list but intentionally skip until their backing implementation is present in the deployed build. Enable them with `ENABLE_S24_E2E=true` or `ENABLE_S4_E2E=true` plus `MAAS_API_BASE_URL_TENANT_A`, `MAAS_API_BASE_URL_TENANT_B`, `TENANT_A_NAMESPACE`, and `TENANT_B_NAMESPACE`.

External OIDC runs require `EXTERNAL_OIDC=true` and `OIDC_ISSUER_URL`, `OIDC_TOKEN_URL`, `OIDC_CLIENT_ID`, `OIDC_USERNAME`, `OIDC_PASSWORD` per your deploy/test setup.

## Parallel execution (pytest-xdist)

By default, `run_e2e_tests.sh` runs tests in **two passes** when `E2E_PARALLEL_WORKERS > 1` (default: 7):

1. **Pass 1:** `-m "not serial"` — parallel across files (`--dist=loadgroup`)
2. **Pass 2:** `-m serial` — cluster-wide mutators (single worker): simulator-subscription lifecycle, UNCONFIGURED model auth, TRLP rebuilds, operator scale tests

For fully serial debugging:

```bash
E2E_PARALLEL_WORKERS=1 ./run-tests-quick.sh
```

Parallel on an existing cluster:

```bash
SKIP_DEPLOYMENT=true ./test/e2e/run-tests-quick.sh
```

| Variable | Default | Description |
|----------|---------|-------------|
| `E2E_PARALLEL_WORKERS` | `7` | Parallel workers for pass 1. Pass 2 (`@serial`) always runs on one worker. Set to `1` to run everything serially in one pass. |
| `E2E_AUTHPOLICY_PHASE_TIMEOUT` | `120` (parallel) / `60` (serial) | MaaSAuthPolicy phase wait |
| `E2E_GATEWAY_ENFORCED_TIMEOUT` | `240` (parallel) / `180` (serial) | Kuadrant gateway auth enforced wait |
| `E2E_MULTITENANCY_PHASE_TIMEOUT` | `180` (parallel) / `120` (serial) | Tenant discovery phase wait |
| `E2E_USE_WORKER_TENANT` | `true` | When `true`, xdist workers bootstrap a dedicated AITenant for Bucket C pilots (`test_subscription.py` first). Set `false` to keep using `models-as-a-service`. |

**19 `@serial` tests** (pass 2): verify with `pytest -m serial tests/ --collect-only -q`.

**Worker tenant (Phase 3 pilot):** each xdist worker (`gw0`, `gw1`, …) bootstraps its own AITenant namespace with baseline `simulator-subscription` / `simulator-access` CRs. Non-serial tests in opted-in modules route `MAAS_SUBSCRIPTION_NAMESPACE`, `GATEWAY_HOST`, and `MAAS_API_BASE_URL` to that tenant for the duration of the test.

Session fixtures suffix resource names with the worker id (for example `e2e-test-inference-key-w0`).
