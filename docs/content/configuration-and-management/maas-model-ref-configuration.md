# MaaSModelRef Configuration

This guide covers how to create and configure **MaaSModelRef** resources to identify models on the cluster. For the conceptual overview, see [MaaS Models](maas-models.md).

## CRD Relationships

The MaaS controller reconciles three custom resources to produce Kuadrant policies:

| You Create | Controller Generates | Purpose |
|------------|---------------------|---------|
| **MaaSModelRef** | (validates HTTPRoute) | Registers model; controller sets `status.endpoint` and `status.phase` |
| **MaaSAuthPolicy** | Kuadrant **AuthPolicy** (per model) | Who can access which models |
| **MaaSSubscription** | Kuadrant **TokenRateLimitPolicy** (per model) | Per-model token rate limits |

Relationships are **many-to-many**: multiple MaaSAuthPolicies and MaaSSubscriptions can reference the same model. The controller aggregates them into a single Kuadrant policy per model.

## Creating a MaaSModelRef

Create a MaaSModelRef for each model you want to expose through MaaS:

```bash
kubectl apply -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: granite-3b-instruct
  namespace: opendatahub
spec:
  modelRef:
    kind: LLMInferenceService
    name: granite-3b-instruct
    namespace: llm
EOF
```

Verify the controller has reconciled and set the endpoint:

```bash
kubectl get maasmodelref -n opendatahub
# Check status.endpoint and status.phase
```

## Spec Fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| modelRef | ModelReference | Yes | Reference to the model endpoint |

### ModelReference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| kind | string | Yes | One of: `LLMInferenceService`, `ExternalModel` |
| name | string | Yes | Name of the model resource |
| namespace | string | No | Namespace of the model resource (defaults to same namespace as MaaSModelRef) |

## Status Fields

| Field | Type | Description |
|-------|------|-------------|
| phase | string | One of: `Pending`, `Ready`, `Unhealthy`, `Failed` |
| endpoint | string | Endpoint URL for the model |
| httpRouteName | string | Name of the HTTPRoute associated with this model |
| httpRouteNamespace | string | Namespace of the HTTPRoute |
| conditions | []Condition | Latest observations of the model's state |

## Referencing Models in Policies and Subscriptions

Both MaaSAuthPolicy and MaaSSubscription reference models by **MaaSModelRef `metadata.name`**:

- **MaaSAuthPolicy** — Use `spec.modelRefs` (list of model names)
- **MaaSSubscription** — Use `spec.modelRefs[].name` for each model

## Related Documentation

- [MaaS Models](maas-models.md) — Conceptual overview
- [Access and Quota Overview](subscription-overview.md) — How policies and subscriptions work together
- [MaaSSubscription Configuration](maas-subscription-configuration.md) — Subscription setup
- [MaaSAuthPolicy Configuration](maas-auth-policy-configuration.md) — Access configuration
- [MaaSModelRef CRD](../reference/crds/maas-model-ref.md) — Full CRD schema reference
