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

!!! warning "RHCL v1.4.0 — silent auth bypass"
    RHCL v1.4.0 contains a Wasm shim bug that silently bypasses gateway authentication.
    Management endpoints return `AUTH_FAILURE`; inference endpoints work but are
    unauthenticated. Upgrade to **RHCL v1.4.1+**. See
    [Troubleshooting #16](troubleshooting.md#16-management-endpoints-return-auth_failure-on-rhcl-v140)
    for diagnosis.

!!! note "Other Kubernetes flavors"
    Other Kubernetes flavors (e.g., upstream Kubernetes, other distributions) are currently being validated.

For the mapping between RHOAI product versions and MaaS releases, see [RHOAI to MaaS Release Mapping](../release-notes/index.md#rhoai-to-maas-release-mapping).


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

## Optional: Observability Prerequisites

If you plan to use MaaS dashboards, showback, or usage metrics, additional platform configuration is required:

- **User Workload Monitoring** — Required for Prometheus to scrape metrics from MaaS components
- **Kuadrant Observability** — Required for rate-limiting and usage metrics (e.g., `authorized_calls`, `limited_calls`)

See [Observability Setup](../observability/setup.md) for detailed configuration steps.

### RHOAI Dashboard Observability Tab

To enable the **Observability** tab in the RHOAI Dashboard (Perses-based dashboards), you need the
Cluster Observability Operator, OpenTelemetry Operator, DSCI monitoring configuration, and a
Dashboard feature flag. See [RHOAI Dashboard Observability Tab](../observability/setup.md#rhoai-dashboard-observability-tab-optional) for the full setup and verification steps.

### GenAI Studio

To enable **GenAI Studio** in the RHOAI Dashboard, you need the LlamaStack Operator enabled in your
DSC and a Dashboard feature flag. See [OdhDashboardConfig Feature Flags](maas-setup.md#odhdashboardconfig-feature-flags) for setup.
