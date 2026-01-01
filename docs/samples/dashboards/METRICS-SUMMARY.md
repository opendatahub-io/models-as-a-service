# 📊 MaaS Metrics Summary

## ✅ AVAILABLE METRICS (Currently Working in Prometheus)

### Limitador Metrics

| Metric | Source | Labels Available | Notes |
| ------ | ------ | ---------------- | ----- |
| `authorized_hits` | Limitador | ✅ `user`, `tier`, `model`, `limitador_namespace` | ✅ Working - Counts successful API calls |
| `authorized_calls` | Limitador | ✅ `user`, `tier`, `model`, `limitador_namespace` | ✅ Working - Rate limiting success counter |
| `limited_calls` | Limitador | ✅ `user`, `tier`, `model`, `limitador_namespace` | ✅ Working - Rate limiting block counter |
| `limitador_up` | Limitador | Standard Prometheus labels | ✅ Working - Health check metric |

**Example metrics (verified on cluster):**

```text
authorized_hits{model="facebook-opt-125m-simulated",tier="free",user="tgitelma-redhat-com-dd264a84",limitador_namespace="llm/facebook-opt-125m-simulated-kserve-route"} 376
authorized_calls{user="ahadas-redhat-com-1e8bdd56",tier="free",model="facebook-opt-125m-simulated",limitador_namespace="llm/facebook-opt-125m-simulated-kserve-route"} 19
limited_calls{model="facebook-opt-125m-simulated",user="tgitelma-redhat-com-dd264a84",tier="free",limitador_namespace="llm/facebook-opt-125m-simulated-kserve-route"} 20
```

**Note**:

- `limitador_namespace` identifies the HTTPRoute (e.g., `llm/facebook-opt-125m-simulated-kserve-route`)
- **TelemetryPolicy** (`deployment/base/observability/telemetry-policy.yaml`) configures extraction of `user`, `tier`, `model` labels
- All custom labels are now exported correctly by Limitador

---

### Authorino Metrics

| Metric | Source | Labels Available | Notes |
| ------ | ------ | ---------------- | ----- |
| `controller_runtime_reconcile_errors_total` | Authorino | `controller`, `namespace` | ✅ Working - Operator reconciliation errors |
| `controller_runtime_reconcile_total` | Authorino | `controller`, `namespace`, `result` | ✅ Working - Operator reconciliation count |
| `controller_runtime_active_workers` | Authorino | `controller` | ✅ Working - Active reconciliation workers |

**Note**: Authorino only exposes **operator/controller metrics**, NOT auth request metrics like `auth_server_response_status_total`. Auth request tracking is done via Istio gateway metrics.

---

### Istio Gateway Metrics (NEW - Service Mesh)

| Metric | Source | Labels Available | Notes |
| ------ | ------ | ---------------- | ----- |
| `istio_requests_total` | Istio Gateway | `response_code`, `destination_service_name`, `destination_service_namespace` | ✅ Working - HTTP requests by status code |
| `istio_request_duration_milliseconds` | Istio Gateway | `destination_service_name`, `response_code` | ✅ Working - Request latency histograms |
| `istio_request_bytes` | Istio Gateway | `destination_service_name` | ✅ Working - Request size histograms |
| `istio_response_bytes` | Istio Gateway | `destination_service_name` | ✅ Working - Response size histograms |

**Useful Queries:**

```promql
# Request rate by service
sum by (destination_service_name) (rate(istio_requests_total[5m]))

# Unauthorized requests (401)
sum(rate(istio_requests_total{response_code="401"}[5m]))

# Rate limited requests (429)
sum(rate(istio_requests_total{response_code="429"}[5m]))

# P95 latency by service
histogram_quantile(0.95, sum by (destination_service_name, le) (rate(istio_request_duration_milliseconds_bucket[5m])))

# P50 latency (median)
histogram_quantile(0.5, sum(rate(istio_request_duration_milliseconds_bucket[5m])) by (le))
```

**Verified Response Codes:**
- `200` - Successful inference requests
- `201` - Successful API key creation
- `401` - Unauthorized (invalid/missing token)
- `429` - Rate limited
- `500` - Server errors
- `503` - Service unavailable

---

### HAProxy Ingress Metrics

