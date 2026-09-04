# Metrics and Dashboards

## Key Metrics

### Token and Request Metrics

| Metric | Type | Labels | Use For |
|--------|------|--------|---------|
| `authorized_hits` | Counter | `user`, `subscription`, `model` | **Billing/Cost** - Total tokens consumed (input + output) |
| `authorized_calls` | Counter | `user`, `subscription` | **API Usage** - Number of API calls allowed |
| `limited_calls` | Counter | `user`, `subscription` | **Rate Limiting** - Requests denied due to quotas |

!!! note "`model` label availability"
    `model` label is only available on `authorized_hits`. `authorized_calls` and `limited_calls` carry `user` and `subscription` only.

!!! note "Total tokens only"
    Token consumption is reported as total tokens (prompt + completion) per request. Input/output split requires upstream Kuadrant wasm-shim changes.

### Latency Metrics

| Metric | Labels | Description |
|--------|--------|-------------|
| `istio_request_duration_milliseconds_bucket` | `destination_service_name`, `subscription` | Gateway-level latency (subscription-only to bound cardinality) |
| `vllm:e2e_request_latency_seconds` | `model_name` | Model inference end-to-end latency |
| `vllm:time_to_first_token_seconds` | `model_name` | Time to First Token (TTFT) |
| `vllm:inter_token_latency_seconds` | `model_name` | Inter-Token Latency (ITL) |

#### Per-Subscription Latency Tracking

Istio Telemetry adds `subscription` dimension to gateway latency via `X-MaaS-Subscription` header injected by AuthPolicy:

```yaml
apiVersion: telemetry.istio.io/v1
kind: Telemetry
metadata:
  name: latency-per-subscription
spec:
  metrics:
  - overrides:
    - match:
        metric: REQUEST_DURATION
      tagOverrides:
        subscription:
          value: request.headers["x-maas-subscription"]
```

### Limitador Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `limitador_up` | Gauge | Limitador running (1 = up) |
| `datastore_partitioned` | Gauge | Partitioned from datastore (0 = healthy) |
| `datastore_latency` | Histogram | Latency to backing datastore |

