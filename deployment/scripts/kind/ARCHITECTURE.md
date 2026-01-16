# MaaS Kind Local Development Architecture

This document provides visual architecture diagrams for the MaaS platform running on Kubernetes Kind.

## Overall System Architecture (High-Level)

```mermaid
graph TB
    subgraph "Docker Desktop on Mac M4"
        subgraph "Kind Cluster (maas-local)"

            subgraph "Gateway Layer"
                GW[Istio Gateway<br/>localhost:80/443]
            end

            subgraph "Policy Layer"
                AUTH[Authorino<br/>API Key Auth]
                LIMIT[Limitador<br/>Rate Limiting]
            end

            subgraph "Application Layer"
                MAAS[MaaS API<br/>:8080]
                LLM[llm-katan<br/>Qwen2.5-0.5B<br/>:8000<br/><i>Could use KServe</i>]
            end

            subgraph "Infrastructure"
                ISTIO[Istio Service Mesh]
                CERT[cert-manager]
                KUADRANT[Kuadrant Operators]
            end
        end

        PORTS[Port Mapping<br/>80→80, 443→443]
    end

    USER[Developer] -->|curl/browser| PORTS
    PORTS --> GW

    GW -->|/maas-api/*| MAAS
    GW -->|/v1/*| LLM

    GW -.->|checks| AUTH
    GW -.->|checks| LIMIT

    ISTIO -.->|manages| GW
    CERT -.->|TLS certs| GW
    KUADRANT -.->|deploys| AUTH
    KUADRANT -.->|deploys| LIMIT

    classDef user fill:#64B5F6,stroke:#1976D2,stroke-width:3px
    classDef gateway fill:#2196F3,stroke:#0D47A1,stroke-width:3px
    classDef policy fill:#9C27B0,stroke:#4A148C,stroke-width:2px
    classDef app fill:#4CAF50,stroke:#1B5E20,stroke-width:3px
    classDef llm fill:#FF9800,stroke:#E65100,stroke-width:3px
    classDef infra fill:#78909C,stroke:#37474F,stroke-width:2px

    class USER user
    class GW,PORTS gateway
    class AUTH,LIMIT policy
    class MAAS app
    class LLM llm
    class ISTIO,CERT,KUADRANT infra
```

## Detailed System Architecture

```mermaid
graph TB
    subgraph "Developer Machine (Mac M4 / Linux)"
        subgraph "Docker Desktop / Docker Engine"
            subgraph "Kind Cluster (maas-local)"
                subgraph "maas-api namespace"
                    MaasAPI[MaaS API Service<br/>:8080]
                    Gateway[Istio Gateway<br/>:80, :443]
                    AuthPolicy[Auth Policy<br/>Authorino]
                    RatePolicy[Rate Limit Policy<br/>Limitador]
                end

                subgraph "llm namespace"
                    LLMKatan[llm-katan<br/>Qwen2.5-0.5B<br/>:8000]
                end

                subgraph "istio-system namespace"
                    Istiod[Istiod<br/>Control Plane]
                    IngressGW[Istio Ingress<br/>Gateway]
                    EgressGW[Istio Egress<br/>Gateway]
                end

                subgraph "kuadrant-system namespace"
                    Kuadrant[Kuadrant<br/>Operator]
                    Authorino[Authorino<br/>AuthN/AuthZ]
                    Limitador[Limitador<br/>Rate Limiter]
                    DNSOp[DNS Operator]
                end

                subgraph "kserve namespace"
                    KServe[KServe<br/>Controller]
                end

                subgraph "cert-manager namespace"
                    CertMgr[cert-manager]
                    Webhook[cert-manager<br/>webhook]
                    CAInjector[cert-manager<br/>cainjector]
                end
            end
        end

        Browser[Web Browser] -->|http://localhost:80| Docker
        CLI[kubectl/curl] -->|Port Forward| Docker
    end

    Docker -->|Port Mappings<br/>80:80, 443:443| Gateway
    Gateway -->|HTTPRoute| MaasAPI
    Gateway -->|HTTPRoute| LLMKatan
    Gateway -->|Policy Check| AuthPolicy
    Gateway -->|Rate Limit Check| RatePolicy

    AuthPolicy -->|Validate| Authorino
    RatePolicy -->|Count| Limitador

    Gateway -.->|Managed by| Istiod
    IngressGW -.->|Configured by| Istiod

    Kuadrant -->|Manages| Authorino
    Kuadrant -->|Manages| Limitador

    MaasAPI -->|Query Models| LLMKatan

    CertMgr -->|Provides TLS| Gateway

    style MaasAPI fill:#4CAF50
    style LLMKatan fill:#FF9800
    style Gateway fill:#2196F3
    style Kuadrant fill:#9C27B0
    style Browser fill:#64B5F6
```

