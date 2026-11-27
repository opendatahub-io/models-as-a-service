# PR build validation

`PR build validation` simulates the Konflux build and packaging path for the three
PR images defined in `.tekton/` and for the four independent Go modules. It is a
complement to the existing module CI and to `build-test.yml`, not a replacement
for them.

The workflow is `.github/workflows/pr-build-validation.yml`. The checks live in
`scripts/konflux_targets.py`; the runner entry point is `scripts/validate-build.sh`.

## Images and modules

The simulator resolves its targets from `.tekton/*-pull-request.yaml` and fails if
the set changes without being onboarded here.

| Target | Tekton file | Context | Output |
| --- | --- | --- | --- |
| `maas-api` | `odh-maas-api-pull-request.yaml` | `maas-api` | `quay.io/opendatahub/maas-api:odh-pr` |
| `maas-controller` | `odh-maas-controller-pull-request.yaml` | `.` | `quay.io/opendatahub/maas-controller:odh-pr` |
| `maas-discovery` | `odh-maas-discovery-pull-request.yaml` | `maas-discovery` | `quay.io/opendatahub/odh-maas-discovery:odh-pr` |

The Go preflight covers `maas-api`, `maas-controller`, `maas-common`, and
`maas-discovery`. It runs inside the same digest-pinned Red Hat
`ubi9/go-toolset` builder image used by the `.konflux` Dockerfiles; upstream Go
does not implement `GOEXPERIMENT=strictfipsruntime`. Each module is downloaded
into its own `GOMODCACHE` and then compiled with `GOPROXY=off`, `GOSUMDB=off`,
`CGO_ENABLED=1`, and `GOEXPERIMENT=strictfipsruntime`. The Dockerfiles currently
define no build tags; the static contract fails if tags are added or the
toolset digest drifts from the simulator.

## Static contracts

- Every Tekton dockerfile and `path-context` exists, every PR target is in the
  matrix, and the output image still uses `:odh-pr`.
- Every `COPY` source in both the upstream and `.konflux` Dockerfiles resolves
  inside its Tekton context. Stage-to-stage copies are excluded.
- The controller image contains exactly the six deployment trees the controller
  renders or reads: `/maas-api/deploy`, `/deployment/base/maas-api`,
  `/deployment/base/maas-controller/policies`,
  `/deployment/base/payload-processing`, `/deployment/base/maas-discovery`, and
  `/deployment/components`.
- The controller image runs as `USER 1001`, and `chmod -R g=u` keeps the embedded
  trees readable by OpenShift's arbitrary UID.
- `RELATED_IMAGE_ODH_MAAS_API_IMAGE` still reads
  `maas-parameters/maas-api-image`; `params.go` still falls back to
  `DefaultMaaSAPIImage`; and the tenant render still patches the `maas-api`
  container from `MaaSAPIImage`.
- Every deployment path literal in controller source exists in the image source
  tree. This covers the operator-rendered dashboards, usage logs, discovery, and
  platform overlays.
- The `.konflux` Dockerfiles use digest-pinned base images. Upstream floating
  `:latest` base tags are warnings, not failures.
- The repository has no root `go.mod` or `Makefile`, and each module has a
  `go.mod`/`go.sum` pair.
- Remote `replace` directives are version-pinned. The check was added after the
  controller and API were found to consume different forks of KServe and Gateway
  API Inference Extension.

### Generated code

`maas-controller/Makefile` owns `verify-codegen` and
`.github/workflows/build-test.yml` already runs it for PRs. The simulator does not
run a second identical regeneration. It fails if that target or its existing
workflow job is removed, which would otherwise leave generated CRD/RBAC/deepcopy
drift unguarded.

## Manifest contracts

The job renders every non-Component `kustomization.yaml` under `deployment/` and
`maas-api/deploy/`, including every base entry point and the `xks` and
`openshift` overlays. The required roots are pinned in
`scripts/konflux_targets.py`, so deleting an entry point is a failure rather than
a silently shorter run.

For each render:

- Every local loader input is resolved before rendering: `resources`,
  `components`, `bases`, generator `envs`/`env`/`files`, patch and replacement
  paths, configurations, CRDs, transformers, OpenAPI, and Helm values/chart
  paths. Symlink escapes outside the checkout are rejected. A containment error
  aborts the job before any Kustomize invocation and writes no render output.
- Remote Kustomize content is never fetched. If a root or any local
  resource/component/base in its transitive local graph references remote
  content, the root is skipped with the consuming-operator warning; that
  dependency must be asserted upstream.
