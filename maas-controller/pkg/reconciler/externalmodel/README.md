# External Model Reconciler

Watches MaaS `ExternalModel` CRs and creates the Kubernetes/Gateway API/Istio
resources required to route traffic from the MaaS gateway to an external AI
model provider.

## What It Creates

For each `ExternalModel`, the reconciler creates child resources named
`maas-<externalmodel-name>`:

| # | Resource | Purpose |
|---|----------|---------|
| 1 | ExternalName Service | DNS bridge so HTTPRoute backendRef can reference the external host |
| 2 | ServiceEntry | Registers the external FQDN in the Istio mesh (required for REGISTRY_ONLY) |
| 3 | DestinationRule | TLS origination (skipped when `tls: false`) |
| 4 | HTTPRoute | Routes `/<namespace>/<externalmodel-name>/*` to the provider, sets Host header |

Resources are created in the `ExternalModel` namespace. The HTTPRoute parentRef
targets the configured MaaS gateway, commonly `openshift-ingress/maas-default-gateway`.
OwnerReferences on the child resources let Kubernetes garbage collection remove
them when the `ExternalModel` is deleted.

The MaaS prefix avoids collisions with the upstream
`inference.opendatahub.io` ExternalModel controller, which uses the model name
directly for its networking resources.

## ExternalModel Spec

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: gpt-4o-mini
  namespace: llm
spec:
  provider: openai
  targetModel: gpt-4o-mini
  endpoint: api.openai.com
  credentialRef:
    name: gpt-4o-mini-api-key
```

## Annotation Reference

| Annotation | Required | Default | Example |
|------------|----------|---------|---------|
| `maas.opendatahub.io/port` | No | `443` | `8000` |
| `maas.opendatahub.io/tls` | No | `true` | `false` |
