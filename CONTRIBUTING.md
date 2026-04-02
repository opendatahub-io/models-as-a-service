# Contributing to Models as a Service (MaaS)

Thanks for your interest in contributing. This guide explains how to work with the repo and submit changes.

## Table of contents

- [Contributing to Models as a Service (MaaS)](#contributing-to-models-as-a-service-maas)
  - [Table of contents](#table-of-contents)
  - [Getting started](#getting-started)
  - [Development setup](#development-setup)
  - [Pull request process](#pull-request-process)
  - [Release strategy](#release-strategy)
  - [Repository layout](#repository-layout)
  - [CI and checks](#ci-and-checks)
  - [Testing](#testing)
  - [Documentation](#documentation)
  - [Getting help](#getting-help)

## Getting started

1. **Fork** the repository on GitHub.
2. **Clone** your fork and add the upstream remote:
   ```bash
   git clone git@github.com:YOUR_USERNAME/models-as-a-service.git
   cd models-as-a-service
   git remote add upstream https://github.com/opendatahub-io/models-as-a-service.git
   ```
3. **Create a branch** from `main` for your work:
   ```bash
   git fetch upstream
   git checkout -b your-feature upstream/main
   ```

## Development setup

- **Prerequisites:** OpenShift cluster (4.19.9+), `kubectl`/`oc`, and for full deployment see [README](README.md#-prerequisites).
- **Deploy locally:** Use the unified script as in the [Quick start](README.md#-quick-start), e.g. `./scripts/deploy.sh --operator-type odh`.
- **MaaS API (Go):** See [maas-api/README.md](maas-api/README.md) for Go toolchain, `make` targets, and local API development.

## Pull request process

1. **Push** your branch to your fork and open a **pull request** against `main`.
2. **Use semantic PR titles** so CI can accept the PR. Format: `type: subject` (subject not starting with a capital).
   - Allowed **types:** `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`.
   - Examples: `feat: add TLS option for deploy script`, `fix: correct sourceNamespace for Kuadrant subscription`, `docs: update quickstart`.
   - Draft/WIP PRs can use the `draft` or `wip` label to skip title validation.
3. **Keep changes focused** and ensure CI passes (see below).
4. **Address review feedback** from [OWNERS](OWNERS); maintainers will approve and merge when ready.

## Release strategy

This project follows a **Stream-Lake-Ocean** release model. Code flows from active development (`main`) through quality-gated branches (`stable`, `rhoai`) to the downstream RHOAI repository. See the full details in [docs/release-strategy.md](docs/release-strategy.md).

## Repository layout

| Area | Purpose |
|------|--------|
| `scripts/` | Deployment and install scripts (e.g. `deploy.sh`, `deployment-helpers.sh`, `install-dependencies.sh`) |
| `deployment/` | Kustomize manifests (base, overlays, networking, components) |
| `maas-api/` | Go API service (keys, tokens, subscriptions); see [maas-api/README.md](maas-api/README.md) |
| `maas-controller/` | Kubernetes controller for MaaS CRDs; see [maas-controller/README.md](maas-controller/README.md) |
| `docs/` | User and admin documentation (MkDocs); [online docs](https://opendatahub-io.github.io/models-as-a-service/) |
| `test/` | E2E and billing/smoke tests |
| `.github/workflows/` | GitHub Actions CI (lint, build, PR title validation, docs) |
| `.tekton/` | Konflux/Tekton pipeline definitions for container image builds |

## CI and checks

This project uses two CI systems: **Konflux** (Tekton-based) for container image builds and integration testing, and **GitHub Actions** for linting, unit tests, and documentation.

### Konflux / Tekton pipelines

Konflux builds multi-arch container images (x86_64, arm64, ppc64le, s390x) for both `maas-api` and `maas-controller` on every PR and push to `main`. Pipeline definitions live in `.tekton/` and reference a shared pipeline from [odh-konflux-central](https://github.com/opendatahub-io/odh-konflux-central) (`pipeline/multi-arch-container-build.yaml`).

| Pipeline | Trigger | Output image |
|----------|---------|--------------|
| `odh-maas-api-on-pull-request` | PR to `main` | `quay.io/opendatahub/maas-api:odh-pr` |
| `odh-maas-api-on-push` | Push to `main` | `quay.io/opendatahub/maas-api:odh-stable` |
| `odh-maas-controller-on-pull-request` | PR to `main` | `quay.io/opendatahub/maas-controller:odh-pr` |
| `odh-maas-controller-on-push` | Push to `main` | `quay.io/opendatahub/maas-controller:odh-stable` |

**Integration tests (e2e):** When a PR build completes, Konflux runs an integration test that provisions an ephemeral OpenShift cluster (HyperShift on AWS), deploys the ODH stack with the newly built images, and runs `test/e2e/scripts/prow_run_smoke_test.sh`. This is defined in `odh-konflux-central` under `integration-tests/models-as-a-service/`.

**Docs-only skip:** PRs that only touch documentation files (`docs/**` or `**/*.md`) skip the Konflux build pipelines and integration tests entirely. This is controlled via a CEL expression in the `.tekton/` pipeline definitions.

### GitHub Actions

| Workflow | Trigger | Path filter | What it checks |
|----------|---------|-------------|----------------|
| PR Title Validation | Every PR | None | Semantic PR title format (`type: subject`) |
| MaaS API | PR + push to `main` | `maas-api/**` (PR only) | golangci-lint, unit tests, image build |
| Build | PR + push to `main` | `maas-controller/api/**`, `deployment/**`, etc. (PR only) | Kustomize manifest validation, CRD codegen verification |
| Docs | PR + push to `main` | `docs/**`, `**/*.md` | Link validation, mkdocs build, GitHub Pages deploy |

- **PR title:** Must follow semantic format (`type: subject`, subject not starting with a capital). Use `draft`/`wip` label to bypass.
- **Kustomize:** Manifests under `deployment/` are validated with `scripts/ci/validate-manifests.sh` (kustomize build).
- **MaaS Controller codegen:** CI verifies that generated deepcopy code (`maas-controller/api/maas/v1alpha1/zz_generated.deepcopy.go`), CRD manifests (`deployment/base/maas-controller/crd/bases/`), and RBAC manifests (`deployment/base/maas-controller/rbac/clusterrole.yaml`) are in sync with the source. If you change any file under `maas-controller/api/` or modify `//+kubebuilder:rbac:` markers in `maas-controller/` Go source files (for example `cmd/manager/main.go` or `pkg/**`), run `make -C maas-controller generate manifests` and commit the results before pushing. The check fails when uncommitted generated changes are detected.
- **MaaS API (on `maas-api/**` changes):** Lint (golangci-lint), tests (`make test`), and image build.

**Workflows requiring owner approval:** Some CI workflows (e.g. those that run on infrastructure or deploy) require approval from an [OWNERS](OWNERS) approver before they can run. If your PR’s workflows are blocked, ping an owner in the PR to request approval. Before asking, validate that the workflow would succeed by running the same steps locally where possible (for example, the Prow-style E2E script below).

**Run locally before pushing:**

- Kustomize: `./scripts/ci/validate-manifests.sh` (from repo root; requires kustomize 5.7.x).
- MaaS Controller codegen: from the repo root, run `make -C maas-controller verify-codegen` (automatically installs the correct `controller-gen` version to `bin/controller-gen`).
- MaaS API: from `maas-api/`, run `make lint` and `make test`.
- Full E2E (Prow-style): `./test/e2e/scripts/prow_run_smoke_test.sh` (from repo root; requires OpenShift cluster and cluster-admin).

## Testing

**New functionality should include tests.** Add or extend tests to cover your changes—for example, unit tests in `maas-api/` or `test/`, or E2E coverage where appropriate. This section will be expanded with more detailed testing guidelines.

## Documentation

- **Source:** [docs/content/](docs/content/) (MkDocs structure; see [docs/README.md](docs/README.md)).
- **Build/docs CI:** See `.github/workflows/docs.yml`.
- When changing behavior or flags, update the [deployment guide](docs/content/quickstart.md) or the [README](README.md) as appropriate.

## Getting help

- **Open an issue** on GitHub for bugs or feature ideas.
- **Deployment issues:** See the [deployment guide](docs/content/quickstart.md) and [README](README.md).
- **Reviewers/approvers:** Listed in [OWNERS](OWNERS).