- Each entry point renders from an isolated temporary copy of `deployment/` and
  `maas-api/deploy/` with `--load-restrictor LoadRestrictionsRootOnly`, so the
  renderer cannot read host files. Staging resolves and validates every source
  before reading it, including entries reached through directory symlinks, and
  rejects directory cycles. The pre-existing `vars` deprecation warning from the
  controller xks overlay is reported as a warning.
- `kubeconform -strict -ignore-missing-schemas -verbose` must pass. The JSON
  report is then checked against an explicit built-in API-group allow-list; a
  `Deploymnet` typo cannot hide as "schema not found", and Gateway API is not
  misclassified as a Kubernetes built-in group. CRD-backed custom kinds are
  allowed to skip.
- `deployment/base/maas-api/core` and `deployment/base/maas-discovery/core` are
  overlay fragments that intentionally omit `spec.selector`. Those two exact
  resources may report only `missing property 'selector'`; any other schema
  error still fails. An entry point that intentionally renders zero resources,
  such as the current `maas-api/policies`, is reported and skipped before schema
  validation.
- No workload container may declare the same `env` name twice.
- Every `RoleBinding`/`ClusterRoleBinding` subject must point at a
  `ServiceAccount` created in the same applied union, and every `roleRef` must
  exist in that union.

### Controller RBAC escalation closure

The check is deliberately stronger than "the role names look consistent". It
runs separately for coherent OpenShift and xKS bundles so a permission in one
overlay cannot hide a gap in the other. Within each bundle it computes the
effective permissions of the deployed `maas-controller` ServiceAccount from only
the RBAC installed with the controller bundle, before any applied-role write.
Roles the controller applies are proposed state; counting their rules as grants
would let a write satisfy its own coverage check. It then walks every
`Role`/`ClusterRole` that the controller server-side applies from the
operator-rendered trees. Each applied rule must be a subset of the controller's
own installed permissions, including API groups, resources, verbs,
`resourceNames`, and namespace scope. Wildcards are handled conservatively;
`resourceNames` that narrow a required rule cannot be satisfied by a broader
applied rule. A matching `escalate` grant on the role's kind, scope, and name
allows that role write without the rule-coverage check, mirroring Kubernetes.
`ClusterRole/maas-api`,
`ClusterRole/maas-api-supplemental`, and
`ClusterRole/payload-processing-reader` must all be present and checked.

`RoleBinding` grants are scoped to the binding namespace and never count as
cluster-wide; only `ClusterRoleBinding` grants do. An unqualified
`ServiceAccount` subject in a `RoleBinding` resolves to that binding's namespace.

Discovery RBAC is included only when the rendered
`MAAS_DISCOVERY_ENABLED` value is true. Both platform parameter files currently
set it to false. OpenShift includes an OpenShift-specific controller role that
can apply the discovery ClusterRole; the xKS controller role does not. If xKS is
changed to enable discovery without adding the required
`config.openshift.io/apiservers` permissions, this check fails. That is an RBAC
change and needs its own companion-PR review; this onboarding does not alter
production RBAC.

The motivating failures are RHOAIENG-101994 and RHOAIENG-101680: the controller
image wrote `aiguardrails` RBAC that its vendored ClusterRole did not hold, so
server-side apply hit RBAC escalation prevention, `ModelsAsAService` became
NotReady, and `DataScienceCluster` was blocked. The static check catches the
permission-subset failure. The full DSC condition remains platform-operator
coverage because the component repo does not run the DSC reconcile loop.

## Kind integration and runtime smoke

The image jobs prove every image is non-root and has an executable entry point.
The controller job also checks that all six embedded trees are readable and runs
`/manager --help` to prove flag parsing. The discovery job starts the image with
`--self-signed` and waits for `/healthz`. The `maas-api` `/health` endpoint needs
cluster access and a database, so it is exercised where those dependencies exist:
the controller Kind job.

The Kind job uses the production controller render, changes only test-time
values (PR images, ephemeral TLS material that OpenShift service-ca would
normally provide, `--metrics-secure=false`, and the test database URL), and
installs pinned upstream CRDs for Gateway API, cert-manager, Authorino, Kuadrant,
and Istio. It then:

1. waits for the controller to become Available, create `Config/default`, and
   prove zero restarts after bootstrap;
2. creates a Gateway, its Service, a PostgreSQL instance, and `maas-db-config`;
3. waits for the controller's own default tenant, `MaasTenantConfig/default-tenant`,
   to reconcile against that Gateway and database;
4. asserts the rendered `maas-api` Deployment uses the PR-built `maas-api` image
   from `RELATED_IMAGE_ODH_MAAS_API_IMAGE`, not `:latest`, and proves the
   controller still has zero restarts; and
