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

- **Limitador**: Rate limiting service that exposes metrics
- **Prometheus**: Metrics collection and storage (uses OpenShift platform Prometheus)
- **ServiceMonitors**: Deployed to configure Prometheus metric scraping
- **Visualization**: Grafana dashboards (see [Grafana documentation](https://grafana.com/docs/grafana/latest/))

## Metrics Collection

### Limitador Metrics

Limitador exposes several key metrics that are collected through a ServiceMonitor by Prometheus:

#### Rate Limiting Metrics

- `limitador_ratelimit_requests_total`: Total number of rate limit requests
- `limitador_ratelimit_allowed_total`: Number of requests allowed
- `limitador_ratelimit_denied_total`: Number of requests denied
- `limitador_ratelimit_errors_total`: Number of rate limiting errors

#### Performance Metrics

- `limitador_ratelimit_duration_seconds`: Duration of rate limit checks
- `limitador_ratelimit_active_connections`: Number of active connections
- `limitador_ratelimit_cache_hits_total`: Cache hit rate
- `limitador_ratelimit_cache_misses_total`: Cache miss rate

#### Tier-Based Metrics

- `limitador_ratelimit_tier_requests_total`: Requests per tier
- `limitador_ratelimit_tier_allowed_total`: Allowed requests per tier
- `limitador_ratelimit_tier_denied_total`: Denied requests per tier

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

For local development and testing, you can also use our [Limitador Persistence](limitador-persistence.md) guide which includes a basic Redis setup script that works with any Kubernetes cluster.

## Visualization

For dashboard visualization options, see:

- **OpenShift Monitoring**: [Monitoring overview](https://docs.redhat.com/en/documentation/openshift_container_platform/4.19/html/monitoring/index)
- **Grafana on OpenShift**: [Red Hat OpenShift AI Monitoring](https://docs.redhat.com/en/documentation/red_hat_openshift_ai_self-managed/2.25.1/html/monitoring_data_science_models/index)

### Included Dashboards

MaaS includes two Grafana dashboards for different personas:

#### Platform Admin Dashboard

Provides a comprehensive view of system health, usage across all users, and resource allocation:

| Section | Metrics |
|---------|---------|
| **Key Metrics** | Total Tokens, Total Requests, Token Rate, Request Rate, Success Rate, Active Users |
| **Traffic Analysis** | Token/Request Rate by Model, Error Rates, Token/Request Rate by Tier, P95 Latency |
| **Error Breakdown** | Rate Limited Requests, Unauthorized Requests |
| **Model Metrics** | vLLM queue depth, inference latency, GPU cache usage, token throughput |
| **Top Users** | By token usage, by declined requests |

#### AI Engineer Dashboard

Personal usage view for individual developers:

| Section | Metrics |
|---------|---------|
| **Usage Summary** | My Total Tokens, My Total Requests, Token Rate, Request Rate, Rate Limited, Success Rate |
| **Usage Trends** | Token Usage by Model, Usage Trends (tokens vs rate limited) |
| **Detailed Analysis** | Token Volume by Model, Rate Limited by Model |

!!! info "Tokens vs Requests"
    Both dashboards show **token consumption** (`authorized_hits`) for billing/cost tracking and **request counts** (`authorized_calls`) for capacity planning. Blue panels indicate request metrics; green panels indicate token metrics.

### Deploying Dashboards

Dashboards are deployed automatically by `install-observability.sh`, or manually:

    # Deploy Grafana operator and instance
    kubectl apply -k deployment/components/observability/grafana/

    # Deploy dashboards
    kubectl apply -k deployment/components/observability/dashboards/

### Sample Dashboard JSON

For manual import, a sample dashboard JSON file is available:

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
| `istio_request_duration_milliseconds_bucket` | Gateway-level latency histogram | `destination_service_name`, `user` |
| `vllm:e2e_request_latency_seconds` | Model inference latency | `model_name` |

#### Per-User Latency Tracking

The MaaS Platform uses an Istio Telemetry resource to add a `user` dimension to gateway latency metrics. This enables tracking request latency per authenticated user.

**How it works:**

1. The `gateway-auth-policy` injects the `X-MaaS-Username` header from the authenticated identity
2. The Istio Telemetry resource extracts this header and adds it as a `user` label to the `REQUEST_DURATION` metric
3. Prometheus scrapes these metrics from the Istio gateway

**Configuration** (`deployment/base/observability/istio-telemetry.yaml`):

    apiVersion: telemetry.istio.io/v1
    kind: Telemetry
    metadata:
      name: latency-per-user
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
            user:
              operation: UPSERT
              value: request.headers["x-maas-username"]

!!! note "Security"
    The `X-MaaS-Username` header is injected by the AuthPolicy from the authenticated identity, not from client input. This prevents users from spoofing the header to manipulate metrics attribution.

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

    # Request rate per model
    sum by (model) (rate(authorized_calls[5m]))

    # Top 10 users by request count
    topk(10, sum by (user) (authorized_calls))

**Rate limiting and success metrics:**

    # Success rate (percentage of requests not rate-limited)
    sum(authorized_calls) / (sum(authorized_calls) + sum(limited_calls))

    # Success rate by tier
    sum by (tier) (authorized_calls) / (sum by (tier) (authorized_calls) + sum by (tier) (limited_calls))

    # Rate limit violations by tier
    sum by (tier) (rate(limited_calls[5m]))

    # Users hitting rate limits
    topk(10, sum by (user) (limited_calls))

**Latency queries:**

    # P99 latency by service
    histogram_quantile(0.99, sum by (destination_service_name, le) (rate(istio_request_duration_milliseconds_bucket[5m])))

    # P50 (median) latency
    histogram_quantile(0.5, sum by (le) (rate(istio_request_duration_milliseconds_bucket[5m])))

    # P99 latency per user (filter out unauthenticated requests)
    histogram_quantile(0.99, sum by (user, le) (rate(istio_request_duration_milliseconds_bucket{user!="",user!="unknown"}[5m])))

!!! tip "Filtering Unauthenticated Requests"
    For per-user latency queries, use `user!="",user!="unknown"` to exclude requests that failed authentication (where the `X-MaaS-Username` header was not injected or has a default value). Token consumption metrics (`authorized_hits`, `authorized_calls`) from Limitador already only include successful requests.

## Maintenance

### Grafana Datasource Token Rotation

The Grafana datasource uses a ServiceAccount token to authenticate with Prometheus. This token is valid for **30 days** and must be rotated periodically.

**To rotate the token:**

    # Delete the existing datasource
    kubectl delete grafanadatasource prometheus -n maas-api

    # Re-run the observability installer (creates new token)
    ./scripts/install-observability.sh --stack grafana

!!! tip "Production Recommendation"
    For production deployments, consider automating token rotation using a CronJob or external secrets operator to avoid dashboard outages.

## Known Limitations

### Currently Blocked Features

Some dashboard features require upstream changes and are currently blocked:

| Feature | Blocker | Workaround |
|---------|---------|------------|
| **Input/Output token breakdown per user** | vLLM doesn't label metrics with `user` | Total tokens available via `authorized_hits`; breakdown requires vLLM changes |

!!! note "Total Tokens vs Token Breakdown"
    Total token consumption per user **is available** via `authorized_hits{user="..."}`. The blocked feature is specifically the input/output token breakdown (prompt vs generation tokens) per user, which requires vLLM to accept user context in requests.

### Available Per-User Metrics

| Feature | Metric | Label |
|---------|--------|-------|
| **Latency per user** | `istio_request_duration_milliseconds_bucket` | `user` |
| **Token consumption per user** | `authorized_hits` | `user` |
