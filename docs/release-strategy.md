# Release Strategy: Stream-Lake-Ocean

Models-as-a-Service (MaaS) uses a **"release anytime"** strategy based on the Stream-Lake-Ocean model. This allows the team to develop freely, contribute stable code to ODH, and deliver production-ready content to RHOAI — all at independent cadences.

## Bodies of Water

The release flow moves code through four stages, each mapped to a branch and environment:

| Body of Water | Branch | Repository | Purpose |
|---|---|---|---|
| **Stream** | `main` | `opendatahub-io/models-as-a-service` | Active development — all feature work lands here |
| **Lake** | `stable` | `opendatahub-io/models-as-a-service` | Created from main — source for [upstream ODH](https://github.com/opendatahub-io/opendatahub-operator/blob/cd1a94b265255a80a127939fef901f2d630f7bc6/get_all_manifests.sh) builds |
|  | `rhoai` | `opendatahub-io/models-as-a-service` | Created from stable — source for downstream RHOAI builds |
| **Ocean** | `main` | `red-hat-data-services/models-as-a-service` | DevOps-owned — production RHOAI deliverables |


## How Promotion Works

Promotions between branches are automated via GitHub Actions workflows that create PRs. Each promotion is gated by a review before merge.

### Stream to Lake (`main` → `stable`)

- **Schedule:** Every Sunday at midnight UTC (also available on-demand)
- **Workflow:** `promote-main-to-stable.yml`
- A PR is created from `main` to `stable` listing all new commits
- If an open promotion PR already exists, it is updated in place

### Lake to RHOAI (`stable` → `rhoai`)

- **Trigger:** On-demand only (via `workflow_dispatch`)
- **Workflow:** `promote-stable-to-rhoai.yml`
- A PR is created from `stable` to `rhoai` listing all new commits
- If an open promotion PR already exists, it is updated in place
- A cron schedule can be enabled in the workflow once the release strategy matures

### RHOAI to Ocean (`rhoai` → downstream)

The sync from the `rhoai` branch to the downstream `red-hat-data-services/models-as-a-service` repository is managed by the DevOps team and is outside the scope of these workflows.

## Running a Promotion Manually

Both promotion workflows support `workflow_dispatch`, so they can be triggered on-demand from the GitHub Actions UI:

1. Go to **Actions** in the repository
2. Select the desired workflow (**Promote Main to Stable** or **Promote Stable to RHOAI**)
3. Click **Run workflow**

This is useful when a fix needs to be fast-tracked without waiting for the next scheduled run.

## Release Branch Status

### Upstream Pipeline

These show how many commits are pending promotion between upstream branches:

| Promotion | Commits Difference | Last Commit |
| --- | :---: | --- |
| `main` → `stable` | [![main to stable][img-main-to-stable]][cmp-main-to-stable] | ![stable][img-last-stable] |
| `stable` → `rhoai` | [![stable to rhoai][img-stable-to-rhoai]][cmp-stable-to-rhoai] | ![rhoai][img-last-rhoai] |

### Downstream Release Branches

Each row shows how far behind a `downstream` [red-hat-data-services](https://github.com/red-hat-data-services/models-as-a-service) release branch is from the `upstream` [opendatahub-io](https://github.com/opendatahub-io/models-as-a-service) source branches:

| `downstream` branch <br>Last Commit | vs `upstream/main`<br>Commit Difference | vs `upstream/stable`<br>Commit Difference | vs `upstream/rhoai`<br>Commit Difference |
| --- | :---: | :---: | :---: |
| ![main][img-last-ds-main] | [![diff][img-ds-main-vs-main]][cmp-ds-main-vs-main] | [![diff][img-ds-main-vs-stable]][cmp-ds-main-vs-stable] | [![diff][img-ds-main-vs-rhoai]][cmp-ds-main-vs-rhoai] |
| ![rhoai-3.4][img-last-ds-rhoai-3.4] | [![diff][img-ds-rhoai-3.4-vs-main]][cmp-ds-rhoai-3.4-vs-main] | [![diff][img-ds-rhoai-3.4-vs-stable]][cmp-ds-rhoai-3.4-vs-stable] | [![diff][img-ds-rhoai-3.4-vs-rhoai]][cmp-ds-rhoai-3.4-vs-rhoai] |
| ![rhoai-3.4-ea.2][img-last-ds-rhoai-3.4-ea.2] | [![diff][img-ds-rhoai-3.4-ea.2-vs-main]][cmp-ds-rhoai-3.4-ea.2-vs-main] | [![diff][img-ds-rhoai-3.4-ea.2-vs-stable]][cmp-ds-rhoai-3.4-ea.2-vs-stable] | [![diff][img-ds-rhoai-3.4-ea.2-vs-rhoai]][cmp-ds-rhoai-3.4-ea.2-vs-rhoai] |
| ![rhoai-3.4-ea.1][img-last-ds-rhoai-3.4-ea.1] | [![diff][img-ds-rhoai-3.4-ea.1-vs-main]][cmp-ds-rhoai-3.4-ea.1-vs-main] | [![diff][img-ds-rhoai-3.4-ea.1-vs-stable]][cmp-ds-rhoai-3.4-ea.1-vs-stable] | [![diff][img-ds-rhoai-3.4-ea.1-vs-rhoai]][cmp-ds-rhoai-3.4-ea.1-vs-rhoai] |
| ![rhoai-3.3][img-last-ds-rhoai-3.3] | [![diff][img-ds-rhoai-3.3-vs-main]][cmp-ds-rhoai-3.3-vs-main] | [![diff][img-ds-rhoai-3.3-vs-stable]][cmp-ds-rhoai-3.3-vs-stable] | [![diff][img-ds-rhoai-3.3-vs-rhoai]][cmp-ds-rhoai-3.3-vs-rhoai] |
| ![rhoai-3.2][img-last-ds-rhoai-3.2] | [![diff][img-ds-rhoai-3.2-vs-main]][cmp-ds-rhoai-3.2-vs-main] | [![diff][img-ds-rhoai-3.2-vs-stable]][cmp-ds-rhoai-3.2-vs-stable] | [![diff][img-ds-rhoai-3.2-vs-rhoai]][cmp-ds-rhoai-3.2-vs-rhoai] |
| ![rhoai-3.0][img-last-ds-rhoai-3.0] | [![diff][img-ds-rhoai-3.0-vs-main]][cmp-ds-rhoai-3.0-vs-main] | [![diff][img-ds-rhoai-3.0-vs-stable]][cmp-ds-rhoai-3.0-vs-stable] | [![diff][img-ds-rhoai-3.0-vs-rhoai]][cmp-ds-rhoai-3.0-vs-rhoai] |

> **Note:** When new downstream release branches are created (e.g. `rhoai-3.5`), add a corresponding row to the table above.

<!-- Upstream badge images -->
[img-main-to-stable]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=stable&head=main&label=%20&cacheSeconds=3600
[img-stable-to-rhoai]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=rhoai&head=stable&label=%20&cacheSeconds=3600
[img-last-stable]: https://img.shields.io/github/last-commit/opendatahub-io/models-as-a-service/stable?label=stable&cacheSeconds=3600
[img-last-rhoai]: https://img.shields.io/github/last-commit/opendatahub-io/models-as-a-service/rhoai?label=rhoai&cacheSeconds=3600

<!-- Upstream compare links -->
[cmp-main-to-stable]: https://github.com/opendatahub-io/models-as-a-service/compare/stable...main
[cmp-stable-to-rhoai]: https://github.com/opendatahub-io/models-as-a-service/compare/rhoai...stable

<!-- Downstream last-commit badge images -->
[img-last-ds-main]: https://img.shields.io/github/last-commit/red-hat-data-services/models-as-a-service/main?label=main&cacheSeconds=3600
[img-last-ds-rhoai-3.4]: https://img.shields.io/github/last-commit/red-hat-data-services/models-as-a-service/rhoai-3.4?label=rhoai-3.4&cacheSeconds=3600
[img-last-ds-rhoai-3.4-ea.2]: https://img.shields.io/github/last-commit/red-hat-data-services/models-as-a-service/rhoai-3.4-ea.2?label=rhoai-3.4-ea.2&cacheSeconds=3600
[img-last-ds-rhoai-3.4-ea.1]: https://img.shields.io/github/last-commit/red-hat-data-services/models-as-a-service/rhoai-3.4-ea.1?label=rhoai-3.4-ea.1&cacheSeconds=3600
[img-last-ds-rhoai-3.3]: https://img.shields.io/github/last-commit/red-hat-data-services/models-as-a-service/rhoai-3.3?label=rhoai-3.3&cacheSeconds=3600
[img-last-ds-rhoai-3.2]: https://img.shields.io/github/last-commit/red-hat-data-services/models-as-a-service/rhoai-3.2?label=rhoai-3.2&cacheSeconds=3600
[img-last-ds-rhoai-3.0]: https://img.shields.io/github/last-commit/red-hat-data-services/models-as-a-service/rhoai-3.0?label=rhoai-3.0&cacheSeconds=3600

<!-- Downstream commit-difference badge images: ds-{branch}-vs-{upstream} -->
[img-ds-main-vs-main]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:main&head=main&label=%20&cacheSeconds=3600
[img-ds-main-vs-stable]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:main&head=stable&label=%20&cacheSeconds=3600
[img-ds-main-vs-rhoai]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:main&head=rhoai&label=%20&cacheSeconds=3600
[img-ds-rhoai-3.4-vs-main]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.4&head=main&label=%20&cacheSeconds=3600
[img-ds-rhoai-3.4-vs-stable]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.4&head=stable&label=%20&cacheSeconds=3600
[img-ds-rhoai-3.4-vs-rhoai]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.4&head=rhoai&label=%20&cacheSeconds=3600
[img-ds-rhoai-3.4-ea.2-vs-main]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.4-ea.2&head=main&label=%20&cacheSeconds=3600
[img-ds-rhoai-3.4-ea.2-vs-stable]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.4-ea.2&head=stable&label=%20&cacheSeconds=3600
[img-ds-rhoai-3.4-ea.2-vs-rhoai]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.4-ea.2&head=rhoai&label=%20&cacheSeconds=3600
[img-ds-rhoai-3.4-ea.1-vs-main]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.4-ea.1&head=main&label=%20&cacheSeconds=3600
[img-ds-rhoai-3.4-ea.1-vs-stable]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.4-ea.1&head=stable&label=%20&cacheSeconds=3600
[img-ds-rhoai-3.4-ea.1-vs-rhoai]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.4-ea.1&head=rhoai&label=%20&cacheSeconds=3600
[img-ds-rhoai-3.3-vs-main]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.3&head=main&label=%20&cacheSeconds=3600
[img-ds-rhoai-3.3-vs-stable]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.3&head=stable&label=%20&cacheSeconds=3600
[img-ds-rhoai-3.3-vs-rhoai]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.3&head=rhoai&label=%20&cacheSeconds=3600
[img-ds-rhoai-3.2-vs-main]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.2&head=main&label=%20&cacheSeconds=3600
[img-ds-rhoai-3.2-vs-stable]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.2&head=stable&label=%20&cacheSeconds=3600
[img-ds-rhoai-3.2-vs-rhoai]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.2&head=rhoai&label=%20&cacheSeconds=3600
[img-ds-rhoai-3.0-vs-main]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.0&head=main&label=%20&cacheSeconds=3600
[img-ds-rhoai-3.0-vs-stable]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.0&head=stable&label=%20&cacheSeconds=3600
[img-ds-rhoai-3.0-vs-rhoai]: https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.0&head=rhoai&label=%20&cacheSeconds=3600

<!-- Downstream compare links: ds-{branch}-vs-{upstream} -->
[cmp-ds-main-vs-main]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:main...main
[cmp-ds-main-vs-stable]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:main...stable
[cmp-ds-main-vs-rhoai]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:main...rhoai
[cmp-ds-rhoai-3.4-vs-main]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:rhoai-3.4...main
[cmp-ds-rhoai-3.4-vs-stable]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:rhoai-3.4...stable
[cmp-ds-rhoai-3.4-vs-rhoai]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:rhoai-3.4...rhoai
[cmp-ds-rhoai-3.4-ea.2-vs-main]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:rhoai-3.4-ea.2...main
[cmp-ds-rhoai-3.4-ea.2-vs-stable]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:rhoai-3.4-ea.2...stable
[cmp-ds-rhoai-3.4-ea.2-vs-rhoai]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:rhoai-3.4-ea.2...rhoai
[cmp-ds-rhoai-3.4-ea.1-vs-main]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:rhoai-3.4-ea.1...main
[cmp-ds-rhoai-3.4-ea.1-vs-stable]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:rhoai-3.4-ea.1...stable
[cmp-ds-rhoai-3.4-ea.1-vs-rhoai]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:rhoai-3.4-ea.1...rhoai
[cmp-ds-rhoai-3.3-vs-main]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:rhoai-3.3...main
[cmp-ds-rhoai-3.3-vs-stable]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:rhoai-3.3...stable
[cmp-ds-rhoai-3.3-vs-rhoai]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:rhoai-3.3...rhoai
[cmp-ds-rhoai-3.2-vs-main]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:rhoai-3.2...main
[cmp-ds-rhoai-3.2-vs-stable]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:rhoai-3.2...stable
[cmp-ds-rhoai-3.2-vs-rhoai]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:rhoai-3.2...rhoai
[cmp-ds-rhoai-3.0-vs-main]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:rhoai-3.0...main
[cmp-ds-rhoai-3.0-vs-stable]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:rhoai-3.0...stable
[cmp-ds-rhoai-3.0-vs-rhoai]: https://github.com/opendatahub-io/models-as-a-service/compare/red-hat-data-services:rhoai-3.0...rhoai
