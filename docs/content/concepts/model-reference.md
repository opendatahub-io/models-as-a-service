# Model Reference

**MaaSModelRef** is a pointer to an **inference service** (on-cluster or external). 

The controller **collects metadata** from that service and uses it to **wire routing on the default gateway** (`maas-default-gateway`). **MaaSAuthPolicy** and **MaaSSubscription** reference the same `MaaSModelRef` names so **access** and **quota** apply on the inference path.

```mermaid
flowchart LR
    subgraph Downstream ["Downstream (cluster or external)"]
        OnCluster["Inference service<br/>(e.g. LLMInferenceService)"]
        External["External model<br/>(API endpoint)"]
    end

    MaaSModelRef["MaaSModelRef"]

    subgraph Policies ["Policies"]
        MaaSAuthPolicy["MaaSAuthPolicy"]
        MaaSSubscription["MaaSSubscription"]
    end

    OnCluster -->|"1. Endpoint, status"| MaaSModelRef
    External -->|"1. Endpoint, status"| MaaSModelRef
    MaaSModelRef -->|"2. For policies"| MaaSAuthPolicy
    MaaSModelRef -->|"2. For policies"| MaaSSubscription

    style MaaSModelRef fill:#1976d2,stroke:#333,stroke-width:2px,color:#fff
    style MaaSAuthPolicy fill:#e65100,stroke:#333,stroke-width:2px,color:#fff
    style MaaSSubscription fill:#e65100,stroke:#333,stroke-width:2px,color:#fff
    style OnCluster fill:#388e3c,stroke:#333,stroke-width:2px,color:#fff
    style External fill:#00695c,stroke:#333,stroke-width:2px,color:#fff
```

For configuration steps, see [Quota and Access Configuration](../configuration-and-management/quota-and-access-configuration.md).
