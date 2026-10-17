# E2E Test Fixtures

This directory contains kustomizations for end-to-end testing that combine public samples with test-only fixtures.

## Contents

### Public Samples (from `docs/samples/maas-system/`)
- **free**: `system:authenticated` group, 100 tokens/min (served ID `facebook/opt-125m`)
- **premium**: `premium-user` group, 1000 tokens/min (served ID `facebook/opt-125m-premium`)

### Test-Only Fixtures
- **unconfigured**: Model with no MaaSAuthPolicy or MaaSSubscription (served ID `test/e2e-unconfigured-model`; validates gateway denies access with 403)
- **distinct**: First distinct model serving `test/e2e-distinct-model` (validates multiple distinct models in subscriptions)
- **distinct-2**: Second distinct model serving `test/e2e-distinct-model-2` (validates multiple distinct models in subscriptions)
- **trlp-test**: Model used to validate TRLP validation behavior
- **embedding**: Embedding model (validates `/v1/embeddings`, TRLP, default-deny)
- **echo-sim**: Simulator running `--mode echo` serving `test/e2e-echo-model` (validates large mirrored I/O and credential leaks)

#### About echo-sim

Every other simulator fixture runs `--mode random`, which synthesizes a response
from `max_tokens` and a histogram. The content is unrelated to the prompt and its
size is not something a test can set, so two things cannot be validated:

- **Large I/O** — in `echo` mode the response mirrors the request, so response size
  is controlled by prompt size. That makes body limits, buffering and streaming
  behavior testable deterministically.
- **Leaks** — a canary string placed in the prompt comes back verbatim, so a test
  can assert exactly where it does and does not appear (response, logs, metrics).

`echo-sim` is additive. It ships its own LLMIS with a distinct served ID, so the
shared `random` simulators used by every other test are untouched.

Like `distinct` and `distinct-2`, the fixture ships only an LLMIS and a
MaaSModelRef — tests create their own MaaSSubscription and MaaSAuthPolicy.

`echo-sim` is the only simulator on `llm-d-inference-sim:v0.9.0`; every other
one is pinned to `v0.8.2`. The split is deliberate and should not be
"tidied up" into a single version:

- echo-sim needs v0.9.0 because the `echo-sim-log-http` overlay passes
  `--log-http`, and v0.8.2 exits at startup with `unknown flag: --log-http`.
- The others cannot move to v0.9.0. In v0.9.0 a model id that resolves on
  HuggingFace is routed through the real HF tokenizer, and `--mode random`
  then initializes its dataset through a render service on `localhost:8082`.
  Nothing in these manifests runs one, so the container crash-loops with
  `failed to initialize random dataset ... connection refused`. v0.9.0 logs
  `Model is not a real HF model, using simulated tokenizer` for made-up ids,
  so only `facebook/opt-125m` — the one real repo id in use — is affected.
  There is no flag to opt out: `--render-url` only repoints the service.
- echo mode never builds that dataset, so v0.9.0 is safe for this fixture.

Behaviour verified against `llm-d-inference-sim:v0.9.0`, worth knowing before
writing tests against it:

| Behaviour | Result |
|---|---|
| Response content | Mirrors the request verbatim, byte for byte |
| Response size | Tracks request size (432KB request produced a 432KB response) |
| `max_tokens` | Does **not** truncate the echo — a 50-token cap still returned the full payload |
| Context window | Rejects anything over `--max-model-len` with HTTP 400, hence the 131072 above |
| Reported `usage` | Counts whitespace-delimited words, not bytes — `'A' * 200000` reported 6 prompt tokens |
| Peak memory | ~130Mi at the largest payload the context window allows |

The `usage` caveat matters: token counts are not proportional to payload size,
so do not use this fixture to assert token accounting or rate-limit maths. Use
the `random`-mode simulators for that.

Concurrency is capped by the simulator's `--max-num-seqs` (default 5). Raise it
in the LLMIS args if a test needs more requests in flight at once.

#### echo-sim-log-http (opt-in, never in CI)

`echo-sim-log-http` is the same model with `--log-http` appended, which makes the
simulator log complete HTTP requests and responses — *"method, URI, headers,
bodies when buffered, status; streamed bodies are not logged"*. It exists for
investigating which credentials actually reach the backend.

It is deliberately **absent** from `kustomization.yaml` above. `--log-http`
records the `Authorization` header, so a single request puts a live API key in
the pod log, and CI archives pod logs as artifacts. Apply it by hand and revert
when done:

```bash
kustomize build test/e2e/fixtures/echo-sim-log-http | kubectl apply -f -
# ... investigate ...
kustomize build test/e2e/fixtures | kubectl apply -f -
```

Both render an LLMIS named `echo-simulator`, so they are mutually exclusive —
applying one replaces the other, and you cannot end up with both by accident.

> [!WARNING]
> Do not add `echo-sim-log-http` to the fixtures kustomization or any CI path.
> Note also that `test_api_key_not_echoed_in_response` still passes while this
> overlay is active: it inspects the response, not the pod log.

## Usage

### For E2E Tests (CI)

```bash
# Deploy all fixtures (public samples + test-only)
kustomize build test/e2e/fixtures | kubectl apply -f -
```

### For Manual Testing

To deploy only the public samples without test fixtures, use:

```bash
# Public samples only (free + premium)
kustomize build docs/samples/maas-system | kubectl apply -f -
```

## Note

⚠️ **Do not use this kustomization for production or sample installations.** It includes test-only models that are designed to validate edge cases and should not be deployed in normal usage scenarios. For sample installations, use `docs/samples/maas-system/` instead.
