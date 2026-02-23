# Release Strategy: Stream-Lake-Ocean

Models-as-a-Service (MaaS) uses a **"release anytime"** strategy based on the Stream-Lake-Ocean model. This allows the team to develop freely, contribute stable code to ODH, and deliver production-ready content to RHOAI — all at independent cadences.

## Bodies of Water

The release flow moves code through four stages, each mapped to a branch and environment:

| Body of Water | Branch | Repository | Purpose |
|---|---|---|---|
| **Stream** | `main` | `opendatahub-io/models-as-a-service` | Active development — all feature work lands here |
| **Lake** | `stable` | `opendatahub-io/models-as-a-service` | ODH quality-gated — validated and ready for ODH consumption |
| **RHOAI** | `rhoai` | `opendatahub-io/models-as-a-service` | Created from stable — source for downstream RHOAI builds |
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
