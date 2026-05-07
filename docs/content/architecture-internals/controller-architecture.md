# MaaS Controller Architecture

This document provides a technical deep-dive into the MaaS Controller architecture, internal components, and resource model.

---

## What Is the MaaS Controller?

The **MaaS Controller** is a Kubernetes controller with two main responsibilities:

1. **Tenant reconciler** — deploys and manages the MaaS platform workloads (`maas-api`, gateway policies, telemetry, DestinationRule) via the **`Tenant`** CR (`maas.opendatahub.io/v1alpha1`). On startup the controller self-bootstraps a `default-tenant` CR in the `models-as-a-service` namespace if one does not exist. The Tenant reconciler renders embedded kustomize manifests at runtime and applies them via Server-Side Apply (SSA).

2. **Subscription reconcilers** — let platform operators define:
    - **Which models** are exposed through MaaS (via **MaaSModelRef**).
    - **Who can access** those models (via **MaaSAuthPolicy**).
    - **Per-user/per-group token rate limits** for those models (via **MaaSSubscription**).

The controller does not run inference. It **reconciles** your high-level MaaS CRs into the underlying Gateway API and Kuadrant resources (HTTPRoutes, AuthPolicies, TokenRateLimitPolicies) that enforce routing, authentication, and rate limiting at the gateway.

---

## High-Level Architecture

```mermaid
flowchart TB
    subgraph Platform["Platform lifecycle"]
        Tenant["Tenant CR\n(default-tenant)"]
    end

    subgraph Operator["Platform operator"]
        MaaSModelRef["MaaSModelRef"]
        MaaSAuthPolicy["MaaSAuthPolicy"]
        MaaSSubscription["MaaSSubscription"]
    end

    subgraph Controller["maas-controller"]
        TenantReconciler["Tenant\nReconciler"]
        ModelReconciler["MaaSModelRef\nReconciler"]
        AuthReconciler["MaaSAuthPolicy\nReconciler"]
        SubReconciler["MaaSSubscription\nReconciler"]
    end

    subgraph PlatformWorkloads["Platform Workloads"]
        MaaSAPI["maas-api\n(Deployment, Service, HTTPRoute)"]
        GatewayPolicies["Gateway default policies\n(AuthPolicy, TokenRateLimitPolicy)"]
        Telemetry["TelemetryPolicy\nIstio Telemetry"]
    end

    subgraph GatewayStack["Gateway API + Kuadrant"]
        HTTPRoute["HTTPRoute"]
        AuthPolicy["AuthPolicy\n(Kuadrant)"]
        TokenRateLimitPolicy["TokenRateLimitPolicy\n(Kuadrant)"]
    end

    subgraph Backend["Backend"]
        LLMIS["LLMInferenceService\n(KServe)"]
    end

    Tenant --> TenantReconciler
    TenantReconciler --> MaaSAPI
    TenantReconciler --> GatewayPolicies
    TenantReconciler --> Telemetry

    MaaSModelRef --> ModelReconciler
    MaaSAuthPolicy --> AuthReconciler
    MaaSSubscription --> SubReconciler

    ModelReconciler --> HTTPRoute
    AuthReconciler --> AuthPolicy
    SubReconciler --> TokenRateLimitPolicy

    HTTPRoute --> AuthPolicy
    HTTPRoute --> TokenRateLimitPolicy
    HTTPRoute --> LLMIS
```

**Summary:** The controller has two sides: the **Tenant reconciler** deploys and manages the MaaS platform workloads (maas-api, gateway policies, telemetry) from the `Tenant` CR; the **subscription reconcilers** turn MaaS CRs into Gateway/Kuadrant resources that attach to per-model HTTPRoutes and backends (e.g. KServe LLMInferenceService).

---

## Interaction with MaaS API (discovery)

The **MaaS API** is deployed as part of the **Tenant** platform bundle; it is not the same process as **maas-controller**, but the two work together: the controller **reconciles** **MaaSModelRef** and related CRs, and the API **lists** models from that cluster state for **GET /v1/models**.

For **GET /v1/models**, the MaaS API uses **MaaSModelRef** CRs as its primary source: it reads them cluster-wide (all namespaces), then **validates access** by probing each model's `/v1/models` endpoint with the client's **Authorization** header (passed through as-is). Only models that return 2xx or 405 are included. The catalogue returned to the client is the set of MaaSModelRef objects the controller reconciles, filtered to those the client can access. **No** token exchange is performed; the header is forwarded as-is.

!!! warning "Trust boundary: model discovery"
    The GET /v1/models flow forwards raw **Authorization** headers to model workloads during access validation. That creates a trust boundary:
    - **Model workloads must not log or forward raw Authorization headers** during discovery probes
    - **Operators should only register models trusted to handle credentials safely** via MaaSModelRef
    - For additional protections on model inference routes, see [Authentication Internals](./authentication-internals.md)
    - Future enhancements may include token exchange or credential mediation to reduce exposure during discovery

For end-user behavior and examples, see [Model Discovery](../user-guide/model-discovery.md) and [Model listing flow](../configuration-and-management/model-listing-flow.md).

---

## Component Diagram (Controller Internals)