| Metric | Source | Labels Available | Notes |
| ------ | ------ | ---------------- | ----- |
| `haproxy_backend_http_responses_total` | HAProxy | `code` (2xx, 4xx, 5xx), `route` | ✅ Working - HTTP routes only |
| `haproxy_backend_http_average_response_latency_milliseconds` | HAProxy | `route` | ⚠️ Limited - TCP passthrough shows 0ms |

**Important**: The `maas-gateway-route` uses **TCP passthrough** (`backend="tcp"`). HAProxy doesn't measure HTTP metrics for TCP passthrough routes - it just forwards raw TCP. Use **Istio gateway metrics** for MaaS latency instead.

**Useful Queries (for HTTP routes only):**

```promql
# Total 2xx responses through cluster ingress (HTTP routes)
sum(haproxy_backend_http_responses_total{code="2xx"})

# 4xx errors for non-TCP routes
sum(rate(haproxy_backend_http_responses_total{code="4xx", backend!="tcp"}[5m]))
```

---

### Kubernetes Metrics (kube-state-metrics)

| Metric | Source | Labels Available | Notes |
| ------ | ------ | ---------------- | ----- |
| `kube_pod_status_phase` | kube-state-metrics | `namespace`, `pod`, `phase` | ✅ Working - Pod health status |

**Useful Queries:**

```promql
# Running pods in maas-api namespace
count(kube_pod_status_phase{namespace="maas-api", phase="Running"} == 1)

# Gateway pods running
count(kube_pod_status_phase{namespace="openshift-ingress", pod=~"maas.*", phase="Running"} == 1)
```

---

### vLLM/KServe Model Metrics

| Metric | Source | Labels Available | Notes |
| ------ | ------ | ---------------- | ----- |
| `vllm:num_requests_running` | vLLM/Simulator | `model_name` | ✅ Available - requests currently processing |
| `vllm:num_requests_waiting` | vLLM/Simulator | `model_name` | ✅ Available - requests in queue |
| `vllm:gpu_cache_usage_perc` | vLLM/Simulator | `model_name` | ✅ Available - GPU KV-cache utilization (0-1) |
| `vllm:e2e_request_latency_seconds` | vLLM/Simulator (v0.6.1+) | `model_name` | ✅ Available - histogram of end-to-end request latency |
| `vllm:time_to_first_token_seconds` | vLLM/Simulator (v0.6.1+) | `model_name` | ✅ **NEW** - TTFT histogram (critical for streaming UX) |
| `vllm:time_per_output_token_seconds` | vLLM/Simulator (v0.6.1+) | `model_name` | ✅ **NEW** - ITL histogram (inter-token latency) |
| `vllm:request_prefill_time_seconds` | vLLM/Simulator (v0.6.1+) | `model_name` | ✅ Available - time in PREFILL phase |
| `vllm:request_decode_time_seconds` | vLLM/Simulator (v0.6.1+) | `model_name` | ✅ Available - time in DECODE phase |
| `vllm:request_inference_time_seconds` | vLLM/Simulator (v0.6.1+) | `model_name` | ✅ Available - histogram of inference processing time |
| `vllm:prompt_tokens_total` | vLLM (real only) | `model_name` | ✅ Counter - total prompt tokens (real vLLM) |
| `vllm:generation_tokens_total` | vLLM (real only) | `model_name` | ✅ Counter - total generation tokens (real vLLM) |
| `vllm:request_prompt_tokens_sum` | vLLM/Simulator | `model_name` | ✅ Histogram sum - prompt tokens per request |
| `vllm:request_generation_tokens_sum` | vLLM/Simulator | `model_name` | ✅ Histogram sum - generation tokens per request |
| `vllm:request_success_total` | vLLM/Simulator | `model_name`, `finish_reason` | ✅ Available - success counter by finish reason |

**Note**: Metrics are scraped via ServiceMonitor `kserve-llm-models` which targets all services with label `app.kubernetes.io/part-of: llminferenceservice`. 

**⚠️ Token Metric Name Differences:**

| Metric Type | Real vLLM | Simulator | Dashboard Query |
| ----------- | --------- | --------- | --------------- |
| Prompt tokens | `vllm:prompt_tokens_total` | `vllm:request_prompt_tokens_sum` | Uses `OR` to support both |
| Generation tokens | `vllm:generation_tokens_total` | `vllm:request_generation_tokens_sum` | Uses `OR` to support both |

The dashboard token panels use PromQL `OR` operator to automatically work with **both** real vLLM and the simulator.

**LLM-Specific Latency Queries:**

