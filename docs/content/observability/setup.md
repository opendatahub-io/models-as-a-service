# Setup

## Prerequisites

### ODH Monitoring Stack

Verify the ODH monitoring stack is available by checking the status of both `MonitoringStackAvailable` and `MonitoringReady` conditions is `True`:

```bash
kubectl get dscinitialization default-dsci -o json | jq '.status.conditions[] | select(.type=="MonitoringReady" or .type=="MonitoringStackAvailable") | {type, status}'
```

See [Observability Prerequisites](../install/prerequisites.md#observability-prerequisites-recommended)

### Perses

For MaaS dashboards to appear in the ODH console, verify that the status of the `PersesAvailable` condition is `True`:

```bash
kubectl get dscinitialization default-dsci -o json | jq '.status.conditions[] | select(.type=="PersesAvailable") | {type, status}'
```

### Loki for Access Logs

If you plan to store access logs for logs-based showback or for auditing, ensure LokiStack named `usage` is ready in monitoring namespace configured in DSCI `spec.monitoring.namespace` by verifying that the status of its `Ready` condition is `True`:

```bash
kubectl get lokistack usage -n <monitoring-namespace> -o json | jq '.status.conditions[] | select(.type=="Ready") | {type, status}'
```

See [Installing Loki for Access logs](../install/prerequisites.md#loki-for-access-logs)

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

Additionally, a Perses dashboard for metrics-based usage is applied by the operator (`LifecycleReconciler.ensureUsageDashboard`) with a controller ownerReference on `Config`. It is **not** created by `install-observability.sh`. The dashboard shows data when `captureUser` and `captureModelUsage` are turned on.

!!! note "Operator prerequisite"
    Operator-managed `PersesDashboard` CRs require a running maas-controller, a `Config` instance, a monitoring namespace, and Perses CRDs (`PersesAvailable` above). Ownership and cleanup differ from Kustomize-applied dashboards — see [Operations: Cleanup](operations.md#cleanup).

#### Logs-based usage dashboards

We have introduced usage dashboards that are based on structured access logs rather than on metrics for both administrators and non-admin users as a tech-preview feature. The data presented in these dashboards is more accurate and consistent, and a bit more enriched, compared to the data in the metrics-based dashboard.

!!! warning "Privacy"
    `usageLogging` records request identity attributes in access logs.
    Review GDPR/privacy requirements, retention, and dashboard access before enabling it.

To enable this feature, you need to turn on `usageLogging` in the `Config`:

```bash
kubectl patch configs.maas.opendatahub.io default --type=merge -p '{"spec":{"usageLogging":true}}'
```

This creates:

- **EnvoyFilter** (`maas-model-access-logs`) - emits OTLP structured access logs from the gateway
- **OpenTelemetry Collector** (`usage-logs-collector`) - collects the logs and exports them to Loki
- **Tenancy Proxy** (`usage-logs-tenancy-proxy`) - filters user-scoped usage data for non-admin users
- **Perses Dashboard** (`dashboard-4-maas-usage-logs-admin`) - usage dashboard for administrators (shows information on all users)
- **Perses Dashboard** (`dashboard-5-maas-usage-logs`) - user-scoped usage dashboard

### Option 2: Kustomize (Development)

!!! warning "Development Only"
    Production deployments should use operator-managed telemetry (Option 1).

```bash
# Deploy base telemetry + conditional ServiceMonitors (does not apply Perses dashboards)
./scripts/observability/install-observability.sh [--namespace NAMESPACE]
```

**Manual deployment:**

```bash
# Base telemetry (requires Gateway + AuthPolicy)
kustomize build deployment/base/observability | kubectl apply -f -

# Conditional ServiceMonitors (auto-detects Kuadrant monitors)
./scripts/observability/install-observability.sh

# Perses metrics Usage dashboard (optional; labeled app.kubernetes.io/managed-by: maas-observability)
kustomize build deployment/components/observability/observability/dashboards | kubectl apply -f -
```

**Kustomize entrypoints:**

| Path | Contents |
|------|----------|
| `deployment/base/observability/` | TelemetryPolicy, Istio Telemetry, metadata-evaluator PrometheusRule |
| `deployment/components/observability/prometheus/` | Standalone Prometheus (dev/test) |
| `deployment/components/observability/observability/dashboards/` | Perses usage dashboard (metrics) |
| `deployment/components/observability/usage-logs/` | Perses usage-log dashboards |

**Operator vs Kustomize drift:**

| Resource | Kustomize | Operator |
|----------|-----------|----------|
| TelemetryPolicy | `base/observability/` | Yes (Tenant reconciler) |
| Istio Telemetry | `base/observability/` | Yes (Tenant reconciler) |
| Limitador ServiceMonitor | Conditional | Kuadrant PodMonitor when `observability.enable: true` |
| Authorino /server-metrics | `authorino-server-metrics-servicemonitor.yaml` | No (Kuadrant only scrapes `/metrics`) |
| Perses usage dashboards | `deployment/components/observability/observability/dashboards/` (`managed-by: maas-observability`) | Yes (`ensureUsageDashboard`; `Config` controller owner). Not applied by `install-observability.sh`. |
