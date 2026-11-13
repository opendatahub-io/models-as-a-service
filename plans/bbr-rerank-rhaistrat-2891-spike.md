# Spike: JSON BBR governance on `/v1/rerank` (RHAISTRAT-2891 phase 1)

**Date:** 2026-11-12  
**Scope:** AuthZ + interim request limits + usage logs (no multipart).

## Pre-chain (Praxis / JSON BBR)

| Check | Expected | Code reference |
|-------|----------|----------------|
| Pre-ExtProc extracts JSON `"model"` | Yes, path-agnostic | `ai-gateway-controller/.../praxis-extproc/.../configmap.yaml` (`json_body` → `X-Gateway-Model-Name`) |
| AuthPolicy model identity on `/v1/*` | `x-gateway-model-name` | `maasauthpolicy_controller.go` (`celModelIdentity`) |
| Caller-supplied routing header | Stripped / ignored | Same pre-chain |

## Cluster probes (isequeir-stg, prior RHAIRFE-2245 bucket 1)

| Path | HTTP | Class | Notes |
|------|------|-------|-------|
| `/v1/rerank` | 404 | B | BBR + auth likely passed; **no rerank backend** on chat sim |

See `plans/rhairfe-2245-spike-matrix.md` and `artifacts/bucket1-spike-*.tsv`.

## Token TRLP vs rerank

- Per-route **TokenRateLimitPolicy** predicates are subscription-scoped, not limited to chat/embeddings.
- Kuadrant TRLP **meters `usage.total_tokens`** from the response body. Rerank responses (Jina/Cohere shape) typically **omit** that field, so **token budgets do not produce 429** on `/v1/rerank`.
- **Interim:** `MaaSSubscription` `tokenRateLimits` values are mirrored into a per-model **RateLimitPolicy** (`maas-rlp-<model>`) scoped to `request.path.endsWith("/v1/rerank")` — one request = one unit until modality-specific quotas land.

## Implemented in MaaS (this change)

1. `maas-controller`: aggregated **RateLimitPolicy** alongside TRLP (`reconcile_request_ratelimit.go`).
2. OTEL usage log EnvoyFilter: include `/v1/rerank` in path suffix matchers.
3. E2E fixture: minimal HTTPS rerank mock + `test_rerank_inference.py` (200 / 403 / 429 + chat regression).

## Out of scope

Multipart (`/v1/audio/transcriptions`), modality CRDs, token showback on rerank scores.
