# Pull request description examples

Right-sized descriptions for `.github/PULL_REQUEST_TEMPLATE.md`, taken from real PRs in this repo. The words are the authors'. They are moved into the template's sections, with titles trimmed to 60 characters and `/area` lines added.

## Bug fix

From [#1599](https://github.com/opendatahub-io/models-as-a-service/pull/1599). What users saw, the root cause, a fix that says why it is safe, and proof from the failing runs.

~~~markdown
fix: stop identity filter dropping maas-api auth headers

## Description

The gateway AuthPolicy's `identity` filter reads `subscription-info`, but that metadata is only fetched for model requests. On every `/maas-api` request the filter fails with `no such key`.

That failure isn't harmless. Authorino evaluates same-priority response configs together and cancels the rest when one fails, so API-key requests sometimes reach maas-api without some or all `X-MaaS-*` headers. maas-api then returns `200 []` for `/v1/models` and `/v1/subscriptions`, or a 500 `AUTH_FAILURE`, depending on which headers were dropped.

This PR gives the filter the same `when` guard `subscription-info` already has. The filter never succeeded on `/maas-api` paths, so its output is unchanged. It just stops failing and taking the headers down with it.

Found while debugging #1598

## How Has This Been Tested?

Verified with a new unit test for the guard and the Authorino debug logs from two red group-test runs on #1598, which show the dropped headers on the failing requests.

## Risk analysis

- Rating: 3
- Why: generated AuthPolicy (Kuadrant contract) changes, but only by adding a guard identical to the one on its sibling evaluators.

/area auth
~~~

## New API

From [#1536](https://github.com/opendatahub-io/models-as-a-service/pull/1536). What customers can't do today, one number for why it matters, the new field, a plain statement of what wasn't run, and the dependency a reviewer has to know about.

~~~markdown
feat: add unlimited token budget to MaaSSubscription refs

## Description

A MaaSSubscription model ref cannot say "no token limit" - `tokenRateLimits` is required, so customers fake it with rates like `99999/1s`.

Each fake rate is a full TokenRateLimitPolicy limit, and Kuadrant copies every limit into every route match of the gateway's WasmPlugin, one object capped at 1.5 MiB by etcd. In one customer config, 13 of 37 subscription/model pairs spend that budget encoding "no limit".

This PR adds `unlimited: true` to model refs, mutually exclusive with `tokenRateLimits`. Unlimited subscriptions on a model share one metering-only limit, so usage still lands in `authorized_hits` and each extra one costs a predicate clause, not a whole limit.

Related to [RHOAIENG-95277](https://redhat.atlassian.net/browse/RHOAIENG-95277)

## How Has This Been Tested?

Covered by unit tests for the CRD rules, TRLP generation and subscription selection, plus e2e tests for rate limiting, WasmPlugin size, metering and all-unlimited models (not yet run on a cluster).

## Reviewer notes

The odh-dashboard BFF drops `unlimited` when it rewrites a subscription, so it needs a companion change.

## Risk analysis

- Rating: 4
- Why: new rate-less TRLP shape whose data-plane behaviour only e2e exercises, plus a dashboard dependency.

/area subscriptions
~~~

## Feature with an example

From [#1778](https://github.com/opendatahub-io/models-as-a-service/pull/1778). The change in two short paragraphs, then what someone sets to use it. The original has no risk rating, so this one is ours.

~~~markdown
feat(grid): inform routing with geo fencing requirements

## Description

Subscription selection already resolves per-user entitlements at the Gateway. Carry the header map through that callback so annotated subscriptions can inject scoped request metadata without extending the CRD schema.

The controller advertises only validated header names, maas-api returns values for the selected subscription, and empty fallbacks replace client-supplied copies.

Add the `experimental.maas.opendatahub.io/headers` annotation to inject one or more upstream request headers when Gateway authentication resolves a request to that subscription. The value must be a JSON object mapping `x-grid-*` header names to arrays of strings; multiple values are sent as a comma-separated header.

```yaml
metadata:
  annotations:
    experimental.maas.opendatahub.io/headers: '{"x-grid-geo-fencing":["region1","region2"]}'
```

## How Has This Been Tested?

E2E proves the provider receives the joined values and cannot be spoofed.

## Risk analysis

- Rating: 3
- Why: changes the generated AuthPolicy (Kuadrant contract) and what maas-api returns, behind an experimental annotation and with e2e on the path.

/area grid
~~~

## Test-only

From [#1594](https://github.com/opendatahub-io/models-as-a-service/pull/1594). The numbers behind the change, what was and wasn't checked, and the one real risk even though no production code moves.

~~~markdown
test: split tenant_isolation to shorten the parallel pass

## Description

With `--dist=loadgroup` each xdist group stays on one worker, and `tenant_isolation` was by far the longest (median 1405s). In 19 of 23 `maas-group-test` runs, pass 1 ended within 2s of that group's summed time.

Two of its modules didn't need to be there. Tenant model inference (533s) and per-tenant IPP isolation (296s) build their own tenants and never touch `shared_test_tenants`, yet they queued on the same worker.

This PR gives them their own groups (`tenant_inference`, `tenant_ipp`) and updates the testing guide's group rules to match.

Test-only; no prod change.

Follow-up to #1587

## How Has This Been Tested?

Checked with pytest collection on Python 3.9 and a review of the state the split modules share. The group test here will be the real check.

## Risk analysis

- Rating: 2
- Why: four tenants and their LLMInferenceServices now start in the first two minutes of pass 1, so a slow controller could push tenant setup past its wait.

/area e2e
~~~
