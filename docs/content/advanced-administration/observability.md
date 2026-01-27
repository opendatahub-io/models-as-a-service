# Observability

This document covers the observability stack for the MaaS Platform, including metrics collection, monitoring, and visualization.

!!! warning "Important"
    [User Workload Monitoring](https://docs.redhat.com/en/documentation/monitoring_stack_for_red_hat_openshift/4.20/html-single/configuring_user_workload_monitoring/index#enabling-monitoring-for-user-defined-projects_preparing-to-configure-the-monitoring-stack-uwm) must be enabled in order to collect metrics.

    Add `enableUserWorkload: true` to the `cluster-monitoring-config` in the `openshift-monitoring` namespace. See [cluster-monitoring-config.yaml](https://github.com/opendatahub-io/models-as-a-service/blob/main/docs/samples/observability/cluster-monitoring-config.yaml) for an example.

## Overview

As part of Dev Preview, MaaS Platform includes a basic observability stack that provides insights into system performance, usage patterns, and operational health.

!!! note
    The observability stack will be enhanced in future releases.

The observability stack consists of:

- **Limitador**: Rate limiting service that exposes metrics
- **Prometheus**: Metrics collection and storage (uses OpenShift platform Prometheus)
- **ServiceMonitors**: Deployed to configure Prometheus metric scraping
- **Visualization**: Grafana dashboards (see [Grafana documentation](https://grafana.com/docs/grafana/latest/))

## Metrics Collection

### Limitador Metrics

Limitador exposes several key metrics that are collected through a ServiceMonitor by Prometheus:

#### Rate Limiting Metrics

- `authorized_hits`: Total tokens consumed for authorized requests (extracted from `usage.total_tokens` in model responses)
- `authorized_calls`: Number of requests allowed
- `limited_calls`: Number of requests denied due to rate limiting

!!! info "Token vs Request Metrics"
    With `TokenRateLimitPolicy`, `authorized_hits` tracks **token consumption** (extracted from LLM response bodies), not request counts. Use `authorized_calls` for request counts.

#### Performance Metrics

- `limitador_ratelimit_duration_seconds`: Duration of rate limit checks
- `limitador_ratelimit_active_connections`: Number of active connections
- `limitador_ratelimit_cache_hits_total`: Cache hit rate
- `limitador_ratelimit_cache_misses_total`: Cache miss rate

#### Labels via TelemetryPolicy

The TelemetryPolicy adds these labels to Limitador metrics:

- `user`: User identifier (extracted from `auth.identity.userid`)
- `tier`: User tier (extracted from `auth.identity.tier`)
- `model`: Model name (extracted from request path)

### ServiceMonitor Configuration

ServiceMonitors are automatically deployed as part of `deploy-openshift.sh` to configure OpenShift's Prometheus to discover and scrape metrics from MaaS components.

**Automatically Deployed ServiceMonitors:**

- **Limitador**: Scrapes rate limiting metrics from Limitador pods in `kuadrant-system`
- **Istio Gateway**: Scrapes Envoy metrics from the MaaS gateway in `openshift-ingress`

**Optional ServiceMonitors (in `docs/samples/observability/`):**

- **KServe LLM Models**: For scraping vLLM metrics from your deployed models

To deploy the optional KServe ServiceMonitor:

```bash
kubectl apply -f docs/samples/observability/kserve-llm-models-servicemonitor.yaml
```

**Manual ServiceMonitor Creation (Advanced):**

If you need to create additional ServiceMonitors for custom services:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: your-service-monitor
  namespace: your-namespace  # Same namespace as the service
  labels:
    app: your-app
spec:
  selector:
    matchLabels:
      app: your-service
  endpoints:
  - port: http
    path: /metrics
    interval: 30s
```

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

For local development and testing, you can also use our [Limitador Persistence](limitador-persistence.md) guide which includes a basic Redis setup script.

## Visualization

For dashboard visualization options, see:

- **OpenShift Monitoring**: [Monitoring overview](https://docs.redhat.com/en/documentation/openshift_container_platform/4.19/html/monitoring/index)
- **Grafana on OpenShift**: [Red Hat OpenShift AI Monitoring](https://docs.redhat.com/en/documentation/red_hat_openshift_ai_self-managed/2.19/html/monitoring_data_science_models/index)

### Included Dashboards

MaaS includes two Grafana dashboards for different personas:

#### Platform Admin Dashboard

| Section | Description |
|---------|-------------|
| **Component Health** | Limitador, Authorino, MaaS API, and Gateway pod status |
| **Alerts** | Firing alerts count and active alerts table |
| **Key Metrics** | Total tokens, total requests, token rate, request rate, success rate, active users (5m) |
| **Traffic Analysis** | Token rate by model, error rates, P95/P99 latency |
| **Top Users** | Top 10 by requests, Top 10 by token usage |
| **Token Consumption** | Token usage by tier and by user |
| **Model Metrics** | vLLM metrics (queue depth, GPU cache, inference latency) |
| **User Tracking** | Per-user token usage and rate limit violations |

!!! info "Active Users Metric"
    The "Active Users (5m)" panel shows users with activity in the **last 5 minutes**, not all-time unique users. This provides an accurate view of current platform usage.

#### AI Engineer Dashboard

| Section | Description |
|---------|-------------|
| **My Usage Summary** | Total tokens, total requests, token rate, request rate, rate-limited requests, success rate |
| **Usage Trends** | Token usage by model, usage trends (tokens vs rate-limited requests) |
| **Hourly Patterns** | Hourly token usage breakdown by model |
| **Detailed Analysis** | Token volume and rate-limited requests by model |

!!! info "User Filtering"
    The AI Engineer Dashboard includes a user dropdown to filter metrics. Select your user ID to see only your personal usage metrics.

!!! warning "Security Consideration"
    The user dropdown in the AI Engineer Dashboard populates from Prometheus label values, which exposes all user IDs to anyone with dashboard access. For multi-tenant environments where user enumeration is a concern, consider restricting dashboard access via Grafana/Perses RBAC or provisioning per-user dashboards with static user filters.

!!! info "Tokens vs Requests"
    Both dashboards show **token consumption** (`authorized_hits`) for billing/cost tracking and **request counts** (`authorized_calls`) for capacity planning. Blue panels indicate request metrics; green panels indicate token metrics.

### Deploying Dashboards

Dashboards are deployed automatically by `install-observability.sh`, or manually:

```bash
# Deploy Grafana operator and instance
kubectl apply -k deployment/components/observability/grafana/

# Deploy dashboards
kubectl apply -k deployment/components/observability/dashboards/
```

### Sample Dashboard JSON

For manual import, sample dashboard JSON files are available:

- [MaaS Token Metrics Dashboard](https://github.com/opendatahub-io/models-as-a-service/blob/main/docs/samples/dashboards/maas-token-metrics-dashboard.json)

To import into Grafana:

1. Go to Grafana → Dashboards → Import
2. Upload the JSON file or paste content
3. Select your Prometheus datasource

## Key Metrics Reference

### Token and Request Metrics

| Metric | Type | Description | Use Case |
|--------|------|-------------|----------|
| `authorized_hits` | Counter | Total tokens consumed (extracted from `usage.total_tokens` in LLM responses) | Billing, cost tracking |
| `authorized_calls` | Counter | Total requests that were successfully authorized | Capacity planning, usage patterns |
| `limited_calls` | Counter | Total requests denied due to rate limiting (HTTP 429) | Rate limit monitoring |

All metrics include labels: `user`, `tier`, `model`, `limitador_namespace`

!!! tip "When to use which metric"
    - **Billing/Cost**: Use `authorized_hits` - represents actual token consumption
    - **API Usage**: Use `authorized_calls` - represents number of API calls
    - **Rate Limiting**: Use `limited_calls` - shows quota violations

### Latency Metrics

| Metric | Description | Labels |
|--------|-------------|--------|
| `istio_request_duration_milliseconds_bucket` | Gateway-level latency histogram | `destination_service_name` |
| `vllm:e2e_request_latency_seconds` | Model inference latency | `model_name` |

### Common Queries

**Token-based queries (billing/cost):**

```promql
# Total tokens consumed per user
sum by (user) (authorized_hits)

# Token consumption rate per model (tokens/sec)
sum by (model) (rate(authorized_hits[5m]))

# Top 10 users by tokens consumed
topk(10, sum by (user) (authorized_hits))

# Token consumption by tier
sum by (tier) (authorized_hits)
```

**Request-based queries (capacity/usage):**

```promql
# Total requests per user
sum by (user) (authorized_calls)

# Request rate per tier (requests/sec)
sum by (tier) (rate(authorized_calls[5m]))

# Request rate per model
sum by (model) (rate(authorized_calls[5m]))

# Top 10 users by request count
topk(10, sum by (user) (authorized_calls))
```

**Rate limiting and success metrics:**

```promql
# Success rate (percentage of requests not rate-limited)
sum(authorized_calls) / (sum(authorized_calls) + sum(limited_calls))

# Success rate by tier
sum by (tier) (authorized_calls) / (sum by (tier) (authorized_calls) + sum by (tier) (limited_calls))

# Rate limit violations by tier
sum by (tier) (rate(limited_calls[5m]))

# Users hitting rate limits
topk(10, sum by (user) (limited_calls))
```

**Latency queries:**

```promql
# P99 latency by service
histogram_quantile(0.99, sum by (destination_service_name, le) (rate(istio_request_duration_milliseconds_bucket[5m])))

# P50 (median) latency
histogram_quantile(0.5, sum by (le) (rate(istio_request_duration_milliseconds_bucket[5m])))
```

## Known Limitations

### Currently Blocked Features

Some dashboard features require upstream changes and are currently blocked:

| Feature | Blocker | Workaround |
|---------|---------|------------|
| **Latency per user** | Istio metrics don't include `user` label | Requires EnvoyFilter to inject user context |
| **Input/Output token breakdown per user** | vLLM doesn't label metrics with `user` | Total tokens available via `authorized_hits`; breakdown requires vLLM changes |

!!! note "Total Tokens vs Token Breakdown"
    Total token consumption per user **is available** via `authorized_hits{user="..."}`. The blocked feature is specifically the input/output token breakdown (prompt vs generation tokens) per user, which requires vLLM to accept user context in requests.
