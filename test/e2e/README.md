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

CI runs `./test/e2e/scripts/prow_run_smoke_test.sh`: pytest on the default smoke modules listed above (including `test_aitenant_lifecycle.py`, `test_tenant_namespace_discovery.py`, `test_gateway_scoped_authpolicy.py`, `test_multi_tenant_integration.py`, and the gated S24/S4 modules), then deployment validation; reports under `ARTIFACT_DIR` when set.

Multi-tenancy discovery tests run by default in `prow_run_smoke_test.sh`, which sets `ENABLE_TENANT_NAMESPACE_DISCOVERY=true` unless explicitly overridden and patches maas-controller before pytest. If set to `false`, `test_tenant_namespace_discovery.py` and `test_multi_tenant_integration.py` skip. When discovery is enabled, `test_namespace_scoping.py` skips (dormant-mode assumptions).

The dormant-mode regression inside `test_tenant_namespace_discovery.py` mutates controller flags and only runs when `ENABLE_TENANT_DISCOVERY_DORMANT_E2E=true`.

The S24/S4 suites are in the smoke list but intentionally skip until their backing implementation is present in the deployed build. Enable them with `ENABLE_S24_E2E=true` or `ENABLE_S4_E2E=true` plus `MAAS_API_BASE_URL_TENANT_A`, `MAAS_API_BASE_URL_TENANT_B`, `TENANT_A_NAMESPACE`, and `TENANT_B_NAMESPACE`.

External OIDC runs require `EXTERNAL_OIDC=true` and `OIDC_ISSUER_URL`, `OIDC_TOKEN_URL`, `OIDC_CLIENT_ID`, `OIDC_USERNAME`, `OIDC_PASSWORD` per your deploy/test setup.