See [Limitador source](https://github.com/Kuadrant/limitador/blob/main/limitador-server/src/prometheus_metrics.rs).

### Authorino Metrics

Exposed on `/server-metrics` (port 8080):

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `auth_server_authconfig_total` | Counter | `namespace`, `authconfig` | Total AuthConfig evaluations |
| `auth_server_authconfig_duration_seconds` | Histogram | `namespace`, `authconfig` | Auth evaluation latency |
| `auth_server_authconfig_response_status` | Counter | `namespace`, `authconfig`, `status` | Auth response status (OK, denied) |
| `auth_server_response_status` | Counter | `status` | Aggregate auth response status |
| `auth_server_evaluator_total` | Counter | `namespace`, `authconfig`, `evaluator_type`, `evaluator_name` | Per-evaluator runs (MaaS enables `metrics` on **`apiKeyValidation`** / **`subscription-info`**) |
| `auth_server_evaluator_cancelled` | Counter | same | Failures/cancellations (metadata alert below) |

!!! note "Two endpoints"
    Kuadrant `authorino-operator-monitor` scrapes `/metrics` (controller-runtime). MaaS `authorino-server-metrics` ServiceMonitor scrapes `/server-metrics` (auth evaluation).

!!! note "Metadata evaluator metrics"
    Query `evaluator_type="METADATA_GENERIC_HTTP"` and `evaluator_name=~"apiKeyValidation|subscription-info"`. Series appear after traffic hits each evaluator.

**Alert:** `authorino-maas-metadata-evaluator-prometheusrule.yaml` — **`MaaSAuthorinoMetadataEvaluatorHighFailureRate`** (`cancelled`/`total` > 10% over 5m, traffic guard, **`for: 5m`**). **Remediate:** maas-api health; Authorino → maas-api TLS/NetworkPolicy; confirm **`/server-metrics`** is scraped.

**Authentication Alerts:** `authorino-maas-authentication-alerts` — two alerts for gateway authentication health (covers all auth methods: OIDC/JWT, API key, TokenReview):

| Alert | Condition | Severity |
|-------|-----------|----------|
| **`MaaSAuthorinoAuthenticationHighFailureRate`** | >10% of auth attempts return `UNAUTHENTICATED` over 5m | warning |
| **`MaaSAuthorinoAuthenticationHighLatency`** | P95 `auth_server_authconfig_duration_seconds` >2s over 5m | warning |

**Remediate:** IdP (Keycloak/OIDC provider) health; JWKS endpoint reachability; API key / token validity; consider increasing `Tenant.spec.externalOIDC.ttl` if IdP is slow but reliable. See [External OIDC Configuration](../advanced-administration/external-oidc.md).

### vLLM Metrics

Exposed on `/metrics` (port 8000). Supported backends: vLLM v0.7.x, llm-d v0.1.x, llm-d-inference-sim v0.8.2.

| Metric | Type | Description |
|--------|------|-------------|
| `vllm:num_requests_running` | Gauge | Requests currently processing |
| `vllm:num_requests_waiting` | Gauge | Requests queued waiting |
| `vllm:request_prompt_tokens` | Histogram | Per-request prompt token counts (`_sum` gives cumulative) |
| `vllm:request_generation_tokens` | Histogram | Per-request generation token counts |
| `vllm:prompt_tokens_total` | Counter | Total prompt tokens processed |
| `vllm:generation_tokens_total` | Counter | Total generation tokens processed |
| `vllm:kv_cache_usage_perc` | Gauge | KV-cache usage (0-1) |
| `vllm:request_queue_time_seconds` | Histogram | Time in queue before processing (vLLM/llm-d only) |
| `vllm:request_success_total` | Counter | Successful requests |
| `vllm:request_prefill_time_seconds` | Histogram | Prefill phase time |
| `vllm:request_decode_time_seconds` | Histogram | Decode phase time |

!!! note "Counter `_total` suffix"
    Python prometheus_client appends `_total` when exposing counters. The actual metric names are `vllm:prompt_tokens_total` and `vllm:generation_tokens_total`.

!!! note "Lazily registered"
    Some metrics only appear after first event (e.g., `request_queue_time_seconds` after first queued request). Dashboard panels show "No Data" until traffic is generated.

See [vLLM metrics docs](https://docs.vllm.ai/en/stable/usage/metrics/).

## Common Queries

**Token consumption (billing):**

```promql
# Total tokens per user
sum by (user) (authorized_hits)

# Token rate per model (tokens/sec)
sum by (model) (rate(authorized_hits[5m]))

# Top 10 users by tokens
topk(10, sum by (user) (authorized_hits))
```

**Request volume (capacity):**

```promql
# Request rate per subscription
sum by (subscription) (rate(authorized_calls[5m]))

# Top 10 users by request count
topk(10, sum by (user) (authorized_calls))
```

**Inference success rate:**

```promql
# Success rate (defaults to 100% when no data)
sum(rate(vllm:request_success_total[5m])) /
  sum(rate(vllm:e2e_request_latency_seconds_count[5m])) OR vector(1)
```

**Rate limiting:**

```promql
# Rate limit ratio (% rejected)
(sum(limited_calls) / (sum(authorized_calls) + sum(limited_calls))) OR vector(0)

# Rate limit violations per second by subscription
sum by (subscription) (rate(limited_calls[5m]))
```

**Latency (per-subscription SLA):**

```promql
# P99 latency per subscription
histogram_quantile(0.99, sum by (subscription, le)
  (rate(istio_request_duration_milliseconds_bucket{subscription!=""}[5m])))

# P50 latency per subscription
histogram_quantile(0.5, sum by (subscription, le)
  (rate(istio_request_duration_milliseconds_bucket{subscription!=""}[5m])))
```

## Perses Dashboards

Usage dashboards appear in the ODH/RHOAI observability console. Enable them with operator-managed telemetry (and optionally `usageLogging`) — see [Setup](setup.md). Operator vs Kustomize ownership and cleanup: [Operations](operations.md#cleanup).

| Dashboard | When | Contents |
|-----------|------|----------|
| **Usage** (`dashboard-3-maas-usage-admin`) | Operator (`ensureUsageDashboard`) or Kustomize dashboards overlay | Token consumption overview and per-user usage (Prometheus) |
| **All usage (logs)** (`dashboard-4-maas-usage-logs-admin`) | `usageLogging: true` | Admin usage from structured access logs (Loki) |
| **Usage (logs)** (`dashboard-5-maas-usage-logs`) | `usageLogging: true` | User-scoped usage from structured access logs |

!!! note "Inference Success Rate"
    Dashboards use `rate()` on vLLM counters to handle pod restarts correctly. `NaN` (when no traffic) is filtered to default to 100%.

!!! info "Tokens vs Requests"
    **Token consumption** (`authorized_hits`) for billing/cost. **Request counts** (`authorized_calls`) for capacity planning.
