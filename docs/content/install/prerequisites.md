# MaaS Installation Overview

_Models-as-a-Service_ is compatible with the Open Data Hub project (ODH) and
Red Hat OpenShift AI (RHOAI). MaaS is installed by enabling it in the DataScienceCluster resource:

* [Install your platform](platform-setup.md) (ODH or RHOAI operators and DSCInitialization).
* [Install MaaS Components](maas-setup.md) (Database, Gateways, DataScienceCluster).

## Version Compatibility

| MaaS Version | OCP | Kuadrant (ODH) / RHCL (RHOAI) | Gateway API |
|--------------|-----|-------------------------------|-------------|
| v0.0.2       | 4.19.9+ | v1.3+ / v1.2+             | v1.2+       |
| v0.1.0       | 4.19.9+ | v1.3+ / v1.2+             | v1.2+       |
| v0.2.0+      | 4.19.9+ | v1.4.2+ / v1.3+           | v1.2+       |

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

A specific requirement for MaaS v0.2.0+ is to set up ODH's Model Serving with Kuadrant v1.4.2 or later.

## Requirements for Red Hat OpenShift AI

MaaS requires Red Hat OpenShift AI (RHOAI) version 3.0 or later, with the Model Serving
component enabled (KServe) and properly configured for deploying models with
`LLMInferenceService` resources.

A specific requirement for MaaS v0.2.0+ is to set up RHOAI Model Serving with Red Hat Connectivity Link (RHCL) v1.3 or later.

## Optional: Observability Prerequisites

If you plan to use MaaS dashboards, showback, or usage metrics, additional platform configuration is required:

- **User Workload Monitoring** — Required for Prometheus to scrape metrics from MaaS components
- **Kuadrant Observability** — Required for rate-limiting and usage metrics (e.g., `authorized_calls`, `limited_calls`)

See [Observability Prerequisites](../advanced-administration/observability.md#prerequisites) for detailed configuration steps.
