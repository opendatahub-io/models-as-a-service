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
| `test_tenant.py` | Tenant singleton: `maas-controller` bootstraps `default-tenant`; Ready/phase; optional payload-processing; CRs not owned by Tenant; DSC deletion → operator CI |

Other modules (for example `test_external_oidc.py`, `test_subscription_list_endpoints.py`) are not in the default Prow pytest list—run them explicitly or use `smoke.sh`, which executes all tests under `tests/`.

**`test_tenant.py` skips:** If the Tenant CRD or `default-tenant` is missing, the whole module skips. That is **transitional** for partial or legacy clusters. The target E2E shape is **install `maas-controller` (and its CRDs)** in CI; the controller then creates the Tenant automatically—so skips should become rare and a missing Tenant after a proper controller install should be treated as a regression.

## CI

CI runs `./test/e2e/scripts/prow_run_smoke_test.sh`: pytest on `test_api_keys.py`, `test_namespace_scoping.py`, `test_negative_security.py`, `test_subscription.py`, `test_models_endpoint.py`, `test_external_models.py`, `test_tenant.py`, then deployment validation; reports under `ARTIFACT_DIR` when set.

External OIDC runs require `EXTERNAL_OIDC=true` and `OIDC_ISSUER_URL`, `OIDC_TOKEN_URL`, `OIDC_CLIENT_ID`, `OIDC_USERNAME`, `OIDC_PASSWORD` per your deploy/test setup.
