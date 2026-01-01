# 🔄 Metrics Export and Enrichment Flow

## Who Exports and Enriches Metrics?

### Current Flow (All Working ✅)

| Component | Role | What It Does | Status |
| --------- | ---- | ------------ | ------ |
| **TelemetryPolicy** | Configuration | Defines which labels to extract (`user`, `tier`, `model`) | ✅ Working |
| **Kuadrant Operator** | Processor | Reads TelemetryPolicy and configures the gateway | ✅ Working |
| **Envoy WasmPlugin** | Extractor | Extracts `user`, `tier`, `model` from request/response | ✅ Working |
| **Authorino** | Identity Provider | Provides `auth.identity.userid` and `auth.identity.tier` | ✅ Working |
| **Limitador** | Rate Limiter & Metrics Exporter | Receives metadata, uses for rate limiting, **exports metrics with labels** | ✅ Working |
| **Prometheus** | Metrics Collector | Scrapes metrics from Limitador | ✅ Working |
| **HAProxy** | Ingress Router | Exports latency metrics for routes | ✅ Working |

---

## Detailed Flow

### Step 1: Label Extraction (✅ Working)

**Component**: Envoy WasmPlugin (configured by TelemetryPolicy)

**What it extracts:**

- `model`: From `request.path.split("/")[2]` - ✅ Works
- `user`: From `auth.identity.userid` - ✅ Works
- `tier`: From `auth.identity.tier` - ✅ Works

**Status**: ✅ All labels extracted correctly

---

### Step 2: Metadata Transmission (✅ Working)

**Component**: Envoy WasmPlugin → Limitador

**What happens:**

- Envoy WasmPlugin sends extracted labels as **dynamic metadata** to Limitador
- Metadata is sent via gRPC to Limitador service

**Status**: ✅ Metadata is transmitted successfully

---

### Step 3: Rate Limiting (✅ Working)

**Component**: Limitador

**What happens:**

- Limitador receives the dynamic metadata (`user`, `tier`, `model`)
- Uses metadata for rate limiting decisions
- Tracks counters per `user`/`tier`/`model` combination

**Status**: ✅ Rate limiting works with custom labels

---

### Step 4: Metrics Export (✅ Working)

**Component**: Limitador → Prometheus

**What happens:**

- Limitador exports metrics WITH custom labels: `authorized_hits{user="...", tier="...", model="..."}`

**Verified Output:**

```text
authorized_hits{model="facebook-opt-125m-simulated",tier="free",user="tgitelma-redhat-com-dd264a84",limitador_namespace="llm/facebook-opt-125m-simulated-kserve-route"} 376
authorized_calls{user="ahadas-redhat-com-1e8bdd56",tier="free",model="facebook-opt-125m-simulated",limitador_namespace="llm/facebook-opt-125m-simulated-kserve-route"} 19
limited_calls{model="facebook-opt-125m-simulated",user="tgitelma-redhat-com-dd264a84",tier="free",limitador_namespace="llm/facebook-opt-125m-simulated-kserve-route"} 20
```

**Status**: ✅ All custom labels exported to Prometheus

---

### Step 5: Istio Gateway Metrics (✅ Working)

**Component**: Istio Gateway (maas-default-gateway) → Prometheus

**What happens:**

- Istio gateway exports HTTP request metrics including latency histograms
- ServiceMonitor `istio-gateway-metrics` in `openshift-ingress` scrapes port 15090
- Provides P50/P95/P99 latency and request counts by response code

**Key Metrics:**
- `istio_requests_total{response_code="..."}` - Request counts by HTTP status
- `istio_request_duration_milliseconds_bucket` - Latency histograms

**Status**: ✅ Full HTTP metrics including histograms

---

### Step 6: HAProxy Metrics (⚠️ Limited)

**Component**: HAProxy → Prometheus

**What happens:**

- HAProxy exports latency metrics for HTTP routes only
- `maas-gateway-route` uses **TCP passthrough** - shows 0ms latency
- Use Istio metrics for MaaS latency instead

**Status**: ⚠️ Limited - TCP passthrough routes don't have HTTP metrics

---

### Step 7: vLLM/KServe Model Metrics (✅ Working)

**Component**: KServe Model Pods → Prometheus

**What happens:**

- KServe model pods expose vLLM-compatible metrics on port 8000 (HTTPS)
- ServiceMonitor `kserve-llm-models` scrapes all pods with label `app.kubernetes.io/part-of: llminferenceservice`
- Provides queue depth, GPU cache usage, latency histograms, and token histograms

**Key Metrics:**
- `vllm:num_requests_running` - Requests currently being processed
- `vllm:num_requests_waiting` - Requests waiting in queue
- `vllm:gpu_cache_usage_perc` - GPU KV cache utilization
- `vllm:e2e_request_latency_seconds` - Histogram of end-to-end request latency (P50/P95/P99)
- `vllm:request_inference_time_seconds` - Histogram of inference processing time

**Note**: Latency histogram metrics are **lazy-initialized** and only appear after traffic is generated.

**Token Metric Compatibility:**
| Metric Type | Real vLLM | Simulator |
| ----------- | --------- | --------- |
| Prompt tokens | `vllm:prompt_tokens_total` | `vllm:request_prompt_tokens_sum` |
| Generation tokens | `vllm:generation_tokens_total` | `vllm:request_generation_tokens_sum` |

