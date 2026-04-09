# Observability

This document covers the observability stack for the MaaS Platform, including metrics collection, monitoring, and visualization.

## Prerequisites

Before deploying the observability stack, ensure the following platform prerequisites are configured. Without these, metrics pipelines for dashboards and showback will not function.

### User Workload Monitoring

[User Workload Monitoring](https://docs.redhat.com/en/documentation/monitoring_stack_for_red_hat_openshift/4.19/html-single/configuring_user_workload_monitoring/index#enabling-monitoring-for-user-defined-projects_preparing-to-configure-the-monitoring-stack-uwm) must be enabled for Prometheus to scrape metrics from MaaS components.

!!! warning "Required for metrics collection"
    Without User Workload Monitoring enabled, ServiceMonitors deployed by MaaS will not be processed and no metrics will be collected.

**Step 1: Create or update the cluster-monitoring-config ConfigMap**

The ConfigMap must exist in the `openshift-monitoring` namespace (not the MaaS application namespace). Cluster admin permissions are required.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-monitoring-config
  namespace: openshift-monitoring
data:
  config.yaml: |
    enableUserWorkload: true
```

Apply with:

```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-monitoring-config
  namespace: openshift-monitoring
data:
  config.yaml: |
    enableUserWorkload: true
EOF
```

**Step 2: Verify User Workload Monitoring is active**

```bash
# Check that prometheus-user-workload pods are running
kubectl get pods -n openshift-user-workload-monitoring

# Expected output: prometheus-user-workload-0 and prometheus-user-workload-1 in Running state
```

If pods are not present, wait a few minutes for the monitoring operator to reconcile.

### Kuadrant Observability

The Kuadrant CR must have `observability.enabled` set to `true` for the operator to create the necessary PodMonitor that scrapes Limitador metrics.

!!! warning "Required for rate-limiting and usage metrics"
    Without Kuadrant observability enabled, metrics like `authorized_hits`, `authorized_calls`, and `limited_calls` will not be scraped into Prometheus. Dashboards showing token consumption and rate limiting will have no data.

**Step 1: Enable observability on the Kuadrant CR**

```bash
kubectl patch kuadrant kuadrant -n kuadrant-system --type=merge \
  -p '{"spec":{"observability":{"enable":true}}}'
```

Or edit the Kuadrant CR directly:

```yaml
apiVersion: kuadrant.io/v1beta1
kind: Kuadrant
metadata:
  name: kuadrant
  namespace: kuadrant-system
spec:
  observability:
    enable: true  # Required for PodMonitor creation
```

**Step 2: Verify the PodMonitor exists**

```bash
# Check that the Kuadrant-created PodMonitor exists
kubectl get podmonitor -n kuadrant-system

# Expected: kuadrant-limitador-monitor should be listed
```

**Step 3: Verify Limitador metrics are being scraped**

```bash
# Query Prometheus for Limitador metrics
curl -sk -H "Authorization: Bearer $(oc whoami -t)" \
  "https://thanos-querier-openshift-monitoring.<cluster>/api/v1/query?query=limitador_up"

# Should return data with limitador_up = 1
```

### Prerequisites Summary

| Prerequisite | Namespace | How to Enable | Verification |
|--------------|-----------|---------------|--------------|
| **User Workload Monitoring** | `openshift-monitoring` | Create `cluster-monitoring-config` ConfigMap with `enableUserWorkload: true` | `kubectl get pods -n openshift-user-workload-monitoring` shows running Prometheus pods |
| **Kuadrant Observability** | `kuadrant-system` | Set `spec.observability.enable: true` on Kuadrant CR | `kubectl get podmonitor -n kuadrant-system` shows `kuadrant-limitador-monitor` |

!!! note "Cluster Admin Permissions"
    Configuring User Workload Monitoring requires cluster admin permissions to create ConfigMaps in the `openshift-monitoring` namespace. If you don't have these permissions, contact your cluster administrator.

## Overview

As part of Dev Preview, MaaS Platform includes a basic observability stack that provides insights into system performance, usage patterns, and operational health.

!!! note
    The observability stack will be enhanced in future releases.

The observability stack consists of:

- **Limitador**: Rate limiting service that exposes usage and rate-limit metrics (with labels from TelemetryPolicy)
- **Authorino**: Authentication/authorization service that exposes auth evaluation metrics (`auth_server_*`)
- **Istio Telemetry**: Adds `subscription` to gateway latency metrics for per-subscription latency (P50/P95/P99)
- **OTel Collector**: Receives Envoy access logs via OTel ALS and exports structured log records to Loki (carries `user_id`, `subscription`, `model`, token counts)
- **Loki (LokiStack)**: Multi-tenant log store for structured Envoy access logs; tenant isolation via `kubernetes_namespace_name` label + LokiStack gateway RBAC
- **vLLM / llm-d / Simulator**: Expose inference metrics (TTFT, ITL, queue depth, token throughput, KV-cache usage); llm-d also exposes EPP routing metrics
- **Prometheus**: Metrics collection and storage (uses OpenShift platform Prometheus)
- **ServiceMonitors**: Deployed to configure Prometheus metric scraping
- **Visualization**: Grafana dashboards or Perses dashboards (OpenShift Console integration)

### Component Metrics Status

| Component | Exposes Metrics? | Scraped into Prometheus? | In Dashboards? |
|-----------|-----------------|--------------------------|----------------|
| **Limitador** | Yes (`/metrics`) | Yes (Kuadrant PodMonitor or MaaS ServiceMonitor) | Yes — 16 panels use `authorized_hits`, `authorized_calls`, `limited_calls`; health check uses `kube_pod_status_phase` |
| **Authorino** | Yes (`/metrics` + `/server-metrics`) | Yes — `/metrics` via Kuadrant operator; `/server-metrics` via MaaS `authorino-server-metrics` ServiceMonitor | Yes — Auth Evaluation Latency (P50/P95/P99), Auth Success/Deny Rate, plus pod-up check |
| **Istio Gateway** | Yes (Envoy `/stats/prometheus`) | Yes (`istio-gateway-metrics` ServiceMonitor) | Yes — latency histograms, request counts, error rates |
| **maas-api** | **No** — returns 404 on `/metrics` | No | Only pod-up check via `kube_pod_status_phase` |
| **vLLM / llm-d / Simulator** | Yes (vLLM metrics on `/metrics` port 8000; llm-d EPP metrics on port 9090) | Yes — vLLM metrics via `kserve-llm-models` ServiceMonitor; EPP metrics require separate scrape config | Yes — TTFT, ITL, queue depth, latency, tokens, cache, prompt/generation ratio, queue wait time (EPP metrics not yet in MaaS dashboards) |

!!! warning "maas-api Metrics Gap"
    The maas-api Go service does **not** expose a `/metrics` endpoint. Metrics such as API key creation rate, token issuance rate, model discovery latency, and request handler durations are not available in Prometheus. Adding Prometheus instrumentation (e.g. `promhttp` handler + application-specific counters/histograms) to the Go service is a recommended future improvement.

## Installation

There are two ways to enable deployment-based observability:

1. **Operator-managed** (recommended): Enable via ModelsAsService CR
2. **Kustomize-based**: Deploy manifests directly

### Option 1: Operator-Managed Telemetry

When using the ODH/RHOAI operator, telemetry can be enabled via the ModelsAsService CR:

```yaml
apiVersion: components.platform.opendatahub.io/v1alpha1
kind: ModelsAsService
metadata:
  name: default-modelsasservice
spec:
  telemetry:
    enabled: true  # Enable TelemetryPolicy and Istio Telemetry
    metrics:
      captureOrganization: true
      captureUser: false      # Disabled by default (GDPR)
      captureGroup: false     # High cardinality
      captureModelUsage: true
```

Or patch an existing CR:

```bash
kubectl patch modelsasservice default-modelsasservice --type=merge \
  -p '{"spec":{"telemetry":{"enabled":true}}}'
```

**What the operator creates when `telemetry.enabled: true`:**

| Resource | Namespace | Purpose |
|----------|-----------|---------|
| TelemetryPolicy (`maas-telemetry`) | Gateway namespace | Adds `subscription`, `model`, `organization_id`, `cost_center` labels to Limitador usage metrics |
| Istio Telemetry (`latency-per-subscription`) | Gateway namespace | Adds `subscription` label to gateway latency metrics |

!!! note "Prerequisites for Operator-Managed Telemetry"
    The operator-managed telemetry feature requires:

    - **OpenShift Service Mesh (Istio)** 2.4+ — for Istio Telemetry CRD
    - **Kuadrant/RHCL** — for TelemetryPolicy CRD and AuthPolicy header injection
    - **Gateway deployed** — Telemetry targets the gateway via selector

    The operator checks for CRD availability before creating resources. If a CRD is not present, that resource is silently skipped.

!!! warning "AuthPolicy Header Dependency"
    The Istio Telemetry reads the `subscription` value from the `X-MaaS-Subscription` header, which must be injected by AuthPolicy:

    ```yaml
    response:
      success:
        headers:
          X-MaaS-Subscription:
            plain:
              expression: 'auth.metadata.apiKeyValidation.subscription'
    ```

    Without this header injection, the `subscription` label on latency metrics will be empty.

**Verify the feature is working:**

```bash
# Check Istio Telemetry was created
kubectl get telemetry -n openshift-ingress latency-per-subscription

# Query Prometheus for subscription label
curl -sk -H "Authorization: Bearer $(oc whoami -t)" \
  "https://thanos-querier-openshift-monitoring.<cluster>/api/v1/label/subscription/values"
```

### Option 2: Kustomize-Based Installation

!!! warning "Development/Testing Only"
    **This is not the standard customer install.** The production path is operator-managed via ODH/RHOAI (Option 1 above).
    
    These Kustomize entrypoints exist so the team can install, iterate, and test observability without a full operator-driven deployment.

#### Kustomize Entrypoint Map

The observability stack is defined across multiple Kustomize directories. Each can be built and applied independently:

| Entrypoint (kustomize build target) | What it deploys | Operator-owned equivalent |
|--------------------------------------|----------------|---------------------------|
| `deployment/base/observability/` | TelemetryPolicy + Istio Telemetry (conditional ServiceMonitors applied only via script) | Operator installs TelemetryPolicy as part of the MaaS stack; Kuadrant operator owns ServiceMonitors when `spec.observability.enable: true` |
| `deployment/components/observability/grafana/` | GrafanaDashboard CRs (Platform Admin, AI Engineer) | Operator does not manage Grafana dashboards; same CRs are used in both paths |
| `deployment/components/observability/prometheus/` | Standalone Prometheus + RBAC + ServiceMonitors in `llm-observability` namespace | OpenShift User Workload Monitoring (built-in Prometheus) — operator path relies on this instead |
| `deployment/components/observability/observability/` | Aggregator that pulls in `prometheus/` above | Same as above — this is a convenience wrapper |
| `deployment/components/observability/observability/dashboards/` | Perses PersesDashboard + Prometheus datasource | No operator equivalent — Perses is optional in both paths |

**Base observability resources** (`deployment/base/observability/`):

| Resource | Purpose |
|----------|---------|
| **TelemetryPolicy** (`gateway-telemetry-policy.yaml`) | Adds `model`, `subscription`, `organization_id`, and `cost_center` labels to Limitador metrics. The `model` label (from `responseBodyJSON`) is available on `authorized_hits`; `authorized_calls` and `limited_calls` carry `subscription`. Per-user tracking is handled by Loki structured logs (`user_id` attribute), not Prometheus labels. |
| **Istio Telemetry** (`istio-gateway-telemetry.yaml`) | Adds `subscription` label to gateway latency (`istio_request_duration_milliseconds_bucket`) via the `X-MaaS-Subscription` header injected by AuthPolicy. Enables per-subscription latency tracking (P50/P95/P99). |

!!! note "Upgrading from tier-based gateway telemetry"
    If an older cluster has `Telemetry` named `latency-per-tier`, remove it after applying the current manifest so only `latency-per-subscription` remains:
    `kubectl delete telemetry latency-per-tier -n openshift-ingress --ignore-not-found`

**Conditional resources** (not in kustomization.yaml, applied only by `install-observability.sh`):

| Resource | Applied when… |
|----------|--------------|
| `limitador-servicemonitor.yaml` | No Kuadrant PodMonitor scraping Limitador `/metrics` |
| `authorino-server-metrics-servicemonitor.yaml` | No existing monitor scraping Authorino `/server-metrics` |
| `istio-gateway-service.yaml` | Gateway deployment exists in `openshift-ingress` |
| `istio-gateway-servicemonitor.yaml` | Gateway deployment exists (paired with the Service above) |

#### Dry-Run Verification

Validate that every kustomization builds cleanly without applying to a cluster:

```bash
# Base telemetry (TelemetryPolicy + Istio Telemetry)
kustomize build deployment/base/observability \
  | kubectl apply --dry-run=client -f -

# Grafana dashboards
kustomize build deployment/components/observability/grafana \
  | kubectl apply --dry-run=client -f -

# Standalone Prometheus stack
kustomize build deployment/components/observability/prometheus \
  | kubectl apply --dry-run=client -f -

# Prometheus aggregator (same content as prometheus/ above)
kustomize build deployment/components/observability/observability \
  | kubectl apply --dry-run=client -f -

# Perses dashboards
kustomize build deployment/components/observability/observability/dashboards \
  | kubectl apply --dry-run=client -f -
```

!!! note "CI Validation"
    CI runs `scripts/ci/validate-manifests.sh` on every PR that touches `deployment/**`, which runs `kustomize build` against all `kustomization.yaml` files. Files with `kind: Component` are skipped (they must be included via a parent's `components:` field), but directories whose kustomization.yaml declares `kind: Kustomization` can build standalone and are validated by CI dry-run.

#### Deploy to a Cluster

**Quick deployment** (recommended):

```bash
# Deploys base telemetry + conditional ServiceMonitors
./scripts/observability/install-observability.sh [--namespace NAMESPACE]
```

**Manual deployment** (step-by-step):

```bash
# 1. Base telemetry (requires Gateway + AuthPolicy to exist first)
kustomize build deployment/base/observability | kubectl apply -f -

# 2. Conditional ServiceMonitors (Limitador, Authorino, Gateway, LLM models)
#    Use the install script — it detects existing Kuadrant monitors to avoid duplicates:
./scripts/observability/install-observability.sh

# 3. Grafana dashboards (discovers Grafana instance cluster-wide)
./scripts/observability/install-grafana-dashboards.sh

# 4. (Optional) Standalone Prometheus — only if NOT using OpenShift User Workload Monitoring
kustomize build deployment/components/observability/observability | kubectl apply -f -

# 5. (Optional) Perses dashboards
kustomize build deployment/components/observability/observability/dashboards | kubectl apply -f -
```

When using the full deployment script, this is applied automatically:

```bash
./scripts/deploy.sh
```

!!! note "Prerequisites"
    - **Tools**: `kubectl`, `kustomize`, `jq`, `yq` must be installed
    - **Cluster state**: Gateway, AuthPolicy (gateway-auth-policy), and subscription selection must be deployed first. The AuthPolicy injects `X-MaaS-Subscription`, which Istio Telemetry reads to label latency by subscription. Without it, the `subscription` label on gateway latency will be empty.
    - **Namespace**: Use `--namespace` if your MaaS API is deployed to a namespace other than `maas-api` (e.g. `--namespace opendatahub`)

#### Operator vs Kustomize Drift Reference

When updating manifests in `deployment/base/observability/` or `deployment/components/observability/`, check whether the operator path produces equivalent resources. Drift between the two is expected in some areas (e.g., the standalone Prometheus stack has no operator equivalent) but should be tracked for the telemetry CRs and ServiceMonitors.

| Resource | Kustomize source | Operator creates? | Notes |
|----------|-----------------|-------------------|-------|
| TelemetryPolicy | `base/observability/gateway-telemetry-policy.yaml` | Yes | Keep in sync — labels must match |
| Istio Telemetry | `base/observability/istio-gateway-telemetry.yaml` | Yes | Keep in sync — header extraction must match |
| Limitador ServiceMonitor | `base/observability/limitador-servicemonitor.yaml` | Kuadrant PodMonitor when `observability.enable: true` | Script skips ours if Kuadrant's exists |
| Authorino /server-metrics | `base/observability/authorino-server-metrics-servicemonitor.yaml` | Not yet (Kuadrant only scrapes `/metrics`) | MaaS supplements the operator gap |
| Istio Gateway ServiceMonitor | `base/observability/istio-gateway-servicemonitor.yaml` | No | MaaS-only; applied conditionally |
| Grafana Dashboards | `components/observability/grafana/` | No | Same CRs used in both paths |
| Standalone Prometheus | `components/observability/prometheus/` | No (uses OpenShift UWM instead) | Dev/test only — not for production |
| Perses Dashboards | `components/observability/observability/dashboards/` | No | Optional in both paths |

#### Keeping Docs Accurate

When you change any YAML under `deployment/base/observability/` or `deployment/components/observability/`:

1. Verify the build still passes: `kustomize build <dir> | kubectl apply --dry-run=client -f -`
2. Update the **entrypoint map table above** if you add, remove, or rename an entrypoint
3. Update this document if the change affects user-facing instructions (metrics, ServiceMonitor behavior, dashboard content)

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
| `authorized_hits` | Counter | `subscription`, `model`, `limitador_namespace` | Total tokens consumed per request (from `usage.total_tokens` in the model response; input + output combined). The `model` label is extracted via `responseBodyJSON("/model")`. |
| `authorized_calls` | Counter | `subscription`, `limitador_namespace` | Requests allowed (not rate-limited). |
| `limited_calls` | Counter | `subscription`, `limitador_namespace` | Requests denied due to token rate limits. |

!!! note "`model` label availability"
    The `model` label is currently available **only on `authorized_hits`**. The `authorized_calls` and `limited_calls` metrics carry `subscription` but not `model`, due to how the wasm-shim constructs the CEL evaluation context for these counters. This is a known upstream limitation tracked for improvement in Kuadrant.

!!! note "Per-user tracking via Loki"
    Per-user metrics (token consumption, request counts, rate-limited requests) are tracked via **Loki structured logs** (`user_id` attribute from Envoy access logs), not Prometheus labels. The `user` label was removed from TelemetryPolicy to avoid high cardinality on Prometheus counters. Perses dashboards query Loki for per-user panels (e.g. `sum by (user_id) (sum_over_time({service_name="maas-gateway"} | user_id!="" | user_id!="-" | unwrap tokens_total [1h]))`).

Gateway latency is labeled by **subscription only** via Istio Telemetry (see [Per-Subscription Latency Tracking](#per-subscription-latency-tracking)); per-user latency is not exposed on the gateway histogram to keep cardinality bounded.

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

!!! note "Authconfig label values are hashed"
    In Kuadrant deployments, the `authconfig` label on Authorino metrics contains SHA-256 hashes (e.g. `18e32965...`) rather than human-readable AuthPolicy names. Kuadrant creates AuthConfig CRs in `kuadrant-system` with hashed names derived from the policy and route configuration. Since all AuthConfig CRs in a MaaS deployment are Kuadrant-managed MaaS auth policies, dashboard panels use `authconfig!=""` to include all evaluations. This is safe because Authorino is deployed exclusively for MaaS via Kuadrant.

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
    vLLM metrics provide input vs output token breakdown **per model** (`vllm:prompt_tokens_total` / `vllm:generation_tokens_total` counters, or `vllm:request_prompt_tokens` / `vllm:request_generation_tokens` histograms). However, these do not carry `user` or `subscription` labels. Per-user input/output token split is available via Loki structured logs (`tokens_prompt` and `tokens_completion` attributes with `user_id`). See [Known Limitations](#known-limitations).

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

- **Limitador** (`limitador-servicemonitor.yaml`): Scrapes rate-limiting metrics from Limitador pods in `kuadrant-system`. **Skipped when Kuadrant's own PodMonitor is already present.** When Kuadrant CR has `spec.observability.enable: true`, the operator creates its own `kuadrant-limitador-monitor` PodMonitor that scrapes the same Limitador pod. Deploying both would cause duplicate metrics.
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

**[Configuring Redis storage for rate limiting](https://docs.kuadrant.io/1.0.x/limitador-operator/doc/storage/)**

This Red Hat documentation provides:

- Step-by-step Redis configuration for OpenShift
- Secret management for Redis credentials
- Limitador custom resource updates
- Production-ready setup instructions

For local development and testing, you can also use our [Limitador Persistence](limitador-persistence.md) guide which includes a basic Redis setup script that works with any Kubernetes cluster.

## Visualization

MaaS provides two visualization options — choose one (or both):

| Platform | Integration | Install Script |
|----------|------------|----------------|
| **Grafana** | Standalone Grafana Operator; GrafanaDashboard CRs | `./scripts/observability/install-grafana-dashboards.sh` |
| **Perses** | OpenShift Console native (via Cluster Observability Operator UIPlugin) | `./scripts/observability/install-perses-dashboards.sh` |

Both options deploy Platform Admin and AI Engineer dashboards with equivalent metrics coverage. Perses additionally includes a **Usage Dashboard** for per-user consumption tracking and chargeback. Choose based on your environment:

- **Grafana**: Feature-rich, standalone UI. Best when a Grafana instance already exists or when you need advanced alerting, annotations, or external sharing. Deploys: Platform Admin, AI Engineer.
- **Perses**: CNCF native, integrated into the OpenShift Console. Best for OpenShift-native workflows where a separate Grafana instance is not desired. Deploys: Platform Admin, AI Engineer, Usage.

For general references:

- **OpenShift Monitoring**: [Monitoring overview](https://docs.redhat.com/en/documentation/openshift_container_platform/4.19/html/monitoring/index)
- **Grafana on OpenShift**: [Red Hat OpenShift AI Monitoring](https://docs.redhat.com/en/documentation/red_hat_openshift_ai_self-managed/2.25/html/managing_and_monitoring_models/index)

### Included Dashboards

MaaS includes dashboards for different personas (Platform Admin and AI Engineer available in both Grafana and Perses; Usage is Perses-only):

#### Platform Admin Dashboard

Provides a comprehensive view of system health, usage across all users, and resource allocation:

| Section | Metrics |
|---------|---------|
| **Component Health** | Limitador up, Authorino pods, MaaS API pods, Gateway pods, Firing Alerts |
| **Key Metrics** | Total Tokens, Total Requests, Token Rate, Request Rate, Inference Success Rate, Active Users, P50 Response Latency, Rate Limit Ratio |
| **Auth Evaluation** | Auth Evaluation Latency (P50/P95/P99), Auth Success/Deny Rate |
| **Traffic Analysis** | Token/Request Rate by Model, Error Rates (4xx excl. 429, 5xx, 429 Rate Limited), Token/Request Rate by Subscription, P95 Latency |
| **Error Breakdown** | Rate Limited Requests, Unauthorized Requests |
| **Model Metrics** | vLLM queue depth, inference latency, KV cache usage, token throughput, prompt vs generation token ratio, queue wait time, TTFT, ITL |
| **Top Users** (Loki) | By token usage, by declined requests |
| **Detailed Breakdown** (Loki) | Token Rate by User, Request Volume by User & Subscription |
| **Resource Allocation** | CPU/Memory/GPU per model pod |

!!! note "Template Variables"
    The Platform Admin dashboard uses Grafana template variables for namespace filtering instead of hardcoded values. This allows the dashboard to adapt to different deployment configurations:

    | Variable | Default | Description |
    |----------|---------|-------------|
    | `$datasource` | `prometheus` | Prometheus datasource |
    | `$maas_namespace` | auto-detected | MaaS API namespace (auto-detected from `kube_pod_info{pod=~"maas-api.*"}`) |
    | `$kuadrant_namespace` | `kuadrant-system` | Kuadrant components namespace |
    | `$gateway_namespace` | `openshift-ingress` | Istio/Gateway namespace |
    | `$llm_namespace` | `All` (auto-detected) | LLM model pods namespace(s) (auto-detected from `vllm:num_requests_running`) |
    | `$model` | `All` | Filter by model name |

    To customize for your environment, change the variable values in Grafana's dashboard settings (gear icon → Variables).

#### AI Engineer Dashboard

Personal usage view for individual developers. All per-user panels query **Loki** structured logs filtered by `user_id=$user`:

| Section | Metrics |
|---------|---------|
| **Usage Summary** (Loki) | My Total Tokens (1h), My Total Requests (1h), Token Rate, Request Rate, Rate Limit Ratio, Inference Success Rate |
| **Usage Trends** (Loki) | Token Usage by Model, Usage Trends (tokens vs rate limited) |
| **Hourly Usage Patterns** (Loki) | Hourly Token Usage by Model |
| **Detailed Analysis** (Loki) | Token Volume by Model, Rate Limited by Subscription |
| **Usage Summary** (Loki) | My Usage Summary by Model & Subscription |

#### Usage Dashboard

Tabular view for per-user consumption tracking and chargeback (Perses only):

| Section | Metrics |
|---------|---------|
| **Overview Stats** | Total Tokens, Total Requests, Active Users |
| **Token Consumption by User** | Table with columns: User, Subscription, Tokens, Requests, Rate Limited |

The Usage Dashboard uses **Loki structured logs** for all per-user data (tokens, requests, rate-limited counts). It supports filtering by User, Subscription, and Model variables, with a `$range` dropdown (1h to 30d, default 7d). Stat panels use `calculation: last` with `[$range]` windows; the table aggregates by `(user_id, subscription)`. All queries include `| user_id!="-"` to exclude probe traffic.

!!! note "Perses Usage Dashboard: why variables use StaticListVariable"
    **Ideally, in Grafana/Prometheus,** the dashboard time range drives every LogQL window (for example via `$__range`). **Perses does not** bind the native time picker to Loki that way. **Workaround:** add a separate **Time Range** variable built with **`StaticListVariable`** (the “Time Range” dropdown). Users pick values such as 24h there; all LogQL panels use **`[$range]`** from that list. Set the console’s native time picker to **at least** the selected `$range` so results are not clipped.

    **Perses** cannot do dynamic time binding to Loki queries or dynamic Loki label pickers, so we use **`StaticListVariable`**—hardcoded dropdown options—instead. **The same applies to the Subscription and Model dropdowns:** label auto-discovery is not available in Perses. The **Subscription** and **Model** filters use **`values:`** in `deployment/components/observability/perses/dashboards/dashboard-usage.yaml`; new names do not appear as dropdown entries until the YAML is updated and the dashboard is re-applied. With the default **`$__all`**, aggregates and the table still include **all** subscriptions and models in Loki (including new ones); only **filtering by a specific new name** requires updating the static lists.

!!! note "Inference Success Rate"
    Both dashboards use `rate()` on vLLM counters (`request_success_total`, `e2e_request_latency_seconds_count`) instead of raw counter values. This handles pod restarts correctly (counters reset independently and raw division produces incorrect results). When no traffic is present, the denominator is clamped with `clamp_min(..., 1e-9)` and multiplied by a traffic existence check `(sum(rate(...)) > 0)`, falling back to `OR vector(0)` so idle periods show 0% instead of a misleading 100%.

!!! info "Inference Success Rate is platform-wide"
    The Inference Success Rate panel in the AI Engineer dashboard shows the **platform-wide** model success rate, not per-user. This is because vLLM metrics do not carry user labels — they are emitted by the model backend and measure all inference requests regardless of caller. All other panels in the AI Engineer dashboard are filtered by the selected user.

!!! info "Tokens vs Requests"
    Both dashboards show **token consumption** (`authorized_hits`) for billing/cost tracking and **request counts** (`authorized_calls`) for capacity planning. Blue panels indicate request metrics; green panels indicate token metrics.

!!! tip "Per-User Token Billing"
    The **Platform Admin dashboard** shows token consumption aggregated by **subscription** and **model** for system-level visibility. The **Usage dashboard** provides a per-user table with subscription, tokens, requests, and rate-limited counts (via Loki structured logs). Per-user token consumption for billing is also available via:

    - **AI Engineer dashboard**: Individual users see their own token usage (Loki `user_id` filter)
    - **Loki LogQL**: Query `sum by (user_id) (sum_over_time({service_name="maas-gateway"} | user_id!="" | user_id!="-" | unwrap tokens_total [24h]))` for billing periods
    - **RFE**: A dedicated `/maas-api/v1/usage` chargeback API endpoint is recommended for production billing workflows

### Prerequisites

**For Grafana dashboards:**

- **Grafana** must be installed (for example via your observability team's process, a centralized instance, or the [Grafana Operator](https://grafana.github.io/grafana-operator/docs/installation/)). The dashboard helper does **not** install Grafana; it only deploys MaaS dashboard definitions and **never fails** (warnings only if none or multiple instances are found).
- Ensure the Grafana instance has label `app=grafana` so MaaS dashboard definitions attach.
- Configure a **Prometheus or Thanos datasource** in Grafana; the MaaS dashboards use the default Prometheus datasource.

**For Perses dashboards (two-step install):**

- **OpenShift 4.18+** with the Cluster Observability Operator available in the operator catalog.
- **Step 1** — Run `scripts/installers/install-perses.sh` to install the Cluster Observability Operator and wait for Perses CRDs to become available.
- **Step 2** — Run `scripts/observability/install-perses-dashboards.sh` to enable the Perses UIPlugin in the OpenShift Console and deploy MaaS dashboard definitions.
- Both scripts must be run in sequence; `install-perses-dashboards.sh` will exit with a warning if CRDs from step 1 are not yet present.
- Perses dashboards are accessed via the OpenShift Console (Observe → Dashboards → Perses tab).

### Deploying Dashboards

Monitoring is installed by `install-observability.sh`. Dashboards are installed by **separate helpers** — one for each visualization platform:

**Grafana:**

    ./scripts/observability/install-grafana-dashboards.sh

Scans for Grafana CRs cluster-wide. If **one** instance is found, deploys dashboards to that namespace and prints a success message. If **none** or **multiple** are found, prints a warning and exits without error. Use flags to target a specific instance:

    ./scripts/observability/install-grafana-dashboards.sh --grafana-namespace maas-api
    ./scripts/observability/install-grafana-dashboards.sh --grafana-label app=grafana

**Perses:**

    ./scripts/observability/install-perses-dashboards.sh

Checks for Perses CRDs and, if they are missing, exits with a warning directing you to run `install-perses.sh` first (operator installation is a separate step handled by `scripts/installers/install-perses.sh`). When CRDs are present, `install-perses-dashboards.sh` enables the Perses UIPlugin in the OpenShift Console and deploys PersesDashboard CRs to `openshift-operators`. After installation, dashboards are accessible at **Observe → Dashboards → Perses tab** in the OpenShift Console.

**Manual Grafana import (dashboard JSON only):**

    kustomize build deployment/components/observability/grafana/dashboards | \
      sed "s/namespace: maas-api/namespace: <your-namespace>/g" | \
      kubectl apply -f -

### Sample Dashboard JSON

For manual Grafana import, a sample dashboard JSON file is available:

- [MaaS Token Metrics Dashboard](https://github.com/opendatahub-io/models-as-a-service/blob/main/docs/samples/dashboards/maas-token-metrics-dashboard.json)

To import into Grafana:

1. Go to Grafana → Dashboards → Import
2. Upload the JSON file or paste content
3. Select your Prometheus datasource

## Key Metrics Reference

### Token and Request Metrics

| Metric | Description | Labels |
|--------|-------------|--------|
| `authorized_hits` | Total tokens consumed (input + output combined, from `usage.total_tokens` in model responses) | `subscription`, `model` |
| `authorized_calls` | Total requests allowed | `subscription` |
| `limited_calls` | Total requests rate-limited | `subscription` |

!!! tip "When to use which metric"
    - **Billing/Cost**: Use `authorized_hits` - represents actual token consumption, with `model` label for per-model breakdown
    - **API Usage**: Use `authorized_calls` - represents number of API calls (per subscription); per-user via Loki `count_over_time`
    - **Rate Limiting**: Use `limited_calls` - shows quota violations (per subscription); per-user via Loki `response_code="429"`

!!! note "Total tokens only (input/output split not yet available)"
    Prometheus token consumption is reported as **total tokens** (prompt + completion) per request via `authorized_hits`. Separate input-token and output-token Prometheus counters are **not yet available** at the gateway level; this would require upstream wasm-shim changes. However, Loki structured logs **do** carry `tokens_prompt` and `tokens_completion` individually, enabling per-user input/output split for billing via LogQL queries.

### Latency Metrics

| Metric | Description | Labels |
|--------|-------------|--------|
| `istio_request_duration_milliseconds_bucket` | Gateway-level latency histogram | `destination_service_name`, `subscription` |
| `vllm:e2e_request_latency_seconds` | Model inference latency | `model_name` |

#### Per-Subscription Latency Tracking

The MaaS Platform uses an Istio Telemetry resource to add a `subscription` dimension to gateway latency metrics. This enables tracking request latency per subscription (e.g. free, premium, enterprise). Gateway latency is labeled by **subscription only** (not by user) to keep metric cardinality bounded and to align with latency-by-subscription requirements (e.g. P50/P95/P99 per subscription). Per-user metrics are available from Loki structured logs (`user_id` attribute).

**How it works:**

1. The `gateway-auth-policy` injects the `X-MaaS-Subscription` header from the resolved subscription
2. The Istio Telemetry resource extracts this header and adds it as a `subscription` label to the `REQUEST_DURATION` metric
3. Prometheus scrapes these metrics from the Istio gateway

**Configuration** (`deployment/base/observability/istio-gateway-telemetry.yaml`):

    apiVersion: telemetry.istio.io/v1
    kind: Telemetry
    metadata:
      name: latency-per-subscription
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
            subscription:
              operation: UPSERT
              value: request.headers["x-maas-subscription"]

!!! note "Security"
    The `X-MaaS-Subscription` header should be injected server-side by AuthPolicy. Ensure your AuthPolicy injects this header from the subscription selection (not client input) for accurate metrics attribution.

### Common Queries

**Token-based queries (billing/cost) — Prometheus:**

    # Token consumption rate per model (tokens/sec)
    sum by (model) (rate(authorized_hits[5m]))

    # Token consumption by subscription
    sum by (subscription) (authorized_hits)

**Token-based queries (billing/cost) — Loki (per-user):**

    # Total tokens consumed per user (last 1h)
    sum by (user_id) (sum_over_time({service_name="maas-gateway", response_code=~"2.."}
    | user_id!="" | user_id!="-" | unwrap tokens_total [1h]))

    # Top 10 users by tokens consumed
    topk(10, sum by (user_id) (sum_over_time({service_name="maas-gateway", response_code=~"2.."}
    | user_id!="" | user_id!="-" | unwrap tokens_total [5m])))

**Request-based queries (capacity/usage) — Prometheus:**

    # Request rate per subscription (requests/sec)
    sum by (subscription) (rate(authorized_calls[5m]))

**Request-based queries (capacity/usage) — Loki (per-user):**

    # Total requests per user (last 1h)
    sum by (user_id) (count_over_time({service_name="maas-gateway", response_code=~"2.."}
    | user_id!="" | user_id!="-" [1h]))

**Inference success rate** (system health — did requests that reached the model succeed?):

    # Inference success rate using rate() to handle counter resets correctly
    # The >= 0 filter removes NaN (0/0 when no traffic), falling back to vector(1) = 100%
    ((sum(rate(vllm:request_success_total[5m])) / clamp_min(sum(rate(vllm:e2e_request_latency_seconds_count[5m])), 1e-9)) * (sum(rate(vllm:e2e_request_latency_seconds_count[5m])) > 0)) OR vector(0)

**Rate limiting metrics** (capacity planning — are users exceeding their quotas?):

    # Rate limit ratio (percentage of requests rejected by rate limiting)
    (sum(limited_calls) / (sum(authorized_calls) + sum(limited_calls))) OR vector(0)

    # Rate limit ratio by subscription
    (sum by (subscription) (limited_calls) / (sum by (subscription) (authorized_calls) + sum by (subscription) (limited_calls))) OR vector(0)

    # Rate limit violations per second by subscription
    sum by (subscription) (rate(limited_calls[5m]))

    # Subscriptions hitting rate limits most
    topk(10, sum by (subscription) (limited_calls))

**Latency metrics** (per-subscription SLA tracking):

    # P99 latency per subscription
    histogram_quantile(0.99, sum by (subscription, le) (rate(istio_request_duration_milliseconds_bucket{subscription!=""}[5m])))

    # P50 latency per subscription
    histogram_quantile(0.5, sum by (subscription, le) (rate(istio_request_duration_milliseconds_bucket{subscription!=""}[5m])))

**Latency queries:**

    # P99 latency by service
    histogram_quantile(0.99, sum by (destination_service_name, le) (rate(istio_request_duration_milliseconds_bucket[5m])))

    # P50 (median) latency
    histogram_quantile(0.5, sum by (le) (rate(istio_request_duration_milliseconds_bucket[5m])))

    # P99 latency per subscription
    histogram_quantile(0.99, sum by (subscription, le) (rate(istio_request_duration_milliseconds_bucket{subscription!=""}[5m])))

!!! tip "Filtering metrics"
    For per-subscription latency queries, use `subscription!=""` to exclude requests where the `X-MaaS-Subscription` header was not injected. Limitador usage metrics already exclude rate-limited requests. `authorized_hits` reflects token usage for successful responses, while `authorized_calls` counts requests that were allowed through rate limiting and may still fail downstream.

## Maintenance

### Grafana Datasource Token Rotation

The Grafana datasource uses a ServiceAccount token to authenticate with Prometheus. This token is valid for **30 days** and must be rotated periodically.

**To rotate the token:**

    # Delete the existing datasource and create a new one (or rotate the token per your Grafana setup).
    # To re-deploy only MaaS dashboard definitions: ./scripts/observability/install-grafana-dashboards.sh

!!! tip "Production Recommendation"
    For production deployments, consider automating token rotation using a CronJob or external secrets operator to avoid dashboard outages.

## Known Limitations

### Perses Loki Plugin Limitations

The Perses Loki plugin (v0.5.0-rc.1) has the following limitations that affect dashboard design:

| Limitation | Impact | Workaround |
|------------|--------|------------|
| **No `$__range` variable** | Loki queries cannot reference the native time picker's range | Usage Dashboard uses a `$range` dropdown variable (StaticListVariable). AI Engineer uses hardcoded `[1h]` matching `duration: 1h`. |
| **No Loki-driven filter variables** | Perses cannot populate Subscription/Model dropdowns by querying Loki for label values | Usage Dashboard uses **`StaticListVariable`** with hardcoded `values:` for **subscription** and **model**; update `dashboard-usage.yaml` when new names must appear as selectable options; **`$__all`** still includes all traffic in aggregates. |
| **No instant query support** | Stat panels cannot make instant queries | All stat panels use `range` queries with `calculation: last` to pick the final data point. |
| **`user_id="-"` probe traffic** | Envoy health probes generate access logs with `user_id="-"` | All Loki queries include `| user_id!="-"` to exclude probe traffic. Usage Dashboard also filtered by `subscription` (defense-in-depth). |
| **Overcounting with `[5m]` + `calculation: sum`** | Without explicit `step`, Loki uses a default step (~15s for 1h range), creating overlapping 5m windows that count each log ~20x | Stat totals use `[$range]` or `[1h]` with `calculation: last` (single non-overlapping window). Time series charts use `[5m]` with `rate()` or `last-number` which are safe. |

### Currently Blocked Features

Some features require upstream changes and are currently blocked:

| Feature | Blocker | Workaround |
|---------|---------|------------|
| **`model` label on `authorized_calls` / `limited_calls`** | Kuadrant wasm-shim does not pass `responseBodyJSON` context for these counters | Use `authorized_hits` for per-model breakdown. `authorized_calls`/`limited_calls` natively support per-subscription |
| **Input/output token split** | Kuadrant TokenRateLimitPolicy sends a single `hits_addend` (total tokens); no mechanism for separate prompt/completion counters | Total tokens available via `authorized_hits`; the response body contains `usage.prompt_tokens` and `usage.completion_tokens` but the wasm-shim does not split them |
| **Input/output token breakdown per user** | vLLM does not label its own metrics with `user` | Total tokens per user available via Loki (`user_id` attribute on structured logs); vLLM prompt/generation token metrics are per-model only |
| **Kuadrant policy health metrics** | `kuadrant_policies_enforced`, `kuadrant_policies_total` etc. are defined in Kuadrant dev but not yet shipped in RHCL 1.x | Enable `observability.enable: true` on the Kuadrant CR; the ServiceMonitors are created but policy-specific gauges will appear in a future operator release |
| **Authorino auth server metrics (upstream)** | The Kuadrant-provided `authorino-operator-monitor` only scrapes `/metrics` (controller-runtime); `/server-metrics` is not scraped by the upstream operator | **Resolved by MaaS**: The `authorino-server-metrics` ServiceMonitor (deployed by `install-observability.sh`) scrapes `/server-metrics`. Auth evaluation latency and success/deny rate are visualized in the Platform Admin dashboard. |
| **Rate-limited requests not in Istio metrics** | When the Kuadrant WASM plugin rejects a request (429), it calls `sendLocalReply()` which short-circuits the Envoy filter chain. These requests appear in Limitador metrics (`limited_calls`) but may not appear in Istio gateway metrics. | Use `limited_calls` from Limitador for rate-limiting visibility (has `subscription` label). Rate-limited requests per user are available in Loki (`response_code="429"` + `user_id`). |
| **maas-api application metrics** | The maas-api Go service does not expose a `/metrics` endpoint | No workaround available. Metrics such as API key creation rate, token issuance rate, model discovery latency, and handler durations require adding Prometheus instrumentation to the Go service (e.g. `promhttp` handler, custom counters/histograms). |
| **PromQL "name does not end in _total" warnings** | Limitador metrics (`authorized_hits`, `authorized_calls`, `limited_calls`) and Authorino's `auth_server_authconfig_response_status` are counters but do not follow the Prometheus naming convention of ending in `_total`. When `rate()` is applied, Prometheus generates a warning that Grafana displays on panels. This is [Grafana issue #84636](https://github.com/grafana/grafana/issues/84636) (open). | The warnings are cosmetic and do not affect data correctness. All dashboard queries correctly apply `rate()` or `increase()` to these counters. The metric names are defined by upstream Kuadrant (Limitador) and Authorino — renaming requires upstream changes. |

!!! note "Total Tokens vs Token Breakdown"
    Total token consumption per user **is available** via Loki structured logs (`user_id` + `tokens_total` attributes). The blocked feature is the input/output split (prompt vs generation tokens) at the gateway level, which requires the wasm-shim to send two separate counter updates to Limitador. Loki logs do carry `tokens_prompt` and `tokens_completion` individually, enabling per-user input/output split for billing when queried directly.

### Available Per-User and Per-Subscription Metrics

Per-subscription metrics are available from **Prometheus** (Limitador counters). Per-user metrics are available from **Loki** (structured Envoy access logs):

| Feature | Source | Metric / Query | Label / Attribute |
|---------|--------|----------------|-------------------|
| **Latency per subscription** | Prometheus | `istio_request_duration_milliseconds_bucket` | `subscription` |
| **Token consumption per subscription** | Prometheus | `authorized_hits` | `subscription` |
| **Token consumption per model** | Prometheus | `authorized_hits` | `model` |
| **Requests per subscription** | Prometheus | `authorized_calls` | `subscription` |
| **Rate limited per subscription** | Prometheus | `limited_calls` | `subscription` |
| **Token consumption per user** | Loki | `sum_over_time({service_name="maas-gateway"} \| unwrap tokens_total [...])` | `user_id` |
| **Requests per user** | Loki | `count_over_time({service_name="maas-gateway"} \| user_id!="" [...])` | `user_id` |
| **Rate limited per user** | Loki | `count_over_time({service_name="maas-gateway", response_code="429"} [...])` | `user_id` |
| **Active users** | Loki | `count(sum by (user_id) (count_over_time({...} \| user_id!="" \| user_id!="-" [...])))` | `user_id` |

!!! note "Per-user tracking moved from Prometheus to Loki"
    The `user` label was removed from TelemetryPolicy to avoid high-cardinality Prometheus metrics. All per-user panels in Perses dashboards (Usage, AI Engineer, Platform Admin) now query Loki structured logs using the `user_id` attribute set by Envoy's access log. This provides the same per-user visibility without Prometheus cardinality concerns. Grafana dashboards that still reference the Prometheus `user` label will need migration.

### Requirements Alignment

| Requirement | Status | Notes |
|-------------|--------|-------|
| **Usage dashboards** (token consumption per user, per subscription, per model) | Met | Perses dashboards (Platform Admin, AI Engineer, Usage) use Prometheus for per-subscription/model metrics (`authorized_hits` with `subscription`, `model`) and Loki structured logs for per-user panels (`user_id` attribute). Grafana dashboards use Prometheus only (per-user panels require migration to Loki). |
| **Latency by subscription** (P50/P95/P99) | Met | `istio_request_duration_milliseconds_bucket` with `subscription` label via Istio Telemetry + `X-MaaS-Subscription` header; subscription-only avoids unbounded cardinality. |
| **Request tracking** (per user, per subscription) | Met | `authorized_calls` with `subscription` label (Prometheus); per-user request counts via Loki `count_over_time` on `user_id`; `limited_calls` for rate-limit violations. |
| **Export for chargeback** (CSV/API) | Not provided (RFE) | Per-user token data exists in Loki structured logs (`user_id` + `tokens_total`) but no dedicated billing API or export endpoint is implemented. **RFE recommendation**: Add `/maas-api/v1/usage` endpoint that queries Loki and returns per-user, per-subscription, per-model token consumption in CSV/JSON for finance and chargeback systems. |
| **Input/output token split** | Not available | Only total tokens (`authorized_hits`); separate input and output counters require upstream Kuadrant wasm-shim changes to send split `hits_addend` values. |
| **`model` label on request/rate-limit counters** | Partial | `model` available on `authorized_hits` only; the Usage Dashboard uses `group_left` joins to propagate `model` to other metrics for display. Requires upstream Kuadrant fix to natively propagate `responseBodyJSON` context to `authorized_calls`/`limited_calls` counters. |
| **Policy enforcement health** | Future | Kuadrant operator metrics (`kuadrant_policies_enforced`, `kuadrant_ready`, etc.) defined upstream but not yet shipped in RHCL 1.x; `limitador_up` and `datastore_partitioned` are available now. |
| **Auth evaluation metrics** | Met | Authorino `/server-metrics` is scraped by the `authorino-server-metrics` ServiceMonitor. Auth evaluation latency (P50/P95/P99) and success/deny rate are available in the Platform Admin dashboard. |
| **maas-api application metrics** | Not available (gap) | The maas-api Go service does not expose `/metrics`. API key creation rate, token issuance rate, and handler latency are not observable. Requires adding Prometheus instrumentation to the Go service. |
