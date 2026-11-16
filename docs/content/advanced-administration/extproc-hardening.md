# ExtProc Hardening and Tuning

The AI Gateway Controller configures an Envoy External Processing (ExtProc) filter for each tenant's payload-processing sidecar. Starting with ai-gateway-controller PR #217, the default ExtProc configuration enforces fail-closed semantics on the post-auth filter, TLS encryption, bounded request bodies, and reduced pod service-account privileges. This page describes the security posture and the experimental annotations available to tune concurrency and body-size limits.

!!! warning "Post-auth filter is fail-closed"

    The post-auth ExtProc filter (`ipp`) runs with `failure_mode_allow: false`. If the ExtProc sidecar is unavailable or unhealthy, Envoy returns **500 Internal Server Error** to the client instead of bypassing processing. Plan replica counts and health probes accordingly.

    In **Praxis** (the default backend), both the pre-auth (`ipp-pre`) and post-auth (`ipp`) filters are fail-closed (`failure_mode_allow: false`). In **legacy IPP** deployments (this repository's `deployment/` manifests), the pre-auth filter is fail-open (`failure_mode_allow: true`) so that requests are not blocked before authentication completes.

## Security Changes

### TLS Trust

ExtProc connections between Envoy and the payload-processing sidecar are encrypted with TLS. In **Praxis** deployments, the server certificate is validated against the service CA and the DNS SAN is checked. In **legacy IPP** deployments, certificate verification is skipped (`insecureSkipVerify: true` in the DestinationRule), so the server certificate and SAN are **not** validated — a known gap addressed by the Praxis backend.

### Pod Service-Account Privileges

The `payload-processing-reader` ClusterRole and ClusterRoleBinding have been scoped down. The cluster-scoped read privileges previously granted to payload-processing pods have been reduced to the minimum required for operation.

### Bounded Request Bodies

The `allow_unbounded_body` option has been removed. All requests processed by ExtProc are subject to a maximum body size (default: 10 MiB). The ExtProc gRPC receive limit is paired at `GRPC_MAX_RECV_MSG_SIZE=11Mi` to accommodate framing overhead.

### No Retry Queue

Envoy allows up to `max_pending_requests` requests (default: 8) to be held pending during connection establishment. Once both the active request limit (`max_requests`) and the pending limit are reached, Envoy rejects additional requests with an error (**500** or **503** depending on Envoy version and configuration). There is no retry or backpressure mechanism beyond the pending queue — size your ExtProc resources and concurrency budget for your expected peak load.

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
| Concurrent large bodies | Each in-flight request up to `max_body_bytes` is held in memory. Each gateway replica can send up to `max_requests` concurrently to the **same** ExtProc pod. Baseline memory estimate: `max_requests × max_body_bytes × gateway_replicas × ~5` (the ~5× factor accounts for processing copies — e.g., a single 10 MiB body can consume ~50 MiB at peak). Factor this into ExtProc resource limits. |
| Gateway replicas | Adding gateway replicas increases aggregate concurrency against the ExtProc sidecar. Size ExtProc memory to accommodate the combined load from all replicas before scaling the gateway horizontally. |
| Overflow behavior | Requests exceeding both the active (`max_requests`) and pending (`max_pending_requests`) limits receive an error (**500** or **503** depending on Envoy version and configuration). Up to `max_pending_requests` requests may be held during connection setup. |
| ExtProc unavailability | All requests fail with **500** while the sidecar is down (post-auth filter has `failure_mode_allow: false`). |

!!! note

    The `experimental-extproc-max-body-bytes` default of 10 MiB is also referenced in the [External Model Setup](../install/external-model-setup.md) guide. If you change this annotation, the effective limit changes for that tenant's ExtProc pipeline.
