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

**Per-Subscription Latency Tracking:**

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

!!! note "Two endpoints"
    Kuadrant `authorino-operator-monitor` scrapes `/metrics` (controller-runtime). MaaS `authorino-server-metrics` ServiceMonitor scrapes `/server-metrics` (auth evaluation).

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
# Success rate (handles counter resets)
((sum(rate(vllm:request_success_total[5m])) / 
  sum(rate(vllm:e2e_request_latency_seconds_count[5m]))) >= 0) OR vector(1)
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

## Grafana Dashboards

MaaS includes two dashboards for different personas.

### Platform Admin Dashboard

| Section | Metrics |
|---------|---------|
| **Component Health** | Limitador up, Authorino pods, MaaS API pods, Gateway pods, Alerts |
| **Key Metrics** | Total Tokens, Total Requests, Token Rate, Request Rate, Success Rate, Active Users, P50 Latency, Rate Limit Ratio |
| **Auth Evaluation** | Auth Latency (P50/P95/P99), Auth Success/Deny Rate |
| **Traffic Analysis** | Token/Request Rate by Model, Error Rates (4xx, 5xx, 429), Token/Request by Subscription, P95 Latency |
| **Model Metrics** | vLLM queue depth, latency, KV cache, token throughput, prompt/gen ratio, queue wait, TTFT, ITL |
| **Top Users** | By token usage, by declined requests |
| **Resource Allocation** | CPU/Memory/GPU per model pod |

**Template variables:**

- `$datasource`: `prometheus`
- `$maas_namespace`: Auto-detected from `kube_pod_info{pod=~"maas-api.*"}`
- `$kuadrant_namespace`: `kuadrant-system`
- `$gateway_namespace`: `openshift-ingress`
- `$llm_namespace`: `llm`
- `$model`: `All`

### AI Engineer Dashboard

| Section | Metrics |
|---------|---------|
| **Usage Summary** | My Total Tokens, My Total Requests, Token Rate, Request Rate, Rate Limit Ratio, Success Rate |
| **Usage Trends** | Token Usage by Model, Usage vs Rate Limited |
| **Detailed Analysis** | Token Volume by Model, Rate Limited by Subscription |

!!! note "Inference Success Rate"
    Dashboards use `rate()` on vLLM counters to handle pod restarts correctly. `NaN` (when no traffic) is filtered to default to 100%.

!!! info "Tokens vs Requests"
    **Token consumption** (`authorized_hits`) for billing/cost. **Request counts** (`authorized_calls`) for capacity planning. Blue = requests, Green = tokens.

### Prerequisites

- **Grafana** installed (via observability team, centralized instance, or [Grafana Operator](https://grafana.github.io/grafana-operator/docs/installation/))
- Grafana instance has label `app=grafana`
- **Prometheus datasource** configured in Grafana

### Deploy Dashboards

```bash
./scripts/observability/install-grafana-dashboards.sh
```

**Behavior:** Discovers Grafana cluster-wide. Deploys to namespace if one instance found. Warns if none or multiple.

**Target specific instance:**

```bash
./scripts/observability/install-grafana-dashboards.sh --grafana-namespace maas-api
./scripts/observability/install-grafana-dashboards.sh --grafana-label app=grafana
```

**Manual deployment:**

```bash
kustomize build deployment/components/observability/grafana | \
  sed "s/namespace: maas-api/namespace: <your-namespace>/g" | \
  kubectl apply -f -
```

### Sample Dashboard JSON

For manual import: [MaaS Token Metrics Dashboard](https://github.com/opendatahub-io/models-as-a-service/blob/main/docs/samples/dashboards/maas-token-metrics-dashboard.json)

1. Grafana → Dashboards → Import
2. Upload JSON or paste content
3. Select Prometheus datasource