## Request Flow Architecture

```mermaid
sequenceDiagram
    participant User as Developer
    participant Browser as Browser/CLI
    participant Docker as Docker Desktop
    participant Gateway as Istio Gateway
    participant Auth as Authorino
    participant RateLimit as Limitador
    participant MaasAPI as MaaS API
    participant LLMKatan as llm-katan

    User->>Browser: curl POST /v1/chat/completions
    Browser->>Docker: http://localhost:80
    Docker->>Gateway: Port 80 (mapped)

    Gateway->>Auth: Check AuthPolicy
    Auth-->>Gateway: Valid token ✓

    Gateway->>RateLimit: Check rate limits
    RateLimit-->>Gateway: Under limit ✓

    Gateway->>LLMKatan: Forward request
    LLMKatan->>LLMKatan: AI Inference<br/>(Qwen2.5-0.5B)
    LLMKatan-->>Gateway: AI Response

    Gateway-->>Docker: Response
    Docker-->>Browser: Response
    Browser-->>User: Display result

    Note over Auth,RateLimit: Kuadrant Policies
    Note over LLMKatan: Real AI Model<br/>CPU-only inference
```

## Component Dependency Graph

```mermaid
graph TD
    Kind[Kind Cluster] --> Docker[Docker Engine]

    Gateway[Gateway API] --> Kind
    CertMgr[cert-manager] --> Kind
    Istio[Istio] --> Kind
    Istio --> Gateway

    Kuadrant[Kuadrant Operators] --> Kind
    Kuadrant --> Gateway
    Authorino[Authorino] --> Kuadrant
    Limitador[Limitador] --> Kuadrant

    KServe[KServe] --> Kind
    KServe --> CertMgr

    MaasAPI[MaaS API] --> Kind
    MaasAPI --> Gateway
    MaasAPI --> Authorino
    MaasAPI --> Limitador

    LLMKatan[llm-katan] --> Kind
    LLMKatan --> Gateway

    Gateway --> Istio
    Gateway --> CertMgr

    style Kind fill:#E3F2FD
    style Docker fill:#BBDEFB
    style MaasAPI fill:#4CAF50
    style LLMKatan fill:#FF9800
    style Kuadrant fill:#9C27B0
```

## Deployment Layers

```mermaid
graph TB
    subgraph "Layer 1: Infrastructure"
        L1A[Docker Desktop/Engine]
        L1B[Kind Cluster]
        L1A --> L1B
    end

    subgraph "Layer 2: Core Platform"
        L2A[Gateway API CRDs]
        L2B[cert-manager]
        L2C[Istio Service Mesh]
        L1B --> L2A
        L1B --> L2B
        L1B --> L2C
    end

    subgraph "Layer 3: Policy & Serving"
        L3A[Kuadrant Operators]
        L3B[KServe Controller]
        L2A --> L3A
        L2B --> L3A
        L2B --> L3B
        L2C --> L3A
    end

    subgraph "Layer 4: Application Services"
        L4A[MaaS API]
        L4B[llm-katan Model]
        L4C[Gateway + Routes]
        L3A --> L4A
        L3B --> L4B
        L2C --> L4C
        L2A --> L4C
    end

    subgraph "Layer 5: Policies"
        L5A[AuthPolicy]
        L5B[RateLimitPolicy]
        L4A --> L5A
        L4A --> L5B
        L4C --> L5A
        L4C --> L5B
    end

    style L1A fill:#E3F2FD
    style L1B fill:#BBDEFB
    style L2A fill:#C5E1A5
    style L2B fill:#C5E1A5
    style L2C fill:#C5E1A5
    style L3A fill:#FFE082
    style L3B fill:#FFE082
    style L4A fill:#4CAF50
    style L4B fill:#FF9800
    style L4C fill:#2196F3
    style L5A fill:#9C27B0
    style L5B fill:#9C27B0
```

