# MaaS Platform Architecture

## Overview

The MaaS Platform is designed as a cloud-native, Kubernetes-based solution that provides policy-based access control, rate limiting, and subscription-based model access for AI model serving. The architecture follows microservices principles and leverages OpenShift/Kubernetes native components for scalability and reliability.

## Architecture

### 🏗️ High-Level Architecture

The MaaS Platform is an end-to-end solution that leverages Kuadrant (Red Hat Connectivity Link) and Open Data Hub (Red Hat OpenShift AI)'s Model Serving capabilities to provide a fully managed, scalable, and secure self-service platform for AI model serving.

**All requests flow through the maas-default-gateway and RHCL components**, which then route requests based on the path:

- `/maas-api/*` and `/v1/models` requests → MaaS API (API key management, model listing, subscription selection)
- Inference requests (`/{namespace}/{model}/v1/chat/completions`) → Model Serving (validates API key via RHCL callback to maas-api)

```mermaid
graph TB
    subgraph "User Layer"
        User[Users]
    end
    
    subgraph "Gateway & Policy Layer"
        GatewayAPI["maas-default-gateway<br/>All Traffic Entry Point"]
        AuthPolicy["<b>Auth Policy</b><br/>Authorino<br/>API Key Validation"]
        RateLimit["<b>Rate Limiting</b><br/>Limitador<br/>Usage Quotas"]
    end
    
    subgraph "MaaS API Path"
        MaaSAPI["MaaS API<br/>Models, API Keys, Subscriptions"]
    end
    
    subgraph "Model Serving Path"
        PathInference["Inference Service"]
        ModelServing["RHOAI Model Serving"]
    end
    
    User -->|"All Requests"| GatewayAPI
    GatewayAPI -->|"All Traffic"| AuthPolicy
    
    AuthPolicy -->|"/maas-api<br/>Auth Only"| MaaSAPI
    MaaSAPI -->|"Returns Data"| User
    
    AuthPolicy -->|"Inference Traffic<br/>Auth + Rate Limit"| RateLimit
    RateLimit --> PathInference
    PathInference --> ModelServing
    ModelServing -->|"Returns Response"| User
    
    style MaaSAPI fill:#1976d2,stroke:#333,stroke-width:2px,color:#fff
    style GatewayAPI fill:#7b1fa2,stroke:#333,stroke-width:2px,color:#fff
    style AuthPolicy fill:#f57c00,stroke:#333,stroke-width:2px,color:#fff
    style RateLimit fill:#f57c00,stroke:#333,stroke-width:2px,color:#fff
    style PathInference fill:#388e3c,stroke:#333,stroke-width:2px,color:#fff
    style ModelServing fill:#388e3c,stroke:#333,stroke-width:2px,color:#fff
```

### Overall Architecture with maas-controller and CRD Flow

The maas-controller (Kubernetes operator) reconciles MaaS CRDs and generates per-route Kuadrant policies. Configuration is fully declarative and CRD-based:

```mermaid
graph TB
    subgraph "Operators & Controllers"
        Controller["<b>maas-controller</b><br/>Kubernetes Operator"]
    end
    
    subgraph "MaaS CRDs (Configuration)"
        ModelRef["<b>MaaSModelRef</b><br/>Registers models"]
        AuthPolicyCRD["<b>MaaSAuthPolicy</b><br/>Per-model access control<br/>group-based"]
        SubscriptionCRD["<b>MaaSSubscription</b><br/>Per-model token rate limits"]
    end
    
    subgraph "Generated Kuadrant Policies"
        PerRouteAuth["Per-HTTPRoute AuthPolicy<br/>API key validation"]
        PerRouteTRLP["Per-HTTPRoute TokenRateLimitPolicy<br/>Subscription-based limits"]
    end
    
    subgraph "MaaS API"
        MaaSAPI2["maas-api<br/>GET /v1/models<br/>/v1/api-keys/*<br/>/v1/subscriptions/select<br/>/internal/v1/api-keys/validate"]
    end
    
    subgraph "Gateway & Inference"
        Gateway2["maas-default-gateway"]
        Inference2["Model Serving"]
    end
    
    ModelRef --> Controller
    AuthPolicyCRD --> Controller
    SubscriptionCRD --> Controller
    
    Controller -->|"Reconciles"| PerRouteAuth
    Controller -->|"Reconciles"| PerRouteTRLP
    
    PerRouteAuth -->|"Validates API keys"| MaaSAPI2
    PerRouteTRLP -->|"Selects subscription"| MaaSAPI2
    
    Gateway2 --> PerRouteAuth
    Gateway2 --> PerRouteTRLP
    PerRouteAuth --> Inference2
    PerRouteTRLP --> Inference2
    
    style Controller fill:#7b1fa2,stroke:#333,stroke-width:2px,color:#fff
    style ModelRef fill:#f57c00,stroke:#333,stroke-width:2px,color:#fff
    style AuthPolicyCRD fill:#f57c00,stroke:#333,stroke-width:2px,color:#fff
    style SubscriptionCRD fill:#f57c00,stroke:#333,stroke-width:2px,color:#fff
    style MaaSAPI2 fill:#1976d2,stroke:#333,stroke-width:2px,color:#fff
```