```promql
# Time to First Token (TTFT) - P50
histogram_quantile(0.5, sum(rate(vllm:time_to_first_token_seconds_bucket[5m])) by (le, model_name))

# Time to First Token (TTFT) - P95
histogram_quantile(0.95, sum(rate(vllm:time_to_first_token_seconds_bucket[5m])) by (le, model_name))

# Inter-Token Latency (ITL) - P50
histogram_quantile(0.5, sum(rate(vllm:time_per_output_token_seconds_bucket[5m])) by (le, model_name))

# Inter-Token Latency (ITL) - P95
histogram_quantile(0.95, sum(rate(vllm:time_per_output_token_seconds_bucket[5m])) by (le, model_name))

# End-to-End Latency - P95
histogram_quantile(0.95, sum(rate(vllm:e2e_request_latency_seconds_bucket[5m])) by (le, model_name))
```

**Important**: Latency histogram metrics (`vllm:e2e_request_latency_seconds`, `vllm:time_to_first_token_seconds`, `vllm:time_per_output_token_seconds`) only appear **after traffic is generated** - they are lazy-initialized. The simulator (v0.6.1+) exposes all these metrics.

---

### TelemetryPolicy Configuration Summary

**Policy**: `user-group` (targets `maas-default-gateway`)  
**File**: `deployment/base/observability/telemetry-policy.yaml`

| Configured Label | Extraction Method | Status |
| ---------------- | ----------------- | ------ |
| `model` | `request.path.split("/")[2]` - Extracts from URL path `/llm/{model}/v1/...` | ✅ Working |
| `tier` | `auth.identity.tier` - From Authorino identity context | ✅ Working |
| `user` | `auth.identity.userid` - From Authorino identity context | ✅ Working |

**How it works:**

1. ✅ Envoy WasmPlugin extracts `model`, `tier`, `user` from request context
2. ✅ Sends this data as **dynamic metadata** to Limitador for rate limiting decisions
3. ✅ **Limitador exports all labels to Prometheus metrics**

**Verified Output:**

```text
authorized_hits{model="facebook-opt-125m-simulated", tier="free", user="tgitelma-redhat-com-dd264a84", limitador_namespace="llm/..."}
```

---

## 📋 Dashboard Queries

### ✅ Working Queries with Custom Labels

All queries using `user`, `tier`, `model` labels are now working!

#### Per-User Queries

```promql
# Requests per user
sum by (user) (authorized_hits)

# Rate limited requests per user
sum by (user) (limited_calls)

# User throughput
rate(authorized_hits{user="tgitelma-redhat-com-dd264a84"}[5m])
```

#### Per-Model Queries

```promql
# Requests per model
sum by (model) (authorized_hits)

# Model error rates
sum by (model) (limited_calls)

# Model throughput
rate(authorized_hits{model="facebook-opt-125m-simulated"}[5m])
```

#### Per-Tier Queries

```promql
# Requests per tier
sum by (tier) (authorized_hits)

# Tier distribution
sum by (tier) (rate(authorized_hits[5m]))
```

#### Combined Queries

```promql
# User activity by model
sum by (user, model) (authorized_hits)

# Top users by requests
topk(10, sum by (user) (authorized_hits))

# Success rate per user
sum by (user) (authorized_calls) / (sum by (user) (authorized_calls) + sum by (user) (limited_calls))
```

---

### Dashboard Query Summary

| Dashboard | Status | Capabilities |
| --------- | ------ | ------------ |
| **Platform Admin** | ✅ Fully Working | Component health, per-model metrics, per-user traffic, tier analysis, latency by route |
| **AI Engineer** | ✅ Fully Working | Per-user filtering, model usage, rate limit tracking, hourly patterns |
| **Token Metrics** | ✅ Fully Working | Revenue calculations, cost per user, billing tables |

---

## ✅ RESOLVED: Latency & Error Metrics

With Istio gateway metrics now scraped, we have:

| Feature | Status | Metric Used |
|---------|--------|-------------|
| **P50/P95/P99 Latency** | ✅ Working | `histogram_quantile(0.95, istio_request_duration_milliseconds_bucket)` |
| **Latency per Service** | ✅ Working | Grouped by `destination_service_name` |
| **Unauthorized Requests** | ✅ Working | `istio_requests_total{response_code="401"}` |
| **Rate Limited Requests** | ✅ Working | `istio_requests_total{response_code="429"}` + `limited_calls` |