## Network Traffic Flow

```mermaid
graph LR
    subgraph "External Access"
        Dev[Developer<br/>Mac M4]
    end

    subgraph "Host Ports"
        P80[localhost:80]
        P443[localhost:443]
    end

    subgraph "Kind Port Mappings"
        KP80[Container:80]
        KP443[Container:443]
    end

    subgraph "Istio Gateway"
        IGW[istio-ingressgateway<br/>LoadBalancer]
    end

    subgraph "Gateway API"
        GW[maas-gateway<br/>Gateway]
        HR1[maas-api-route<br/>HTTPRoute]
        HR2[llm-katan-route<br/>HTTPRoute]
    end

    subgraph "Services"
        S1[maas-api:8080<br/>ClusterIP]
        S2[llm-katan:8000<br/>ClusterIP]
    end

    subgraph "Pods"
        P1[maas-api<br/>Pod]
        P2[llm-katan<br/>Pod]
    end

    Dev -->|curl/browser| P80
    Dev -->|curl/browser| P443
    P80 -->|mapped| KP80
    P443 -->|mapped| KP443
    KP80 --> IGW
    KP443 --> IGW
    IGW --> GW
    GW --> HR1
    GW --> HR2
    HR1 -->|/maas-api/*| S1
    HR2 -->|/v1/*| S2
    S1 --> P1
    S2 --> P2

    style Dev fill:#FFC107
    style P1 fill:#4CAF50
    style P2 fill:#FF9800
    style GW fill:#2196F3
```

## Pod Distribution

```mermaid
graph TB
    subgraph "Kind Node: maas-local-control-plane"
        subgraph "cert-manager"
            CM1[cert-manager]
            CM2[cert-manager-webhook]
            CM3[cert-manager-cainjector]
        end

        subgraph "istio-system"
            I1[istiod]
            I2[istio-ingressgateway]
            I3[istio-egressgateway]
        end

        subgraph "kserve"
            K1[kserve-controller-manager<br/>2 containers]
        end

        subgraph "kuadrant-system"
            KU1[kuadrant-operator]
            KU2[authorino-operator]
            KU3[authorino]
            KU4[limitador-operator]
            KU5[limitador]
            KU6[dns-operator]
        end

        subgraph "maas-api"
            M1[maas-api]
            M2[maas-gateway-istio]
        end

        subgraph "llm"
            L1[llm-katan<br/>Qwen2.5-0.5B]
        end
    end

    style CM1 fill:#90CAF9
    style CM2 fill:#90CAF9
    style CM3 fill:#90CAF9
    style I1 fill:#81C784
    style I2 fill:#81C784
    style I3 fill:#81C784
    style K1 fill:#FFB74D
    style KU1 fill:#BA68C8
    style KU2 fill:#BA68C8
    style KU3 fill:#BA68C8
    style KU4 fill:#BA68C8
    style KU5 fill:#BA68C8
    style KU6 fill:#BA68C8
    style M1 fill:#4CAF50
    style M2 fill:#4CAF50
    style L1 fill:#FF9800
```

## Policy Enforcement Flow

```mermaid
stateDiagram-v2
    [*] --> Request: HTTP Request
    Request --> GatewayCheck: Arrives at Gateway

    GatewayCheck --> AuthPolicy: Check Authentication

    state AuthPolicy {
        [*] --> ExtractToken
        ExtractToken --> ValidateToken: Send to Authorino
        ValidateToken --> TokenValid: Valid?
        TokenValid --> [*]: ✓ Authorized
        ValidateToken --> [*]: ✗ 401 Unauthorized
    }

    AuthPolicy --> RateLimitPolicy: Authorized ✓

    state RateLimitPolicy {
        [*] --> CountRequests
        CountRequests --> CheckLimit: Query Limitador
        CheckLimit --> UnderLimit: Under Limit?
        UnderLimit --> [*]: ✓ Allowed
        CheckLimit --> [*]: ✗ 429 Rate Limited
    }

    RateLimitPolicy --> RouteToBackend: Allowed ✓
    RouteToBackend --> MaasAPI: /maas-api/*
    RouteToBackend --> LLMKatan: /v1/*

    MaasAPI --> Response
    LLMKatan --> Response
    Response --> [*]: Return to Client

    AuthPolicy --> Rejected: ✗
    RateLimitPolicy --> Rejected: ✗
    Rejected --> [*]: Error Response
```

