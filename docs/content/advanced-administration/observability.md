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

    kubectl apply -f docs/samples/observability/kserve-llm-models-servicemonitor.yaml

**Manual ServiceMonitor Creation (Advanced):**

If you need to create additional ServiceMonitors for custom services:

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

For dashboard visualization, refer to the official documentation:

- **OpenShift Monitoring**: [Monitoring overview](https://docs.redhat.com/en/documentation/openshift_container_platform/4.19/html/monitoring/index)
- **Grafana**: [Grafana Documentation](https://grafana.com/docs/grafana/latest/)

### Sample Dashboard

A sample Grafana dashboard for token metrics is available:

- [MaaS Token Metrics Dashboard](https://github.com/opendatahub-io/models-as-a-service/blob/main/docs/samples/dashboards/maas-token-metrics-dashboard.json)

To import into Grafana:

1. Go to Grafana → Dashboards → Import
2. Upload the JSON file or paste content
3. Select your Prometheus datasource

## Key Metrics Reference

### Token Consumption Metrics

| Metric | Description | Labels |
|--------|-------------|--------|
| `authorized_hits` | Total tokens consumed (from `usage.total_tokens`) | `user`, `tier`, `model` |
| `authorized_calls` | Total requests allowed | `user`, `tier`, `model` |
| `limited_calls` | Total requests rate-limited | `user`, `tier`, `model` |

### Latency Metrics

| Metric | Description | Labels |
|--------|-------------|--------|
| `istio_request_duration_milliseconds_bucket` | Gateway-level latency histogram | `destination_service_name` |
| `vllm:e2e_request_latency_seconds` | Model inference latency | `model_name` |

### Common Queries

    # Token consumption per user
    sum by (user) (authorized_hits)

    # Request rate per tier
    sum by (tier) (rate(authorized_calls[5m]))

    # Success rate by tier
    sum by (tier) (authorized_calls) / (sum by (tier) (authorized_calls) + sum by (tier) (limited_calls))

    # P99 latency by service
    histogram_quantile(0.99, sum by (destination_service_name, le) (rate(istio_request_duration_milliseconds_bucket[5m])))

    # Top 10 users by tokens consumed
    topk(10, sum by (user) (authorized_hits))

    # Rate limit violations by tier
    sum by (tier) (rate(limited_calls[5m]))

## Known Limitations

### Currently Blocked Features

Some dashboard features require upstream changes and are currently blocked:

| Feature | Blocker | Workaround |
|---------|---------|------------|
| **Latency per user** | Istio metrics don't include `user` label | Requires EnvoyFilter to inject user context |
| **Input/Output token breakdown per user** | vLLM doesn't label metrics with `user` | Total tokens available via `authorized_hits`; breakdown requires vLLM changes |

!!! note "Total Tokens vs Token Breakdown"
    Total token consumption per user **is available** via `authorized_hits{user="..."}`. The blocked feature is specifically the input/output token breakdown (prompt vs generation tokens) per user, which requires vLLM to accept user context in requests.