### Architecture Details

The MaaS Platform architecture is designed to be modular and scalable. It is composed of the following components:

- **maas-default-gateway**: The single entry point for all traffic (both API requests and inference requests).
- **RHCL (Red Hat Connectivity Link)**: The policy engine that handles authentication and authorization for all requests. Routes requests to appropriate backend based on path:
  - `/maas-api/*` → MaaS API (validates OpenShift tokens for API key management)
  - Inference paths (`/v1/models`, `/v1/chat/completions`) → Model Serving (validates API keys via maas-api callback)
- **maas-controller**: Kubernetes operator that reconciles MaaSModelRef, MaaSAuthPolicy, and MaaSSubscription CRDs. Generates per-HTTPRoute AuthPolicy and TokenRateLimitPolicy resources.
- **MaaS API**: The central component for model listing, API key management, and subscription selection. Serves GET /v1/models, /v1/api-keys/*, /v1/subscriptions/select, and /internal/v1/api-keys/validate (Authorino callback).
- **Open Data Hub (Red Hat OpenShift AI)**: The model serving platform that handles inference requests.

### Detailed Component Architecture

#### MaaS API Component Details

The MaaS API provides a self-service platform for users to manage API keys, list models, and select subscriptions. All requests to the MaaS API pass through the `maas-default-gateway` where authentication is performed against the user's OpenShift token. API keys (sk-oai-* format) are used for inference requests and validated via the Authorino callback to `/internal/v1/api-keys/validate`.

```mermaid
graph TB
    subgraph "External Access"
        User[Users]
        AdminUI[Admin/User UI]
    end
    
    subgraph "Gateway & Auth"
        Gateway[**maas-default-gateway**<br/>Entry Point]
        AuthPolicy[**Auth Policy**<br/>Validates OpenShift Token]
    end
    
    subgraph "MaaS API Service"
        API[**MaaS API**<br/>Go + Gin Framework]
        ModelsHandler[**Model Listing**<br/>GET /v1/models]
        APIKeysHandler[**API Key Management**<br/>/v1/api-keys/*]
        SubscriptionHandler[**Subscription Selection**<br/>/v1/subscriptions/select]
        ValidateHandler[**API Key Validation**<br/>/internal/v1/api-keys/validate]
    end
    
    subgraph "Configuration - MaaS CRDs"
        ModelRefCRD[**MaaSModelRef**<br/>Model registration]
        AuthPolicyCRD[**MaaSAuthPolicy**<br/>Group-based access]
        SubscriptionCRD[**MaaSSubscription**<br/>Token rate limits]
    end
    
    User -->|"Request with<br/>OpenShift Token"| Gateway
    AdminUI -->|"Request with<br/>OpenShift Token"| Gateway
    Gateway -->|"/maas-api path"| AuthPolicy
    AuthPolicy -->|"Authenticated Request"| API
    
    API --> ModelsHandler
    API --> APIKeysHandler
    API --> SubscriptionHandler
    API --> ValidateHandler
    
    ModelsHandler -->|"Lists from cache"| ModelRefCRD
    SubscriptionHandler -->|"Selects from"| SubscriptionCRD
    ValidateHandler -->|"Validates key"| APIKeysHandler
    
    style API fill:#1976d2,stroke:#333,stroke-width:2px,color:#fff
    style ModelRefCRD fill:#f57c00,stroke:#333,stroke-width:2px,color:#fff
    style AuthPolicyCRD fill:#f57c00,stroke:#333,stroke-width:2px,color:#fff
    style SubscriptionCRD fill:#f57c00,stroke:#333,stroke-width:2px,color:#fff
```

**Key Features:**

- **Subscription-Based Access Control via MaaSAuthPolicy and MaaSSubscription CRDs**: Access and rate limits are configured declaratively via MaaSAuthPolicy (group-based access) and MaaSSubscription (per-model token limits). The maas-controller generates per-HTTPRoute AuthPolicy and TokenRateLimitPolicy resources.
- **CRD-Based Configuration**: MaaSModelRef, MaaSAuthPolicy, and MaaSSubscription CRDs provide schema-validated, GitOps-friendly configuration. No ConfigMaps or gateway-level tier mapping.
- **API Keys (sk-oai-*)**: Users create API keys via POST /v1/api-keys. Keys are validated by Authorino via callback to maas-api /internal/v1/api-keys/validate.
- **Model Listing**: GET /v1/models returns models from MaaSModelRef CRs (cached via informer).
- **Usage Metrics**: Limitador sends usage data to Prometheus for observability dashboards.

#### Inference Service Component Details

Once a user has obtained their API key through the MaaS API, they can use it to make inference requests to the Gateway API. RHCL's per-route policies validate the API key (via maas-api callback) and enforce subscription-based rate limits:

```mermaid
graph TB
    subgraph "Client Layer"
        Client[Client Applications<br/>with API Key sk-oai-*]
    end
    
    subgraph "Gateway Layer"
        GatewayAPI[**maas-default-gateway**<br/>maas.CLUSTER_DOMAIN]
        Envoy[**Envoy Proxy**]
    end
    
    subgraph "RHCL Policy Engine"
        Kuadrant[**Kuadrant**<br/>Policy Attachment]
        Authorino[**Authorino**<br/>Authentication Service]
        Limitador[**Limitador**<br/>Rate Limiting Service]
    end
    
    subgraph "Per-Route Policies (generated by maas-controller)"
        PerRouteAuth[**AuthPolicy**<br/>per HTTPRoute]
        PerRouteTRLP[**TokenRateLimitPolicy**<br/>per HTTPRoute]
    end
    
    subgraph "MaaS API Callbacks"
        ValidateAPI[**/internal/v1/api-keys/validate**<br/>API key validation]
        SubSelectAPI[**/v1/subscriptions/select**<br/>Subscription selection]
    end
    
    subgraph "Model Serving"
        RHOAI[**RHOAI Platform**]
        Models[**LLM Models**<br/>Qwen, Granite, Llama]
    end
    
    subgraph "Observability"
        Prometheus[**Prometheus**<br/>Metrics Collection]
    end
    
    Client -->|Inference Request + API Key| GatewayAPI
    GatewayAPI --> Envoy
    
    Envoy --> Kuadrant
    Kuadrant --> Authorino
    Kuadrant --> Limitador
    
    Authorino --> PerRouteAuth
    PerRouteAuth -->|"Validate key"| ValidateAPI
    
    Limitador --> PerRouteTRLP
    PerRouteTRLP -->|"Select subscription"| SubSelectAPI
    
    Envoy -->|"Forward if allowed"| RHOAI
    RHOAI --> Models
    
    Limitador -->|Usage Metrics| Prometheus
    
    style GatewayAPI fill:#7b1fa2,stroke:#333,stroke-width:2px,color:#fff
    style Kuadrant fill:#f57c00,stroke:#333,stroke-width:2px,color:#fff
    style Authorino fill:#f57c00,stroke:#333,stroke-width:2px,color:#fff
    style Limitador fill:#f57c00,stroke:#333,stroke-width:2px,color:#fff
    style PerRouteAuth fill:#d32f2f,stroke:#333,stroke-width:2px,color:#fff
    style PerRouteTRLP fill:#d32f2f,stroke:#333,stroke-width:2px,color:#fff
    style ValidateAPI fill:#1976d2,stroke:#333,stroke-width:2px,color:#fff
    style SubSelectAPI fill:#1976d2,stroke:#333,stroke-width:2px,color:#fff
    style RHOAI fill:#388e3c,stroke:#333,stroke-width:2px,color:#fff
    style Models fill:#388e3c,stroke:#333,stroke-width:2px,color:#fff
    style Prometheus fill:#1976d2,stroke:#333,stroke-width:2px,color:#fff
