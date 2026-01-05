# 📊 MaaS Grafana Dashboards

This directory contains Grafana dashboard samples for the MaaS platform.

## 📁 Dashboard Files

| File | Description |
| ---- | ----------- |
| `platform-admin-dashboard.json` | Unified view for platform administrators |
| `ai-engineer-dashboard.json` | API key-filtered view for AI engineers |
| `maas-token-metrics-dashboard.json` | Legacy token metrics dashboard |

## 📖 Documentation Files

| File | Description |
| ---- | ----------- |
| `METRICS-SUMMARY.md` | **Main reference** - Complete metrics documentation, queries, and limitations |
| `METRICS-EXPORT-FLOW.md` | Architecture flow showing how metrics are exported |
| `PROMETHEUS-COUNTER-BEHAVIOR.md` | Educational guide on Prometheus counter behavior |

## 🎯 Available Metrics

### ✅ All Metrics Working!

| Category | Metrics | Labels |
| -------- | ------- | ------ |
| **Limitador** | `authorized_hits`, `authorized_calls`, `limited_calls`, `limitador_up` | ✅ `user`, `tier`, `model`, `limitador_namespace` |
| **Istio Gateway** | `istio_requests_total`, `istio_request_duration_milliseconds_bucket` | ✅ `response_code`, `destination_service_name` |
| **vLLM/KServe** | `vllm:num_requests_running`, `vllm:num_requests_waiting`, `vllm:gpu_cache_usage_perc`, `vllm:e2e_request_latency_seconds`, `vllm:request_inference_time_seconds` | ✅ `model_name` |
| **Kubernetes** | `kube_pod_status_phase`, `ALERTS` | ✅ `namespace`, `pod`, `alertname` |
| **Authorino** | `controller_runtime_reconcile_*` | ⚠️ Operator metrics only |

**Verified on cluster:**

```text
authorized_hits{model="facebook-opt-125m-simulated",tier="free",user="tgitelma-redhat-com-dd264a84",...} 376
istio_requests_total{response_code="200",destination_service_name="facebook-opt-125m-simulated-kserve-workload-svc",...} 55
```

See `METRICS-SUMMARY.md` for full details and query examples.

## 🔧 How to Use

1. **Automated Deployment (Recommended):**
   ```bash
   ./scripts/install-observability.sh
   ```
   This script installs Grafana, configures Prometheus datasource, and deploys all dashboards.

2. **Manual Import:**
   - Go to Grafana → Dashboards → Import
   - Upload the desired dashboard JSON file
   - Configure Prometheus datasource

3. **Prerequisites:**
   - User-workload-monitoring enabled in OpenShift
   - ServiceMonitors deployed for Limitador, Istio Gateway, and KServe models
   - Kuadrant policies configured with TelemetryPolicy

## 📈 Working Queries

```promql
# Requests per user
sum by (user) (authorized_hits)

# Requests per model
sum by (model) (authorized_hits)

# Top 10 users
topk(10, sum by (user) (authorized_hits))

# Success rate per user
sum by (user) (authorized_calls) / (sum by (user) (authorized_calls) + sum by (user) (limited_calls))

# P95 latency by service (Istio)
histogram_quantile(0.95, sum by (destination_service_name, le) (rate(istio_request_duration_milliseconds_bucket[5m])))

# Unauthorized requests (401)
sum(rate(istio_requests_total{response_code="401"}[5m]))

# Overall error rate (4xx + 5xx)
sum(rate(istio_requests_total{response_code=~"4.."}[5m])) + sum(rate(istio_requests_total{response_code=~"5.."}[5m]))

# Firing alerts in MaaS namespaces
count(ALERTS{alertstate="firing", namespace=~"llm|kuadrant-system|maas-api"})

# Model inference P95 latency (vLLM)
histogram_quantile(0.95, sum(rate(vllm:e2e_request_latency_seconds_bucket[5m])) by (le, model_name))

# Total model requests (scales with dashboard time range)
sum(increase(vllm:e2e_request_latency_seconds_count[$__range]))

# Token throughput (works with both real vLLM and simulator)
# Uses OR to support both metric naming conventions
sum(rate(vllm:prompt_tokens_total[5m])) or sum(rate(vllm:request_prompt_tokens_sum[5m]))  # Prompt tokens/s
sum(rate(vllm:generation_tokens_total[5m]))  # Generation tokens/s

# Resource allocation per model pod (CPU requests/limits)
kube_pod_container_resource_requests{namespace="llm", resource="cpu", container="main"}
kube_pod_container_resource_limits{namespace="llm", resource="cpu", container="main"}

# Resource allocation per model pod (Memory requests/limits)
kube_pod_container_resource_requests{namespace="llm", resource="memory", container="main"}
kube_pod_container_resource_limits{namespace="llm", resource="memory", container="main"}

# Requests & errors per user (authorized vs rate-limited)
sum by (user) (rate(authorized_calls[5m]))
sum by (user) (rate(limited_calls[5m]))
```

