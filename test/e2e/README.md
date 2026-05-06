# MaaS E2E Testing

```bash
./test/e2e/scripts/prow_run_smoke_test.sh
```

```bash
SKIP_DEPLOYMENT=true ./test/e2e/scripts/prow_run_smoke_test.sh   # cluster already has MaaS
```

```bash
./test/e2e/smoke.sh   # pytest all of tests/
```

Local: `cd test/e2e`, Python venv, `pip install -r requirements.txt`. HTTP tests need `GATEWAY_HOST` / routes as in `tests/test_helper.py`.

`prow_run_smoke_test.sh` runs pytest on `test_api_keys.py`, `test_namespace_scoping.py`, `test_negative_security.py`, `test_subscription.py`, `test_models_endpoint.py`, `test_external_models.py`, `test_tenant.py`, then `./scripts/validate-deployment.sh`. HTML/XML under `ARTIFACT_DIR` when set.

Tenant checks only: `pytest tests/test_tenant.py -v` (requires MaaS + `tenants.maas.opendatahub.io`; namespace `DEPLOYMENT_NAMESPACE`, default `opendatahub`). DSC-driven Tenant lifecycle belongs in the operator repo.