5. waits for that Deployment and calls `/health` through a port-forward.

The job drives the default tenant rather than creating a second one because the
API enforces the singleton name `default-tenant`, and the AITenant admission
webhook requires a dedicated Gateway per tenant. A hand-built tenant would need
a second Gateway and its own AITenant to prove nothing more than the default
tenant does about the image contract.

Missing CRDs or missing controller permissions fail the job. There is no
`continue-on-error` and no ignored resource list. If an upstream CRD release
cannot be fetched, the job fails with the URL and the install error rather than
claiming coverage.

## Skip gate

Add `[skip konflux-sim]` to the PR title or the `skip-konflux-sim` label to skip
the checks. `workflow_dispatch` always runs. The summary job is the only status
that should be required: it fails if `check-skip` did not produce a boolean, if a
validation job skips unexpectedly, or if a required job fails or is cancelled.

## Historical evidence and coverage limits

Only the following evidence was supplied for this onboarding. Ticket keys and
descriptions are not inferred beyond it.

| Evidence | Classification | Coverage |
| --- | --- | --- |
| RHOAIENG-101994, RHOAIENG-101680 | Controller applies RBAC (notably `aiguardrails`/`ClusterRole/maas-api`) that its vendored identity does not hold; SSA escalation is blocked, `ModelsAsAService` is NotReady, DSC is blocked. | Static RBAC escalation closure catches the permission gap. Full DSC condition is platform-operator handoff. |
| MaaS namespace is not recreated after deletion | Slack analysis only; no ticket key was supplied. | Not claimed as a simulator check. The controller has namespace self-heal logic, but its live deletion/recreation path needs cluster/operator E2E. |
| RHOAIENG-75666 | MaaS HTTPRoute naming/inference returns 500. | The HTTPRoute is rendered and schema-checked; route naming and inference behaviour are platform/runtime handoff. |
| RHOAIENG-95836 | Module CRDs are missing. | CRD presence and schema rendering are checked here; module-operator consumption is checked only in the operator repo. |
| `testMaaSPolicies`, `testMaaSSubscriptions`, `testApiKeys`, `testOverviewTab` | Dashboard/Cypress E2E tests in `odh-dashboard`. | Not runnable in this repository; dashboard/platform E2E handoff. |

### Not covered

- `DataScienceCluster` / `DataScienceInitialization` Ready conditions, Managed
  versus Unmanaged convergence, and DSC component teardown.
- Openshift platform bootstrap, SCC enforcement, service-ca injection, cloud
  load balancers, storage classes, upgrades, and multi-arch image execution.
- Full Kuadrant/Authorino/Istio/KServe reconciliation and real inference
  traffic. Kind installs the pinned CRDs needed to render and apply the tenant,
  not the full platform stack.
- `opendatahub-operator` and `ai-gateway-operator` image pinning and their
  consumption of this repository's RBAC. Those facts can only be asserted in
  those repositories. If either checkout is absent, static mode prints an
  `EXTERNAL_UNASSERTED` warning.
- Dashboard tests named above. They belong to `odh-dashboard` and its E2E
  pipeline.

### Platform-operator handoff

`opendatahub-operator/manifests-config.yaml` declares all three image variables
used here. Its Kind E2E currently selects KServe rather than MaaS, so adding or
enabling a MaaS suite there is a separate follow-up. `ai-gateway-operator` also
consumes the controller RBAC and must be checked alongside any RBAC change; this
change does not modify production RBAC. When a sibling checkout is present the
static job checks the local facts it can see and emits an explicit
`EXTERNAL_UNASSERTED` warning when a repository is absent; full image pinning and
consumption remain upstream CI responsibilities.

### Proof boundaries

Static contracts and manifest rendering are runnable without a container daemon.
Image builds, runtime smoke, and Kind require the GitHub-hosted runner and are
treated as CI proofs when no local Docker or Kind daemon is available. The
workflow does not mark those jobs `continue-on-error`; a missing daemon, CRD,
permission, or health endpoint fails the job.

## Local commands

```bash
./scripts/validate-build.sh static
./scripts/validate-build.sh hermetic maas-controller
./scripts/validate-build.sh image maas-api
./scripts/validate-build.sh smoke-image maas-controller
./scripts/validate-build.sh manifests
./scripts/validate-build.sh kind   # requires a Kind cluster and loaded PR images
```

`manifests` downloads pinned `kustomize` v5.7.1 and `kubeconform` v0.7.0 with
SHA-256 verification. Set `KUSTOMIZE_BIN`, `KUBECONFORM_BIN`, or `RENDER_DIR` to
override the local tooling/output paths.