```mermaid
flowchart TB
    subgraph Cluster["Kubernetes cluster"]
        subgraph maas_controller["maas-controller (Deployment)"]
            Manager["Controller Manager"]
            TenantReconciler["Tenant\nReconciler"]
            ModelReconciler["MaaSModelRef\nReconciler"]
            AuthReconciler["MaaSAuthPolicy\nReconciler"]
            SubReconciler["MaaSSubscription\nReconciler"]
        end

        CRDs["CRDs: Tenant,\nMaaSModelRef,\nMaaSAuthPolicy,\nMaaSSubscription"]
        RBAC["RBAC: ClusterRole,\nServiceAccount, etc."]
    end

    Watch["Watch MaaS CRs,\nGateway API, Kuadrant,\nLLMInferenceService"]
    Manager --> TenantReconciler
    Manager --> ModelReconciler
    Manager --> AuthReconciler
    Manager --> SubReconciler
    TenantReconciler --> Watch
    ModelReconciler --> Watch
    AuthReconciler --> Watch
    SubReconciler --> Watch
    CRDs --> Watch
    RBAC --> maas_controller
```

- Single binary: **manager** runs four reconcilers (Tenant + three subscription reconcilers).
- Registers **Kubernetes core**, **Gateway API**, **KServe (v1alpha1)**, and **MaaS (v1alpha1)** schemes; uses **unstructured** for Kuadrant resources.
- Reads/writes MaaS CRs, HTTPRoutes, Gateways, AuthPolicies, TokenRateLimitPolicies, and LLMInferenceServices (read-only for model metadata/routes).

---

## What the Controller Creates (Runtime View)

```mermaid
flowchart LR
    subgraph MaaS["MaaS CRs (your intent)"]
        MM["MaaSModelRef\n(model ref)"]
        MAP["MaaSAuthPolicy\n(modelRefs + subjects)"]
        MS["MaaSSubscription\n(owner + modelRefs + limits)"]
    end

    subgraph Generated["Generated (per model / route)"]
        HR["HTTPRoute"]
        AP["AuthPolicy"]
        TRL["TokenRateLimitPolicy"]
    end

    MM --> HR
    MAP --> AP
    MS --> TRL
    HR --> AP
    HR --> TRL
```

| Your resource   | Controller creates / uses                                      |
|-----------------|-----------------------------------------------------------------|
| **MaaSModelRef**   | **HTTPRoute** (or validates KServe-created route for LLMInferenceService)  |
| **MaaSAuthPolicy** | One **AuthPolicy** per referenced model; targets that model's HTTPRoute |
| **MaaSSubscription** | One **TokenRateLimitPolicy** per referenced model; targets that model's HTTPRoute |

All generated resources are labeled `app.kubernetes.io/managed-by: maas-controller`.

---

## Data Model (Simplified)

```mermaid
erDiagram
    MaaSModelRef ||--o{ HTTPRoute : "creates or validates"
    MaaSModelRef }o--|| LLMInferenceService : "references (kind: LLMInferenceService)"
    MaaSAuthPolicy ||--o{ AuthPolicy : "one per model"
    MaaSAuthPolicy }o--o{ MaaSModelRef : "modelRefs"
    MaaSSubscription ||--o{ TokenRateLimitPolicy : "one per model"
    MaaSSubscription }o--o{ MaaSModelRef : "modelRefs"
    AuthPolicy }o--|| HTTPRoute : "targetRef"
    TokenRateLimitPolicy }o--|| HTTPRoute : "targetRef"
    HTTPRoute }o--|| Gateway : "parentRef"
```

- **MaaSModelRef**: `spec.modelRef.kind` = LLMInferenceService or ExternalModel; `spec.modelRef.name` = name of the referenced model resource.
- **MaaSAuthPolicy**: `spec.modelRefs` (list of ModelRef objects with name and namespace), `spec.subjects` (groups, users).
- **MaaSSubscription**: `spec.owner` (groups, users), `spec.modelRefs` (list of ModelSubscriptionRef objects with name, namespace, and required `tokenRateLimits` array to define per-model rate limits).

---

## References

### Other internals (this guide)

- [Reconciliation Flow](./reconciliation-flow.md) — Ownership, watches, status, deletion lifecycle
- [Authentication Internals](./authentication-internals.md) — Subscription selection, TRLP keys, identity context

### Install and operations

- [MaaS Setup](../install/maas-setup.md) — Platform install, **Tenant** CR, gateway
- [Validation](../install/validation.md) — Post-install checks
- [Troubleshooting](../install/troubleshooting.md) — Common failures
- [Access and Quota Overview](../concepts/subscription-overview.md) — Subscriptions and access model
- [Quota and Access Configuration](../configuration-and-management/quota-and-access-configuration.md) — Quotas, auth policies, subscriptions

### Discovery and API

- [Model Discovery](../user-guide/model-discovery.md) — Using **GET /v1/models** from a client perspective
- [Inference](../user-guide/inference.md) — Calling inference endpoints
- [Model Listing Flow](../configuration-and-management/model-listing-flow.md) — How listing fits the platform
- [MaaS API Overview](../reference/maas-api-overview.md) — REST surface (discovery, keys, admin)

### Architecture context

- [Architecture](../concepts/architecture.md) — Product-level architecture (Concepts)
