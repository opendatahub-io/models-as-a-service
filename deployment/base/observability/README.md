# Base Observability (Telemetry CRs + Conditional ServiceMonitors)

!!! note "Development / testing path"
    The standard customer install is operator-managed.
    See the [components observability README](../../components/observability/README.md)
    for the full entrypoint map and operator-equivalence table.

## What `kustomize build` produces

The `kustomization.yaml` in this directory includes **only**:

| Resource | Purpose |
|----------|---------|
| `gateway-telemetry-policy.yaml` | Kuadrant **TelemetryPolicy** — injects `user`, `subscription`, and `model` labels into Limitador metrics |
| `istio-gateway-telemetry.yaml` | Istio **Telemetry** — adds `subscription` label to `istio_request_duration_milliseconds_bucket` via the `X-MaaS-Subscription` header |

These are safe to apply directly and are included by
[`deployment/overlays/openshift/`](../../overlays/openshift/kustomization.yaml).

## Conditional resources (script-applied only)

The following files live in this directory but are **not** in `kustomization.yaml`.
They are applied by [`scripts/observability/install-observability.sh`](../../../scripts/observability/install-observability.sh),
which detects existing Kuadrant monitors to avoid duplicate scraping:

| Resource | Applied when… |
|----------|--------------|
| `limitador-servicemonitor.yaml` | No Kuadrant PodMonitor scraping Limitador `/metrics` |
| `authorino-server-metrics-servicemonitor.yaml` | No existing monitor scraping Authorino `/server-metrics` |
| `istio-gateway-service.yaml` | Gateway deployment exists in `openshift-ingress` |
| `istio-gateway-servicemonitor.yaml` | Gateway deployment exists (paired with the Service above) |

## Verify

```bash
kustomize build deployment/base/observability \
  | kubectl apply --dry-run=client -f -
```

## When editing files here

1. Run the verify command above.
2. If you add or remove a resource from `kustomization.yaml`, update the tables in
   this README **and** in [`deployment/components/observability/README.md`](../../components/observability/README.md).
3. If the change affects metrics labels or ServiceMonitor selectors, update
   [`docs/content/advanced-administration/observability.md`](../../../docs/content/advanced-administration/observability.md).
