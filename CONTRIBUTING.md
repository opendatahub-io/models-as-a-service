# Contributing to Models as a Service (MaaS)

Thanks for your interest in contributing. This guide explains how to work with the repo and submit changes.

## Table of contents

- [Contributing to Models as a Service (MaaS)](#contributing-to-models-as-a-service-maas)
  - [Table of contents](#table-of-contents)
  - [Getting started](#getting-started)
  - [Development setup](#development-setup)
  - [Pull request process](#pull-request-process)
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

## Repository layout

| Area | Purpose |
|------|--------|
| `scripts/` | Deployment and install scripts (e.g. `deploy.sh`, `deployment-helpers.sh`, `install-dependencies.sh`) |
| `deployment/` | Kustomize manifests (base, overlays, networking, components) |
| `maas-api/` | Go API service (keys, tokens, tiers); see [maas-api/README.md](maas-api/README.md) |
| `docs/` | User and admin documentation (MkDocs); [online docs](https://opendatahub-io.github.io/models-as-a-service/) |
| `test/` | E2E and billing/smoke tests |
| `.github/workflows/` | CI (build, PR title validation, MaaS API lint/build) |

## CI and checks

- **PR title:** Must follow semantic format (`type: subject`, subject not starting with a capital). Use `draft`/`wip` label to bypass.
- **Kustomize:** Manifests under `deployment/` are validated with `scripts/ci/validate-manifests.sh` (kustomize build).
- **MaaS API (on `maas-api/**` changes):** Lint (golangci-lint), tests (`make test`), and image build.

**Workflows requiring owner approval:** Some CI workflows (e.g. those that run on infrastructure or deploy) require approval from an [OWNERS](OWNERS) approver before they can run. If your PR’s workflows are blocked, ping an owner in the PR to request approval. Before asking, validate that the workflow would succeed by running the same steps locally where possible (for example, the Prow-style E2E script below).

**Run locally before pushing:**

- Kustomize: `./scripts/ci/validate-manifests.sh` (from repo root; requires kustomize 5.7.x).
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
