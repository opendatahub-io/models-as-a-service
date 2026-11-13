# ExtProc Hardening and Tuning

The AI Gateway Controller configures an Envoy External Processing (ExtProc) filter for each tenant's payload-processing sidecar. Starting with ai-gateway-controller PR #217, the default ExtProc configuration enforces fail-closed semantics, TLS certificate validation, bounded request bodies, and reduced pod service-account privileges. This page describes the security posture and the experimental annotations available to tune concurrency and body-size limits.

!!! warning "Fail-closed by default"

    ExtProc now runs with `failure_mode_allow: false`. If the ExtProc sidecar is unavailable or unhealthy, Envoy returns **503 Service Unavailable** to the client instead of bypassing processing. Plan replica counts and health probes accordingly.

## Security Changes

### TLS Trust

ExtProc connections between Envoy and the payload-processing sidecar use OpenShift service-serving certificates (`tls.mode: provided`) with full certificate and SAN validation. The previous `ACCEPT_UNTRUSTED` mode has been removed.

### Pod Service-Account Privileges

The `payload-processing-reader` ClusterRole and ClusterRoleBinding have been removed. Payload-processing pods no longer hold cluster-scoped read privileges.

### Bounded Request Bodies

The `allow_unbounded_body` option has been removed. All requests processed by ExtProc are subject to a maximum body size (default: 10 MiB). The ExtProc gRPC receive limit is paired at `GRPC_MAX_RECV_MSG_SIZE=11Mi` to accommodate framing overhead.

### No Retry Queue

Requests that exceed the concurrency budget are not queued. Envoy returns **503** immediately when all ExtProc connections are in use. There is no retry or backpressure mechanism — size your gateway replicas and concurrency budget for your expected peak load.

## Experimental Annotations

!!! warning "Experimental — subject to change"

    The annotations below are experimental. Their names, semantics, and valid ranges may change or be removed in a future release without notice.

These annotations are set on the **MaasTenantConfig** resource for a tenant.

### `maas.opendatahub.io/experimental-extproc-max-requests`

Controls the ExtProc concurrency budget. This value is applied to the Envoy circuit breaker as `max_requests`, `max_pending_requests`, `max_connections`, and HTTP/2 `max_concurrent_streams`.

| Parameter | Value |
|---|---|
| Annotation | `maas.opendatahub.io/experimental-extproc-max-requests` |
| Default | 8 |
| Valid range | 1–1024 |
| Applies to | MaasTenantConfig |

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaasTenantConfig
metadata:
  name: example-tenant
  annotations:
    maas.opendatahub.io/experimental-extproc-max-requests: "16"
```

### `maas.opendatahub.io/experimental-extproc-max-body-bytes`

Controls the maximum request body size that ExtProc will accept for processing. This sets `server.max_body_bytes` in the Praxis configuration.

| Parameter | Value |
|---|---|
| Annotation | `maas.opendatahub.io/experimental-extproc-max-body-bytes` |
| Default | 10 MiB |
| Valid range | 1 byte – 32 MiB |
| Applies to | MaasTenantConfig |

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaasTenantConfig
metadata:
  name: example-tenant
  annotations:
    maas.opendatahub.io/experimental-extproc-max-body-bytes: "20971520"  # 20 MiB
```

## Capacity Planning

Size your gateway replicas and `experimental-extproc-max-requests` value against expected concurrent large-body requests. The default pairing assumes approximately 10 MiB bodies within the pre-auth memory limit.

| Factor | Guidance |
|---|---|
| Concurrent large bodies | Each in-flight request up to `max_body_bytes` is held in memory. Multiply `max_requests` by `max_body_bytes` for peak memory estimate per replica. |
| Gateway replicas | Increase replicas before raising `max_requests` beyond the default. Horizontal scaling distributes memory pressure across pods. |
| Overflow behavior | Requests exceeding the concurrency budget receive **503** immediately (fail-closed, no queue). |
| ExtProc unavailability | All requests fail with **503** while the sidecar is down (`failure_mode_allow: false`). |

!!! note

    The `experimental-extproc-max-body-bytes` default of 10 MiB is also referenced in the [External Model Setup](../install/external-model-setup.md) guide. If you change this annotation, the effective limit changes for that tenant's ExtProc pipeline.
