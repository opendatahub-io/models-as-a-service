# MaaS Platform Architecture

## Overview

The MaaS Platform is designed as a cloud-native, Kubernetes-based solution that provides policy-based access control, rate limiting, and tier-based subscriptions for AI model serving. The architecture follows microservices principles and leverages OpenShift/Kubernetes native components for scalability and reliability.

## Architecture

### 🏗️ High-Level Architecture

The MaaS Platform is an end-to-end solution that leverages Kuadrant (Red Hat Connectivity Link) and Open Data Hub (Red Hat OpenShift AI)'s Model Serving capabilities to provide a fully managed, scalable, and secure self-service platform for AI model serving.

**All requests flow through the maas-default-gateway and RHCL components**, which then route requests based on the path:

- `/maas-api/*` requests → MaaS API (token retrieval, validates OpenShift Token via RHCL)
- Inference requests (`/v1/models`, `/v1/chat/completions`) → Model Serving (validates Service Account Token via RHCL)

```mermaid
graph TB
    subgraph "User Layer"
        User[Users]
    end
    
    subgraph "Gateway & Policy Layer"
        GatewayAPI["maas-default-gateway<br/>All Traffic Entry Point"]
        AuthPolicy["<b>Auth Policy</b><br/>Authorino<br/>Token Validation"]
        RateLimit["<b>Rate Limiting</b><br/>Limitador<br/>Usage Quotas"]
    end
    
    subgraph "Token Management Path"
        MaaSAPI["MaaS API<br/>Token Retrieval"]
    end
    
    subgraph "Model Serving Path"
        PathInference["Inference Service"]
        ModelServing["RHOAI Model Serving"]
    end
    
    User -->|"All Requests"| GatewayAPI
    GatewayAPI -->|"All Traffic"| AuthPolicy
    
    AuthPolicy -->|"/maas-api<br/>Auth Only"| MaaSAPI
    MaaSAPI -->|"Returns Token"| User
    
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

### Architecture Details

The MaaS Platform architecture is designed to be modular and scalable. It is composed of the following components:

- **maas-default-gateway**: The single entry point for all traffic (both token requests and inference requests).
- **RHCL (Red Hat Connectivity Link)**: The policy engine that handles authentication and authorization for all requests. Routes requests to appropriate backend based on path:
  - `/maas-api/*` → MaaS API (validates OpenShift tokens)
  - Inference paths (`/v1/models`, `/v1/chat/completions`) → Model Serving (validates Service Account tokens)
- **MaaS API**: The central component for token generation and management, accessed via `/maas-api` path.
- **Open Data Hub (Red Hat OpenShift AI)**: The model serving platform that handles inference requests.

### Detailed Component Architecture

#### MaaS API Component Details

The MaaS API provides a self-service platform for users to request tokens for their inference requests. All requests to the MaaS API pass through the `maas-default-gateway` where authentication is performed against the user's OpenShift token via the Auth Policy component. By leveraging Kubernetes native objects like ConfigMaps and ServiceAccounts, it offers model owners a simple way to configure access to their models based on a familiar group-based access control model.

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
        TierMapping[**Tier Mapping Logic**]
        TokenGen[**Service Account Token Generation**]
    end
    
    subgraph "Configuration"
        ConfigMap[**ConfigMap**<br/>tier-to-group-mapping]
        K8sGroups[**Kubernetes Groups**<br/>tier-free-users<br/>tier-premium-users<br/>tier-enterprise-users]
    end
    
    subgraph "free namespace"
        FreeSA1[**ServiceAccount**<br/>freeuser1-sa]
        FreeSA2[**ServiceAccount**<br/>freeuser2-sa]
    end
    
    subgraph "premium namespace"
        PremiumSA1[**ServiceAccount**<br/>prem-user1-sa]
    end
    
    subgraph "enterprise namespace"
        EnterpriseSA1[**ServiceAccount**<br/>ent-user1-sa]
    end
    
    User -->|"Request with<br/>OpenShift Token"| Gateway
    AdminUI -->|"Request with<br/>OpenShift Token"| Gateway
    Gateway -->|"/maas-api path"| AuthPolicy
    AuthPolicy -->|"Authenticated Request"| API
    
    API --> TierMapping
    API --> TokenGen
    
    TierMapping --> ConfigMap
    ConfigMap -->|Maps Groups to Tiers| K8sGroups
    TokenGen --> FreeSA1
    TokenGen --> FreeSA2
    TokenGen --> PremiumSA1
    TokenGen --> EnterpriseSA1
    
    K8sGroups -->|Group Membership| TierMapping
    
    style API fill:#1976d2,stroke:#333,stroke-width:2px,color:#fff
    style ConfigMap fill:#f57c00,stroke:#333,stroke-width:2px,color:#fff
    style K8sGroups fill:#f57c00,stroke:#333,stroke-width:2px,color:#fff
    style FreeSA1 fill:#388e3c,stroke:#333,stroke-width:2px,color:#fff
    style FreeSA2 fill:#388e3c,stroke:#333,stroke-width:2px,color:#fff
    style PremiumSA1 fill:#388e3c,stroke:#333,stroke-width:2px,color:#fff
    style EnterpriseSA1 fill:#388e3c,stroke:#333,stroke-width:2px,color:#fff
```

**Key Features:**

- **Tier-to-Group Mapping**: Uses ConfigMap in the same namespace as MaaS API to map Kubernetes groups to tiers
- **Configurable Tiers**: Out of the box, the MaaS Platform comes with three default tiers: free, premium, and enterprise. These tiers are configurable and can be extended to support more tiers as needed.
- **Service Account Tokens**: Generates tokens for the appropriate tier's service account based on user's group membership
- **Future Enhancements**: Planned improvements for more sophisticated token management and the ability to integrate with external identity providers.

#### Inference Service Component Details

Once a user has obtained their token through the MaaS API, they can use it to make inference requests to the Gateway API. RHCL's Application Connectivity Policies then validate the token and enforce access control and rate limiting policies:

```mermaid
graph TB
    subgraph "Client Layer"
        Client[Client Applications<br/>with Service Account Token]
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
    
    subgraph "Policy Components"
        AuthPolicy[**AuthPolicy**<br/>gateway-auth-policy]
        RateLimitPolicy[**RateLimitPolicy**<br/>gateway-rate-limits]
        TokenRateLimitPolicy[**TokenRateLimitPolicy**<br/>gateway-level]
    end
    
    subgraph "Model Access Control"
        RBAC[**Kubernetes RBAC**<br/>Service Account Permissions]
        LLMInferenceService[**LLMInferenceService**<br/>Model Access Control]
    end
    
    subgraph "Model Serving"
        RHOAI[**RHOAI Platform**]
        Models[**LLM Models**<br/>Qwen, Granite, Llama]
    end
    
    subgraph "Observability"
        Prometheus[**Prometheus**<br/>Metrics Collection]
    end
    
    Client -->|Inference Request + Service Account Token| GatewayAPI
    GatewayAPI --> Envoy
    
    Envoy --> Kuadrant
    Kuadrant --> Authorino
    Kuadrant --> Limitador
    
    Authorino --> AuthPolicy
    Limitador --> RateLimitPolicy
    Limitador --> TokenRateLimitPolicy
    
    Envoy -->|Check Model Access| RBAC
    RBAC --> LLMInferenceService
    LLMInferenceService -->|POST Permission Check| RHOAI
    RHOAI --> Models
    
    Limitador -->|Usage Metrics| Prometheus
    
    style GatewayAPI fill:#7b1fa2,stroke:#333,stroke-width:2px,color:#fff
    style Kuadrant fill:#f57c00,stroke:#333,stroke-width:2px,color:#fff
    style Authorino fill:#f57c00,stroke:#333,stroke-width:2px,color:#fff
    style Limitador fill:#f57c00,stroke:#333,stroke-width:2px,color:#fff
    style AuthPolicy fill:#d32f2f,stroke:#333,stroke-width:2px,color:#fff
    style RateLimitPolicy fill:#d32f2f,stroke:#333,stroke-width:2px,color:#fff
    style TokenRateLimitPolicy fill:#d32f2f,stroke:#333,stroke-width:2px,color:#fff
    style RBAC fill:#d32f2f,stroke:#333,stroke-width:2px,color:#fff
    style LLMInferenceService fill:#d32f2f,stroke:#333,stroke-width:2px,color:#fff
    style RHOAI fill:#388e3c,stroke:#333,stroke-width:2px,color:#fff
    style Models fill:#388e3c,stroke:#333,stroke-width:2px,color:#fff
    style Prometheus fill:#1976d2,stroke:#333,stroke-width:2px,color:#fff
```

**Policy Engine Flow:**

1. **User Request**: A user makes an inference request to the Gateway API with a valid token.
2. **Service Account Authentication**: Authorino validates service account tokens using gateway-auth-policy
3. **Rate Limiting**: Limitador enforces usage quotas per tier/user using gateway and per-route policies
4. **Model Access Control**: RBAC checks if service account has POST access to the specific LLMInferenceService
5. **Request Forwarding**: Only requests with proper model access are forwarded to RHOAI
6. **Metrics Collection**: Limitador sends usage data to Prometheus for observability dashboards

## 🔄 Component Flows

### 1. Token Retrieval Flow (MaaS API)

The MaaS API generates service account tokens based on user group membership and tier configuration:

```mermaid
sequenceDiagram
    participant User
    participant Gateway as Gateway API
    participant Authorino
    participant MaaS as MaaS API
    participant TierMapper as Tier Mapper
    participant K8s as Kubernetes API

    User->>Gateway: POST /maas-api/v1/tokens<br/>Authorization: Bearer {openshift-token}
    Gateway->>Authorino: Enforce MaaS API AuthPolicy
    Authorino->>K8s: TokenReview (validate OpenShift token)
    K8s-->>Authorino: User identity (username, groups)
    Authorino->>Gateway: Authenticated
    Gateway->>MaaS: Forward request with user context

    Note over MaaS,TierMapper: Determine User Tier
    MaaS->>TierMapper: GetTierForGroups(user.groups)
    TierMapper->>K8s: Get ConfigMap(tier-to-group-mapping)
    K8s-->>TierMapper: Tier configuration
    TierMapper-->>MaaS: User tier (e.g., "premium")

    Note over MaaS,K8s: Ensure Tier Resources
    MaaS->>K8s: Create Namespace({instance}-tier-{tier}) if needed
    MaaS->>K8s: Create ServiceAccount({username-hash}) if needed

    Note over MaaS,K8s: Generate Token
    MaaS->>K8s: CreateToken(namespace, SA name, TTL)
    K8s-->>MaaS: TokenRequest with token and expiration

    MaaS-->>User: {<br/>  "token": "...",<br/>  "expiration": "4h",<br/>  "expiresAt": 1234567890<br/>}
```

### 3. Model Inference Flow

The inference flow routes validated requests to RHOAI models:

The Gateway API and RHCL components validate service account tokens and enforce policies:

```mermaid
sequenceDiagram
    participant Client
    participant GatewayAPI
    participant Kuadrant
    participant Authorino
    participant Limitador
    participant AuthPolicy
    participant RateLimitPolicy
    participant LLMInferenceService
    
    Client->>GatewayAPI: Inference Request + Service Account Token
    GatewayAPI->>Kuadrant: Applying Policies
    Kuadrant->>Authorino: Validate Service Account Token
    Authorino->>AuthPolicy: Check Token Validity
    AuthPolicy-->>Authorino: Token Valid + Tier Info
    Authorino-->>Kuadrant: Authentication Success
    Kuadrant->>Limitador: Check Rate Limits
    Limitador->>RateLimitPolicy: Apply Tier-based Limits
    RateLimitPolicy-->>Limitador: Rate Limit Status
    Limitador-->>Kuadrant: Rate Check Result
    Kuadrant-->>GatewayAPI: Policy Decision (Allow/Deny)
    GatewayAPI ->> LLMInferenceService: Forward Request
    LLMInferenceService-->>Client: Response
```
