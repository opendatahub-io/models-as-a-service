# External Model Reconciler

Watches `MaaSModelRef` CRs with `kind: ExternalModel` and creates the Istio
resources required to route traffic from the MaaS gateway to an external AI
model provider.

## What It Creates

For each `ExternalModel` MaaSModelRef, the reconciler creates:

| # | Resource | Purpose |
|---|----------|---------|
| 1 | ExternalName Service | DNS bridge so HTTPRoute backendRef can reference the external host |
| 2 | ServiceEntry | Registers the external FQDN in the Istio mesh (required for REGISTRY_ONLY) |
| 3 | DestinationRule | TLS origination (skipped when `tls: false`) |
| 4 | HTTPRoute | Routes `/external/<provider>/*` to the provider, sets Host header |

Resources are created in the gateway namespace (`openshift-ingress`), which is
cross-namespace from the MaaSModelRef. Since Kubernetes garbage collection does
not follow cross-namespace OwnerReferences, the reconciler uses a **finalizer**
to explicitly delete all managed resources when the CR is removed.

## MaaSModelRef Spec

Until the CRD is enriched with external model fields (tracked separately), the
reconciler reads configuration from annotations:

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: gpt-4o-mini
  namespace: opendatahub
  annotations:
    maas.opendatahub.io/provider: "openai"
    maas.opendatahub.io/endpoint: "api.openai.com"
    maas.opendatahub.io/port: "443"                         # default
    maas.opendatahub.io/tls: "true"                         # default
    maas.opendatahub.io/path-prefix: "/external/openai/"    # default: /external/<provider>/
    maas.opendatahub.io/extra-headers: "anthropic-version=2023-06-01"  # optional
spec:
  modelRef:
    kind: ExternalModel
    name: gpt-4o-mini
```

When the CRD is updated, the reconciler will read from `spec` fields instead.

## Annotation Reference

| Annotation | Required | Default | Example |
|------------|----------|---------|---------|
| `maas.opendatahub.io/endpoint` | Yes | - | `api.openai.com` |
| `maas.opendatahub.io/provider` | No | `spec.modelRef.name` | `openai` |
| `maas.opendatahub.io/port` | No | `443` | `8000` |
| `maas.opendatahub.io/tls` | No | `true` | `false` |
| `maas.opendatahub.io/path-prefix` | No | `/external/<provider>/` | `/v1/` |
| `maas.opendatahub.io/extra-headers` | No | - | `anthropic-version=2023-06-01` |

## Provider Examples

### OpenAI

```yaml
annotations:
  maas.opendatahub.io/provider: "openai"
  maas.opendatahub.io/endpoint: "api.openai.com"
```

### Anthropic

```yaml
annotations:
  maas.opendatahub.io/provider: "anthropic"
  maas.opendatahub.io/endpoint: "api.anthropic.com"
  maas.opendatahub.io/extra-headers: "anthropic-version=2023-06-01"
```

### Self-hosted vLLM (no TLS)

```yaml
annotations:
  maas.opendatahub.io/provider: "vllm"
  maas.opendatahub.io/endpoint: "vllm.example.com"
  maas.opendatahub.io/port: "8000"
  maas.opendatahub.io/tls: "false"
```

## Moving to MaaS

This reconciler is designed to be portable to the MaaS controller repo. To
integrate with the existing MaaS provider pattern:

1. Implement `BackendHandler` interface in `providers_external.go`
2. Call `BuildExternalNameService`, `BuildServiceEntry`, `BuildDestinationRule`,
   `BuildHTTPRoute` from `ReconcileRoute`
3. Set status endpoint and phase from `Status`
4. Finalizer handles cleanup in `CleanupOnDelete`

The resource builder functions in `resources.go` are stateless and can be
copied directly into the MaaS controller.
