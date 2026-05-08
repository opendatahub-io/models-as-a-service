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

## Release Notes

Release notes in `docs/content/release-notes/index.md` summarize user-visible changes for each MaaS version. Keep them concise and focused on **what changed** and **why it matters**—link to detailed docs for **how** to migrate or configure.

### What to Capture

**Always include:**
- Breaking changes with migration links
- New features and user-visible capabilities
- Critical bug fixes (security, data loss, production blockers)
- Deprecations with timeline
- Known limitations (link to detailed docs, don't duplicate)

**Defer to other docs:**
- Migration procedures → link to `docs/content/migration/`
- Configuration details → link to `docs/content/configuration-and-management/`
- Troubleshooting → link to admin/ops pages

### Process for New Releases

1. **Generate commit log:** `git log --oneline --no-merges vPREV..vNEW > /tmp/commits.txt`
2. **Categorize:** Identify breaking changes (`feat!:`), new features (`feat:`), critical fixes
3. **Draft:** Add new `## v0.X.Y` section at top of `docs/content/release-notes/index.md`
4. **Link:** Ensure breaking changes link to migration guides; home page links to latest release
5. **Validate:** Confirm all breaking changes have migration links, limitations link (not duplicate) detailed docs

**Template:**

```markdown
## v0.X.Y

**Release Date:** YYYY-MM-DD

### Breaking Changes
- **What changed:** Impact and migration link

### New Features
- Brief description with benefit

### Known Limitations
- **Title:** Brief description. See [link] for workarounds.

[Full Changelog](https://github.com/.../compare/vPREV...vNEW)
```

Release notes should fit on ~2 screens. If longer, content belongs in migration or reference docs.
