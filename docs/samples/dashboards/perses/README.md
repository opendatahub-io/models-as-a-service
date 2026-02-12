# Perses Dashboard Samples

Dashboard definitions live in a single location (source of truth):

```
deployment/components/observability/perses/dashboards/
├── dashboard-ai-engineer.yaml
└── dashboard-platform-admin.yaml
```

## Dashboards

| Dashboard | Description | File |
|-----------|-------------|------|
| AI Engineer Dashboard | User-focused dashboard showing personal API usage, rate limits, and trends | `deployment/components/observability/perses/dashboards/dashboard-ai-engineer.yaml` |
| Platform Admin Dashboard | Admin-focused dashboard with system health, all users, and model metrics | `deployment/components/observability/perses/dashboards/dashboard-platform-admin.yaml` |

## Format

These dashboards are in Perses native YAML format, designed to work with the Perses Operator's `PersesDashboard` CRD.

### Key Differences from Grafana

| Aspect | Grafana | Perses |
|--------|---------|--------|
| Format | JSON | YAML |
| Panel types | `stat`, `timeseries`, `table`, `gauge` | `StatChart`, `TimeSeriesChart`, `TimeSeriesTable`, `GaugeChart` |
| Variables | `templating.list[]` | `variables[]` |
| Layout | `gridPos` coordinates | Grid layouts with `$ref` to panels |
| Deployment | `GrafanaDashboard` CRD | `PersesDashboard` CRD |
| Console Access | External route | Integrated in OpenShift Console |

## Deployment

### Using the Install Script (Recommended)

```bash
./scripts/install-perses-dashboards.sh
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

## Customization

To customize dashboards:

1. Copy the dashboard file from `deployment/components/observability/perses/dashboards/`
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
        kind: StatChart  # or TimeSeriesChart, TimeSeriesTable, GaugeChart
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
