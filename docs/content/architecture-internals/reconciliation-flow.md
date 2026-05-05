# Reconciliation Flow

This document describes how the MaaS Controller **reconciles** resources, **ownership**, and **lifecycle**.

---

## Tenant reconciler

The **Tenant** reconciler watches the singleton **`Tenant`** CR named **`default-tenant`** (`maas.opendatahub.io/v1alpha1`). It:

- Validates **gateway** reference (default `openshift-ingress` / `maas-default-gateway` when unset) and **cluster dependencies** (for example AuthConfig / Kuadrant CRDs).
- Runs **prerequisite checks** in the application namespace before applying manifests.
- **Renders and applies** embedded **kustomize** manifests with **Server-Side Apply**, then waits until the **maas-api** `Deployment` in the app namespace is available.
- Manages **finalizers** and teardown for **Removed** / **Unmanaged** management-state annotations.

Cross-namespace and cluster-scoped platform pieces (gateway AuthPolicy, telemetry, cluster RBAC) are tracked with **labels** and cleaned up when needed; in-namespace workloads use **`ownerReference`** where appropriate (see diagram below).

### Tenant status

`Tenant.status` exposes a high-level **`phase`**: **`Pending`**, **`Active`**, **`Degraded`**, or **`Failed`**, plus **conditions** aligned with platform aggregation (for example **`Ready`**, **`DependenciesAvailable`**, **`MaaSPrerequisitesAvailable`**, **`DeploymentsAvailable`**, **`Degraded`**). Typical outcomes:

- **`Failed`** — blocking prerequisites missing, gateway invalid, or platform apply failed.
- **`Pending`** — manifests applied but **maas-api** not ready yet.
- **`Active`** — platform applied and **maas-api** deployment available; may still be **`Degraded`** if non-blocking warnings exist.

