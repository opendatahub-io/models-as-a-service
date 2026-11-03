# Upgrade to 3.6

## Recommended downtime

When upgrading from RHOAI **3.5** to **3.6**, schedule a **short maintenance window**.

MaaS rewrites gateway `AuthPolicy` identity fields and per-model `TokenRateLimitPolicy` predicates so **new** subscriptions can rate-limit on a short ID from maas-api. Until maas-api, Authorino’s subscription-info cache, and the controller-managed policies are all on the new contract, rate-limit matching for those new subscriptions can briefly fail open. A small planned downtime (or a traffic pause through the gateway) avoids that window.

Existing subscriptions keep matching on `selected_subscription_key` and do not depend on `rateLimitId` being present.

## Existing subscriptions keep their live quotas

Subscriptions that already exist at upgrade time stay on the pre-1546 identity:

- TRLP **`when`** predicates and Limitador **counters** continue to use `auth.identity.selected_subscription_key`.
- maas-api does **not** return `rateLimitId` for those CRs.
- The controller stamps `status.rateLimitIdentity: legacy` so a later reconcile cannot silently switch them to short IDs. Status is ignored by GitOps diffing by default, so the stamp does not flap OutOfSync.

Subscriptions **created after** the 3.6 controller is running are stamped `status.rateLimitIdentity: short`. Those use `selected_subscription_id` (16-hex SHA-256 of `namespace/name@modelNs/model`) for matching and counters, and may expose `X-MaaS-Subscription-Rate-Limit-Id` when select returns `rateLimitId`.

Delete-and-recreate of an old subscription is treated as a **new** CR: it opts into short IDs and starts a **fresh** Limitador counter for that subscription. In-place spec edits do not migrate identity unless `spec.rateLimitIdentity` is set.

Explicitly setting `spec.rateLimitIdentity` to `short` or `legacy` is allowed as an operator escape hatch, but **also resets that subscription’s in-window Limitador counter** because the TRLP predicate and counter expression change. Unknown values are rejected by the CRD / validating webhook. Clearing the spec field leaves the sticky `status.rateLimitIdentity` unchanged.

## Trade-off: size savings only accrue on new subscriptions

Short IDs exist to keep long `namespace/name@modelNs/model` strings out of Kuadrant WASM / EnvoyFilter predicates (the ~1.5 MiB object-size problem). Grandfathering existing CRs buys quota continuity at the cost of that size win:

| | Existing subscriptions | Subscriptions created after upgrade |
|--|--|--|
| TRLP match / counters | `selected_subscription_key` (long string in each predicate clause) | `selected_subscription_id` (16 hex chars) |
| Live Limitador quota | Unchanged by the identity change | Starts at zero (correct for a new CR) |
| EnvoyFilter / WasmPlugin size | Still pays the long-key clause cost | Compact clauses |

Further costs while a cluster has both generations:

