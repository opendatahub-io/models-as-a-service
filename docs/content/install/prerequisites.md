# MaaS Installation Overview

_Models-as-a-Service_ is compatible with the Open Data Hub project (ODH) and
Red Hat OpenShift AI (RHOAI). MaaS is installed by enabling it in the DataScienceCluster resource:

* [Install your platform](platform-setup.md) (ODH or RHOAI operators and DSCInitialization).
* [Install MaaS Components](maas-setup.md) (Database, Gateways, DataScienceCluster).

## Version Compatibility

| MaaS Version | OCP | Kuadrant (ODH) / RHCL (RHOAI) | Gateway API |
|--------------|-----|-------------------------------|-------------|
| v0.0.2       | 4.19.9+ | v1.3+ / v1.2+             | v1.2+       |
| v0.1.0+      | 4.19.9+ | v1.4.2+ / v1.3            | v1.2+       |

!!! note "Other Kubernetes flavors"
    Other Kubernetes flavors (e.g., upstream Kubernetes, other distributions) are currently being validated.



## Required Tools

The following tools are used across the installation guides:

* `kubectl` or `oc` — cluster access
* `curl` — used by Operator Setup (ODH/LWS)
* `jq` — used for validation and version parsing
* `kustomize` — used for Gateway AuthPolicy (MaaS Components)
* `envsubst` — used for policy templates (MaaS Components)

## Requirements for Open Data Hub project

MaaS requires Open Data Hub version 3.0 or later, with the Model Serving component
enabled (KServe) and properly configured for deploying models with `LLMInferenceService`
resources.

## Requirements for Red Hat OpenShift AI

MaaS requires Red Hat OpenShift AI (RHOAI) version 3.0 or later, with the Model Serving
component enabled (KServe) and properly configured for deploying models with
`LLMInferenceService` resources.

A specific requirement for MaaS v0.1.0+ is to set up RHOAI Model Serving with Red Hat Connectivity Link (RHCL) v1.3 or later.

## Observability Prerequisites (Recommended)

Observability is not required for a minimal install, but is recommended. If you skip it, metrics collection, dashboards, and usage tracking will not function. You can enable observability at any time after the initial install.

!!! warning "Consequences of skipping observability"
    Without observability configured, ServiceMonitors will not be processed, Prometheus will not scrape MaaS components, and dashboards will have no data. Some Tenant condition checks may also be affected. See [Enable Observability](maas-setup.md#enable-observability) for the setup steps and tradeoffs.

The following platform components are required for observability:

- **User Workload Monitoring** — Required for Prometheus to scrape metrics from MaaS components
- **Kuadrant Observability** — Required for rate-limiting and usage metrics (e.g., `authorized_calls`, `limited_calls`)
- **Telemetry on the Tenant CR** — Enabled by default; deploys TelemetryPolicy and Istio Telemetry for per-subscription latency tracking

See [Observability Setup](../observability/setup.md) for detailed configuration steps, the `scripts/observability/install-observability.sh` script, and Grafana dashboard deployment.

For RHOAI-specific observability, see [Managing observability in RHOAI](https://docs.redhat.com/en/documentation/red_hat_openshift_ai_self-managed/3.4/html/managing_openshift_ai/managing-observability_managing-rhoai).

### RHOAI Dashboard Observability Tab

To enable the **Observability** tab in the RHOAI Dashboard (Perses-based dashboards), you need the
Cluster Observability Operator, OpenTelemetry Operator, DSCI monitoring configuration, and a
Dashboard feature flag. See [RHOAI Dashboard Observability Tab](../observability/setup.md#rhoai-dashboard-observability-tab-optional) for the full setup and verification steps.

### GenAI Studio

To enable **GenAI Studio** in the RHOAI Dashboard, you need the LlamaStack Operator enabled in your
DSC and a Dashboard feature flag. See [OdhDashboardConfig Feature Flags](maas-setup.md#odhdashboardconfig-feature-flags) for setup.
