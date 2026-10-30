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
- Dual-listener AuthConfig regression tests are `@pytest.mark.nightly` (pass 3 only when `MAAS_PYTEST_INCLUDE_NIGHTLY=true`; `/group-test` does not run them). They use `E2E_MODEL_REF` / `E2E_MODEL_NAMESPACE`, `AUTHORINO_NAMESPACE` (default `kuadrant-system`), and `E2E_DUAL_LISTENER_WASM_STABILITY_WINDOW` (default 60s).

## Pytest modules

```bash
cd test/e2e && source .venv/bin/activate   # after setup above
pytest tests/<file>.py -v
```

| File | Focus |
|------|--------|
| `test_smoke.py` | Read-only smoke checks: non-streaming and streaming (`text/event-stream`) chat/completions |
| `test_subscription.py` | Subscription / inference flows |
| `test_trlp_rate_grouping.py` | TRLP limits grouped by rate: shared limits, per-subscription and per-user budgets, rate edits, gateway wasm config size guard |
| `test_api_keys.py` | `/v1/api-keys` |
| `test_models_endpoint.py` | `/v1/models` |
| `test_negative_security.py` | Security / negative paths on the **default** gateway (`GATEWAY_HOST`) |
| `test_namespace_scoping.py` | Namespace wiring |
| `test_external_models.py` | External model refs |
| `test_inference_priority.py` | Subscription `inferencePriority` (llm-d flow control): InferenceObjective lifecycle, status, gateway headers; skips without the `inferenceobjectives.llm-d.ai` CRD |
| `test_tenant.py` | `default-tenant` (subscription namespace): Ready/phase, optional payload-processing (gateway namespace), user CRs not owned by Tenant |
| `test_aitenant_lifecycle.py` | `AITenant` bootstrap create/delete; reserved namespace rejection; **serial:** revocation operand recovery when `maas-api-cleanup` SA is removed mid-delete |
| `test_tenant_namespace_discovery.py` | Multi-tenant namespace discovery (S1), webhooks (S6); smoke enables `ENABLE_TENANT_NAMESPACE_DISCOVERY=true` by default |
| `test_gateway_scoped_authpolicy.py` | Gateway-scoped `maas-gateway-auth` (S10 / #912); runs in default CI |
| `test_multi_tenant_integration.py` | Multi-tenant lifecycle and coexistence scenarios; smoke enables tenant namespace discovery by default |
| `test_tenant_model_inference.py` | Per-tenant inference, symmetric cross-gateway API-key isolation, AuthPolicy EnvoyFilter workload selectors (RHOAIENG-99372), and per-tenant gateway negative auth (RHOAIENG-100758; `TestTenantGatewayNegativeSecurity`) |
| `test_multi_tenant_maas_api.py` | Per-tenant `maas-api` infrastructure (default CI); dual HTTP/HTTPS AuthConfig + wasm stability checks are nightly-only |
| `test_tenant_auth_isolation.py` | Tenant-scoped API-key/OIDC isolation (S4); gated by `ENABLE_S4_E2E=true` and tenant API URLs |
| `test_tenant_subscription_isolation.py` | Tenant-scoped subscription listing/selection (S4); gated by `ENABLE_S4_E2E=true` and tenant API URLs |
| `test_tenant_rate_limit_isolation.py` | Tenant-scoped rate-limit isolation (S4); gated by `ENABLE_S4_E2E=true` and tenant API URLs |
| `test_config_tenant.py` | Cluster `Config/default`: anchor present, owner refs on Tenant and `maas-controller` Deployment (skips if Config CRD missing) |
| `test_tenant_external_model_attachment.py` | Multi-tenant AGC/Praxis external models: user-namespace models attached to tenant gateways (per-gateway status, credential copies, real inference), lifecycle cleanup, `NotAllowedByListeners` (P0) |
| `test_tenant_external_model_shared.py` | Multi-tenant AGC/Praxis external models: one model shared across two tenant gateways with isolated authorization and token budgets (P0; the quota model is served by a dedicated in-cluster simulator — service-ca-signed cert trusted through the provider's `tls.caCertificates` config — since token accounting reads `usage.total_tokens`, which the header-echo endpoint never returns) |
| `test_tenant_external_model_conflicts.py` | Multi-tenant AGC/Praxis external models: client-facing `modelName` conflicts and same-named objects in two namespaces (P0, overlay collision regression) |
| `test_tenant_external_model_compat.py` | Multi-tenant AGC/Praxis external models: omitted-`gatewayRefs` compatibility (no-op own-tenant attachment) and legacy IPP coexistence (serial, gated on `TENANT_EXTERNAL_MODEL_LEGACY_IPP_E2E`) |
| `test_tenant_external_model_crossns.py` | Multi-tenant AGC/Praxis external models: ReferenceGrant-gated cross-namespace providers, grant revoke/narrow, credential source selection (P1; gated on `TENANT_EXTERNAL_MODEL_CROSSNS_E2E`) |
| `test_tenant_external_model_default_tenant.py` | 3.5-shaped external models (no `gatewayRefs`, `auth.type: simple`) in a namespace of no tenant (`llm`) served through the default tenant's Gateway by ai-gateway-controller; `gatewayRefs` replace the implicit attachment, deleting the model removes its objects (RHOAIENG-101754; skips without the AGC or when the default tenant is on IPP) |

The shared CI runner and `smoke.sh` execute all tests under `tests/`. Individual modules can still be run directly, for example `pytest tests/test_subscription_list_endpoints.py -v`.

**Skips:** `test_tenant.py` and `test_config_tenant.py` skip the whole module when the needed CRD or object is absent (partial cluster or older bundle). Neither module deletes Config or exercises DSC disable; that stays in operator or manual teardown.

## CI

CI runs `./test/e2e/scripts/prow_run_smoke_test.sh`, a thin orchestrator that sequences standalone scripts:

| Phase | Script | Responsibility |
|-------|--------|----------------|
| 1. Deploy platform | `scripts/deploy-platform.sh` | cert-manager, ODH, deploy.sh, OIDC/Keycloak |
| 2. Deploy models | `scripts/deploy-models.sh` | e2e fixtures (LLMIS, MaaSModelRef, AuthPolicy, Subscription) |
| 3. Setup tokens | `scripts/setup-test-tokens.sh` | admin + regular user tokens (htpasswd / SA fallback) |
| 4. Validate | `scripts/validate-deployment.sh` | pods, CRDs, deployment readiness |
| 5. Pre-test waits | `prow_run_smoke_test.sh` | gateway reachability, auth chain, OIDC readiness |
| 6. Test | `scripts/run_e2e_tests.sh` | pytest: two-pass xdist (parallel + serial) |

`prow_run_smoke_test.sh` contains only orchestration (prereqs, env defaults, phase sequencing, artifact collection). `deploy-platform.sh` and `deploy-models.sh` are runnable standalone (they bootstrap `PROJECT_ROOT` and source helpers if not already loaded). `setup-test-tokens.sh` must be sourced (exports `TOKEN`/`ADMIN_OC_TOKEN` into the parent shell).

`run_e2e_tests.sh` can also be called directly on an existing cluster (env vars must be exported) or via `run-tests-quick.sh` which sets up the env and delegates to the same runner.

Deployment uses **Red Hat Connectivity Link (RHCL)** from the cluster `redhat-operators` catalog (`POLICY_ENGINE=rhcl` by default, channel head unless `RHCL_STARTING_CSV` is set) into **`kuadrant-system`**. Reports are written to `ARTIFACT_DIR` when set.

Multi-tenancy discovery tests run by default in `prow_run_smoke_test.sh`, which sets `ENABLE_TENANT_NAMESPACE_DISCOVERY=true` unless explicitly overridden and patches maas-controller before pytest. If set to `false`, `test_tenant_namespace_discovery.py` and `test_multi_tenant_integration.py` skip. When discovery is enabled, `test_namespace_scoping.py` skips (dormant-mode assumptions).

The dormant-mode regression inside `test_tenant_namespace_discovery.py` mutates controller flags and only runs when `ENABLE_TENANT_DISCOVERY_DORMANT_E2E=true`.

The tenant authentication, subscription, and rate-limit isolation suites are in the smoke list but skip unless `ENABLE_S4_E2E=true` and both tenant API URLs and namespaces are configured. `test_multi_tenant_maas_api.py` (per-tenant maas-api checks) runs in the default suite; dual-listener AuthConfig regression cases are nightly-only. If the AITenant CRD is missing, `require_aitenant_crd()` skips the module when `ENABLE_TENANT_NAMESPACE_DISCOVERY` is unset or false, and fails when discovery is enabled (`true`) because discovery E2E requires the CRD.

The multi-tenant external model suites (`test_tenant_external_model_*.py`) auto-detect support: they skip unless the installed `ExternalModel` CRD carries the additive `gatewayRefs`/`status.gateways` schema, the `ai-gateway-controller` companion is deployed, and per-tenant payload processing is praxis. On AGC builds that report aggregate model status only (no per-gateway `status.gateways` results), attachment status is read from the model's top-level conditions and the shared-model serving tests skip: maas-controller cannot resolve a multi-gateway model's route without per-gateway results, so per-tenant auth/quota cannot be wired for it. The same gap shapes the attachment lifecycle tests: a model attached to two gateways is verified at the route/credential level, and its MaaS stack is built only after a detach leaves a single gateway (`status.httpRouteName` is single-attachment only). The shared model is served by one MaaSModelRef per tenant with `spec.tenantRef` pinned — per-gateway status makes the controller's tenant selection require it for models attached to more than one gateway, so a single tenantRef-less modelRef cannot select an attachment — and each tenant's subscription references its own modelRef. Shared-model requests carry the client-facing `spec.modelName` (deliberately distinct from the modelRefs' names) and the request proofs additionally gate on `status.resolvedModelAlias` carrying that name (see `_require_client_name_alias`): with per-gateway status but without the client-facing identity fix, the per-tenant stacks go green while path-routed requests cannot authorize; unknown-key rejection needs no model-identity resolution and runs with per-gateway status alone. Models whose client-facing `modelName` differs from their CRD name cannot be called through the gateway yet: the auth resolves model identities from MaaSModelRef names and `status.resolvedModelAlias` (published from the ExternalModel CRD name) while the routes match the client-facing name, so the same-named conflict scenario proves the default-named model with a real request and skips the overridden model's request proof until that is fixed. Two sub-suites stay env-gated until their backing stories are packaged:

- `TENANT_EXTERNAL_MODEL_CROSSNS_E2E=true` — P1 cross-namespace provider scenarios (needs AGC cross-namespace provider support and the ReferenceGrant CRD).
- `TENANT_EXTERNAL_MODEL_LEGACY_IPP_E2E=true` — legacy IPP coexistence (serial; needs the MaaS IPP image pin containing the legacy-controller exclusion guard).

Installed AGC/Praxis image and CRD-schema evidence for these suites is written to `ARTIFACTS_DIR/tenant-external-model-evidence-*.json` at session start (completion evidence for the multi-tenant external model scenarios).

External OIDC runs require `EXTERNAL_OIDC=true` and `OIDC_ISSUER_URL`, `OIDC_TOKEN_URL`, `OIDC_CLIENT_ID`, `OIDC_USERNAME`, `OIDC_PASSWORD` per your deploy/test setup.

## Nightly tier (`@pytest.mark.nightly`)

Tests marked `nightly` (for example `test_external_oidc.py` and per-tenant OIDC discovery) are **always excluded from passes 1–2**. They run only in **pass 3** (`-m nightly`) when `MAAS_PYTEST_INCLUDE_NIGHTLY=true` (set automatically when `EXTERNAL_OIDC=true` in prow, or explicitly on the ODH MaaS nightly in shepard). PR `/group-test` does not set that flag, so nightly tests are skipped there.

Local opt-in via the runner:

```bash
MAAS_PYTEST_INCLUDE_NIGHTLY=true EXTERNAL_OIDC=true \
  ./test/e2e/scripts/run_e2e_tests.sh
```

Or target nightly tests directly:

```bash
EXTERNAL_OIDC=true OIDC_ISSUER_URL=... OIDC_TOKEN_URL=... \
  pytest tests/ -m nightly -v
```

To run only the OIDC tests (pass 3) and skip passes 1–2, set `E2E_RUN_ONLY_OIDC_TESTS=true`. The dedicated OIDC CI job uses this because PR `/group-test` already covers passes 1–2. It requires `EXTERNAL_OIDC=true` and cannot be combined with `--serial-only`:

```bash
E2E_RUN_ONLY_OIDC_TESTS=true EXTERNAL_OIDC=true \
  ./test/e2e/scripts/run_e2e_tests.sh
```

Delegated Grid administration needs the strict OIDC contract on the default AITenant. When `EXTERNAL_OIDC=true`, `scripts/deploy.sh` also sets `audience`, `accessTokenPurposeClaim`, and `accessTokenPurpose` from `OIDC_AUDIENCE`, `OIDC_ACCESS_TOKEN_PURPOSE_CLAIM`, and `OIDC_ACCESS_TOKEN_PURPOSE` when they are provided. The test realm's tokens must carry matching `aud` and purpose claims.

## Parallel execution (pytest-xdist)

By default, `run_e2e_tests.sh` runs tests in **two marker-filtered passes** (default: 7 xdist workers on pass 1). `--serial-only` runs pass 2 (`-m "serial and not legacy_ipp"`) only and skips non-serial tests:

1. **Pass 1:** `-m "not serial and not legacy_ipp"` — parallel across files when `E2E_PARALLEL_WORKERS > 1` (`--dist=loadgroup`), or single-worker serial execution when `E2E_PARALLEL_WORKERS=1`
2. **Pass 2:** `-m "serial and not legacy_ipp"` — cluster-wide mutators (single worker): simulator-subscription lifecycle, UNCONFIGURED model auth, TRLP rebuilds, operator scale tests. Skipped when pass 1 fails (saves CI time on red runs).

Legacy maas-controller IPP suites (per-tenant IPP isolation, maas EnvoyFilter chain, ExternalModel reconciler) are marked `legacy_ipp` and skipped by default. Opt in with `pytest -m legacy_ipp` (from `test/e2e/` with env vars exported, or target specific files).

Pass 1 and pass 2 must stay separate: several modules mix `@serial` tests with worker-tenant tests, and module-scoped fixtures reject a single session that selects both.

For single-worker debugging (still two passes, no xdist):

```bash
E2E_PARALLEL_WORKERS=1 ./run-tests-quick.sh
```

Parallel on an existing cluster:

```bash
SKIP_DEPLOYMENT=true ./test/e2e/run-tests-quick.sh
```

| Variable | Default | Description |
|----------|---------|-------------|
| `E2E_PARALLEL_WORKERS` | `7` | Parallel workers for pass 1 (`-m "not serial and not legacy_ipp"`). Pass 2 (`@serial`, not `legacy_ipp`) always runs on one worker. Set to `1` for single-worker pass 1 (no xdist); marker split is unchanged. |
| `E2E_AUTHPOLICY_PHASE_TIMEOUT` | `120` | MaaSAuthPolicy phase wait (`test_helper` default; parallel runner re-exports the same) |
| `E2E_MAAS_SUBSCRIPTION_PHASE_TIMEOUT` | `120` | MaaSSubscription phase wait |
| `E2E_GATEWAY_ENFORCED_TIMEOUT` | `240` (parallel) / `180` (serial) | Kuadrant gateway auth enforced wait |
| `E2E_MULTITENANCY_PHASE_TIMEOUT` | `180` (parallel) / `120` (serial) | Tenant discovery phase wait |
| `E2E_SUBSCRIPTION_INFERENCE_READY_TIMEOUT` | `300` | Subscription discovery + direct/mirrored TRLP enforcement |
| `E2E_SUBSCRIPTION_TRLP_TIMEOUT` | `180` | Mirrored TRLP ready wait on tenant subscriptions |
| `E2E_MODEL_BACKEND_READY_TIMEOUT` | `300` | LLMInferenceService backend ready (tenant model provisioning) |
| `E2E_USE_WORKER_TENANT` | `true` | When `true`, xdist workers bootstrap a dedicated AITenant for Bucket C pilots (`test_subscription.py` first). Set `false` to keep using `models-as-a-service`. |

`@serial` tests run in pass 2; verify the current set with `pytest -m serial tests/ --collect-only -q`.

**Worker tenant (Phase 3 pilot):** each xdist worker (`gw0`, `gw1`, …) bootstraps its own AITenant namespace with baseline `simulator-subscription` / `simulator-access` CRs. Non-serial tests in opted-in modules route `MAAS_SUBSCRIPTION_NAMESPACE`, `GATEWAY_HOST`, and `MAAS_API_BASE_URL` to that tenant for the duration of the test.

Session fixtures suffix resource names with the worker id (for example `e2e-test-inference-key-w0`).