---

## ❌ REMAINING GAPS (Blocked by Dependencies)

| Missing Feature | Why It's Needed | What's Blocking It |
|-----------------|-----------------|-------------------|
| **Latency per User/API Key** | Track response time per customer | Istio metrics don't include `user` label - requires EnvoyFilter |
| **Token Consumption per User** | Track tokens per customer | vLLM doesn't label metrics with `user` - requires vLLM code changes |

**Recently Resolved ✅:**

| Feature | Solution |
|---------|----------|
| **Model Resource Allocation** | ✅ Available via `kube_pod_container_resource_requests{namespace="llm"}` |
| **Model Inference Latency** | ✅ Available via `vllm:e2e_request_latency_seconds` histogram (P50/P95/P99) |
| **Request/Error per User** | ✅ Available via Limitador `authorized_calls{user="..."}` and `limited_calls{user="..."}` |

### Current Workarounds

| Missing Feature | Current Workaround |
|-----------------|-------------------|
| Model Status | Using `kube_pod_status_phase` to show running pod counts |
| Token Consumption | Using request counts as proxy for usage |
| Per-user latency | Showing service-level latency (maas-api vs model service) |

---

## ✅ Current Status

### Custom Labels Are Working!

**Verified on cluster** - All custom labels (`user`, `tier`, `model`) are now being exported by Limitador.

| Component | Status | Notes |
| --------- | ------ | ----- |
| **TelemetryPolicy** | ✅ Correctly configured | Extracts user, tier, model |
| **Limitador Export** | ✅ All labels exported | Custom Limitador build |
| **Prometheus Scraping** | ✅ Working | ServiceMonitors deployed |
| **Istio Gateway Metrics** | ✅ Working | P50/P95/P99 latency, HTTP status codes |
| **kube-state-metrics** | ✅ Working | Pod status |
| **HAProxy Metrics** | ⚠️ Limited | TCP passthrough for MaaS - use Istio instead |

---

## 📊 Summary Table

| Category | Metrics Available | Custom Labels | Status |
| -------- | ----------------- | ------------- | ------ |
| **Limitador** | ✅ 4 metrics | ✅ `user`, `tier`, `model`, `limitador_namespace` | ✅ Fully working |
| **Istio Gateway** | ✅ Requests, latency histograms | ✅ `response_code`, `destination_service_name` | ✅ Fully working |
| **Authorino** | ✅ Controller metrics only | ❌ No auth request metrics | ⚠️ Operator metrics only |
| **HAProxy** | ✅ HTTP routes only | ✅ `route`, `code` | ⚠️ TCP passthrough shows 0ms |
| **kube-state-metrics** | ✅ Pod status | ✅ `namespace`, `pod`, `phase` | ✅ Fully working |
| **TelemetryPolicy** | ✅ Configured | ✅ All labels exported | ✅ Fully working |

---

## 📋 Complete Metrics List

### ✅ Limitador Metrics (All Custom Labels Working)

| Metric Name | Available Labels | Filtering Capability |
| ----------- | ---------------- | -------------------- |
| `authorized_hits` | `user`, `tier`, `model`, `limitador_namespace` | ✅ By user, tier, model, route |
| `authorized_calls` | `user`, `tier`, `model`, `limitador_namespace` | ✅ By user, tier, model, route |
| `limited_calls` | `user`, `tier`, `model`, `limitador_namespace` | ✅ By user, tier, model, route |
| `limitador_up` | Standard labels | ✅ Health check |

### ✅ Istio Gateway Metrics (Primary for Latency & Errors)

| Metric Name | Available Labels | Filtering Capability |
| ----------- | ---------------- | -------------------- |
| `istio_requests_total` | `response_code`, `destination_service_name`, `destination_service_namespace` | ✅ By status code, service |
| `istio_request_duration_milliseconds_bucket` | `destination_service_name`, `le` | ✅ Histogram for P50/P95/P99 |
| `istio_request_bytes_bucket` | `destination_service_name`, `le` | ✅ Request size distribution |
| `istio_response_bytes_bucket` | `destination_service_name`, `le` | ✅ Response size distribution |

### ⚠️ Authorino Metrics (Operator Only)

| Metric Name | Available Labels | Filtering Capability |
| ----------- | ---------------- | -------------------- |
| `controller_runtime_reconcile_errors_total` | `controller`, `namespace` | ✅ By controller |
| `controller_runtime_reconcile_total` | `controller`, `result` | ✅ By result |

