# params.env Ownership Contract

This document defines the ownership model for `params.env` files used by the
MaaS deployment overlays, establishing which component may read or write each
key to prevent circular writes and reconciliation races.

---

## Problem

Prior to this contract, both the platform operator (RHOAI/ODH) and
`maas-controller` could mutate `params.env` on disk during their respective
reconciliation loops. Because kustomize reads `params.env` to generate the
`maas-parameters` ConfigMap, competing writers caused thrashing: each reconcile
cycle could overwrite values set by the other component, leading to
unpredictable configuration and difficult-to-diagnose drift.

## Ownership Model

**`params.env` is a read-only template.** No reconciler mutates the on-disk
file. Runtime values are resolved in memory and written to a temporary copy
for the kustomize build, then the original is restored.

### Single Writer: maas-controller (Tenant Reconciler)

The `maas-controller` Tenant reconciler is the **sole owner** of runtime
parameter resolution for platform workloads. It:

1. Reads `params.env` as a source of **compile-time defaults** (image tags,
   static configuration).
2. Overlays **runtime values** from the Tenant CR (`gatewayRef`, `apiKeys`,
   `externalOIDC`, `telemetry`) and cluster state (`cluster-audience` from
   `Authentication/cluster`).
3. Applies **`RELATED_IMAGE_*` environment variable overrides** for
   disconnected/pinned-image deployments (same mechanism as ODH operator
   component support).
4. Writes the merged result to a **temporary** params.env for the kustomize
   build, then **restores the original** after rendering.

### Platform Operator (RHOAI/ODH)

The platform operator **does not modify** `params.env` in the maas-controller
overlay directory. Image overrides flow through `RELATED_IMAGE_*` environment
variables on the controller Deployment, which the Tenant reconciler reads at
build time. Namespace and gateway configuration flow through the Tenant CR
spec.

## Key Ownership Table

| Key                          | Default Source         | Runtime Override Source                        |
|------------------------------|-----------------------|------------------------------------------------|
| `maas-api-image`             | params.env (template) | `RELATED_IMAGE_ODH_MAAS_API_IMAGE` env var     |
| `maas-controller-image`      | params.env (template) | `RELATED_IMAGE_ODH_MAAS_CONTROLLER_IMAGE`      |
| `payload-processing-image`   | params.env (template) | `RELATED_IMAGE_ODH_AI_GATEWAY_PAYLOAD_*`       |
| `maas-api-key-cleanup-image` | params.env (template) | `RELATED_IMAGE_UBI_MINIMAL_IMAGE` env var      |
| `gateway-namespace`          | params.env (template) | Tenant CR `spec.gatewayRef.namespace`           |
| `gateway-name`               | params.env (template) | Tenant CR `spec.gatewayRef.name`                |
| `app-namespace`              | params.env (template) | Tenant CR namespace (derived)                   |
| `cluster-audience`           | params.env (template) | `Authentication/cluster` `.spec.serviceAccountIssuer` |
| `api-key-max-expiration-days`| params.env (template) | Tenant CR `spec.apiKeys.maxExpirationDays`      |
| `metadata-cache-ttl`         | params.env (template) | (static default, no runtime override)           |
| `authz-cache-ttl`            | params.env (template) | (static default, no runtime override)           |
| `payload-processing-replicas`| params.env (template) | (static default, no runtime override)           |

## Migration Notes

- **From params.env mutation to CR-driven**: If you previously patched
  `params.env` directly to change gateway or namespace settings, use the
  Tenant CR spec instead. The on-disk file is now treated as immutable
  defaults.
- **Upgrades**: Existing deployments continue to work because the Tenant
  reconciler reads the same default values. Runtime overrides that were
  previously written to disk are now applied in memory on every reconcile
  cycle, ensuring consistency.
