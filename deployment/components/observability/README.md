# Observability Components (Kustomize — Development / Testing Only)

> **Not the standard customer install.**
> The production path is operator-managed via ODH / RHOAI.
> These Kustomize entrypoints exist so the team can install, iterate, and test
> observability without a full operator-driven deployment.

## Kustomize Entrypoint Map

| Entrypoint (kustomize build target) | What it deploys | Operator-owned equivalent |
|--------------------------------------|----------------|---------------------------|
| [`deployment/base/observability/`](../../base/observability/) | TelemetryPolicy + Istio Telemetry (conditional ServiceMonitors applied only via script — see [base README](../../base/observability/README.md)) | Operator installs TelemetryPolicy as part of the MaaS stack; Kuadrant operator owns ServiceMonitors when `spec.observability.enable: true` |
| [`deployment/components/observability/grafana/`](grafana/) | GrafanaDashboard CRs (Platform Admin, AI Engineer) | Operator does not manage Grafana dashboards; same CRs are used in both paths |
| [`deployment/components/observability/prometheus/`](prometheus/) | Standalone Prometheus + RBAC + ServiceMonitors in `llm-observability` namespace | OpenShift User Workload Monitoring (built-in Prometheus) — operator path relies on this instead |
| [`deployment/components/observability/observability/`](observability/) | Aggregator that pulls in `prometheus/` above | Same as above — this is a convenience wrapper |
| [`deployment/components/observability/observability/dashboards/`](observability/dashboards/) | Perses PersesDashboard + Prometheus datasource | No operator equivalent — Perses is optional in both paths |

## Quick Start (Verify and Apply)

### Dry-run all entrypoints

Validate that every kustomization builds cleanly without applying to a cluster:

```bash
# Base telemetry (TelemetryPolicy + Istio Telemetry)
kustomize build deployment/base/observability \
  | kubectl apply --dry-run=client -f -

# Grafana dashboards
kustomize build deployment/components/observability/grafana \
  | kubectl apply --dry-run=client -f -

# Standalone Prometheus stack
kustomize build deployment/components/observability/prometheus \
  | kubectl apply --dry-run=client -f -

# Prometheus aggregator (same content as prometheus/ above)
kustomize build deployment/components/observability/observability \
  | kubectl apply --dry-run=client -f -

# Perses dashboards
kustomize build deployment/components/observability/observability/dashboards \
  | kubectl apply --dry-run=client -f -
```

CI runs `scripts/ci/validate-manifests.sh` on every PR that touches `deployment/**`,
which runs `kustomize build` against **all** `kustomization.yaml` files in the repo
(Components are skipped since they cannot build standalone).

### Apply to a dev cluster

```bash
# 1. Base telemetry (requires Gateway + AuthPolicy to exist first)
kustomize build deployment/base/observability | kubectl apply -f -

# 2. Conditional ServiceMonitors (Limitador, Authorino, Gateway, LLM models)
#    Use the install script — it detects existing Kuadrant monitors to avoid duplicates:
./scripts/observability/install-observability.sh

# 3. Grafana dashboards (discovers Grafana instance cluster-wide)
./scripts/observability/install-grafana-dashboards.sh

# 4. (Optional) Standalone Prometheus — only if NOT using OpenShift User Workload Monitoring
kustomize build deployment/components/observability/observability | kubectl apply -f -

# 5. (Optional) Perses dashboards
kustomize build deployment/components/observability/observability/dashboards | kubectl apply -f -
```

## Directory Layout