**Note**: Authorino does NOT expose `auth_server_response_status_total` in this deployment. Use Istio `istio_requests_total{response_code="401"}` for unauthorized requests.

### ⚠️ HAProxy Metrics (Limited for MaaS)

| Metric Name | Available Labels | Filtering Capability |
| ----------- | ---------------- | -------------------- |
| `haproxy_backend_http_responses_total` | `code`, `route` | ⚠️ HTTP routes only (not TCP passthrough) |
| `haproxy_backend_http_average_response_latency_milliseconds` | `route` | ⚠️ Shows 0ms for TCP passthrough |

**Note**: `maas-gateway-route` uses TCP passthrough. Use Istio metrics for MaaS latency.

### ✅ Kubernetes Metrics

| Metric Name | Available Labels | Filtering Capability |
| ----------- | ---------------- | -------------------- |
| `kube_pod_status_phase` | `namespace`, `pod`, `phase` | ✅ By namespace, pod, phase |

### 📍 Metrics Status Summary

| Metric Type | Source | Status |
| ----------- | ------ | ------ |
| **Resource allocation** | kube-state-metrics | ✅ **Available** - `kube_pod_container_resource_requests{namespace="llm"}` |
| **Request/Error per User** | Limitador | ✅ **Available** - `authorized_calls{user="..."}`, `limited_calls{user="..."}` |
| **Model Inference Latency** | vLLM | ✅ **Available** - `vllm:e2e_request_latency_seconds` (v0.6.1+) |
| **Token consumption (total)** | vLLM | ⚠️ **Partial** - `vllm:prompt_tokens_total` (requires real vLLM, not simulator) |
| **Latency per User** | Istio | ❌ **Blocked** - Requires EnvoyFilter to inject user label |
| **Token per User** | vLLM | ❌ **Blocked** - Requires vLLM code changes to label by user |

---

## 🔗 Related Files

- **TelemetryPolicy**: `deployment/base/observability/telemetry-policy.yaml`
- **ServiceMonitors**: `deployment/base/observability/prometheus-servicemonitors.yaml`
- **LLM Models ServiceMonitor**: `deployment/components/observability/monitors/kserve-llm-models-servicemonitor.yaml`
- **Platform Admin Dashboard JSON**: `docs/samples/dashboards/platform-admin-dashboard.json`
- **AI Engineer Dashboard JSON**: `docs/samples/dashboards/ai-engineer-dashboard.json`
- **Token Metrics Dashboard JSON**: `docs/samples/dashboards/maas-token-metrics-dashboard.json`
- **Install Script**: `scripts/install-observability.sh`

### GitOps Dashboard Installation (Persistent)

- **Dashboard Kustomization**: `deployment/components/observability/dashboards/kustomization.yaml`
- **Platform Admin CRD**: `deployment/components/observability/dashboards/dashboard-platform-admin.yaml`
- **AI Engineer CRD**: `deployment/components/observability/dashboards/dashboard-ai-engineer.yaml`

**Deploy persistent dashboards:**
```bash
# Ensure Grafana instance has the label
oc label grafana grafana -n llm-observability app=grafana

# Apply dashboard CRDs
oc apply -k deployment/components/observability/dashboards
```

**Dashboards installed via CRDs:**
- ✅ Platform Admin Dashboard → `MaaS v1.0` folder
- ✅ AI Engineer Dashboard → `MaaS v1.0` folder
- ⚠️ Token Metrics Dashboard → Manual import only (source in `docs/samples/dashboards/`)

---

## 📝 Notes

1. **Custom Labels Working**: All custom labels (`user`, `tier`, `model`) are now exported by Limitador and available for dashboard queries.

2. **TelemetryPolicy**: The policy correctly extracts labels from request context and Limitador exports them to Prometheus.

3. **Dashboard Compatibility**: All dashboards can now use full filtering by user, tier, and model.

4. **Latency Metrics**: Available at route level via HAProxy. For per-user latency, additional instrumentation would be needed.

5. **Blocked Features**: Only per-user latency (requires EnvoyFilter) and per-user tokens (requires vLLM changes) remain blocked.

6. **Verified Users**:
   - `tgitelma-redhat-com-dd264a84`
   - `ahadas-redhat-com-1e8bdd56`

7. **Verified Models**:
   - `facebook-opt-125m-simulated`
