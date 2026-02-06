# Perses Dashboard Samples

This directory contains Perses dashboard definitions for MaaS observability.

## Dashboards

| Dashboard | Description | File |
|-----------|-------------|------|
| AI Engineer Dashboard | User-focused dashboard showing personal API usage, rate limits, and trends | `ai-engineer-dashboard.yaml` |
| Platform Admin Dashboard | Admin-focused dashboard with system health, all users, and model metrics | `platform-admin-dashboard.yaml` |

## Format

These dashboards are in Perses native YAML format, designed to work with the Perses Operator's `PersesDashboard` CRD.

### Key Differences from Grafana

| Aspect | Grafana | Perses |
|--------|---------|--------|
| Format | JSON | YAML |
| Panel types | `stat`, `timeseries`, `table`, `gauge` | `StatChart`, `TimeSeriesChart`, `Table`, `GaugeChart` |
| Variables | `templating.list[]` | `variables[]` |
| Layout | `gridPos` coordinates | Grid layouts with `$ref` to panels |
| Deployment | `GrafanaDashboard` CRD | `PersesDashboard` CRD |
| Console Access | External route | Integrated in OpenShift Console |

## Deployment

### Using the Install Script (Recommended)

```bash
# Install Perses with dashboards
./scripts/install-observability.sh --stack perses

# Or install both Grafana and Perses
./scripts/install-observability.sh --stack both
```

### Manual Deployment

```bash
# Apply Perses dashboards to openshift-operators namespace
kubectl apply -f deployment/components/observability/perses/dashboards/dashboard-ai-engineer.yaml -n openshift-operators
kubectl apply -f deployment/components/observability/perses/dashboards/dashboard-platform-admin.yaml -n openshift-operators
```

### Accessing Dashboards

After deployment, access Perses dashboards via the OpenShift Console:

1. Navigate to OpenShift Console
2. Go to **Observe → Dashboards**
3. Select the **Perses** tab or **Dashboards (Perses)**
4. Select project **openshift-operators** to view MaaS dashboards

## Dashboard Panels

### AI Engineer Dashboard

| Section | Panels |
|---------|--------|
| My Usage Summary | My Total Tokens, Current Rate, Rate Limited Requests, Success Rate |
| Usage Trends | Usage by Model, Request Trends |
| Hourly Patterns | Hourly Usage by Model |
| Detailed Analysis | Request Volume by Model, Rate Limited by Model |
| Usage Summary | Summary by Model & Tier |

### Platform Admin Dashboard

| Section | Panels |
|---------|--------|
| Component Health | Limitador, Authorino, MaaS API, Gateway pods, Firing Alerts |
| Key Metrics | Total Hits, Current Rate, Success Rate, Active Users, Latency |
| Traffic Analysis | Request Rate by Model/Tier, Error Rate, Latency (P95/P99), Rate Limit Violations |
| Top Users | Top 10 by Hits, Top 10 by Tokens |
| Token Consumption | Consumption by Tier, Consumption by User |
| Model Metrics | vLLM queue depth, GPU cache, inference latency |
| User Tracking | Per-user request and error rates |

## PromQL Queries

Both Grafana and Perses dashboards use the same PromQL queries since they query the same Prometheus backend:

```promql
# Token consumption per user
sum by (user) (authorized_hits)

# Request count per user  
sum by (user) (authorized_calls)

# Rate limit violations
sum by (tier) (rate(limited_calls[5m]))

# Success rate by tier
sum by (tier) (authorized_calls) / (sum by (tier) (authorized_calls) + sum by (tier) (limited_calls))

# Top 10 users by tokens
topk(10, sum by (user) (authorized_hits))

# P99 latency by service
histogram_quantile(0.99, sum by (destination_service_name, le) (rate(istio_request_duration_milliseconds_bucket[5m])))
```

## Metrics Terminology

| Metric | What It Tracks |
|--------|----------------|
| `authorized_hits` | **Token consumption** (extracted from `usage.total_tokens` in LLM responses) |
| `authorized_calls` | **Request counts** (number of successful API calls) |
| `limited_calls` | **Rate-limited requests** (requests denied due to quota) |

!!! note
    With `TokenRateLimitPolicy`, `authorized_hits` tracks tokens, not requests. Use `authorized_calls` for request counts.

## Customization

To customize dashboards:

1. Copy the dashboard file you want to modify
2. Edit the `spec.panels` section
3. Update `spec.layouts` if adding/removing panels
4. Apply with `kubectl apply -f your-modified-dashboard.yaml -n openshift-operators`

### Adding a New Panel

```yaml
panels:
  myNewPanel:
    kind: Panel
    spec:
      display:
        name: "My New Panel"
        description: "Description of what this panel shows"
      plugin:
        kind: StatChart  # or TimeSeriesChart, Table, GaugeChart
        spec:
          calculation: last-number
          format:
            unit: decimal
      queries:
        - kind: TimeSeriesQuery
          spec:
            plugin:
              kind: PrometheusTimeSeriesQuery
              spec:
                query: sum(my_metric)
```

Then reference it in the layout:

```yaml
layouts:
  - kind: Grid
    spec:
      display:
        title: "My Section"
      items:
        - x: 0
          "y": 0
          width: 12
          height: 8
          content:
            "$ref": "#/spec/panels/myNewPanel"
```

## Related Documentation

- [Perses Documentation](https://perses.dev/)
- [Perses Operator](https://github.com/perses/perses-operator)
- [OpenShift Cluster Observability Operator](https://docs.openshift.com/container-platform/latest/observability/cluster_observability_operator/cluster-observability-operator-overview.html)
- [MaaS Observability Guide](../../content/advanced-administration/observability.md)
