# Observability

This document covers the observability stack for the MaaS Platform, including metrics collection, monitoring, and visualization.

!!! warning "Important"
    [User Workload Monitoring](https://docs.redhat.com/en/documentation/monitoring_stack_for_red_hat_openshift/4.20/html-single/configuring_user_workload_monitoring/index#enabling-monitoring-for-user-defined-projects_preparing-to-configure-the-monitoring-stack-uwm) must be enabled in order to collect metrics.

    Add `enableUserWorkload: true` to the `cluster-monitoring-config` in the `openshift-monitoring` namespace

## Overview

As part of Dev Preview MaaS Platform includes a basic observability stack that provides insights into system performance, usage patterns, and operational health. The observability stack consists of:

!!! note
   The observability stack will be enhanced in the future.

- **Limitador**: Rate limiting service that exposes metrics
- **Prometheus**: Metrics collection and storage (uses OpenShift platform Prometheus on OpenShift clusters)
- **ServiceMonitors**: Automatically deployed to configure Prometheus metric scraping
- **Visualization Options**:
    - **Grafana**: Established, feature-rich dashboard visualization
    - **Perses**: CNCF native, lightweight dashboard visualization (integrates with OpenShift Console)

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

## Installing the Observability Stack

The observability stack can be installed during the main deployment or separately. You can choose between Grafana, Perses, or both visualization platforms.

### During Main Deployment

When running `deploy-openshift.sh`, you'll be prompted to install the observability stack:

    ./scripts/deploy-openshift.sh
    # When prompted, answer 'y' to install observability
    # Then select: 1) grafana, 2) perses, or 3) both

Or use flags to control installation:

    # Install with Grafana (prompts for stack choice)
    ./scripts/deploy-openshift.sh --with-observability

    # Install with specific stack (no prompts)
    ./scripts/deploy-openshift.sh --with-observability --observability-stack grafana
    ./scripts/deploy-openshift.sh --with-observability --observability-stack perses
    ./scripts/deploy-openshift.sh --with-observability --observability-stack both

    # Skip observability installation
    ./scripts/deploy-openshift.sh --skip-observability

### Standalone Installation

To install the observability stack separately:

    # Interactive mode (prompts for stack selection)
    ./scripts/install-observability.sh

    # Install specific stack
    ./scripts/install-observability.sh --stack grafana
    ./scripts/install-observability.sh --stack perses
    ./scripts/install-observability.sh --stack both

    # Install to custom namespace
    ./scripts/install-observability.sh --namespace my-namespace --stack grafana

### What Gets Installed

The observability stack includes:

1. **ServiceMonitors**: Automatically deployed during main deployment to configure metric scraping
2. **Dashboards** (deployed to both platforms if "both" selected):
   - Platform Admin Dashboard
   - AI Engineer Dashboard

**Grafana Stack:**

- Grafana Operator (installed automatically if not present)
- Grafana Instance (deployed to target namespace)
- Prometheus Datasource (configured with authentication token)
- GrafanaDashboard CRDs

**Perses Stack:**

- Cluster Observability Operator (installed automatically if not present)
- UIPlugin for OpenShift Console integration
- PersesDashboard CRDs (deployed to `openshift-operators`)
- Prometheus Datasource

!!! note "ServiceMonitors Deployment"
    ServiceMonitors are deployed automatically in step 14 of `deploy-openshift.sh`, even if the observability stack (Grafana/Perses) is not installed. This ensures that OpenShift's Prometheus can collect metrics from MaaS components regardless of whether visualization tools are installed.

## Grafana

### Accessing Grafana

After installation, get the Grafana URL:

    # Get the route
    kubectl get route grafana-ingress -n maas-api -o jsonpath='{.spec.host}'

    # Access at: https://<route-host>

**Default Credentials:**

- Username: `admin`
- Password: `admin`

!!! warning "Security"
    Change the default Grafana credentials immediately after first login. The credentials are stored in the Grafana instance manifest and should be updated for production deployments.

### Grafana Datasource Configuration

The Prometheus datasource is automatically configured to connect to OpenShift's platform Prometheus:

- **URL**: `https://thanos-querier.openshift-monitoring.svc.cluster.local:9091`
- **Authentication**: Bearer token from OpenShift service account
- **TLS**: Skip verification (internal cluster communication)

!!! note "OpenShift Platform Prometheus"
    On OpenShift clusters, the platform provides Prometheus in the `openshift-monitoring` namespace. The MaaS Platform does not deploy a custom Prometheus instance. Instead, ServiceMonitors are used to configure the platform Prometheus to scrape metrics from MaaS components.

The datasource is created dynamically by `install-observability.sh` with proper token injection. A static datasource manifest is not used to ensure authentication tokens are properly configured.

## Perses

Perses is a CNCF native dashboarding solution that integrates directly with the OpenShift Console.

### Accessing Perses Dashboards

After installation, access Perses dashboards through the OpenShift Console:

1. Navigate to the OpenShift Console
2. Go to **Observe → Dashboards**
3. Select the **Perses** tab (if available) or **Dashboards (Perses)**
4. Select project `openshift-operators` to view MaaS dashboards

!!! info "Console Integration"
    Perses dashboards are integrated via the UIPlugin CRD, which adds a new dashboard view to the OpenShift Console's Observe section.

### Perses Components

The Perses installation includes:

- **Cluster Observability Operator**: Provides Perses CRDs and operator
- **UIPlugin**: Enables Perses dashboards in OpenShift Console
- **PersesDashboard CRDs**: Dashboard definitions in YAML format
- **PersesDatasource**: Prometheus datasource configuration

### Perses vs Grafana

| Aspect | Grafana | Perses |
|--------|---------|--------|
| **Format** | JSON | YAML |
| **Console Integration** | External route | Built into OpenShift Console |
| **Feature Set** | Full-featured, extensive plugins | Lightweight, focused |
| **CRD** | `GrafanaDashboard` | `PersesDashboard` |
| **Authentication** | Standalone auth | Uses OpenShift RBAC |

## Dashboards

### Available Dashboards

Both Grafana and Perses include equivalent dashboards:

1. **Platform Admin Dashboard**: Overview of system-wide metrics, usage patterns, and health
2. **AI Engineer Dashboard**: User-focused metrics showing personal API usage and rate limits

!!! warning "Security Consideration"
    The user dropdown in the AI Engineer Dashboard populates from Prometheus label values, which exposes all user IDs to anyone with dashboard access. For multi-tenant environments where user enumeration is a concern, consider restricting dashboard access via Grafana/Perses RBAC or provisioning per-user dashboards with static user filters.

### Dashboard Panels

#### Platform Admin Dashboard

| Section | Description |
|---------|-------------|
| **Component Health** | Limitador, Authorino, MaaS API, and Gateway pod status |
| **Alerts** | Firing alerts count and active alerts table |
| **Key Metrics** | Total tokens, current rate, success rate, active users |
| **Traffic Analysis** | Request rate by model, error rates, P95/P99 latency |
| **Top Users** | Top 10 by request hits, Top 10 by token consumption |
| **Token Consumption** | Token usage by tier and by user |
| **Model Metrics** | vLLM metrics (queue depth, GPU cache, inference latency) |
| **User Tracking** | Per-user request and error rates |

#### AI Engineer Dashboard

| Section | Description |
|---------|-------------|
| **My Usage Summary** | Total tokens consumed, current rate, rate-limited requests, success rate |
| **Usage Trends** | Usage by model, request trends (success vs rate-limited) |
| **Hourly Patterns** | Hourly usage breakdown by model |
| **Detailed Analysis** | Request volume and rate-limited requests by model |

### Manual Dashboard Import

#### Grafana

1. Go to Grafana → Dashboards → Import
2. Upload the JSON file or paste content
3. Available dashboards:
   - [Platform Admin Dashboard](https://github.com/opendatahub-io/models-as-a-service/blob/main/docs/samples/dashboards/platform-admin-dashboard.json)
   - [AI Engineer Dashboard](https://github.com/opendatahub-io/models-as-a-service/blob/main/docs/samples/dashboards/ai-engineer-dashboard.json)

#### Perses

Apply dashboard YAML directly:

    kubectl apply -f deployment/components/observability/perses/dashboards/dashboard-ai-engineer.yaml -n openshift-operators
    kubectl apply -f deployment/components/observability/perses/dashboards/dashboard-platform-admin.yaml -n openshift-operators

See more detailed description of the dashboards in the [dashboards README](https://github.com/opendatahub-io/models-as-a-service/tree/main/docs/samples/dashboards).

## Key Metrics Reference

### Token and Request Metrics

| Metric | Type | Description | Use Case |
|--------|------|-------------|----------|
| `authorized_hits` | Counter | Total tokens consumed (from `usage.total_tokens`) | Billing, token quota tracking |
| `authorized_calls` | Counter | Total requests allowed | Request counting, capacity planning |
| `limited_calls` | Counter | Total requests rate-limited | Rate limit monitoring |

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

```yaml
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
```

!!! note "Security"
    The `X-MaaS-Username` header is injected by the AuthPolicy from the authenticated identity, not from client input. This prevents users from spoofing the header to manipulate metrics attribution.

### Common Queries

**Token-based queries:**

    # Token consumption per user
    sum by (user) (authorized_hits)

    # P99 latency per user (filter out unauthenticated requests)
    histogram_quantile(0.99, sum by (user, le) (rate(istio_request_duration_milliseconds_bucket{user!="",user!="unknown"}[5m])))

    # Top 10 users by tokens consumed
    topk(10, sum by (user) (authorized_hits))

    # Token rate per tier
    sum by (tier) (rate(authorized_hits[5m]))

**Request-based queries:**

    # Request rate per tier
    sum by (tier) (rate(authorized_calls[5m]))

    # Total requests per user
    sum by (user) (authorized_calls)

**Rate limiting queries:**

    # Rate limit violations by tier
    sum by (tier) (rate(limited_calls[5m]))

    # Rate limited requests per user
    sum by (user) (limited_calls)

**Success and latency metrics:**

    # Success rate by tier (requests)
    sum by (tier) (authorized_calls) / (sum by (tier) (authorized_calls) + sum by (tier) (limited_calls))

    # P99 latency by service
    histogram_quantile(0.99, sum by (destination_service_name, le) (rate(istio_request_duration_milliseconds_bucket[5m])))

!!! tip "Filtering Unauthenticated Requests"
    For per-user latency queries, use `user!="",user!="unknown"` to exclude requests that failed authentication (where the `X-MaaS-Username` header was not injected or has a default value). Token consumption metrics (`authorized_hits`, `authorized_calls`) from Limitador already only include successful requests.

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