## 🔗 Related Files

- **TelemetryPolicy**: `deployment/base/observability/telemetry-policy.yaml`
- **ServiceMonitors**: `deployment/components/observability/prometheus/`

## 📊 Dashboard Panels

### Platform Admin Dashboard

**Variables (dropdown selectors):**
- `Datasource` - Prometheus datasource
- `MaaS Namespace` - Filter by namespace (default: All)
- `Model` - Filter model metrics by model name (default: All)

**Sections:**

| Section | Panels |
| ------- | ------ |
| **🏥 Component Health** | Limitador status, Authorino status, MaaS API pods, Gateway pods |
| **🚨 Alerts** | Firing alerts count, Active alerts table (MaaS namespaces only) |
| **📊 Key Metrics** | Total authorized hits, Current rate, Success rate, Active users, P50 latency |
| **📈 Traffic Analysis** | Request rate by model, Overall error rate (4xx/5xx/rate-limited), Request rate by tier, P95 latency by service |
| **🏆 Top Users** | Top 10 by hits, Top 10 by declined requests |
| **🤖 Model Metrics** | Requests running, Requests waiting, GPU cache usage, Total requests, Model queue depth, Model inference latency (P50/P95/P99) |
| **🔤 Token Metrics** | Tokens (1h), Token throughput (works with simulator and real vLLM) |
| **📦 Resource Allocation** | Resource allocation per model table (CPU/Memory requests/limits) |
| **👤 User Tracking** | Requests & errors per user (authorized vs rate-limited) |
| **📋 Detailed Breakdown** | Request rate by user, Request volume by user/model/tier |
| **🔮 Blocked Features** | Latency per user (placeholder), Token consumption per user (placeholder), Implementation notes |

### AI Engineer Dashboard
- **User-filtered views**: Per-user request volumes and rate limiting

## 📝 Notes

- Dashboards are compatible with Kuadrant v1.2.0+ (with custom Limitador build)
- ✅ Per-user, per-model, per-tier filtering is fully working
- ✅ P50/P95/P99 latency from Istio gateway histograms
- ✅ Error tracking (401, 429, 5xx) from Istio + Limitador
- ✅ Alert integration (MaaS-filtered firing alerts)
- ✅ vLLM/KServe model metrics (queue depth, GPU cache, inference latency)
- ✅ Model selector dropdown to filter model metrics
- ✅ **Resource allocation per model** - CPU/Memory requests/limits from kube-state-metrics
- ✅ **Requests & errors per user** - authorized vs rate-limited from Limitador
- ✅ Token metrics work with both real vLLM (`vllm:prompt_tokens_total`) and simulator (`vllm:request_prompt_tokens_sum`) - dashboards use `OR` queries for compatibility
- ⚠️ Model latency histograms only appear after traffic is generated (lazy-initialized)
- ❌ **Latency per user** - Blocked: Istio metrics don't include `user` label (requires EnvoyFilter)
- ❌ **Token consumption per user** - Blocked: vLLM doesn't label metrics with `user` (requires vLLM changes)
- Requires Prometheus Operator for ServiceMonitor support
- Dashboard auto-refreshes every 30 seconds

To customize the dashboard:
1. Import into Grafana
2. Edit panels as needed
3. Export updated JSON
4. Replace this file with your custom version
