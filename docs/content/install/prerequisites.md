# MaaS Installation Overview

_Models-as-a-Service_ is compatible with the Open Data Hub project (ODH) and
Red Hat OpenShift AI (RHOAI). MaaS is installed by enabling it in the DataScienceCluster resource:

* [Install your platform](platform-setup.md) (ODH or RHOAI operators and DSCInitialization).
* [Install MaaS Components](maas-setup.md) (Database, Gateways, DataScienceCluster).

## Version Compatibility

| MaaS Version | OCP | Kuadrant (ODH) / RHCL (RHOAI) | Gateway API |
|--------------|-----|-------------------------------|-------------|
| v0.0.2       | 4.19.9+ | v1.3+ / v1.2+             | v1.2+       |
| v0.1.0+      | 4.19.9+ | v1.4.2+ / v1.3, v1.4.2+   | v1.2+       |

!!! warning "RHCL v1.4.0 and v1.4.1 -- known issues"
    RHCL v1.4.0 and v1.4.1 ship wasm-shim v0.12.0, which has bugs handling
    nested JSON objects in auth dynamic metadata. This causes:

    - **v1.4.0**: Silent authentication bypass on management endpoints
      ([RHOAIENG-78563](https://redhat.atlassian.net/browse/RHOAIENG-78563)).
    - **v1.4.0 and v1.4.1**: Token rate limits silently bypassed -- 429
      responses never fire
      ([RHOAIENG-75778](https://redhat.atlassian.net/browse/RHOAIENG-75778)).

    These were fixed upstream in wasm-shim v0.12.1
    ([Kuadrant/wasm-shim#312](https://github.com/Kuadrant/wasm-shim/pull/312)),
    shipped in **RHCL v1.4.2**. Upgrade to RHCL v1.4.2+ or use RHCL v1.3.x.
    See [Troubleshooting #16](troubleshooting.md#16-token-rate-limits-bypassed-on-rhcl-v140v141)
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

A specific requirement for MaaS v0.1.0+ is to set up RHOAI Model Serving with Red Hat Connectivity Link (RHCL) v1.3 or v1.4.2+. RHCL v1.4.0 and v1.4.1 have known issues with token rate limiting and authentication -- see the warning above.

## Optional: Observability Prerequisites

If you plan to use MaaS dashboards, showback, or usage metrics, the ODH monitoring stack needs to be enabled in the [Platform Operator](../install/platform-setup.md#install-platform-operator).

To enable the ODH monitoring stack, you need to configure DSCI `monitoring.metrics`. For example:

```shell
kubectl apply -f - <<EOF
apiVersion: dscinitialization.opendatahub.io/v2
kind: DSCInitialization
metadata:
    name: default-dsci
spec:
    applicationsNamespace: opendatahub
    monitoring:
        managementState: Managed
        namespace: opendatahub
        metrics:
            storage:
                size: 90Gi
    trustedCABundle:
        managementState: Managed
EOF
```

Note that enabling the ODH monitoring stack also requires to install the Cluster Observability Operator and OpenTelemetry Operator.

See [Managing observability (RHOAI 3.4)](https://docs.redhat.com/en/documentation/red_hat_openshift_ai_self-managed/3.4/html/managing_openshift_ai/managing-observability_managing-rhoai).

### Loki for Access logs

To enable storing access logs from the MaaS gateway for logs-based showback or auditing, you need to configure a LokiStack. First, install the Loki Operator, and then configure a LokiStack named `usage` in the monitoring namespace configured in DSCI `spec.monitoring.namespace`, for example:

```yaml
apiVersion: loki.grafana.com/v1
kind: LokiStack
metadata:
  name: usage
  namespace: opendatahub
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
      name: <storage-secret-name>
      type: s3
  tenants:
    mode: openshift-logging
```

## GenAI Studio

To enable **GenAI Studio** in the RHOAI Dashboard, you need the LlamaStack Operator enabled in your
DSC and a Dashboard feature flag. See [OdhDashboardConfig Feature Flags](maas-setup.md#odhdashboardconfig-feature-flags) for setup.