For `spec` fields (gateway, API keys, telemetry, external OIDC), see [Tenant CR](../install/maas-setup.md#tenant-cr).

---

## Tenant resource layout

The `Tenant` CR is namespace-scoped and lives in the **application namespace** for MaaS platform configuration (default **`models-as-a-service`**; configurable via install). It owns resources across three scopes — same-namespace children use standard **`ownerReference`**, while cluster-scoped and cross-namespace children use **tracking labels**.

```mermaid
graph TB
    subgraph "models-as-a-service namespace"
        Tenant["Tenant CR<br/>default-tenant"]
        API["maas-api Deployment"]
        CM["ConfigMaps"]
        SVC["Services"]
        SA["ServiceAccounts"]
        NP["NetworkPolicies"]
        HR["HTTPRoutes"]
        AP2["maas-api AuthPolicy"]
    end

    subgraph "openshift-ingress namespace"
        AP["gateway AuthPolicy"]
        DR["DestinationRule"]
        TP["TelemetryPolicy"]
        IT["Istio Telemetry"]
    end

    subgraph "Cluster-scoped"
        CR["ClusterRoles"]
        CRB["ClusterRoleBindings"]
    end

    Tenant -->|ownerRef| API
    Tenant -->|ownerRef| CM
    Tenant -->|ownerRef| SVC
    Tenant -->|ownerRef| SA
    Tenant -->|ownerRef| NP
    Tenant -->|ownerRef| HR
    Tenant -->|ownerRef| AP2
    Tenant -.->|tracking labels| CR
    Tenant -.->|tracking labels| CRB
    Tenant -.->|tracking labels| AP
    Tenant -.->|tracking labels| DR
    Tenant -.->|tracking labels| TP
    Tenant -.->|tracking labels| IT

    style Tenant fill:#4a90d9,color:#fff
    style AP fill:#f5a623,color:#fff
    style DR fill:#f5a623,color:#fff
    style TP fill:#f5a623,color:#fff
    style IT fill:#f5a623,color:#fff
```

**Solid arrows** = standard ownerReference (automatic GC). **Dashed arrows** = tracking labels (finalizer-based cleanup). **Orange resources** = cross-namespace children that require tracking labels.

---

## Reconciler Behavior

### MaaSModelRef Reconciler

**What it does:**
- Validates that the referenced model resource exists (e.g., LLMInferenceService)
- Validates that HTTPRoute exists for the model (or creates it for certain kinds)
- Updates status with endpoint URL and readiness phase

**Watch triggers:**
- MaaSModelRef changes
- HTTPRoute changes (fixes startup race when KServe creates route after MaaSModelRef)
- LLMInferenceService changes (for backend spec/status updates)

**Status transitions:**
- `Pending` → HTTPRoute doesn't exist yet (KServe still deploying)
- `Ready` → HTTPRoute exists and model is accessible
- `Failed` → Referenced model not found or validation failed

**Finalizer behavior:**
- Adds finalizer to MaaSModelRef
- On deletion, triggers cascade deletion of all AuthPolicies and TokenRateLimitPolicies for this model
- Removes finalizer after cleanup completes

### MaaSAuthPolicy Reconciler

**What it does:**
- Creates one Kuadrant AuthPolicy per referenced model
- Aggregates multiple MaaSAuthPolicies targeting the same model into a single AuthPolicy
- Attaches AuthPolicy to the model's HTTPRoute via `targetRef`

**Watch triggers:**
- MaaSAuthPolicy changes
- MaaSModelRef changes (re-reconcile when model created/deleted)
- HTTPRoute changes (re-reconcile when route appears)
- Generated AuthPolicy changes (overwrite manual edits unless opted out)

**Opt-out annotation:**
```yaml
apiVersion: kuadrant.io/v1
kind: AuthPolicy
metadata:
  annotations:
    opendatahub.io/managed: "false"  # Controller won't overwrite or delete
```

### MaaSSubscription Reconciler

**What it does:**
- Creates one Kuadrant **TokenRateLimitPolicy** per model (aggregating every **MaaSSubscription** that references that model)
- For each subscription that applies to the model, adds a **limit** entry with rates from the CR and a **`when`** predicate that matches  
  `auth.identity.selected_subscription_key` to the model-scoped key  
  `{subNamespace}/{subName}@{modelNamespace}/{modelName}`  
  AuthPolicy is responsible for resolving subscription selection (via maas-api) before TRLP runs; see [Authentication Internals](./authentication-internals.md).
- Exempts `/v1/models` from token consumption limits where configured so discovery still works when quotas are exhausted

**Watch triggers:**
- MaaSSubscription changes
- MaaSModelRef changes (re-reconcile when model created/deleted)
- HTTPRoute changes (re-reconcile when route appears)
- Generated TokenRateLimitPolicy changes (overwrite manual edits unless opted out)

**Opt-out annotation:**
```yaml
apiVersion: kuadrant.io/v1alpha1
kind: TokenRateLimitPolicy
metadata:
  annotations:
    opendatahub.io/managed: "false"  # Controller won't overwrite or delete
```

---

## Lifecycle: Deletion Behavior

**MaaSModelRef deleted:**
- Controller uses finalizer to cascade-delete all AuthPolicies and TokenRateLimitPolicies for that model
- Parent MaaSAuthPolicy and MaaSSubscription CRs remain intact
- Underlying LLMInferenceService is not affected

**MaaSSubscription deleted:**
- Aggregated TokenRateLimitPolicy is deleted, then rebuilt from remaining subscriptions
- If no subscriptions remain, model falls back to gateway defaults (401/403 from auth, or 429 from TRLP safety net)

**MaaSAuthPolicy deleted:**
- Aggregated AuthPolicy is rebuilt from remaining auth policies
- If no auth policies remain, model falls back to gateway default deny (401/403)

**Orphaned policies warning:**
An opted-out policy (`opendatahub.io/managed: "false"`) can become permanently orphaned (no longer reconciled and not deleted) when:
- The last MaaSAuthPolicy/MaaSSubscription referencing a model is deleted
- A model is removed from `spec.modelRefs` (edit rather than deletion)
- A MaaSModelRef is deleted

Manually delete orphaned opted-out resources when no longer needed.

---

## Related documentation

- [Controller Architecture](./controller-architecture.md) — Components and data model
- [Authentication Internals](./authentication-internals.md) — Subscription selection and gateway identity
- [Inference](../user-guide/inference.md) — Runtime HTTP path for model calls
- [Model Discovery](../user-guide/model-discovery.md) — Listing and discovering models