## Resource Hierarchy

```mermaid
graph TD
    Cluster[Kind Cluster: maas-local]

    Cluster --> NS1[Namespace: cert-manager]
    Cluster --> NS2[Namespace: istio-system]
    Cluster --> NS3[Namespace: kserve]
    Cluster --> NS4[Namespace: kuadrant-system]
    Cluster --> NS5[Namespace: maas-api]
    Cluster --> NS6[Namespace: llm]

    NS5 --> GW[Gateway: maas-gateway]
    NS5 --> SA[ServiceAccount: maas-api]
    NS5 --> SVC1[Service: maas-api]
    NS5 --> SVC2[Service: maas-gateway-istio]
    NS5 --> DEP1[Deployment: maas-api]
    NS5 --> DEP2[Deployment: maas-gateway-istio]
    NS5 --> AP1[AuthPolicy: maas-api-auth-policy]
    NS5 --> AP2[AuthPolicy: gateway-auth-policy]
    NS5 --> RL1[RateLimitPolicy: gateway-rate-limits]
    NS5 --> HR1[HTTPRoute: maas-api-route]

    NS6 --> SVC3[Service: llm-katan]
    NS6 --> DEP3[Deployment: llm-katan]
    NS6 --> HR2[HTTPRoute: llm-katan-route]

    GW --> HR1
    GW --> HR2

    HR1 --> SVC1
    HR2 --> SVC3

    SVC1 --> DEP1
    SVC3 --> DEP3

    Cluster --> GWC[GatewayClass: istio]
    Cluster --> CR[ClusterRole: maas-api]
    Cluster --> CRB[ClusterRoleBinding: maas-api]

    style Cluster fill:#E3F2FD
    style NS5 fill:#C8E6C9
    style NS6 fill:#FFE0B2
    style GW fill:#2196F3
    style DEP1 fill:#4CAF50
    style DEP3 fill:#FF9800
```

## Kustomize Overlay Structure

```mermaid
graph TD
    subgraph "deployment/base/"
        B1[networking/<br/>Gateway, Kuadrant]
        B2[maas-api/<br/>Deployment, Service, RBAC]
        B3[policies/<br/>AuthPolicy, RateLimitPolicy]
    end

    subgraph "deployment/overlays/kind/"
        K1[kustomization.yaml<br/>Main overlay]
        K2[namespaces.yaml]
        K3[gateway-class.yaml]
        K4[gateway-certificate.yaml]
        K5[Patches<br/>Gateway, HTTPRoute, Policies]
    end

    subgraph "deployment/overlays/kind/test-models/llm-katan/"
        L1[kustomization.yaml]
        L2[deployment.yaml]
        L3[service.yaml]
        L4[httproute.yaml]
    end

    K1 -->|inherits| B1
    K1 -->|inherits| B2
    K1 -->|inherits| B3
    K1 -->|includes| K2
    K1 -->|includes| K3
    K1 -->|includes| K4
    K1 -->|applies| K5

    L1 -->|references| K1
    L1 -->|includes| L2
    L1 -->|includes| L3
    L1 -->|includes| L4

    style K1 fill:#4CAF50
    style L1 fill:#FF9800
    style B1 fill:#90CAF9
    style B2 fill:#90CAF9
    style B3 fill:#90CAF9
```

## Setup Flow

