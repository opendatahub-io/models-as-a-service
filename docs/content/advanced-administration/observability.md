# Observability

This document covers the observability stack for the MaaS Platform, including metrics collection, monitoring, and visualization.

!!! warning "Important"
     [User Workload Monitoring](https://docs.redhat.com/en/documentation/openshift_container_platform/4.19/html/monitoring/configuring-user-workload-monitoring) must be enabled in order to collect metrics.

     Add `enableUserWorkload: true` to the `cluster-monitoring-config` in the `openshift-monitoring` namespace

## Overview

As part of Dev Preview MaaS Platform includes a basic observability stack that provides insights into system performance, usage patterns, and operational health. The observability stack consists of:

!!! note
    The observability stack will be enhanced in the future.

- **Limitador**: Rate limiting service that exposes metrics
- **Prometheus**: Metrics collection and storage (uses OpenShift platform Prometheus on OpenShift clusters)
- **ServiceMonitors**: Automatically deployed to configure Prometheus metric scraping
- **Grafana**: Metrics visualization and dashboards
- **Future**: Migration to Perses for enhanced dashboard management

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

ServiceMonitors are automatically deployed during the main deployment (step 14) to configure OpenShift's Prometheus to discover and scrape metrics from MaaS components.

**Automatically Deployed ServiceMonitors:**

- **Limitador**: Scrapes rate limiting metrics from Limitador pods
- **Authorino**: Scrapes authentication metrics from Authorino pods
- **vLLM Models**: Scrapes metrics from vLLM simulator and model services
- **MaaS API**: Scrapes metrics from MaaS API services

These ServiceMonitors are deployed in the `maas-api` namespace and use `namespaceSelector` to discover services in other namespaces (e.g., `kuadrant-system`, `llm`).

**Manual ServiceMonitor Creation (Advanced):**

If you need to create additional ServiceMonitors for custom services, use the following template:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: your-service-monitor
  namespace: maas-api
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
  namespaceSelector:
    matchNames:
    - your-namespace
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

For local development and testing, you can also use our [Limitador Persistence](limitador-persistence.md) guide which includes a basic Redis setup script that works with any Kubernetes cluster.

## Installing the Observability Stack

The observability stack can be installed during the main deployment or separately.

### During Main Deployment

When running `deploy-openshift.sh`, you'll be prompted to install the observability stack:

```bash
./scripts/deploy-openshift.sh
# When prompted, answer 'y' to install observability
```

Or use flags to control installation:

```bash
# Install with observability
./scripts/deploy-openshift.sh --with-observability

# Skip observability installation
./scripts/deploy-openshift.sh --skip-observability
```

### Standalone Installation

To install the observability stack separately:

```bash
# Install to default namespace (maas-api)
./scripts/install-observability.sh

# Install to custom namespace
./scripts/install-observability.sh --namespace my-namespace
```

### What Gets Installed

The observability stack includes:

1. **Grafana Operator**: Installed automatically if not present
2. **Grafana Instance**: Deployed to the target namespace
3. **Prometheus Datasource**: Configured with authentication token (connects to OpenShift platform Prometheus)
4. **ServiceMonitors**: Automatically deployed during main deployment to configure metric scraping
5. **Dashboards**:
   - Platform Admin Dashboard
   - AI Engineer Dashboard

!!! note "ServiceMonitors Deployment"
    ServiceMonitors are deployed automatically in step 14 of `deploy-openshift.sh`, even if the observability stack (Grafana) is not installed. This ensures that OpenShift's Prometheus can collect metrics from MaaS components regardless of whether Grafana is installed.

### Accessing Grafana

After installation, get the Grafana URL:

```bash
# Get the route
kubectl get route grafana-ingress -n maas-api -o jsonpath='{.spec.host}'

# Access at: https://<route-host>
```

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

## Grafana Dashboards

### Available Dashboards

The observability stack includes two pre-configured dashboards:

1. **Platform Admin Dashboard**: Overview of system-wide metrics, usage patterns, and health
2. **AI Engineer Dashboard**: Model-specific metrics, token usage, and performance

### Manual Dashboard Import

You can also manually import dashboard JSON files:

1. **Import into Grafana:**
   - Go to Grafana → Dashboards → Import
   - Upload the JSON file or paste the URL

2. **Available Dashboards:**
   - [Platform Admin Dashboard](https://github.com/opendatahub-io/models-as-a-service/blob/main/docs/samples/dashboards/platform-admin-dashboard.json)
   - [AI Engineer Dashboard](https://github.com/opendatahub-io/models-as-a-service/blob/main/docs/samples/dashboards/ai-engineer-dashboard.json)
   - [MaaS Token Metrics Dashboard](https://github.com/opendatahub-io/models-as-a-service/blob/main/docs/samples/dashboards/maas-token-metrics-dashboard.json)

See more detailed description of the Grafana Dashboards in [its README of the repository](https://github.com/opendatahub-io/models-as-a-service/tree/main/docs/samples/dashboards).
