# Legacy External Model Reconciler (`maas.opendatahub.io`)

> **Deprecated — do not create new resources with this API**
>
> `maas.opendatahub.io/v1alpha1` **ExternalModel** is legacy. Use **`inference.opendatahub.io/v1alpha1`** **`ExternalProvider`** and **`ExternalModel`** instead (see [External model setup](../../../../docs/content/install/external-model-setup.md) and the [ExternalModel CRD reference](../../../../docs/content/reference/crds/external-model.md)).
>
> - **RHOAI 3.6+ (Praxis):** New legacy ExternalModels are **rejected at admission** by the maas-controller validating webhook (`vexternalmodel.kb.io`). GitOps and `kubectl apply` must use the inference API group.
> - **RHOAI 3.5 (Go IPP):** Existing legacy objects could be converted automatically by the IPP `legacy-migration` controller into inference CRs; that path does **not** run on Praxis extproc.
>
> This package remains for **existing** legacy CRs: supersede handling, `status.phase: Migrated`, and teardown of `maas-*` networking when a matching inference ExternalModel owns the route.

## Supported API (use this for new work)

Define a provider and model under `inference.opendatahub.io` (same namespace). `MaaSModelRef` resolves inference ExternalModels first.

```yaml
apiVersion: inference.opendatahub.io/v1alpha1
kind: ExternalProvider
metadata:
  name: gpt-4o-mini
  namespace: llm
spec:
  provider: openai
  endpoint: api.openai.com
  auth:
    type: apikey
    secretRef:
      name: gpt-4o-mini-api-key
---
apiVersion: inference.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: gpt-4o-mini
  namespace: llm
spec:
  externalProviderRefs:
    - ref:
        name: gpt-4o-mini
      targetModel: gpt-4o-mini
      apiFormat: openai-chat
      path: /v1/chat/completions
```

Networking for inference ExternalModels is reconciled by **ai-gateway-payload-processing** (IPP) or **ai-gateway-controller** (Praxis), not by this package.

---

## What this reconciler does (legacy only)

Watches **`maas.opendatahub.io`** `ExternalModel` CRs and, when not superseded by an inference ExternalModel, created the Kubernetes/Gateway API/Istio resources required to route traffic from the MaaS gateway to an external provider using the **old** single-CR shape.

When an **`inference.opendatahub.io`** ExternalModel exists for the same name/namespace and has published `status.httpRouteName`, this reconciler tears down legacy `maas-*` children and sets legacy `status.phase` to **`Migrated`**.

## Legacy resources created

For each legacy `ExternalModel`, the reconciler created child resources named `maas-<externalmodel-name>`:

| # | Resource | Purpose |
|---|----------|---------|
| 1 | ExternalName Service | DNS bridge so HTTPRoute backendRef can reference the external host |
| 2 | ServiceEntry | Registers the external FQDN in the Istio mesh (required for REGISTRY_ONLY) |
| 3 | DestinationRule | TLS origination (skipped when `tls: false`) |
| 4 | HTTPRoute | Routes `/<namespace>/<externalmodel-name>/*` to the provider, sets Host header |

Resources are created in the `ExternalModel` namespace. The HTTPRoute parentRef targets the configured MaaS gateway, commonly `openshift-ingress/maas-default-gateway`. OwnerReferences on the child resources let Kubernetes garbage collection remove them when the legacy `ExternalModel` is deleted.

The `maas-` prefix avoids collisions with the **`inference.opendatahub.io`** ExternalModel controller, which uses the model name directly for its networking resources.

## Legacy ExternalModel spec (deprecated)

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

## Annotation reference (legacy CR only)

| Annotation | Required | Default | Example |
|------------|----------|---------|---------|
| `maas.opendatahub.io/port` | No | `443` | `8000` |
| `maas.opendatahub.io/tls` | No | `true` | `false` |
