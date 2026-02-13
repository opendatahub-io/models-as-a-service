# Observability

This document covers the observability stack for the MaaS Platform, including metrics collection, monitoring, and visualization.

!!! warning "Important"
    [User Workload Monitoring](https://docs.redhat.com/en/documentation/monitoring_stack_for_red_hat_openshift/4.19/html-single/configuring_user_workload_monitoring/index#enabling-monitoring-for-user-defined-projects_preparing-to-configure-the-monitoring-stack-uwm) must be enabled in order to collect metrics.

    Add `enableUserWorkload: true` to the `cluster-monitoring-config` in the `openshift-monitoring` namespace

## Overview

As part of Dev Preview, MaaS Platform includes a basic observability stack that provides insights into system performance, usage patterns, and operational health.

!!! note
    The observability stack will be enhanced in future releases.

The observability stack consists of:

- **Limitador**: Rate limiting service that exposes usage and rate-limit metrics (with labels from TelemetryPolicy)
- **Authorino**: Authentication/authorization service that exposes auth evaluation metrics (`auth_server_*`)
- **Istio Telemetry**: Adds `tier` to gateway latency metrics for per-tier latency (P50/P95/P99)
- **vLLM / llm-d / Simulator**: Expose inference metrics (TTFT, ITL, queue depth, token throughput, KV-cache usage); llm-d also exposes EPP routing metrics
- **Prometheus**: Metrics collection and storage (uses OpenShift platform Prometheus)
- **ServiceMonitors**: Deployed to configure Prometheus metric scraping
- **Visualization**: Grafana dashboards (see [Grafana documentation](https://grafana.com/docs/grafana/latest/))

### Component Metrics Status

| Component | Exposes Metrics? | Scraped into Prometheus? | In Dashboards? |
|-----------|-----------------|--------------------------|----------------|
| **Limitador** | Yes (`/metrics`) | Yes (Kuadrant PodMonitor or MaaS ServiceMonitor) | Yes — 16 panels use `authorized_hits`, `authorized_calls`, `limited_calls`, `limitador_up` |
| **Authorino** | Yes (`/metrics` + `/server-metrics`) | Yes — `/metrics` via Kuadrant operator; `/server-metrics` via MaaS `authorino-server-metrics` ServiceMonitor | Yes — Auth Evaluation Latency (P50/P95/P99), Auth Success/Deny Rate, plus pod-up check |
| **Istio Gateway** | Yes (Envoy `/stats/prometheus`) | Yes (`istio-gateway-metrics` ServiceMonitor) | Yes — latency histograms, request counts, error rates |
| **maas-api** | **No** — returns 404 on `/metrics` | No | Only pod-up check via `kube_pod_status_phase` |
| **vLLM / llm-d / Simulator** | Yes (vLLM metrics on `/metrics` port 8000; llm-d EPP metrics on port 9090) | Yes — vLLM metrics via `kserve-llm-models` ServiceMonitor; EPP metrics require separate scrape config | Yes — TTFT, ITL, queue depth, latency, tokens, cache, prompt/generation ratio, queue wait time (EPP metrics not yet in MaaS dashboards) |

!!! warning "maas-api Metrics Gap"
    The maas-api Go service does **not** expose a `/metrics` endpoint. Metrics such as API key creation rate, token issuance rate, model discovery latency, and request handler durations are not available in Prometheus. Adding Prometheus instrumentation (e.g. `promhttp` handler + application-specific counters/histograms) to the Go service is a recommended future improvement.

## Installation

The observability stack is defined in `deployment/base/observability/`. It includes:

| Resource | Purpose |
|----------|---------|
| **TelemetryPolicy** (`telemetry-policy.yaml`) | Adds `user`, `tier`, and `model` labels to Limitador metrics. The `model` label (from `responseBodyJSON`) is available on `authorized_hits`; `authorized_calls` and `limited_calls` carry `user` and `tier`. |
| **Istio Telemetry** (`istio-telemetry.yaml`) | Adds `tier` label to gateway latency (`istio_request_duration_milliseconds_bucket`) for per-tier P50/P95/P99. |

**Deploy observability** (after Gateway and AuthPolicy are in place, so `X-MaaS-Tier` is injected):

    ./scripts/install-observability.sh [--namespace NAMESPACE]

When using the full deployment script, this is applied automatically:

    ./scripts/deploy.sh

!!! note "Prerequisites"
    - **Tools**: `kubectl`, `kustomize`, `jq`, `yq` must be installed
    - **Cluster state**: Gateway, AuthPolicy (gateway-auth-policy), and tier lookup must be deployed first. The AuthPolicy injects `X-MaaS-Tier`, which Istio Telemetry reads to label latency by tier. Without it, the `tier` label on gateway latency will be empty.
    - **Namespace**: Use `--namespace` if your MaaS API is deployed to a namespace other than `maas-api` (e.g. `--namespace opendatahub`)

**Optional:** To scrape the Istio gateway (Envoy) metrics, use the ServiceMonitor in `deployment/components/observability/monitors/` if your deployment includes that component.

## Metrics Collection

### Limitador Metrics

Limitador exposes the following Prometheus metrics (verified against [Limitador source code](https://github.com/Kuadrant/limitador/blob/main/limitador-server/src/prometheus_metrics.rs)):

#### Core Limitador Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `limitador_up` | Gauge | — | Limitador is running (1 = up) |
| `datastore_partitioned` | Gauge | — | Limitador is partitioned from backing datastore (0 = healthy) |
| `datastore_latency` | Histogram | — | Latency to the underlying counter datastore |

#### MaaS Usage Metrics (Limitador + TelemetryPolicy)

When Kuadrant TelemetryPolicy and TokenRateLimitPolicy are applied, Limitador exposes these counters with custom labels injected by the wasm-shim from auth context and the model response body. These are the primary metrics for usage dashboards and chargeback:

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `authorized_hits` | Counter | `user`, `tier`, `model`, `limitador_namespace` | Total tokens consumed per request (from `usage.total_tokens` in the model response; input + output combined). The `model` label is extracted via `responseBodyJSON("/model")`. |
| `authorized_calls` | Counter | `user`, `tier`, `limitador_namespace` | Requests allowed (not rate-limited). |
| `limited_calls` | Counter | `user`, `tier`, `limitador_namespace` | Requests denied due to token rate limits. |

!!! note "`model` label availability"
    The `model` label is currently available **only on `authorized_hits`**. The `authorized_calls` and `limited_calls` metrics carry `user` and `tier` labels but not `model`, due to how the wasm-shim constructs the CEL evaluation context for these counters. This is a known upstream limitation tracked for improvement in Kuadrant.

Gateway latency is labeled by **tier only** via Istio Telemetry (see [Per-Tier Latency Tracking](#per-tier-latency-tracking)); per-user latency is not exposed on the gateway histogram to keep cardinality bounded.

### Authorino Metrics

Authorino exposes metrics on two separate endpoints:

| Endpoint | Metrics | Scraped? |
|----------|---------|----------|
| `/metrics` | Controller-runtime (reconcile counts, workqueue depth) | Yes (`authorino-operator-monitor`, provided by Kuadrant) |
| `/server-metrics` | Auth evaluation metrics (see below) | Yes (`authorino-server-metrics`, deployed by MaaS `install-observability.sh`) |

**Auth server metrics** (exposed on `/server-metrics`, port 8080):

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `auth_server_authconfig_total` | Counter | `namespace`, `authconfig` | Total AuthConfig evaluations |
| `auth_server_authconfig_duration_seconds` | Histogram | `namespace`, `authconfig` | Auth evaluation latency |
| `auth_server_authconfig_response_status` | Counter | `namespace`, `authconfig`, `status` | Auth response status per AuthConfig (OK, denied, etc.) |
| `auth_server_response_status` | Counter | `status` | Aggregate auth response status across all AuthConfigs |
| `grpc_server_handled_total` | Counter | `grpc_method`, `grpc_code` | gRPC requests handled |
| `grpc_server_handling_seconds` | Histogram | `grpc_method` | gRPC request latency |
| `grpc_server_msg_received_total` | Counter | `grpc_method` | gRPC messages received |
| `grpc_server_msg_sent_total` | Counter | `grpc_method` | gRPC messages sent |
| `grpc_server_started_total` | Counter | `grpc_method` | gRPC requests started |

!!! note "MaaS ServiceMonitor"
    The Kuadrant-provided `authorino-operator-monitor` only scrapes `/metrics` (controller-runtime stats). MaaS deploys an additional `authorino-server-metrics` ServiceMonitor to scrape `/server-metrics` for auth evaluation metrics. This is deployed automatically by `install-observability.sh`.

!!! note "Lazily registered metrics"
    Authorino upstream [documents](https://github.com/Kuadrant/authorino/blob/main/docs/user-guides/observability.md) additional per-evaluator metrics (`auth_server_evaluator_total`, `auth_server_evaluator_duration_seconds`, `auth_server_evaluator_cancelled`, `auth_server_evaluator_denied`). These are **lazily registered** and only appear when specific evaluator types (e.g. OPA, HTTP authorization) are triggered. The MaaS AuthPolicy uses `kubernetesTokenReview`, which does not emit these metrics. They are not listed in the table above because they are not present in a standard MaaS deployment.

### vLLM / Model Server Metrics

MaaS supports three model serving backends that expose Prometheus metrics on `/metrics` (port 8000), scraped by the `kserve-llm-models` ServiceMonitor:

- **vLLM** (current stable) — full-featured LLM inference server
- **llm-d** — llm-d inference platform (runs vLLM as backend + EPP routing layer)
- **llm-d-inference-sim** (v0.7.1) — lightweight simulator for testing without GPUs

**Supported versions:**

| Backend | Minimum Version | Sample Manifests |
|---------|----------------|------------------|
| vLLM | v0.7.x stable | — |
| llm-d | v0.1.x | — |
| llm-d-inference-sim | **v0.7.1** | `docs/samples/models/simulator/` |

#### vLLM Metrics (port 8000)

All three backends expose `vllm:`-prefixed metrics. The table below shows which metrics each backend provides.

| Metric | Type | Simulator | vLLM | llm-d | Description |
|--------|------|:---------:|:----:|:-----:|-------------|
| `vllm:num_requests_running` | Gauge | Y | Y | Y | Requests currently being processed |
| `vllm:num_requests_waiting` | Gauge | Y | Y | Y | Requests queued waiting for processing |
| `vllm:e2e_request_latency_seconds` | Histogram | Y | Y | Y | End-to-end inference latency |
| `vllm:time_to_first_token_seconds` | Histogram | Y | Y | Y | Time to First Token (TTFT) |
| `vllm:request_prompt_tokens` | Histogram | Y | Y | Y | Per-request prompt token counts (`_sum` gives cumulative total) |
| `vllm:request_generation_tokens` | Histogram | Y | Y | Y | Per-request generation token counts (`_sum` gives cumulative total) |
| `vllm:inter_token_latency_seconds` | Histogram | Y | Y | Y | Inter-Token Latency (ITL) |
| `vllm:kv_cache_usage_perc` | Gauge | Y | Y | Y | KV-cache usage (0-1) |
| `vllm:prompt_tokens_total` | Counter | Y | Y | Y | Total prompt tokens processed |
| `vllm:generation_tokens_total` | Counter | Y | Y | Y | Total generation tokens processed |
| `vllm:request_queue_time_seconds` | Histogram | — | Y | Y | Time requests wait in queue before processing (vLLM/llm-d only) |
| `vllm:request_success_total` | Counter | Y | Y | Y | Successful requests (`_total` suffix added by prometheus_client) |
| `vllm:request_prefill_time_seconds` | Histogram | Y | Y | Y | Time spent in prefill (prompt processing) phase |
| `vllm:request_decode_time_seconds` | Histogram | Y | Y | Y | Time spent in decode (token generation) phase |
| `vllm:request_inference_time_seconds` | Histogram | Y | — | — | Total inference time (simulator-specific) |
| `vllm:request_params_max_tokens` | Histogram | Y | — | — | Distribution of `max_tokens` request parameter |
| `vllm:max_num_generation_tokens` | Histogram | Y | — | — | Max generation tokens per request |
| `vllm:lora_requests_info` | Gauge | Y | — | — | LoRA adapter request info |
| `vllm:cache_config_info` | Gauge | Y | — | — | Cache configuration info (simulator-specific) |
| `vllm:time_per_output_token_seconds` | Histogram | Y | — | — | Legacy ITL name (kept by simulator for backward compat; not used by dashboards) |

!!! note "Simulator metric alignment"
    As of v0.7.1, the simulator fully aligns with current vLLM metric names (`kv_cache_usage_perc`, `inter_token_latency_seconds`, `prompt_tokens_total`, `generation_tokens_total`). Older simulator versions (v0.6.x) used different names (`gpu_cache_usage_perc`, `time_per_output_token_seconds`) and are **no longer supported** by MaaS dashboards. The simulator also exposes additional metrics not used by MaaS dashboards (e.g. `request_inference_time_seconds`, `request_params_max_tokens`).

!!! note "Lazily registered metrics"
    Some vLLM/simulator metrics are **lazily registered** — they only appear in `/metrics` output after the first event that triggers them. For example, `request_queue_time_seconds` (on real vLLM) only appears after a request actually queues (when `max-num-seqs` is exceeded). Similarly, histogram counters like `e2e_request_latency_seconds` only appear after the first inference request completes. Dashboard panels will show "No Data" until sufficient traffic has been generated. This is normal Prometheus client behavior, not a configuration issue.

!!! note "Counter `_total` suffix"
    vLLM code defines counters as `vllm:prompt_tokens` and `vllm:generation_tokens`, but the Python prometheus_client library appends `_total` when exposing metrics. The **actual scraped metric names** in Prometheus are `vllm:prompt_tokens_total` and `vllm:generation_tokens_total`. The [llm-d official dashboard](https://github.com/llm-d/llm-d/blob/main/docs/monitoring/grafana/dashboards/llm-d-vllm-overview.json) confirms this by using the `_total` form.

#### llm-d EPP (Endpoint Picker) Metrics

When using llm-d, the inference gateway's Endpoint Picker (EPP) exposes additional routing and scheduling metrics on a **separate port (9090)**. These are complementary to vLLM metrics and require a separate ServiceMonitor:

| Metric | Type | Description |
|--------|------|-------------|
| `inference_model_request_total` | Counter | Total inference requests per model |
| `inference_model_request_error_total` | Counter | Total errored requests per model |
| `inference_model_request_duration_seconds` | Histogram | Request duration through the EPP |
| `inference_model_input_tokens` | Counter | Input tokens routed per model |
| `inference_model_output_tokens` | Counter | Output tokens routed per model |
| `inference_model_running_requests` | Gauge | Currently running requests per model |
| `inference_pool_average_kv_cache_utilization` | Gauge | Average KV-cache utilization across the pool |
| `inference_pool_average_queue_size` | Gauge | Average queue size across the pool |
| `inference_pool_ready_pods` | Gauge | Number of ready pods in the inference pool |

!!! info "EPP metrics not yet in MaaS dashboards"
    EPP metrics are not currently scraped or visualized by MaaS. When deploying llm-d with the EPP, refer to the [llm-d monitoring docs](https://llm-d.ai/docs/usage/monitoring) and the [inference gateway dashboard](https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/v1.0.1/tools/dashboards/inference_gateway.json) for EPP-specific visualization.

!!! note "Input/Output Token Split"
    vLLM metrics provide input vs output token breakdown **per model** (`vllm:prompt_tokens_total` / `vllm:generation_tokens_total` counters, or `vllm:request_prompt_tokens` / `vllm:request_generation_tokens` histograms). However, these do not carry `user` or `tier` labels. For per-user billing with input/output split, upstream changes to the Kuadrant wasm-shim are required (see [Known Limitations](#known-limitations)).

#### Dashboard Metric Queries

Dashboard panels use histogram `_sum` as primary data source. All queries work across vLLM, llm-d, and llm-d-inference-sim v0.7.1:

| Panel | PromQL metric |
|-------|---------------|
| Tokens (1h) | `request_prompt_tokens_sum` + `request_generation_tokens_sum` |
| Token Throughput | `rate(request_prompt_tokens_sum)`, `rate(request_generation_tokens_sum)` |
| Prompt/Gen Ratio | `rate(request_prompt_tokens_sum)` / total |
| ITL | `inter_token_latency_seconds_bucket` |
| KV Cache | `kv_cache_usage_perc` |
| Queue Wait Time | `request_queue_time_seconds_bucket` (vLLM/llm-d only) |

See the [vLLM metrics documentation](https://docs.vllm.ai/en/stable/usage/metrics/) for the full vLLM metric list and deprecation policy, and the [llm-d monitoring documentation](https://llm-d.ai/docs/usage/monitoring) for llm-d-specific setup.

### ServiceMonitor Configuration

ServiceMonitors are deployed by `install-observability.sh` to configure OpenShift's Prometheus to discover and scrape metrics from MaaS components.

**Automatically Deployed:**

- **Istio Gateway**: Scrapes Envoy metrics from the MaaS gateway in `openshift-ingress` (deployed if the gateway exists)
- **KServe LLM Models**: Scrapes vLLM metrics from model pods in the `llm` namespace (deployed if the `llm` namespace exists)

**Conditionally Deployed (auto-detected by `install-observability.sh`):**

- **Limitador** (`servicemonitor.yaml`): Scrapes rate limiting metrics from Limitador pods in `kuadrant-system`. **Skipped when Kuadrant's own PodMonitor is already present.** When Kuadrant CR has `spec.observability.enable: true`, the operator creates its own `kuadrant-limitador-monitor` PodMonitor that scrapes the same Limitador pod. Deploying both would cause duplicate metrics.
- **Authorino Server Metrics** (`authorino-server-metrics-servicemonitor.yaml`): Scrapes auth evaluation metrics from Authorino's `/server-metrics` endpoint in `kuadrant-system`. **Skipped if a Kuadrant-provided monitor already scrapes `/server-metrics`.** This collects `auth_server_authconfig_duration_seconds`, `auth_server_authconfig_response_status`, and other auth server metrics that are **not** scraped by the Kuadrant-provided `authorino-operator-monitor` (which only covers `/metrics` for controller-runtime stats).

**Already Provided by Kuadrant (when `observability.enable: true`):**

- **Limitador PodMonitor** (`kuadrant-limitador-monitor`): Created by the Kuadrant operator
- **Authorino Operator Monitor** (`authorino-operator-monitor`): Scrapes Authorino controller metrics from `/metrics` only

!!! note "Authorino Metrics Coverage"
    The Kuadrant-provided `authorino-operator-monitor` only scrapes `/metrics` (controller-runtime stats). The MaaS `authorino-server-metrics` ServiceMonitor supplements this by scraping `/server-metrics` for auth evaluation metrics (`auth_server_authconfig_duration_seconds`, `auth_server_authconfig_response_status`, etc.). The `install-observability.sh` script auto-detects whether a Kuadrant-provided monitor already scrapes `/server-metrics` and skips deploying the MaaS ServiceMonitor to avoid duplicates. See [Authorino Observability](https://docs.kuadrant.io/1.0.x/authorino/docs/user-guides/observability/) for details.

## High Availability for MaaS Metrics

For production deployments where metric persistence across pod restarts and scaling events is critical, you should configure Limitador to use Redis as a backend storage solution.

### Why High Availability Matters

By default, Limitador stores rate-limiting counters in memory, which means:

- All hit counts are lost when pods restart
- Metrics reset when pods are rescheduled or scaled down
- No persistence across cluster maintenance or updates

### Setting Up Persistent Metrics

To enable persistent metric counts, refer to the detailed guide:

**[Configuring Redis storage for rate limiting](https://docs.redhat.com/en/documentation/red_hat_connectivity_link/1.1/html/installing_connectivity_link_on_openshift/configure-redis_connectivity-link)**

This Red Hat documentation provides:

- Step-by-step Redis configuration for OpenShift
- Secret management for Redis credentials
- Limitador custom resource updates
- Production-ready setup instructions

For local development and testing, you can also use our [Limitador Persistence](limitador-persistence.md) guide which includes a basic Redis setup script that works with any Kubernetes cluster.

## Visualization

For dashboard visualization options, see:

- **OpenShift Monitoring**: [Monitoring overview](https://docs.redhat.com/en/documentation/openshift_container_platform/4.19/html/monitoring/index)
- **Grafana on OpenShift**: [Red Hat OpenShift AI Monitoring](https://docs.redhat.com/en/documentation/red_hat_openshift_ai_self-managed/2.25/html/managing_and_monitoring_models/index)

### Included Dashboards

MaaS includes two Grafana dashboards for different personas:

#### Platform Admin Dashboard

Provides a comprehensive view of system health, usage across all users, and resource allocation:

| Section | Metrics |
|---------|---------|
| **Component Health** | Limitador up, Authorino pods, MaaS API pods, Gateway pods, Firing Alerts |
| **Key Metrics** | Total Tokens, Total Requests, Token Rate, Request Rate, Inference Success Rate, Active Users, P50 Response Latency, Rate Limit Ratio |
| **Auth Evaluation** | Auth Evaluation Latency (P50/P95/P99), Auth Success/Deny Rate |
| **Traffic Analysis** | Token/Request Rate by Model, Error Rates (4xx excl. 429, 5xx, 429 Rate Limited), Token/Request Rate by Tier, P95 Latency |
| **Error Breakdown** | Rate Limited Requests, Unauthorized Requests |
| **Model Metrics** | vLLM queue depth, inference latency, KV cache usage, token throughput, prompt vs generation token ratio, queue wait time, TTFT, ITL |
| **Top Users** | By token usage, by declined requests |
| **Detailed Breakdown** | Token Rate by User, Request Volume by User & Tier |
| **Resource Allocation** | CPU/Memory/GPU per model pod |

!!! note "Template Variables"
    The Platform Admin dashboard uses Grafana template variables for namespace filtering instead of hardcoded values. This allows the dashboard to adapt to different deployment configurations:

    | Variable | Default | Description |
    |----------|---------|-------------|
    | `$datasource` | `prometheus` | Prometheus datasource |
    | `$maas_namespace` | auto-detected | MaaS API namespace (auto-detected from `kube_pod_info{pod=~"maas-api.*"}`) |
    | `$kuadrant_namespace` | `kuadrant-system` | Kuadrant components namespace |
    | `$gateway_namespace` | `openshift-ingress` | Istio/Gateway namespace |
    | `$llm_namespace` | `llm` | LLM model pods namespace |
    | `$model` | `All` | Filter by model name |

    To customize for your environment, change the variable values in Grafana's dashboard settings (gear icon → Variables).

#### AI Engineer Dashboard

Personal usage view for individual developers:

| Section | Metrics |
|---------|---------|
| **Usage Summary** | My Total Tokens, My Total Requests, Token Rate, Request Rate, Rate Limit Ratio, Inference Success Rate |
| **Usage Trends** | Token Usage by Model, Usage Trends (tokens vs rate limited) |
| **Detailed Analysis** | Token Volume by Model, Rate Limited by Tier |

!!! note "Inference Success Rate"
    Both dashboards use `rate()` on vLLM counters (`request_success_total`, `e2e_request_latency_seconds_count`) instead of raw counter values. This handles pod restarts correctly (counters reset independently and raw division produces incorrect results). When no traffic is present, `rate()/rate()` produces `NaN`; the dashboards use `((ratio) >= 0) OR vector(1)` to filter `NaN` and default to 100% (healthy) when no traffic exists.

!!! info "Tokens vs Requests"
    Both dashboards show **token consumption** (`authorized_hits`) for billing/cost tracking and **request counts** (`authorized_calls`) for capacity planning. Blue panels indicate request metrics; green panels indicate token metrics.

!!! tip "Per-User Token Billing"
    The **Platform Admin dashboard** shows token consumption aggregated by **tier** and **model** for system-level visibility. Per-user token consumption for billing is available via:

    - **AI Engineer dashboard**: Individual users see their own token usage
    - **Prometheus API**: Query `sum by (user) (increase(authorized_hits[24h]))` for billing periods
    - **RFE**: A dedicated `/maas-api/v1/usage` chargeback API endpoint is recommended for production billing workflows

### Prerequisites

- **Grafana** must be installed (for example via your observability team's process, a centralized instance, or the [Grafana Operator](https://grafana.github.io/grafana-operator/docs/installation/)). The dashboard helper does **not** install Grafana; it only deploys MaaS dashboard definitions and **never fails** (warnings only if none or multiple instances are found).
- Ensure the Grafana instance has label `app=grafana` so MaaS dashboard definitions attach.
- Configure a **Prometheus or Thanos datasource** in Grafana; the MaaS dashboards use the default Prometheus datasource.

### Deploying Dashboards

Monitoring is installed by `install-observability.sh`. Dashboards are installed by a **separate helper** that discovers Grafana cluster-wide:

    ./scripts/install-grafana-dashboards.sh

**Behavior:** Scans for Grafana CRs cluster-wide. If **one** instance is found, deploys dashboards to that namespace and prints a success message. If **none** or **multiple** are found, prints a warning (and, for multiple, lists them) and exits without error. Use flags to target a specific instance:

    ./scripts/install-grafana-dashboards.sh --grafana-namespace maas-api
    ./scripts/install-grafana-dashboards.sh --grafana-label app=grafana

To deploy only the dashboard manifests manually (same namespace as your Grafana):

    kustomize build deployment/components/observability/dashboards | \
      sed "s/namespace: maas-api/namespace: <your-namespace>/g" | \
      kubectl apply -f -

### Sample Dashboard JSON

For manual import, a sample dashboard JSON file is available:

- [MaaS Token Metrics Dashboard](https://github.com/opendatahub-io/models-as-a-service/blob/main/docs/samples/dashboards/maas-token-metrics-dashboard.json)

To import into Grafana:

1. Go to Grafana → Dashboards → Import
2. Upload the JSON file or paste content
3. Select your Prometheus datasource

## Key Metrics Reference

### Token and Request Metrics

| Metric | Description | Labels |
|--------|-------------|--------|
| `authorized_hits` | Total tokens consumed (input + output combined, from `usage.total_tokens` in model responses) | `user`, `tier`, `model` |
| `authorized_calls` | Total requests allowed | `user`, `tier` |
| `limited_calls` | Total requests rate-limited | `user`, `tier` |

!!! tip "When to use which metric"
    - **Billing/Cost**: Use `authorized_hits` - represents actual token consumption, with `model` label for per-model breakdown
    - **API Usage**: Use `authorized_calls` - represents number of API calls (per user, per tier)
    - **Rate Limiting**: Use `limited_calls` - shows quota violations (per user, per tier)

!!! note "Total tokens only (input/output split not yet available)"
    Token consumption is reported as **total tokens** (prompt + completion) per request. The pipeline reads `usage.total_tokens` from the model response via Kuadrant's TokenRateLimitPolicy. Separate input-token (`prompt_tokens`) and output-token (`completion_tokens`) counters are **not yet available** at the gateway level; this would require upstream changes in the Kuadrant wasm-shim to send separate `hits_addend` values for each token type. Chargeback and usage tracking per user, per subscription (tier), and per model are supported using `authorized_hits`.

### Gateway Traffic Metrics

| Metric | Description | Labels |
|--------|-------------|--------|
| `istio_requests_total` | Total gateway requests (used for error rate tracking: 4xx, 5xx, 401) | `response_code`, `destination_service_name` |
| `istio_request_duration_milliseconds_bucket` | Gateway-level latency histogram | `destination_service_name`, `tier` |

### Model Latency Metrics

| Metric | Description | Labels |
|--------|-------------|--------|
| `vllm:e2e_request_latency_seconds` | Model inference latency | `model_name` |

#### Per-Tier Latency Tracking

The MaaS Platform uses an Istio Telemetry resource to add a `tier` dimension to gateway latency metrics. This enables tracking request latency per access tier (e.g. free, premium, enterprise). Gateway latency is labeled by **tier only** (not by user) to keep metric cardinality bounded and to align with latency-by-tier requirements (e.g. P50/P95/P99 per tier). Per-user metrics remain available from Limitador (`authorized_hits`, `authorized_calls`, `limited_calls`).

**How it works:**

1. The `gateway-auth-policy` injects the `X-MaaS-Tier` header from the resolved tier
2. The Istio Telemetry resource extracts this header and adds it as a `tier` label to the `REQUEST_DURATION` metric
3. Prometheus scrapes these metrics from the Istio gateway

**Configuration** (`deployment/base/observability/istio-telemetry.yaml`):

    apiVersion: telemetry.istio.io/v1
    kind: Telemetry
    metadata:
      name: latency-per-tier
      namespace: openshift-ingress
    spec:
      selector:
        matchLabels:
          gateway.networking.k8s.io/gateway-name: maas-default-gateway
      metrics:
      - providers:
        - name: prometheus
        overrides:
        - match:
            metric: REQUEST_DURATION
            mode: CLIENT_AND_SERVER
          tagOverrides:
            tier:
              operation: UPSERT
              value: request.headers["x-maas-tier"]

!!! note "Security"
    The `X-MaaS-Tier` header should be injected server-side by AuthPolicy. Ensure your AuthPolicy injects this header from the tier lookup (not client input) for accurate metrics attribution.

### Common Queries

**Token-based queries (billing/cost):**

    # Total tokens consumed per user
    sum by (user) (authorized_hits)

    # Token consumption rate per model (tokens/sec)
    sum by (model) (rate(authorized_hits[5m]))

    # Top 10 users by tokens consumed
    topk(10, sum by (user) (authorized_hits))

    # Token consumption by tier
    sum by (tier) (authorized_hits)

**Request-based queries (capacity/usage):**

    # Total requests per user
    sum by (user) (authorized_calls)

    # Request rate per tier (requests/sec)
    sum by (tier) (rate(authorized_calls[5m]))

    # Top 10 users by request count
    topk(10, sum by (user) (authorized_calls))

**Inference success rate** (system health — did requests that reached the model succeed?):

    # Inference success rate using rate() to handle counter resets correctly
    # The >= 0 filter removes NaN (0/0 when no traffic), falling back to vector(1) = 100%
    ((sum(rate(vllm:request_success_total[5m])) / sum(rate(vllm:e2e_request_latency_seconds_count[5m]))) >= 0) OR vector(1)

**Rate limiting metrics** (capacity planning — are users exceeding their quotas?):

    # Rate limit ratio (percentage of requests rejected by rate limiting)
    (sum(limited_calls) / (sum(authorized_calls) + sum(limited_calls))) OR vector(0)

    # Rate limit ratio by tier
    (sum by (tier) (limited_calls) / (sum by (tier) (authorized_calls) + sum by (tier) (limited_calls))) OR vector(0)

    # Rate limit violations per second by tier
    sum by (tier) (rate(limited_calls[5m]))

    # Users hitting rate limits most
    topk(10, sum by (user) (limited_calls))

**Latency queries:**

    # P99 latency by service
    histogram_quantile(0.99, sum by (destination_service_name, le) (rate(istio_request_duration_milliseconds_bucket[5m])))

    # P50 (median) latency
    histogram_quantile(0.5, sum by (le) (rate(istio_request_duration_milliseconds_bucket[5m])))

    # P99 latency per tier
    histogram_quantile(0.99, sum by (tier, le) (rate(istio_request_duration_milliseconds_bucket{tier!=""}[5m])))

!!! tip "Filtering by tier"
    For per-tier latency queries, use `tier!=""` to exclude requests where the `X-MaaS-Tier` header was not injected. Token consumption metrics (`authorized_hits`, `authorized_calls`) from Limitador already only include successful requests.

## Maintenance

### Grafana Datasource Token Rotation

The Grafana datasource uses a ServiceAccount token to authenticate with Prometheus. This token is valid for **30 days** and must be rotated periodically.

**To rotate the token:**

    # Delete the existing datasource and create a new one (or rotate the token per your Grafana setup).
    # To re-deploy only MaaS dashboard definitions: ./scripts/install-grafana-dashboards.sh

!!! tip "Production Recommendation"
    For production deployments, consider automating token rotation using a CronJob or external secrets operator to avoid dashboard outages.

## Known Limitations

### Currently Blocked Features

Some features require upstream changes and are currently blocked:

| Feature | Blocker | Workaround |
|---------|---------|------------|
| **`model` label on `authorized_calls` / `limited_calls`** | Kuadrant wasm-shim does not pass `responseBodyJSON` context for these counters | Use `authorized_hits` for per-model breakdown; `authorized_calls`/`limited_calls` support per-user and per-tier |
| **Input/output token split** | Kuadrant TokenRateLimitPolicy sends a single `hits_addend` (total tokens); no mechanism for separate prompt/completion counters | Total tokens available via `authorized_hits`; the response body contains `usage.prompt_tokens` and `usage.completion_tokens` but the wasm-shim does not split them |
| **Input/output token breakdown per user** | vLLM does not label its own metrics with `user` | Total tokens per user available via `authorized_hits{user="..."}`; vLLM prompt/generation token metrics are per-model only |
| **Kuadrant policy health metrics** | `kuadrant_policies_enforced`, `kuadrant_policies_total` etc. are defined in Kuadrant dev but not yet shipped in RHCL 1.x | Enable `observability.enable: true` on the Kuadrant CR; the ServiceMonitors are created but policy-specific gauges will appear in a future operator release |
| **Authorino auth server metrics (upstream)** | The Kuadrant-provided `authorino-operator-monitor` only scrapes `/metrics` (controller-runtime); `/server-metrics` is not scraped by the upstream operator | **Resolved by MaaS**: The `authorino-server-metrics` ServiceMonitor (deployed by `install-observability.sh`) scrapes `/server-metrics`. Auth evaluation latency and success/deny rate are visualized in the Platform Admin dashboard. |
| **maas-api application metrics** | The maas-api Go service does not expose a `/metrics` endpoint | No workaround available. Metrics such as API key creation rate, token issuance rate, model discovery latency, and handler durations require adding Prometheus instrumentation to the Go service (e.g. `promhttp` handler, custom counters/histograms). |
| **PromQL "name does not end in _total" warnings** | Limitador metrics (`authorized_hits`, `authorized_calls`, `limited_calls`) and Authorino's `auth_server_authconfig_response_status` are counters but do not follow the Prometheus naming convention of ending in `_total`. When `rate()` is applied, Prometheus generates a warning that Grafana displays on panels. This is [Grafana issue #84636](https://github.com/grafana/grafana/issues/84636) (open). | The warnings are cosmetic and do not affect data correctness. All dashboard queries correctly apply `rate()` or `increase()` to these counters. The metric names are defined by upstream Kuadrant (Limitador) and Authorino — renaming requires upstream changes. |

!!! note "Total Tokens vs Token Breakdown"
    Total token consumption per user **is available** via `authorized_hits{user="..."}`. The blocked feature is the input/output split (prompt vs generation tokens) at the gateway level, which requires the wasm-shim to send two separate counter updates to Limitador.

### Available Per-User and Per-Tier Metrics

| Feature | Metric | Label |
|---------|--------|-------|
| **Latency per tier** | `istio_request_duration_milliseconds_bucket` | `tier` |
| **Token consumption per user** | `authorized_hits` | `user` |
| **Token consumption per tier** | `authorized_hits` | `tier` |
| **Token consumption per model** | `authorized_hits` | `model` |
| **Requests per user** | `authorized_calls` | `user` |
| **Requests per tier** | `authorized_calls` | `tier` |
| **Rate limited per user** | `limited_calls` | `user` |
| **Rate limited per tier** | `limited_calls` | `tier` |

### Requirements Alignment

| Requirement | Status | Notes |
|-------------|--------|-------|
| **Usage dashboards** (token consumption per user, per subscription/tier, per model) | Met | Grafana dashboard + `authorized_hits` with `user`, `tier`, `model`; Prometheus scrapes Limitador `/metrics`. |
| **Latency by tier** (P50/P95/P99) | Met | `istio_request_duration_milliseconds_bucket` with `tier` label; tier-only avoids unbounded cardinality. |
| **Request tracking** (per user, per tier) | Met | `authorized_calls` with `user` and `tier` labels; `limited_calls` for rate-limit violations. |
| **Export for chargeback** (CSV/API) | Not provided (RFE) | Per-user token data exists in Prometheus (`authorized_hits{user="..."}`) but no dedicated billing API or export endpoint is implemented. **RFE recommendation**: Add `/maas-api/v1/usage` endpoint that queries Prometheus and returns per-user, per-tier, per-model token consumption in CSV/JSON for finance and chargeback systems. |
| **Input/output token split** | Not available | Only total tokens (`authorized_hits`); separate input and output counters require upstream Kuadrant wasm-shim changes to send split `hits_addend` values. |
| **`model` label on request/rate-limit counters** | Partial | `model` available on `authorized_hits` only; requires upstream Kuadrant fix to propagate `responseBodyJSON` context to `authorized_calls`/`limited_calls` counters. |
| **Policy enforcement health** | Future | Kuadrant operator metrics (`kuadrant_policies_enforced`, `kuadrant_ready`, etc.) defined upstream but not yet shipped in RHCL 1.x; `limitador_up` and `datastore_partitioned` are available now. |
| **Auth evaluation metrics** | Met | Authorino `/server-metrics` is scraped by the `authorino-server-metrics` ServiceMonitor. Auth evaluation latency (P50/P95/P99) and success/deny rate are available in the Platform Admin dashboard. |
| **maas-api application metrics** | Not available (gap) | The maas-api Go service does not expose `/metrics`. API key creation rate, token issuance rate, and handler latency are not observable. Requires adding Prometheus instrumentation to the Go service. |