- **A mixed-rate group splits into two TRLP limits** (`tokens-{limit}-per-{window}` for legacy members, `tokens-{limit}-per-{window}-id` for short-ID members). Kuadrant copies every limit into every route-match ActionSet, so mixed generations partially undo rate grouping (see [Subscription Cardinality](../advanced-administration/subscription-cardinality.md#gateway-config-size)) until the legacy members are gone.
- There is **no automatic backfill**. To reclaim the size win for an old subscription you must recreate it (or set `spec.rateLimitIdentity: short`), which **resets that subscription’s in-window quota**.
- Historical **Prometheus** and **Loki** usage is unaffected either way. Telemetry still labels by subscription name (`auth.identity.selected_subscription` / `X-MaaS-Subscription`).

If the cluster still uses per-subscription TRLP limit names from before rate grouping, those names still change to `tokens-{limit}-per-{window}` on upgrade. That rename is independent of short IDs and can still start fresh Limitador counters for the current window.

## What else to know

- `auth.identity.selected_subscription_key` remains for telemetry and debugging, and is still the rate-limit identity for legacy subscriptions.
- Rate limiting for **new** subscriptions matches `auth.identity.selected_subscription_id` (see [Authentication Internals](../architecture-internals/authentication-internals.md)).
- Response header `X-MaaS-Subscription-Rate-Limit-Id` may be injected when select returns `rateLimitId` (new subscriptions only). Gateway AuthPolicy rejects inbound client values for that header; also ensure Praxis / identity-header strip configs treat `x-maas-*` (prefix or explicit list) so it cannot be spoofed upstream.

## IPP → Praxis API translation migration

When a tenant flips to Praxis (`maas.opendatahub.io/payload-processing-type=praxis`),
maas-controller inspects the live `payload-processing-plugins` ConfigMap
**before** deleting legacy IPP resources ([RHOAIENG-98846](https://issues.redhat.com/browse/RHOAIENG-98846)):

| RHOAI 3.5 ConfigMap | Behavior |
| --- | --- |
| Exact product default | IPP already translated requests, sending each provider its `targetModel` and `path`. Praxis applies those only with `spec.apiTranslation.enabled`, so before cleanup maas-controller durably writes `spec.apiTranslation.enabled: true`, unless an operator already set the field. Cleanup proceeds only when active ExternalModels use OpenAI Chat Completions passthrough. |
| Exact default plus response `api-translation` | IPP translated requests and responses. Before cleanup, maas-controller durably writes `spec.apiTranslation.enabled: true`, unless an operator already set the field. The compatibility check still applies. |
| ODH 3.5.0 default (no `model-provider-resolver` in pre-processing, no `maas-headers-guard` in processing, response `api-translation`) | An earlier shape of the previous row, handled the same way. |
| Unknown or customized ConfigMap | Migration **blocked**; the controller does not infer equivalent Praxis behavior. |

The legacy IPP request translator was a multi-provider catch-all documented for
OpenAI, Anthropic, Azure OpenAI, Vertex AI, and Bedrock Mantle. Praxis currently
qualifies only OpenAI `openai-chat` pass-through and Anthropic `messages`
translation for ExternalModels. The `apiTranslation` permission is not a
tenant-wide translator choice: it permits both the shared **Anthropic Messages
client → OpenAI-compatible backend** flow and the ExternalModel **OpenAI Chat
Completions client → selected Anthropic Messages provider** flow. In the
ExternalModel path, `intelligent_route` selects the provider per request, and
the selected provider's profile remains bound through request and response
translation. OpenAI-compatible candidates remain pass-through.

An active Azure OpenAI, Vertex AI, Bedrock, custom provider, or unqualified
format/auth binding is not claimed as compatible. The AIGC resolver fails that
reconcile before publishing or cleaning serving resources, preserving the
last-known route and overlay; the migration compatibility check likewise must
keep IPP state until that binding is qualified or an operator explicitly
accepts the compatibility risk. A passing shared `/v1/messages` request does
not prove ExternalModel compatibility, and a recognized IPP ConfigMap
fingerprint alone does not prove every historical provider direction.

The pure-default IPP ConfigMap already enabled request-side `api-translation`;
its response chain was empty. Baseline #2 added response-side processing.
Recognizing any of these fingerprints preserves that migration intent, but does not
prove support for every provider or response mode. The recognized fingerprints
and migration decisions are intentionally strict.

For a recognized ConfigMap baseline, an explicit
`spec.apiTranslation.enabled: false` accepts the compatibility loss for an
unsupported active ExternalModel and lets cleanup proceed. It does not bypass
the fingerprint check for a customized ConfigMap. The force annotation below
can bypass that separate check. Both choices accept compatibility risk; they
are overrides, not parity. Review and test affected model/provider bindings
before using them. MaaS records an accepted risk on `MaasTenantConfig` and keeps
it in the `Degraded` condition after the one-shot cleanup while affected
provider bindings remain active. The warning clears when no active
ExternalModel needs the unsupported legacy transform. Forced cleanup of a
customized ConfigMap retains a warning that the migration does not establish
IPP parity.

While blocked:

- `MaasTenantConfig` stays `Ready=False` with a message naming the force annotation
- Annotation `maas.opendatahub.io/payload-processing-migration=blocked` is set
- Legacy IPP resources remain until an operator acknowledges the risk

Force migration after reviewing release notes. Back up the current tenant plugins
ConfigMap from the gateway namespace first — cleanup deletes it and the controller
does not retain the previous custom data:

```bash
kubectl get configmap <tenant-plugins-configmap> -n <gateway-namespace> -o yaml > plugins-backup.yaml
kubectl annotate maastenantconfig default-tenant -n <tenant-namespace> \
  maas.opendatahub.io/force-payload-processing-migration=true --overwrite
```

Known-good ConfigMaps do **not** need the force annotation when their active
ExternalModels are compatible. Applying force on a customized ConfigMap
replaces it with the Praxis plugins schema; re-apply any still-needed
customizations in the Praxis ConfigMap shape afterward. An override does not
establish parity for Azure, Vertex, Bedrock, OpenAI variants, streaming, or
other provider behavior without matching deployed evidence.

## ExternalModels outside tenant namespaces

In 3.5 the default tenant served ExternalModels in every namespace. 3.6 keeps
that behavior: ai-gateway-controller serves an ExternalModel without
`spec.gatewayRefs`, in a namespace with no `MaasTenantConfig` that no
`AITenant` claims, through the default tenant's Gateway
(`AITenant/models-as-a-service`) without changing its spec
([RHOAIENG-101754](https://redhat.atlassian.net/browse/RHOAIENG-101754)). Its
HTTPRoute stays in the model namespace, so the Gateway listener's
`allowedRoutes` must still admit that namespace, as in 3.5. `auth.type: simple`
is served as `apikey`. While the default tenant selects IPP, IPP keeps serving
these models.

When the default tenant switches to Praxis, the compatibility check above and
the cleanup of IPP-owned HTTPRoutes also cover these models, not only those in
the tenant namespace. The same cleanup removes the Service, ServiceEntry and
DestinationRule that IPP created next to each ExternalProvider;
ai-gateway-controller applies its own provider transport.

## Legacy `maas.opendatahub.io` ExternalModels

A legacy ExternalModel that IPP migrated reports `status.phase: Migrated`. When
it reconciles a legacy object, maas-controller removes its owner reference from
the `inference.opendatahub.io` ExternalModel and ExternalProvider created for it,
even before the object is Migrated, so deleting the deprecated legacy object no
longer deletes the live model. On releases without this change, or if
maas-controller has not reconciled the legacy object since the upgrade, delete
it with `kubectl delete externalmodels.maas.opendatahub.io <name> -n <namespace> --cascade=orphan`.
## Legacy metrics usage dashboard is opt-in

`Config.spec.usageMetricsDashboard` defaults to `false`. After upgrade, the operator removes any controller-owned copy of the Prometheus-based Perses dashboard (`dashboard-3-maas-usage-admin`). To keep it:

```bash
kubectl patch configs.maas.opendatahub.io default --type=merge \
  -p '{"spec":{"usageMetricsDashboard":true}}'
```

Prefer the logs-based usage dashboards. The operator deploys them with the usage-log resources. See [Setup](../observability/setup.md).
