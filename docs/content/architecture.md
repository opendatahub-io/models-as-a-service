# MaaS Platform Architecture

## Overview

The MaaS Platform is a Kubernetes-native layer for AI model serving built on [Gateway API](https://gateway-api.sigs.k8s.io/) and policy controllers ([Kuadrant](https://docs.kuadrant.io/), [Authorino](https://docs.kuadrant.io/1.0.x/authorino/), [Limitador](https://docs.kuadrant.io/1.0.x/limitador/)). It provides policy-based authentication and authorization, plus subscription-based rate limiting. Future work includes improved request routing and discovery.

## Architecture

### 🏗️ High-Level Architecture

The MaaS Platform is an end-to-end solution built on [Kuadrant](https://docs.kuadrant.io/).

All traffic flows through the Gateway    **maas-default-gateway** (Gateway API). Then utilizes [Authorino](https://docs.kuadrant.io/1.0.x/authorino/) to enforcing authentication, authorization and [Limitador](https://docs.kuadrant.io/1.0.x/limitador/) to enforce and track token usage. Auth policies use **caching** (e.g., subscription selection results, API key validation) to reduce latency on the hot path.

**Main Flows:**

- **Key Minting** — For obtaining API keys to authenticate programmatic access. 
- **Inference** — For calling models to generate completions.

```mermaid
graph TB
    subgraph UserLayer["User Layer"]
        User[User]
    end
    
    subgraph GatewayPolicyLayer["Gateway & Policy Layer"]
        Gateway[Gateway]
        AuthPolicy[AuthPolicy]
        MaaSAuthPolicy[MaaSAuthPolicy]
        MaaSSubscription[MaaSSubscription]
    end
    
    subgraph TokenManagementLayer["Token Management Layer"]
        MaaSAPI[MaaS API]
    end
    
    subgraph ModelServingLayer["Model Serving Layer"]
        InferenceService[Inference Service]
        LLM[LLM]
    end
    
    User -->|"Request Key"| Gateway
    Gateway --> AuthPolicy
    AuthPolicy --> MaaSAPI
    MaaSAPI -->|"Return API Key"| User
    
    User -->|"Inference"| Gateway
    Gateway --> MaaSAuthPolicy
    MaaSAuthPolicy -.->|"Validate API Key"| MaaSAPI
    MaaSAuthPolicy -->|"Rate Limit"| MaaSSubscription
    MaaSSubscription --> InferenceService
    InferenceService --> LLM
    LLM -->|"Return Response"| User
    
    linkStyle 0,1,2,3 stroke:#1976d2,stroke-width:2px
    linkStyle 4,5,6,7,8,9,10 stroke:#388e3c,stroke-width:2px
    
    style MaaSAPI fill:#1976d2,stroke:#333,stroke-width:2px,color:#fff
    style Gateway fill:#7b1fa2,stroke:#333,stroke-width:2px,color:#fff
    style AuthPolicy fill:#e65100,stroke:#333,stroke-width:2px,color:#fff
    style MaaSAuthPolicy fill:#e65100,stroke:#333,stroke-width:2px,color:#fff
    style MaaSSubscription fill:#e65100,stroke:#333,stroke-width:2px,color:#fff
    style InferenceService fill:#388e3c,stroke:#333,stroke-width:2px,color:#fff
    style LLM fill:#388e3c,stroke:#333,stroke-width:2px,color:#fff
```

### Key Minting Flow — Request & Validation

**Flow summary:**

1. User sends `POST /v1/api-keys` with Bearer `{identity-token}`.
2. Gateway routes the request to AuthPolicy (Authorino).
3. AuthPolicy validates the presented identity token via the configured auth method (`kubernetesTokenReview` for OpenShift, or OIDC JWT validation when enabled).
4. Gateway forwards the authenticated request and user context to the Key Minting Service.

```mermaid
graph TB
    subgraph UserLayer["User"]
        U[User]
    end
    
    subgraph GatewayLayer["Gateway & Policy"]
        G[Gateway]
        AP[AuthPolicy<br/>Authorino]
    end
    
    subgraph KeyMintingLayer["MaaS API"]
        KMS[MaaS API]
    end
    
    U -->|"1. POST /v1/api-keys<br/>Bearer {identity-token}"| G
    G -->|"2. Route /maas-api"| AP
    AP -->|"3. Validate identity token<br/>TokenReview or OIDC JWT"| G
    G -->|"4. Forward + user context"| KMS
    
    style KMS fill:#1976d2,stroke:#333,stroke-width:2px,color:#fff
    style G fill:#7b1fa2,stroke:#333,stroke-width:2px,color:#fff
    style AP fill:#e65100,stroke:#333,stroke-width:2px,color:#fff
```

!!! Tip "OIDC Support"
    The `maas-api` route can be configured to validate external OIDC tokens (for example Keycloak-issued JWTs) in addition to the existing OpenShift TokenReview flow. Model routes still use the current API-key policy, so the interim OIDC flow is: authenticate with OIDC at `maas-api`, mint an `sk-oai-*` key, then use that key for model discovery and inference.


### Key Minting Service (Default Implementation)

**Flow summary:**

1. Gateway forwards the authenticated request and user context to the Key Minting Service (MaaS API).
2. The service generates a random `sk-oai-*` key and hashes it with SHA-256.
3. Only the hash and metadata (username, groups, name, optional `expiresAt` when TTL is set) are stored in PostgreSQL.
4. The plaintext key is returned to the user **once**, along with `expiresAt` when a TTL was specified; the key cannot be retrieved again.

Keys can be permanent (no expiration) or have an optional **TTL** (`expiresIn`, e.g., `30d`, `90d`, `1h`); the response includes `expiresAt` when a TTL is set.

```mermaid
graph TB
    subgraph UserLayer["User"]
        U[User]
    end
    
    subgraph GatewayLayer["Gateway & Policy"]
        G[Gateway]
    end
    
    subgraph KeyMintingService["Key Minting Service (Default)"]
        API[MaaS API]
        Gen[Generate sk-oai-* key]
        Hash[Hash with SHA-256]
    end
    
    subgraph Storage["Storage (Default)"]
        DB[(PostgreSQL<br/>key hashes + metadata + TTL)]
    end
    
    U --> G
    G -->|"Forward + user context"| API
    API --> Gen
    Gen --> Hash
    Hash -->|"Store hash + expiresAt"| DB
    API -->|"Return key ONCE"| U
    
    style API fill:#1976d2,stroke:#333,stroke-width:2px,color:#fff
    style G fill:#7b1fa2,stroke:#333,stroke-width:2px,color:#fff
    style DB fill:#336791,stroke:#333,stroke-width:2px,color:#fff
```

!!! tip "Future Plans"
    This is the **default implementation**. Future plans include integration with other key store providers (e.g., HashiCorp Vault, cloud secret managers).

!!! note "PostgreSQL"
    A **PostgreSQL database is required** and is **not included** with the MaaS deployment. The deploy script provides a basic PostgreSQL deployment for development and testing—**this is not intended for production use**. For production, provision and configure your own PostgreSQL instance.

### Inference Flow — Through MaaS Objects

**Flow summary:**

1. User sends inference request with an API key.
2. Gateway routes to MaaSAuthPolicy (Authorino).
3. MaaSAuthPolicy validates the key via MaaS API and selects subscription; on failure returns 401/403.
4. MaaSSubscription (Limitador) checks token rate limits; on exceed returns 429.
5. Request reaches Inference Service and LLM; completion returned to user.

```mermaid
graph TB
    subgraph UserLayer["User"]
        U[User]
    end
    
    subgraph GatewayLayer["Gateway & Policy"]
        G[Gateway]
        MAP[MaaSAuthPolicy<br/>Authorino]
        MS[MaaSSubscription<br/>Limitador]
    end
    
    subgraph MaaSLayer["Token Management"]
        API[MaaS API]
    end
    
    subgraph ModelLayer["Model Serving"]
        INV[Inference Service]
        LLM[LLM]
    end
    
    U -->|"1. Inference + API key"| G
    G -->|"2. Route"| MAP
    MAP -.->|"3. Validate key"| API
    MAP -->|"4. Auth OK"| MS
    MS -->|"5. Within limits"| INV
    INV -->|"6. Forward"| LLM
    LLM -->|"7. Completion"| U
    
    MAP -.->|"401/403"| U
    MS -.->|"429"| U
    
    linkStyle 7 stroke:#c62828,stroke-width:2px,stroke-dasharray:5,5
    linkStyle 8 stroke:#c62828,stroke-width:2px,stroke-dasharray:5,5
    
    style API fill:#1976d2,stroke:#333,stroke-width:2px,color:#fff
    style G fill:#7b1fa2,stroke:#333,stroke-width:2px,color:#fff
    style MAP fill:#e65100,stroke:#333,stroke-width:2px,color:#fff
    style MS fill:#e65100,stroke:#333,stroke-width:2px,color:#fff
    style INV fill:#388e3c,stroke:#333,stroke-width:2px,color:#fff
    style LLM fill:#388e3c,stroke:#333,stroke-width:2px,color:#fff
```

### Auth & Validation Flow (Deep Dive)

The MaaSAuthPolicy delegates to the MaaS API for key validation and subscription selection. The subscription name comes from the PostgreSQL key record (set at key creation).

**Flow summary:**

1. Authorino calls MaaS API to validate the API key.
2. MaaS API validates the key (format, not revoked, not expired) and returns username, groups, and subscription.
3. Authorino calls MaaS API to check subscription (groups, username, requested subscription from the key).
4. If the user lacks access to the requested subscription → error (403).
5. On success, returns selected subscription; Authorino caches the result (e.g., 60s TTL). Identity information (username, groups, subscription, key ID) is made available to TokenRateLimitPolicy and observability through AuthPolicy's `filters.identity` mechanism, but is **not forwarded** as HTTP headers to upstream model workloads (defense-in-depth security). Clients do not send subscription headers on inference; subscription comes from the API key record created at mint time.

```mermaid
graph TB
    subgraph AuthLayer["MaaSAuthPolicy (Authorino)"]
        A[Authorino]
    end
    
    subgraph MaaSLayer["MaaS API"]
        Validate[Validate API Key]
        SubSelect[Check Subscription]
    end
    
    subgraph Storage["Storage"]
        DB[(PostgreSQL)]
    end
    
    A -->|"1. Validate key"| Validate
    Validate -->|"Lookup hash, check not expired"| DB
    DB -->|"metadata"| Validate
    
    A -->|"2. Check subscription"| SubSelect
    SubSelect -.->|"3. No access to requested sub → 403"| A
    SubSelect -->|"4. Selected subscription"| A
    
    linkStyle 4 stroke:#c62828,stroke-width:2px,stroke-dasharray:5,5
    
    style Validate fill:#1976d2,stroke:#333,stroke-width:2px,color:#fff
    style SubSelect fill:#1976d2,stroke:#333,stroke-width:2px,color:#fff
    style DB fill:#336791,stroke:#333,stroke-width:2px,color:#fff
```

### Observability Flow

Token usage and rate-limit data flow from Limitador into Prometheus and onward to dashboards.

**Flow summary:**

1. Limitador stores token usage counters (e.g., `authorized_hits`, `authorized_calls`, `limited_calls`) with labels (`user`, `model`).
2. A ServiceMonitor (or Kuadrant PodMonitor) configures Prometheus to scrape Limitador's `/metrics` endpoint.
3. Prometheus stores the metrics in its time-series database.
4. Grafana (or other visualization tools) queries Prometheus to build dashboards for usage, billing, and operational health.

```mermaid
graph LR
    subgraph RateLimiting["Rate Limiting"]
        Limitador[Limitador<br/>Token usage counters<br/>authorized_hits, authorized_calls, limited_calls]
    end
    
    subgraph Scraping["Metric Scraping"]
        SM[ServiceMonitor<br/>or PodMonitor]
    end
    
    subgraph Storage["Metrics Storage"]
        Prometheus[(Prometheus)]
    end
    
    subgraph Visualization["Visualization"]
        Grafana[Grafana<br/>Dashboards]
    end
    
    Limitador -->|"/metrics"| SM
    SM -->|"Scrape"| Prometheus
    Prometheus -->|"Query"| Grafana
    
    style Limitador fill:#e65100,stroke:#333,stroke-width:2px,color:#fff
    style Prometheus fill:#1976d2,stroke:#333,stroke-width:2px,color:#fff
    style Grafana fill:#388e3c,stroke:#333,stroke-width:2px,color:#fff
```

## 🔄 Component Flows

### 1. API Key Creation Flow (MaaS API)

Users create API keys by authenticating with an accepted identity token (OpenShift today, or OIDC when configured on the `maas-api` route). The MaaS API generates a key, stores only the hash in PostgreSQL, and returns the plaintext once:

```mermaid
sequenceDiagram
    participant User
    participant Gateway as Gateway API
    participant Authorino
    participant MaaS as MaaS API
    participant DB as PostgreSQL

    User->>Gateway: POST /maas-api/v1/api-keys<br/>Authorization: Bearer {identity-token}
    Gateway->>Authorino: Enforce MaaS API AuthPolicy
    Authorino->>Authorino: Validate token (TokenReview or OIDC JWT)
    Authorino->>Gateway: Authenticated
    Gateway->>MaaS: Forward request with user context

    Note over MaaS,DB: Create API Key
    MaaS->>MaaS: Generate sk-oai-* key, hash with SHA-256
    MaaS->>MaaS: Resolve subscription (explicit or highest priority)
    MaaS->>DB: Store hash + metadata (user, groups, subscription, name)
    DB-->>MaaS: Stored

    MaaS-->>User: { "key": "sk-oai-...", "id": "...", ... }<br/>Plaintext shown ONLY ONCE
```

### 2. Model Inference Flow

Inference requests use the API key. Authorino validates it via HTTP callback (with caching); Limitador enforces subscription-based token limits:

```mermaid
sequenceDiagram
    participant Client
    participant GatewayAPI
    participant Authorino
    participant MaaS as MaaS API
    participant Limitador
    participant LLMInferenceService
    
    Client->>GatewayAPI: Inference + API Key
    GatewayAPI->>Authorino: Validate credentials
    
    alt API key (sk-oai-*)
        Authorino->>MaaS: POST /internal/v1/api-keys/validate
        MaaS->>MaaS: Lookup hash in PostgreSQL
        MaaS-->>Authorino: { valid, userId, groups, subscription }
    end
    
    Authorino->>MaaS: POST /internal/v1/subscriptions/select (subscription check)
    MaaS-->>Authorino: Selected subscription
    
    Authorino->>GatewayAPI: Auth success (cached)
    GatewayAPI->>Limitador: Check TokenRateLimitPolicy
    Limitador-->>GatewayAPI: Within limits
    GatewayAPI->>LLMInferenceService: Forward request
    LLMInferenceService-->>Client: Response
```
