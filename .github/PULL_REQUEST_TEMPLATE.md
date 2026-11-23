<!-- Read these comments, then delete every one.

A reviewer should get the point in one minute. Length follows risk, not diff size: without Risk analysis, the description stays under 150 words for a bug fix and under 300 for a new API or behaviour change. A larger change can say more in <details>: design detail, alternatives, extra evidence. `.github/PR_DESCRIPTION_EXAMPLES.md` shows the right size.

- Title: `type: subject` or `type(scope): subject`, lowercase subject, at most 60 characters. Types: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`.
- Say what and why. Explain how only where the diff doesn't make it obvious. No file lists, and no story of how you found the bug.
- Say each thing once. Delete a section that doesn't apply instead of writing "N/A". Risk analysis always applies.
- Leave out agent output: "Generated with" lines, task lists, session links, bold lead-ins, emoji.
- Update the description when review changes the PR.
-->

## Description

<!-- Bug fix: what users saw and the root cause, then the fix. Feature: what users can do now and why they need it, then the change, with a short example. Test-only: the gap the test closes.
End with the Jira or issue link on its own line, such as `Fixes RHOAIENG-123`. -->

## How Has This Been Tested?

<!-- Results, not methods: the test that fails on `main` and passes here, or one before/after number. A command without its result is not evidence. Mention a skipped check only when a reviewer would expect it. Put output longer than ten lines in <details>. -->

## Reviewer notes

<!-- Breaking changes first: a removed or renamed field, a changed default, or behaviour existing users will notice. Say who is affected and what they must do.
Then the decisions you want an opinion on: what you chose, the alternative, and what it makes worse.
If the PR changes what the controller writes into workloads, say whether pods roll on upgrade. Name what is out of scope and the follow-ups, one line each. -->

## Risk analysis

<!-- A rating from 0 (lowest) to 5 (highest), and one line on why: what could break, what is untested, or why it is safe. If several rules apply, use the highest.
- 0: documentation only, or negligible runtime impact.
- 1 to 3: any code change, including Go, scripts that affect builds or deploys, Kustomize and CI logic, is at least 1. Most are 2 or 3, depending on scope, blast radius and how well tests cover the path.
- 4 or 5: the behaviour is unlikely to be covered by the Prow smoke path. Check the steps and env flags in `test/e2e/scripts/prow_run_smoke_test.sh`, since paths gated by variables may not run in default CI.
- 4 or 5: the change involves external systems or is tightly coupled to upstream operators, such as parent operator integration, operator catalog images, or contracts outside this repo. -->

- Rating:
- Why:

## Parent operator PRs

<!-- Delete this section unless the PR changes maas-controller RBAC: ClusterRole or Role rules, bindings, `//+kubebuilder:rbac:` markers, or regenerated `deployment/base/maas-controller/rbac/` manifests.
Link the companion PRs in https://github.com/opendatahub-io/ai-gateway-operator/ and https://github.com/opendatahub-io/opendatahub-operator
If they don't exist yet, delete this section and put this notice at the very top of the description instead:

> [!IMPORTANT]
> **Parent operator RBAC not yet updated.** This PR changes maas-controller RBAC.
> Companion PRs are still needed in:
> - https://github.com/opendatahub-io/ai-gateway-operator/
> - https://github.com/opendatahub-io/opendatahub-operator
> Do not merge until those repos reflect the same RBAC changes (link the PRs here when ready).
-->

<!-- Last line of the description: `/area` and 1 to 3 names from `.github/labels.yml`, such as `/area tenancy e2e`, at the start of its own line and outside any comment. Prow reads it only when the PR opens. To change the labels later, post it as a comment, since edits to the description are ignored. -->
