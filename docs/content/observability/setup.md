# Setup

## Prerequisites

### ODH Monitoring Stack

For metrics to be collected and stored in the ODH monitoring stack, configure DSCI `monitoring.metrics` - see [Platform Setup](../install/platform-setup.md#install-platform-operator).

Verify the status of both `MonitoringStackAvailable` and `MonitoringReady` conditions is `True`:

```bash
kubectl get dscinitialization default-dsci -o json | jq '.status.conditions[] | select(.type=="MonitoringReady" or .type=="MonitoringStackAvailable") | {type, status}'
```

### ODH Observability Dashboard tabs (Optional)

For usage dashboard(s) to appear in the ODH observability dashboard, Perses needs to be deployed.

Verify the static of `PersesAvailable` condition is `True`:

```bash
kubectl get dscinitialization default-dsci -o json | jq '.status.conditions[] | select(.type=="PersesAvailable") | {type, status}'
```

See [Managing observability (RHOAI 3.4)](https://docs.redhat.com/en/documentation/red_hat_openshift_ai_self-managed/3.4/html/managing_openshift_ai/managing-observability_managing-rhoai).

## Installation

### Option 1: Operator-Managed (Recommended)

Enable via MaasTenantConfig CR:

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaasTenantConfig
metadata:
  name: default-tenant
  namespace: models-as-a-service
spec:
  telemetry:
    enabled: true
    metrics:
      captureOrganization: true
      captureUser: false      # GDPR
      captureGroup: false     # High cardinality
      captureModelUsage: true
```

Or patch:

```bash
kubectl patch maastenantconfig default-tenant -n models-as-a-service --type=merge \
  -p '{"spec":{"telemetry":{"enabled":true}}}'
```

This creates:

- **TelemetryPolicy** (`maas-telemetry`) - Adds `subscription`, `model`, `organization_id` labels to Limitador metrics (user and group labels disabled by default)
- **Istio Telemetry** (`latency-per-subscription`) - Adds `subscription` label to gateway latency

**Verify:**

```bash
kubectl get telemetry -n openshift-ingress latency-per-subscription
```

!!! note "Prerequisites"
    Requires OpenShift Service Mesh 2.4+, Kuadrant/RHCL, and deployed Gateway.

!!! warning "AuthPolicy Dependency"
    Istio Telemetry reads `X-MaaS-Subscription` header injected by AuthPolicy. Without header injection, `subscription` label will be empty.

Additionally, a Perses dashboard for metrics-based usage will be added to the ODH observability dashboard, and it would show data when `captureUser` and `captureModelUsage` are turned on.

#### Logs based usage dashboards

We have introduced usage dashboards that are based on structured access logs rather than on metrics for both administrators and non-admin users as a tech-preview feature. The data presented in these dashboards is more accurate and consistent, and a bit more enriched, compared to the data in the metrics based dashboard.

In order to enable this feature, you need to turn on `usageLogging` in the `Config`:

```bash
kubectl patch configs.maas.opendatahub.io default --type=merge -p '{"spec":{"usageLogging":true}}'
```

This creates:

- **EnvoyFilter** (`maas-model-access-logs`) - emits OTLP structured access logs from the gateway
- **OpenTelemetry Collector** (`usage-logs-collector`) - collects the logs and exports them to Loki
- **Tenancy Proxy** (`usage-logs-tenancy-proxy`) - filters user-scoped usage data for non-admin users
- **Perses Dashboard** (`dashboard-4-maas-usage-logs-admin`) - usage dashboard for administrators (shows information on all users)
- **Perses Dashboard** (`dashboard-5-maas-usage-logs`) - user-scoped usage dashboard

!!! note "Prerequisites"
    Requires Loki (Operator) and to configure LokiStack named `usage`, for example:

```yaml
apiVersion: loki.grafana.com/v1
kind: LokiStack
metadata:
  name: usage
  namespace: redhat-ods-monitoring
spec:
  limits:
    global:
      otlp:
        streamLabels:
          resourceAttributes:
            - name: service.name
            - name: subscription
            - name: model
            - name: response_type
            - name: kubernetes_namespace_name
  managementState: Managed
  size: 1x.demo
  storage:
    schemas:
      - effectiveDate: '2024-10-01'
        version: v13
    secret:
      credentialMode: static
      name: minio-secret
      type: s3
  storageClassName: gp3-csi
  tenants:
    mode: openshift-logging
```


### Option 2: Kustomize (Development)

!!! warning "Development Only"
    Production deployments should use operator-managed telemetry (Option 1).

```bash
# Deploy base telemetry + conditional ServiceMonitors
./scripts/observability/install-observability.sh [--namespace NAMESPACE]

# Deploy Grafana dashboards
./scripts/observability/install-grafana-dashboards.sh
```

**Manual deployment:**

```bash
# Base telemetry (requires Gateway + AuthPolicy)
kustomize build deployment/base/observability | kubectl apply -f -

# Conditional ServiceMonitors (auto-detects Kuadrant monitors)
./scripts/observability/install-observability.sh

# Grafana dashboards (discovers Grafana instance)
./scripts/observability/install-grafana-dashboards.sh
```

**Kustomize entrypoints:**

| Path | Contents |
|------|----------|
| `deployment/base/observability/` | TelemetryPolicy, Istio Telemetry, metadata-evaluator PrometheusRule |
| `deployment/components/observability/grafana/` | GrafanaDashboard CRs |
| `deployment/components/observability/prometheus/` | Standalone Prometheus (dev/test) |

**Operator vs Kustomize drift:**

| Resource | Kustomize | Operator |
|----------|-----------|----------|
| TelemetryPolicy | `base/observability/` | Yes (Tenant reconciler) |
| Istio Telemetry | `base/observability/` | Yes (Tenant reconciler) |
| Limitador ServiceMonitor | Conditional | Kuadrant PodMonitor when `observability.enable: true` |
| Authorino /server-metrics | `authorino-server-metrics-servicemonitor.yaml` | No (Kuadrant only scrapes `/metrics`) |
| Grafana Dashboards | `components/observability/grafana/` | No (same CRs used) |