```

**Policy Engine Flow:**

1. **User Request**: A user makes an inference request to the Gateway API with an API key (sk-oai-*).
2. **API Key Authentication**: Authorino validates the API key via per-route AuthPolicy, which calls maas-api `/internal/v1/api-keys/validate`. The response includes user identity and groups.
3. **Rate Limiting**: Limitador enforces usage quotas per model and subscription using per-route TokenRateLimitPolicy. Subscription selection is done via maas-api `/v1/subscriptions/select` based on user groups.
4. **Request Forwarding**: Only requests with valid API keys and within rate limits are forwarded to RHOAI.
5. **Metrics Collection**: Limitador sends usage data to Prometheus for observability dashboards.

## 🔄 Component Flows

### 1. API Key Creation and Inference Flow

Users create API keys via the MaaS API, then use them for inference requests:

```mermaid
sequenceDiagram
    participant User
    participant Gateway as Gateway API
    participant Authorino
    participant MaaS as MaaS API
    participant Limitador
    participant SubSelect as /v1/subscriptions/select
    participant RHOAI as Model Serving

    Note over User,MaaS: API Key Creation (via /maas-api)
    User->>Gateway: POST /maas-api/v1/api-keys<br/>Authorization: Bearer {openshift-token}
    Gateway->>Authorino: Enforce MaaS API AuthPolicy
    Authorino->>Gateway: Authenticated
    Gateway->>MaaS: Forward request with user context
    MaaS-->>User: API key (sk-oai-*) - shown once

    Note over User,RHOAI: Inference Request
    User->>Gateway: POST /v1/chat/completions<br/>Authorization: Bearer {api-key}
    Gateway->>Authorino: Enforce per-route AuthPolicy
    Authorino->>MaaS: POST /internal/v1/api-keys/validate<br/>{"key": "sk-oai-..."}
    MaaS-->>Authorino: {"valid": true, "userId": "...", "groups": [...]}
    Authorino-->>Gateway: Authenticated
    Gateway->>Limitador: Check rate limits
    Limitador->>MaaS: POST /v1/subscriptions/select<br/>{model, userId, groups}
    MaaS-->>Limitador: Subscription + token limits
    Limitador-->>Gateway: Rate check result
    Gateway->>RHOAI: Forward request
    RHOAI-->>User: Model response
```

### 2. Model Inference Flow

The inference flow routes validated requests to RHOAI models. The Gateway API and RHCL components validate API keys and enforce per-route policies:

```mermaid
sequenceDiagram
    participant Client
    participant GatewayAPI
    participant Kuadrant
    participant Authorino
    participant Limitador
    participant MaaSAPI
    participant RHOAI
    
    Client->>GatewayAPI: Inference Request + API Key
    GatewayAPI->>Kuadrant: Applying Per-Route Policies
    Kuadrant->>Authorino: Validate API Key
    Authorino->>MaaSAPI: POST /internal/v1/api-keys/validate
    MaaSAPI-->>Authorino: User identity + groups
    Authorino-->>Kuadrant: Authentication Success
    Kuadrant->>Limitador: Check Rate Limits
    Limitador->>MaaSAPI: POST /v1/subscriptions/select
    MaaSAPI-->>Limitador: Subscription + limits
    Limitador-->>Kuadrant: Rate Check Result
    Kuadrant-->>GatewayAPI: Policy Decision (Allow/Deny)
    GatewayAPI->>RHOAI: Forward Request
    RHOAI-->>Client: Response
```
