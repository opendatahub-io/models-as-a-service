# Responses: future expansion and capability discovery

| | |
|---|---|
| Status | Proposed |
| Authors | Pierangelo Di Pilato, Christina Xu, Marius Ion Danciu |
| Source | [Main design](../responses-and-guardrails.md) |

These proposals are separate from the initial Responses enablement contract. Automatic capability discovery and agentic bindings are future work; they are not prerequisites for the initial tenant API.

This topic document copies the relevant sections of the main proposal for focused review. The main document is retained
in full as the consolidated reference; these documents do not record separate design approval.

Read alongside:

- [Responses and guardrails: high-level design](01-guardrails-responses-high-level-design.md)
- [Guardrails: API, Praxis compilation and reconciliation](02-guardrails-low-level-details.md)
- [Responses: enablement, storage and request lifecycle](02-responses-low-level-details.md)

In this document:

- [Future KServe capability discovery for MaaS](#future-kserve-capability-discovery-for-maas)
- [Deferred agentic flows](#deferred-agentic-flows)
- [References and reviews](#references-and-reviews)

## Future KServe capability discovery for MaaS

This is a separate, proposed KServe integration deliverable. It is not required to introduce explicit MaaS capability
configuration, and none of the status fields below exist in the inspected KServe API. Keep the initial three modes
`ChatCompletions`, `Native` and `Unsupported`; discovery supplies compatibility evidence, not another enablement switch.

The inspected KServe `LLMInferenceService` v1alpha2 status already exposes addresses, router/workload observations and
applied configuration references. These provide places to associate observations with a deployed service, but do not
establish which model APIs are supported. This integration is scoped exclusively to `LLMInferenceService`; Do not assume
an LLMInferenceService uses a particular engine or the mere presence of a `/v1/responses` route establishes Responses
feature coverage.

### Concrete automatic discovery path for LLMInferenceService

Automatic discovery is feasible without asking model publishers to duplicate the task declaration. The strongest signal
comes from the loaded engine itself. For example, vLLM's server obtains `engine_client.get_supported_tasks()` and uses
the result when constructing its API application. This is an existing internal engine interface, not a universal HTTP
metadata endpoint.
See [vLLM server initialization](https://docs.vllm.ai/en/stable/api/vllm/entrypoints/openai/api_server/).

For a supported vLLM image/version, propose the following integration:

1. A small in-process reporting extension reads the initialized engine's supported tasks, resolved model identity and
   enabled API handlers. It also checks API prerequisites such as an available chat template and enabled tool parser.
   Report after model loading, and refresh on reload. A sidecar can relay a protected report but cannot inspect another
   process's Python state; it needs an explicit engine endpoint or shared report file.
2. KServe associates the report with the actual ready workloads behind the `LLMInferenceService`, using its workload
   references and resolved model/base configuration. It verifies workload identity and revision, then publishes the
   normalized capability observation in the service's proposed status. Engine pods need not receive Kubernetes status
   write credentials; KServe remains the status writer.
3. MaaS watches that status and computes effective Responses availability from the configured mode, observed backend
   capability and Praxis adapter support. Unsupported generation makes Responses unavailable automatically, without
   asking the user to change a defaulted mode or rewriting their spec. Explicit `Unsupported` always remains
   restrictive.

| Engine evidence                                                                | Normalized result                                                                                                    |
|--------------------------------------------------------------------------------|----------------------------------------------------------------------------------------------------------------------|
| Loaded engine supports generation and usable Chat Completions handler/template | Chat Completions supported; eligible for Praxis translation, subject to feature compatibility                        |
| Authoritative loaded-engine task set excludes generation                       | Chat Completions/Responses generation unsupported; embedding or reranking remains independently discoverable         |
| Embedding task and enabled embedding handler                                   | Embeddings supported; do not infer the absence of other tasks                                                        |
| Scoring/classification task                                                    | Reranking is only a candidate until the adapter confirms the rerank API and required model behavior                  |
| Native Responses handler and confirmed feature support                         | Native Responses generation supported for that subset; does not select `Native` automatically or certify persistence |
| Unknown engine, incomplete report, or stale workload/model revision            | Unknown; no inferred negative capability and no claim of verified support                                            |

An earlier implementation can inspect resolved container arguments and configuration for known runtime versions, or
perform bounded conformance probes, without modifying the engine. Explicit settings can establish some facts; automatic
runner selection, custom entrypoints, missing chat templates and runtime model conversion make generic argument parsing
incomplete. Architecture names in model metadata are hints, not the final answer. Avoid scraping startup logs as the
stable discovery contract. The engine report is the preferred automatic path; configuration/probe adapters are fallback
sources with explicit provenance and confidence, as detailed below.

### Discovery and publication

KServe could publish normalized capability observations in `LLMInferenceService.status.capabilities`, using an engine
adapter and the resolved service configuration:

1. Resolve the effective runtime configuration after `baseRefs` merging, including the loaded model identity, task and
   serving options. A versioned, approved runtime profile can declare candidate APIs and their limitations; model
   architecture metadata or command-line heuristics alone remain hints.
2. Prefer a runtime-provided, model-specific capability report through a documented, authenticated metadata interface.
   Where none exists, a trusted adapter can interpret known engine configuration for a pinned version. Do not invent a
   universal discovery endpoint or execute model repository code to inspect capabilities.
3. Optionally confirm advertised APIs with explicitly authorized, bounded synthetic conformance probes. Probes can
   consume inference capacity and must not use customer content, create retained conversations, invoke tools or become
   normal readiness traffic. A timeout or inaccessible endpoint is `Unknown`, not `Unsupported`.
4. Publish per-capability `Supported`, `Unsupported` or `Unknown`, plus evidence source, observation time and the
   service, workload/runtime and model revisions to which it applies. Aggregate across every replica/backend eligible
   for the route; incompatible rolling revisions must not produce an optimistic service-wide capability claim.

Illustrative **proposed status** for an embedding-only service, not an apply-ready resource or existing KServe schema:

```yaml
kind: LLMInferenceService
status:
  capabilities:
    observedGeneration: 12
    runtimeRevision: RENDER_RUNTIME_REVISION
    modelRevision: RENDER_MODEL_REVISION
    source: RuntimeReport
    apis:
      chatCompletions:
        support: Unsupported
      responses:
        support: Unsupported
      embeddings:
        support: Supported
      rerank:
        support: Unknown
```

A native Responses report should distinguish request-generation support from individual features such as streaming,
modalities and tools. Backend-native persistence is not a prerequisite for Praxis-managed persistence and is not a
substitute for it. KServe reports backend facts; it must not publish MaaS adapter modes, guardrail policy or tenant
entitlements as runtime capabilities. Final field names and versioning require a KServe API review.

### MaaS consumption and precedence

MaaS reads observations through its resolved `modelRef`, checks resource UID and observed runtime/model revision, then
combines them with the configured mode and the selected Praxis adapter's capabilities. It records the effective result
and provenance in model status and recompiles affected plans on changes. It does not rewrite `spec` or probe the backend
on the inference path.

| Model setting and observation                                         | Proposed behavior                                                                                                                       |
|-----------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------------------------------------|
| Explicit `Unsupported`                                                | Always keep Responses unavailable, even if KServe reports generation support                                                            |
| Explicit or defaulted `ChatCompletions`; chat generation supported    | Admit only the Responses subset supported by Praxis translation and the loaded model                                                    |
| Explicit or defaulted `ChatCompletions`; chat generation unsupported  | Reject activation for Responses and surface incompatibility; keep unrelated model APIs available                                        |
| Explicit `Native`; native generation supported                        | Validate the requested feature subset; retain Praxis ownership/storage and policy enforcement                                           |
| Explicit `Native`; native generation unsupported                      | Reject activation; do not silently change adapter mode                                                                                  |
| Discovery not integrated/available                                    | Retain the initial declaration/default and existing compatibility-validation contract; do not claim automatic verification              |
| Discovery required for this deployment, but observation unknown/stale | Keep affected Responses plans unavailable pending fresh evidence; do not turn a probe failure into a supported or unsupported assertion |

A reported embedding/reranking capability alone does not prove generation is unsupported: a service can support multiple
tasks. Require a reliable negative chat-generation observation to invalidate `ChatCompletions` on that basis. Do not
silently upgrade the default to `Native` when KServe reports native support. If a future API wants discovery to select
the mode automatically, define that opt-in separately rather than changing this default's meaning.

Invalidation must cover model reloads, mutable configuration updates, serving arguments, image/runtime revisions,
replica rollouts and endpoint changes, not only changes to the top-level resource generation. KServe remains the writer
of its observations; MaaS applies its own access and feature policy. ExternalModel providers need a separate discovery
adapter or explicit declarations; this KServe proposal does not imply automatic external-provider discovery.

Acceptance should cover generation-only, embedding-only, reranker-only and multi-task services; native versus translated
feature coverage; unknown/timeouts; spoofed or stale observations; mixed replicas; mode conflicts; and model UID
recreation. Ship this independently from initial manual capability declarations.

## Deferred agentic flows

The supplied `full-flow-agentic.yaml` and `agentic-loop.yaml` remain references for existing file-search and MCP
execution using `iterative_request_router` and the corresponding callout/dispatch filters. File search and MCP are
outside the initial tenant API; their bindings, authorization and guarded iteration require separate deliverables. This
design does not introduce additional filter types or speculative configuration fields for them.

## References and reviews

See the [source references](../responses-and-guardrails.md#references) in the main design and
the [review record](01-guardrails-responses-high-level-design.md#reviews) in the high-level design.
