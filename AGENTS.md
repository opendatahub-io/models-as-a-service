---
alwaysApply: true
---

# Models as a Service (MaaS)

Kubernetes-native platform for managing inference model endpoints, built with Go, controller-runtime, and Gateway API.

## Repository structure

Two independent Go modules — **no root `go.mod` or root `Makefile`**. Always `cd` into the correct subproject before running Go tooling.

| Directory | What it is |
|-----------|-----------|
| `maas-controller/` | Kubernetes controller (kubebuilder, controller-runtime) |
| `maas-api/` | HTTP API service (keys, tokens, subscriptions) |
| `deployment/` | Kustomize manifests (base, overlays, components) |
| `docs/` | MkDocs user/admin documentation |
| `test/e2e/` | pytest-based E2E tests |
| `scripts/` | Deploy and CI helper scripts |

## CRDs

API group: `maas.opendatahub.io/v1alpha1`

Types in `maas-controller/api/maas/v1alpha1/`: **Tenant**, **MaaSModelRef**, **MaaSAuthPolicy**, **MaaSSubscription**, **ExternalModel**.

## Build and test commands

### maas-controller (from repo root)

```bash
make -C maas-controller generate manifests   # after changing api/ types or RBAC markers
make -C maas-controller verify-codegen       # verify generated code is in sync
make -C maas-controller lint                 # golangci-lint
make -C maas-controller test                 # unit tests with -race
```

### maas-api (from maas-api/)

```bash
make lint
make test
```

### Kustomize manifests (from repo root)

```bash
./scripts/ci/validate-manifests.sh           # requires kustomize 5.7.x
```

## Codegen rule

If you change any file under `maas-controller/api/` or modify `//+kubebuilder:rbac:` markers anywhere in `maas-controller/`, you **must** run `make -C maas-controller generate manifests` and include the generated files in your commit. CI will reject PRs with stale generated code.

## Kustomize / deployment

- `deployment/base/maas-controller/default` — operator bootstrap (CRDs, RBAC, Deployment)
- `maas-api/deploy/overlays/odh` — tenant overlay rendered at runtime inside the controller container
- `deployment/overlays/odh/params.env` — build-time defaults; runtime values come from Tenant CR via `CustomizeParams`
- The controller image embeds `maas-api/deploy`, `deployment/base/maas-api`, components, and policies via Dockerfile COPY

When editing Kustomize files, always run `./scripts/ci/validate-manifests.sh` before committing.

## PR titles

Semantic format required: `type: subject` (lowercase subject).

Allowed types: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`.

## Testing conventions

- Go tests use `testing` + `gomega` or `testify` — match the style of the package you're editing.
- E2E tests are pytest under `test/e2e/tests/`.
- New functionality must include tests.

## Documentation policy

- **Search before writing.** Before creating or updating any doc, search `docs/content/` and existing markdown files for overlapping content. If the topic is already covered, update that file — do not create a new one.
- **One source of truth.** Never duplicate information across files. Link to the canonical location instead of repeating content.
- **Update, don't duplicate.** If a feature changes behavior already documented somewhere, find and update that section in place.
- **No shadow docs.** Do not create parallel docs (e.g., a new `docs/content/foo.md` when `docs/content/advanced-administration/foo.md` already exists, or a root-level `*.md` that restates what's in `docs/`).

## Things to never do

- Do not create a root-level `go.mod` or `Makefile`.
- Do not guess image tags, registry paths, or namespace names — ask or check `params.env`.
- Do not edit `zz_generated.deepcopy.go` or CRD YAML by hand — always regenerate.
- Do not create a new doc file without first confirming no existing file covers the same topic.

