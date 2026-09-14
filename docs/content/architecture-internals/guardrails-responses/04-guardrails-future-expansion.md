# Guardrails: future expansion

| | |
|---|---|
| Status | Deferred proposal; outside the initial API |
| Authors | Pierangelo Di Pilato, Christina Xu, Marius Ion Danciu |

The initial [guardrails API](02-guardrails-low-level-details.md#attachment-selection-and-composition) uses an additive
list of `ref + checks` entries. Every selected check enforces. This document preserves the richer required/default
composition proposal for separate review; it is not a prerequisite for initial guardrails support.

## Motivation and compatibility

Optional defaults could let tenant administrators suggest checks that model or subscription administrators may replace,
while preserving mandatory protections. This adds editing, precedence and rollout complexity, so it is deferred.

The examples below sketch a future `guardrails.required` and `guardrails.defaults` structure. Introducing that structure
would change the initial list-shaped field: define a versioned API conversion or a separate compatible extension before
shipping it. Existing attachments must retain mandatory, additive behavior after conversion; they must never become
overridable defaults implicitly. No attachment alias is needed in either shape.

Within every attachment, `ref` still contains explicit `name` and `namespace`. An omitted or empty attachment `checks`
selects all checks in that AIGuardrail, including future additions. A nonempty list selects named checks. This is distinct
from an empty **list of attachments** in a defaults operation, which can clear optional defaults under `Replace`.
All current namespace validation, provider permission, fail-closed behavior, check identity and execution ordering apply.

## Exact inheritance and merge semantics

Separate **required checks** from **overridable defaults**. “Default” means a real enforcing check unless an authorized
editor explicitly replaces/disables it; it does not mean audit-only. Required checks can be changed by their resource
owner, but another scope cannot remove or weaken them.

For an authenticated `(tenant UID, subscription UID, model UID)` tuple:

1. Collect required bindings from AITenant, MaasTenantConfig, Model, selected Subscription and its matching `modelRefs[]` entry.
2. Start with AITenant defaults; apply MaasTenantConfig defaults, Model defaults, Subscription defaults, then the matching subscription model-entry
   defaults. The consuming application's most specific scope wins for optional defaults. Model safety requirements
   belong in `required`.
3. Combine required and resolved default bindings and expand their policies into a set of selected checks. Deduplicate
   by namespace, AIGuardrail name and check name, preserving all contributing attachment provenance. Run selected
   checks in the deterministic catalog order for each applicable phase; all must pass. A block stops the operation, and an
   error fails closed. There is no “later pass overrides earlier block.”

| Defaults field                                    | Effect on inherited defaults                          |
|---------------------------------------------------|-------------------------------------------------------|
| Absent, or `mode: Inherit`                        | Preserve inherited list; local checks must be absent  |
| `mode: Merge` (default when `checks` is supplied) | Append local checks; do not replace inherited entries |
| `mode: Replace`                                   | Replace the whole optional list with the local list   |
| `mode: Disable`                                   | Empty the optional list; local checks must be absent  |
| `checks: []` with `Merge`                         | No change                                             |
| `checks: []` with `Replace`                       | Explicitly empty the optional list                    |

At Tenant scope defaults seed the list. Explicit `null` is invalid. An attachment is identified by its source resource
UID, attachment path, required/default category and referenced namespace/name. No attachment-level `name` is needed.
Deduplicate execution by `(namespace, AIGuardrail name, check name)`, preserving every origin. Defaults replacement
changes only the optional attachment list; it cannot remove a check selected by any required attachment. Execution
order remains the deterministic catalog order, independently of scope precedence.

Example: Tenant requires `safety`, defaults to `topic`; Model requires `medical`, merges `pii`; Subscription requires
`finance` and replaces defaults with `support`. The selected policy set is `{safety, medical, finance, support}`. With
Subscription
`Merge`, it is `{safety, medical, finance, topic, pii, support}`. With Subscription
`Disable`, it is `{safety, medical, finance}`. No override removes `medical`. A matching subscription model entry with
required
`audit` and defaults `Replace: [specialist]` selects `{safety, medical, finance, audit, specialist}`. Its `Disable`
selects
`{safety, medical, finance, audit}`. Other model entries do not participate. Validate uniqueness of subscription model
namespace/name keys so a request cannot match ambiguous entries.

The future API must retain enforcing, fail-closed checks. It does not expose fail-open, audit-only or redaction as if
they were equivalent to enforcement. A future exception should be an explicit, scoped, expiring waiver authorized by the
owner of the required policy. Do not add a global `guardrails.enabled: false`
that removes inherited requirements. Likewise, do not try to rank arbitrary NeMo configs by “strictness”: different
configs are not generally comparable.

## Resolution algorithm and worked edge cases

The resolver operates on admitted resources from one consistent published snapshot. Resource names are for
configuration; UIDs identify the authorized objects and prevent name reuse from inheriting old authorization. Resolve
MaaS attachment scopes and validate tenant approval and model applicability. Require current AI Gateway-accepted
AIGuardrail binding revisions for the provider edge; MaaS does not resolve NeMo namespaces or evaluate allowedConsumers
itself. Provider readiness gates activation separately from this composition.

```text
resolve(tenant, maasTenantConfig, model, selectedSubscription, snapshot):
    assert model resolves to tenant
    assert selectedSubscription is authorized for this model and principal
    entry = unique selectedSubscription.modelRefs entry matching model
    assert all AIGuardrail refs resolve within permitted MaaS scopes and tenant approval
    assert each policy has a current AI Gateway-accepted provider binding revision in snapshot

    required = tenant.required + maasTenantConfig.required + model.required + selectedSubscription.required + entry.required
    defaults = tenant.defaults.checks
    defaults = apply(defaults, maasTenantConfig.defaults)
    defaults = apply(defaults, model.defaults)
    defaults = apply(defaults, selectedSubscription.defaults)
    defaults = apply(defaults, entry.defaults)

    bindings = required + defaults
    selected = expand each binding.checks (omitted/empty selects all), retaining all origins
    selected = deduplicate by (namespace, AIGuardrail.name, check.name)
    plan = sort selected by AIGuardrail.name, then the check index in that policy.spec.checks
    validate every effective check against model applicability, protocol, modality and capabilities
    return immutable plan with resource UIDs, revisions and origin for each binding

apply(inherited, local):
    Inherit or absent: return inherited
    Merge:            return inherited + local.checks
    Replace:          return local.checks
    Disable:          return []
```

This pseudocode assumes defaulting has normalized `checks` without a mode to `Merge`. It does not grant access:
authentication and subscription selection happen first. Resolution must reject missing/deleted objects rather than
substituting a same-name replacement or falling back to a different subscription with weaker checks.

| Scenario                                                                             | Result                                                                                                                          |
|--------------------------------------------------------------------------------------|---------------------------------------------------------------------------------------------------------------------------------|
| Tenant required Input check; Model replaces defaults                                 | Tenant Input check remains                                                                                                      |
| Tenant default Input+Output check; Subscription replaces it with Input-only          | Allowed delegation for defaults; Output is no longer effective unless another binding requires it                               |
| Tenant required Input+Output check; Subscription adds Input-only through another attachment | Original Input+Output binding remains; the new binding is separate                                                              |
| Model requires a check; Subscription disables defaults                               | Model requirement remains                                                                                                       |
| Subscription A replaces defaults; caller is authorized under Subscription B          | A has no effect on the request                                                                                                  |
| Model default is incompatible but Subscription replaces it                           | Validate the final effective plan for runtime compatibility; local declarations must still be structurally valid and resolvable |
| Effective default is unsupported or its provider is unavailable                      | Reject; an overridable default still enforces until explicitly overridden                                                       |
| Model is deleted and recreated with the same name                                    | Existing stored model UID and old generated configuration do not authorize the new object                                       |
| Policy is removed before its bindings are migrated                                   | Affected bindings become unresolved; no unguarded fallback                                                                      |

## Five-scope policy resolution: five scopes, two NeMo servers and subscription-specific overrides

Input fragments use the shared attachment shape defined earlier. Every policy ref names its namespace explicitly and must pass same-tenant membership validation; each
NeMo binding satisfies allowedConsumers and tenant approval. The fragments below show tenant, model and subscription
resources, with MaasTenantConfig adding the tenant-admin baseline and the fifth scope under the matching
`MaaSSubscription.spec.modelRefs[]` entry. They omit unrelated and required resource fields and are not apply-ready
manifests.

```yaml
kind: AITenant
spec:
  guardrails:
    required: [ { ref: { name: safety-v1, namespace: <tenant-namespace> }, checks: [] } ]
    defaults:
      checks: [ { ref: { name: topic-v1, namespace: <tenant-namespace> }, checks: [] } ]
---
kind: MaasTenantConfig
metadata:
  name: default-tenant
  namespace: <tenant-namespace>
spec:
  guardrails:
    required: [{ref: {name: privacy-v1, namespace: <tenant-namespace>}, checks: [pii]}]
---
kind: MaaSModelRef
metadata:
  name: granite-7b
  namespace: <model-namespace>
spec:
  guardrails:
    required: [ { ref: { name: privacy-v1, namespace: <tenant-namespace> }, checks: [] } ]
    defaults:
      mode: Merge
      checks: [ { ref: { name: tone-v1, namespace: <tenant-namespace> }, checks: [] } ]
---
kind: MaaSSubscription
spec:
  guardrails:
    required: [ { ref: { name: audit-v1, namespace: <tenant-namespace> }, checks: [] } ]
    defaults:
      mode: Replace
      checks: [ { ref: { name: support-v1, namespace: <tenant-namespace> }, checks: [] } ]
  modelRefs:
    - name: granite-7b
      namespace: <model-namespace>
      guardrails:
        defaults:
          mode: Replace
          checks: [ { ref: { name: specialist-v1, namespace: <tenant-namespace> }, checks: [] } ]
```

Resolved definitions: `safety-v1` has Input check `safety` on server A; `privacy-v1`
has Input checks `pii`, then `regex` on server A; `audit-v1` has Input check `audit`
on server B; `specialist-v1` has Input check `specialist` on server B. Lexical AIGuardrail order makes this request
execute
`audit, pii, regex, safety, specialist`, preserving the two checks within `privacy-v1`. MaasTenantConfig also requires `pii`, which executes once. Tenant `topic`, model `tone`
and subscription `support` defaults are not selected; their tenant-local checks may still be configured for other
requests. Another subscription can have a different plan while sharing the same logical model.

The order is determined from the tenant-local catalog, independently of attachment precedence. For each check, emit an
existing `ai_guardrails` entry with the same verified binding-selection `conditions` used above, preserving order.
However, the current NeMo provider cannot express these distinct config IDs on one server. This specific fixture
therefore has no complete supported NeMo lowering yet:
report the unsupported selector requirement until the minimal provider addition above is implemented; do not drop checks
or assume separate endpoints select configurations. The resolution example specifies MaaS semantics independently of
adapter coverage.

## Additional acceptance criteria

A future implementation must test all defaults modes, omitted/empty/null distinctions, precedence across all five
scopes, selected-subscription isolation and required-check preservation. Conversion must retain initial attachments
and their all-checks/subset behavior. UI and status must explain which defaults were replaced and by which scope.

See the [initial compilation and runtime contract](02-guardrails-low-level-details.md#materializing-maas-configuration-in-praxis)
and [shared acceptance matrix](02-guardrails-low-level-details.md#acceptance-matrix).