```
deployment/
├── base/observability/                          # Always-on telemetry CRs + conditional monitors
│   ├── kustomization.yaml                       # → gateway-telemetry-policy, istio-gateway-telemetry
│   ├── gateway-telemetry-policy.yaml            # Kuadrant TelemetryPolicy (user/subscription/model labels)
│   ├── istio-gateway-telemetry.yaml             # Istio Telemetry (subscription label on latency)
│   ├── limitador-servicemonitor.yaml            # Conditional — script-applied to avoid Kuadrant duplicates
│   ├── authorino-server-metrics-servicemonitor.yaml  # Conditional — script-applied
│   ├── istio-gateway-service.yaml               # Conditional — script-applied when gateway exists
│   └── istio-gateway-servicemonitor.yaml        # Conditional — script-applied when gateway exists
│
└── components/observability/                    # Optional / alternate stacks
    ├── grafana/                                 # GrafanaDashboard CRs
    │   ├── kustomization.yaml                   # namespace: maas-api (adjust for your Grafana)
    │   ├── dashboard-platform-admin.yaml
    │   └── dashboard-ai-engineer.yaml
    ├── prometheus/                              # Standalone Prometheus (not OpenShift UWM)
    │   ├── kustomization.yaml                   # namespace: llm-observability
    │   ├── prometheus-config.yaml
    │   ├── prometheus-deployment.yaml
    │   ├── prometheus-rbac.yaml
    │   ├── models-aas-servicemonitor.yaml
    │   └── kuadrant-servicemonitors.yaml
    └── observability/
        ├── kustomization.yaml                   # Aggregator — includes ../prometheus
        └── dashboards/
            ├── kustomization.yaml               # Perses dashboards
            ├── usage-dashboard.yaml
            └── prometheus-data-source.yaml
```

## Operator vs Kustomize Drift Reference

When updating manifests in this directory, check whether the operator path
produces equivalent resources. Drift between the two is expected in some areas
(e.g., the standalone Prometheus stack has no operator equivalent) but should be
tracked for the telemetry CRs and ServiceMonitors.

| Resource | Kustomize source | Operator creates? | Notes |
|----------|-----------------|-------------------|-------|
| TelemetryPolicy | `base/observability/gateway-telemetry-policy.yaml` | Yes | Keep in sync — labels must match |
| Istio Telemetry | `base/observability/istio-gateway-telemetry.yaml` | Yes | Keep in sync — header extraction must match |
| Limitador ServiceMonitor | `base/observability/limitador-servicemonitor.yaml` | Kuadrant PodMonitor when `observability.enable: true` | Script skips ours if Kuadrant's exists |
| Authorino /server-metrics | `base/observability/authorino-server-metrics-servicemonitor.yaml` | Not yet (Kuadrant only scrapes `/metrics`) | MaaS supplements the operator gap |
| Istio Gateway ServiceMonitor | `base/observability/istio-gateway-servicemonitor.yaml` | No | MaaS-only; applied conditionally |
| Grafana Dashboards | `components/observability/grafana/` | No | Same CRs used in both paths |
| Standalone Prometheus | `components/observability/prometheus/` | No (uses OpenShift UWM instead) | Dev/test only — not for production |
| Perses Dashboards | `components/observability/observability/dashboards/` | No | Optional in both paths |

## Keeping Docs Accurate

When you change any YAML under `deployment/base/observability/` or
`deployment/components/observability/`:

1. Verify the build still passes: `kustomize build <dir> | kubectl apply --dry-run=client -f -`
2. Update **this README** if you add, remove, or rename an entrypoint.
3. Update [`docs/content/advanced-administration/observability.md`](../../../docs/content/advanced-administration/observability.md) if the change affects user-facing instructions (metrics, ServiceMonitor behavior, dashboard content).

## Further Reading

- [Observability (admin docs)](../../../docs/content/advanced-administration/observability.md) — full metrics reference, PromQL queries, dashboard details
- [`scripts/observability/install-observability.sh`](../../../scripts/observability/install-observability.sh) — conditional ServiceMonitor deployment logic
- [`scripts/observability/install-grafana-dashboards.sh`](../../../scripts/observability/install-grafana-dashboards.sh) — Grafana discovery and dashboard deployment
- [`scripts/ci/validate-manifests.sh`](../../../scripts/ci/validate-manifests.sh) — CI manifest validation