Dashboard queries use `OR` operator to support both naming conventions automatically.

**Status**: ✅ Queue depth, latency histograms, and token metrics all available

---

## Component Summary

| Component | Exports Metrics? | Enriches with Custom Labels? | Status |
| --------- | ---------------- | ---------------------------- | ------ |
| **TelemetryPolicy** | ❌ No | ✅ Configures extraction | ✅ Working |
| **Envoy WasmPlugin** | ❌ No | ✅ Extracts labels | ✅ Working |
| **Istio Gateway** | ✅ Yes (HTTP metrics, histograms) | ✅ `destination_service_name`, `response_code` | ✅ Working |
| **Authorino** | ✅ Yes (operator metrics only) | ❌ No auth request metrics | ⚠️ Limited |
| **Limitador** | ✅ Yes | ✅ Yes - exports all labels | ✅ Working |
| **vLLM/KServe** | ✅ Yes (queue, GPU cache, latency, tokens) | ✅ `model_name` | ✅ Fully Working |
| **HAProxy** | ✅ Yes (HTTP routes only) | ❌ No (route-level only) | ⚠️ TCP passthrough = 0ms |
| **Prometheus** | ❌ No | ❌ No | ✅ Working |

---

## Available Labels

| Label | Source | Example Value |
| ----- | ------ | ------------- |
| `user` | `auth.identity.userid` | `tgitelma-redhat-com-dd264a84` |
| `tier` | `auth.identity.tier` | `free`, `premium`, `enterprise` |
| `model` | `request.path` | `facebook-opt-125m-simulated` |
| `limitador_namespace` | HTTPRoute | `llm/facebook-opt-125m-simulated-kserve-route` |
| `route` | HAProxy | `maas-gateway-route` |

---

## ✅ Resolved Issues

| Feature | How It Was Fixed |
|---------|------------------|
| **P50/P95/P99 Latency** | ✅ Now using `istio_request_duration_milliseconds_bucket` histograms |
| **Unauthorized Requests (401)** | ✅ Now using `istio_requests_total{response_code="401"}` |
| **Rate Limited Requests (429)** | ✅ Now using `istio_requests_total{response_code="429"}` + Limitador `limited_calls` |

## What's Still Missing (Blocked by Dependencies)

| Missing Feature | Why | Blocked By |
|-----------------|-----|------------|
| **Latency per API Key/User** | Istio metrics don't include user labels | Would need custom Envoy filter |
| **Model Resource Allocation** | CPU/GPU/Memory per model | Requires DCGM Exporter |
| ~~Per-Request Token Counts~~ | ~~Token counts not exposed by simulators~~ | ✅ RESOLVED - Simulator exposes `vllm:request_*_tokens_sum` |

**Resolved:**
- ✅ **Model Inference Latency** - Available via `vllm:e2e_request_latency_seconds` (P50/P95/P99)

---

## Verification Commands

```bash
# Check Limitador metrics directly
oc exec -n kuadrant-system deploy/limitador-limitador -- curl -s localhost:8080/metrics | grep -E "^(authorized_hits|authorized_calls|limited_calls)"

# Check Istio gateway metrics directly
oc exec -n openshift-ingress deploy/maas-default-gateway-openshift-default -- pilot-agent request GET /stats/prometheus | grep "istio_requests_total"

# Check Istio latency histograms
oc exec -n openshift-ingress deploy/maas-default-gateway-openshift-default -- pilot-agent request GET /stats/prometheus | grep "istio_request_duration"

# Check vLLM/KServe model metrics (generates traffic first, then check metrics)
kubectl exec -n llm deploy/facebook-opt-125m-simulated-kserve -- curl -sk https://localhost:8000/metrics | grep "vllm:"

# Check latency histograms (only available after traffic)
kubectl exec -n llm deploy/facebook-opt-125m-simulated-kserve -- curl -sk https://localhost:8000/metrics | grep -E "vllm:(e2e_request_latency|request_inference_time)"

# Verify ServiceMonitors exist
oc get servicemonitor istio-gateway-metrics -n openshift-ingress
oc get servicemonitor kserve-llm-models -n maas-api
```

---

## Dashboard Deployment

### GitOps Installation (Persistent)

Dashboards can be deployed as Kubernetes CRDs for GitOps management:

```bash
# Ensure Grafana instance has the required label
oc label grafana grafana -n llm-observability app=grafana

# Deploy dashboards via Kustomize
oc apply -k deployment/components/observability/dashboards
```

### Dashboard CRD Structure

```text
deployment/components/observability/dashboards/
├── kustomization.yaml           # Kustomize config
├── dashboard-platform-admin.yaml # GrafanaDashboard CRD
└── dashboard-ai-engineer.yaml    # GrafanaDashboard CRD
```

### Installed Dashboards

| Dashboard | Folder | Installation Method |
|-----------|--------|---------------------|
| Platform Admin | MaaS v1.0 | ✅ GitOps (CRD) |
| AI Engineer | MaaS v1.0 | ✅ GitOps (CRD) |
| Token Metrics | - | Manual import from `docs/samples/dashboards/` |
