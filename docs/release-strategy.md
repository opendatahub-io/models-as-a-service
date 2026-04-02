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
| `main` → `stable` | ![main to stable](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=stable&head=main&label=%20) | ![GitHub last commit (stable)](https://img.shields.io/github/last-commit/opendatahub-io/models-as-a-service/stable?label=stable) |
| `stable` → `rhoai` | ![stable to rhoai](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=rhoai&head=stable&label=%20) | ![GitHub last commit (rhaoi)](https://img.shields.io/github/last-commit/opendatahub-io/models-as-a-service/rhoai?label=rhoai) |

### Downstream Release Branches

Each row shows how far behind a `downstream` [red-hat-data-services](https://github.com/red-hat-data-services/models-as-a-service) release branch is from the `upstream` [opendatahub-io](https://github.com/opendatahub-io/models-as-a-service) source branches:

| `downstream` branch <br>Last Commit | vs `upstream/main`<br>Commit Difference | vs `upstream/stable`<br>Commit Difference | vs `upstream/rhoai`<br>Commit Difference |
| --- | :---: | :---: | :---: |
| ![main](https://img.shields.io/github/last-commit/red-hat-data-services/models-as-a-service/main?label=main) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:main&head=main&label=%20) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:main&head=stable&label=%20) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:main&head=rhoai&label=%20) |
| ![rhoai-3.4](https://img.shields.io/github/last-commit/red-hat-data-services/models-as-a-service/rhoai-3.4?label=rhoai-3.4) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.4&head=main&label=%20) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.4&head=stable&label=%20) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.4&head=rhoai&label=%20) |
| ![rhoai-3.4-ea.2](https://img.shields.io/github/last-commit/red-hat-data-services/models-as-a-service/rhoai-3.4-ea.2?label=rhoai-3.4-ea.2) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.4-ea.2&head=main&label=%20) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.4-ea.2&head=stable&label=%20) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.4-ea.2&head=rhoai&label=%20) |
| ![rhoai-3.4-ea.1](https://img.shields.io/github/last-commit/red-hat-data-services/models-as-a-service/rhoai-3.4-ea.1?label=rhoai-3.4-ea.1) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.4-ea.1&head=main&label=%20) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.4-ea.1&head=stable&label=%20) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.4-ea.1&head=rhoai&label=%20) |
| ![rhoai-3.3](https://img.shields.io/github/last-commit/red-hat-data-services/models-as-a-service/rhoai-3.3?label=rhoai-3.3) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.3&head=main&label=%20) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.3&head=stable&label=%20) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.3&head=rhoai&label=%20) |
| ![rhoai-3.2](https://img.shields.io/github/last-commit/red-hat-data-services/models-as-a-service/rhoai-3.2?label=rhoai-3.2) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.2&head=main&label=%20) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.2&head=stable&label=%20) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.2&head=rhoai&label=%20) |
| ![rhoai-3.0](https://img.shields.io/github/last-commit/red-hat-data-services/models-as-a-service/rhoai-3.0?label=rhoai-3.0) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.0&head=main&label=%20) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.0&head=stable&label=%20) | ![badge](https://img.shields.io/github/commits-difference/opendatahub-io/models-as-a-service?base=red-hat-data-services:rhoai-3.0&head=rhoai&label=%20) |

> **Note:** When new downstream release branches are created (e.g. `rhoai-3.5`), add a corresponding row to the table above.
