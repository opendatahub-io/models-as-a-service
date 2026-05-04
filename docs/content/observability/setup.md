# Setup

## Prerequisites

### User Workload Monitoring

[User Workload Monitoring](https://docs.redhat.com/en/documentation/monitoring_stack_for_red_hat_openshift/4.19/html-single/configuring_user_workload_monitoring/index#enabling-monitoring-for-user-defined-projects_preparing-to-configure-the-monitoring-stack-uwm) must be enabled for Prometheus to scrape metrics.

!!! warning "Required"
    Without User Workload Monitoring, ServiceMonitors will not be processed and no metrics will be collected.

```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: cluster-monitoring-config
  namespace: openshift-monitoring
data:
  config.yaml: |
    enableUserWorkload: true
EOF

# Verify prometheus-user-workload pods are running
kubectl get pods -n openshift-user-workload-monitoring
```

### Kuadrant Observability

Enable observability on the Kuadrant CR for rate-limiting metrics (`authorized_hits`, `authorized_calls`, `limited_calls`):

```bash
kubectl patch kuadrant kuadrant -n kuadrant-system --type=merge \
  -p '{"spec":{"observability":{"enable":true}}}'

# Verify PodMonitor was created
kubectl get podmonitor -n kuadrant-system kuadrant-limitador-monitor
```

### RHOAI Dashboard Observability Tab (Optional)

For Perses-based dashboards in the RHOAI Dashboard:

- Install **Cluster Observability Operator** and **OpenTelemetry Operator** from OperatorHub
- Configure DSCI `monitoring.metrics` - see [Platform Setup](../install/platform-setup.md#install-platform-operator)
- Enable `observabilityDashboard: true` on OdhDashboardConfig - see [Feature Flags](../install/maas-setup.md#odhdashboardconfig-feature-flags)

```bash
# Verify
kubectl get csv -A | grep -E 'cluster-observability|opentelemetry'
kubectl get pods -n redhat-ods-monitoring | grep perses
```

See [Managing observability (RHOAI 3.4)](https://docs.redhat.com/en/documentation/red_hat_openshift_ai_self-managed/3.4/html/managing_openshift_ai/managing-observability_managing-rhoai).

## Installation

### Option 1: Operator-Managed (Recommended)

Enable via Tenant CR:

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: Tenant
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
kubectl patch tenant default-tenant -n models-as-a-service --type=merge \
  -p '{"spec":{"telemetry":{"enabled":true}}}'
```

This creates:

- **TelemetryPolicy** (`maas-telemetry`) - Adds `user`, `subscription`, `model` labels to Limitador metrics
- **Istio Telemetry** (`latency-per-subscription`) - Adds `subscription` label to gateway latency

**Verify:**

```bash
kubectl get telemetry -n openshift-ingress latency-per-subscription
```

!!! note "Prerequisites"
    Requires OpenShift Service Mesh 2.4+, Kuadrant/RHCL, and deployed Gateway.

!!! warning "AuthPolicy Dependency"
    Istio Telemetry reads `X-MaaS-Subscription` header injected by AuthPolicy. Without header injection, `subscription` label will be empty.

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
| `deployment/base/observability/` | TelemetryPolicy + Istio Telemetry |
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