```mermaid
flowchart TD
    Start([Start Setup]) --> Check{Prerequisites<br/>Installed?}

    Check -->|No| Install[./deployment/scripts/kind/<br/>install-prerequisites.sh]
    Install --> Docker{Docker<br/>Running?}
    Check -->|Yes| Docker

    Docker -->|No| StartDocker[Start Docker Desktop]
    StartDocker --> Setup
    Docker -->|Yes| Setup[./deployment/scripts/kind/<br/>setup-kind.sh]

    Setup --> CreateCluster[Create Kind Cluster<br/>kind-config.yaml]
    CreateCluster --> InstallGW[Install Gateway API CRDs]
    InstallGW --> InstallCert[Install cert-manager]
    InstallCert --> InstallIstio[Install Istio]
    InstallIstio --> InstallKuadrant[Install Kuadrant via Helm]
    InstallKuadrant --> InstallKServe[Install KServe]
    InstallKServe --> DeployMaaS[Deploy MaaS Components<br/>kubectl apply -k overlays/kind/]
    DeployMaaS --> DeployModel{Deploy Test<br/>Model?}

    DeployModel -->|Yes| InstallLLM[kubectl apply -k<br/>overlays/kind/test-models/llm-katan/]
    DeployModel -->|No| Complete
    InstallLLM --> WaitReady[Wait for llm-katan<br/>~30 seconds]
    WaitReady --> Complete([✓ Setup Complete])

    Complete --> Test[Test with curl or<br/>port-forward]

    style Start fill:#4CAF50
    style Complete fill:#4CAF50
    style InstallLLM fill:#FF9800
    style DeployMaaS fill:#2196F3
```

## Legend

```mermaid
graph LR
    L1[Infrastructure Component]
    L2[MaaS Core Service]
    L3[AI Model Service]
    L4[Policy/Security]
    L5[Gateway/Routing]
    L6[User Interface]

    style L1 fill:#E3F2FD
    style L2 fill:#4CAF50
    style L3 fill:#FF9800
    style L4 fill:#9C27B0
    style L5 fill:#2196F3
    style L6 fill:#FFC107
```

---

## Quick Reference

### Access Points
- **Browser/CLI**: `http://localhost:80`
- **HTTPS**: `https://localhost:443`
- **MaaS API**: `http://localhost/maas-api/v1/`
- **llm-katan**: `http://localhost/v1/chat/completions`

### Key Resources
- **Cluster Name**: `maas-local`
- **Namespaces**: `maas-api`, `llm`, `istio-system`, `kuadrant-system`, `kserve`, `cert-manager`
- **Total Pods**: 17 pods across 6 namespaces

### Port Mappings
| Host Port | Container Port | Service |
|-----------|----------------|---------|
| 80 | 80 | HTTP Gateway |
| 443 | 443 | HTTPS Gateway |

### Resource Requirements
- **CPU**: 4 cores recommended
- **Memory**: 8GB minimum, 16GB recommended
- **Disk**: 20GB free space

## KServe Architecture

The Kind setup includes **full KServe support with Knative Serving**, providing a production-like environment that matches the OpenShift deployment architecture.

### Why KServe + Knative?

**In Production (OpenShift):**
- Models are deployed as KServe `InferenceService` resources
- MaaS API discovers models by querying KServe InferenceServices
- Automatic scaling, versioning, and traffic management

**In Kind (Local Development):**
- Same architecture as production
- Models can be deployed as `InferenceService` (production-like) or plain Deployments (simpler)
- MaaS API supports both discovery mechanisms
- Full integration testing before deploying to production

### Components

| Component | Version | Purpose |
|-----------|---------|---------|
| **Knative Serving** | v1.10.1 | Serverless platform for KServe with auto-scaling |
| **KServe** | v0.11.0 | Model serving orchestration |
| **Istio** | minimal profile | Service mesh and networking |

### KServe Request Flow

```mermaid
graph LR
    Client[Client Request] --> Kind[Kind Port :80]
    Kind --> Gateway[Istio Gateway<br/>:30080]
    Gateway --> Knative[Knative Service<br/>llm-katan-predictor]
    Knative --> KServe[KServe Container<br/>llm-katan pod]
    KServe --> vLLM[vLLM Model Server]
```

### InferenceService vs Plain Deployment

**Plain Deployment (simpler, used for llm-katan):**
```yaml
apiVersion: apps/v1
kind: Deployment  # Manual deployment
---
apiVersion: v1
kind: Service  # Manual service
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute  # Manual routing
```

**InferenceService (production-like, optional):**
```yaml
apiVersion: serving.kserve.io/v1beta1
kind: InferenceService
metadata:
  name: llm-katan
spec:
  predictor:
    containers:
    - name: kserve-container
      image: ghcr.io/.../llm-katan:latest
```

**KServe automatically creates:**
- ✅ Knative Service
- ✅ Kubernetes Deployment
- ✅ Service endpoints
- ✅ Auto-scaling configuration
- ✅ Traffic routing

