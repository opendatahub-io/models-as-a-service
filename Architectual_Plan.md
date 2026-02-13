# **MaaS Subscription Architecture v2**

**Author:** [Jamie Land](mailto:jland@redhat.com)  
**Date:** January 2026  
**Status:** Proposal \- Completed

---

## **Overview**

This document proposes transitioning Models-as-a-Service (MaaS) from a tier-based access system to a **subscription-driven architecture**. The goal is to separate commercial entitlements from access control to better support multi-tenant enterprise scenarios.

**Related:** [Sister Document](https://docs.google.com/document/d/1cxC581LsxPZYtJOJ08yGtBligP4p5MLK7Ed0sfFT70g/edit?usp=sharing)

---

## **Problem Statement**

The current tier-based system presents several challenges:

| Challenge | Description | Impact |
| :---- | :---- | :---- |
| **Inflexible Access** | Users are restricted to a single tier, preventing access to models in lower or overlapping tiers | Users cannot access models they should have permission to use (or think they have permission to use) |
| **Organizational Complexity** | Cannot represent users belonging to multiple organizations with different access levels | If a user belongs to two organizations with different tiers they will only have access to the “higher level” tier |
| **Sub-optimal GitOps Compatibility** | Current configurations are not utilizing the Kubernetes API’s schema enforcement, making the system "black-box" to standard GitOps tooling | Common tools used in GitOps such as Kustomize `patching` are more difficult when dealing with a ConfigMap vs a CRD  |
| **Non-Native API Integration** | Access control is currently managed through data in a **ConfigMap** rather than the Kubernetes API server | Prevents the use of native API documentation and is less familiar to our core users |
| **Metering Confusion** | Unclear which tier is being billed when users have multiple memberships | Cost allocation challenges; no explicit subscription choice |

   
---

## **Key Design Principles**

The subscription architecture solves these problems through **separation of concerns**:

### **1\. Explicit Subscription Context**

**Problem:** Users could belong to multiple tiers and did to understand which one would take effect for a specific request.  
**Solution:** A user can be part of multiple subscriptions which can be prioritized. But we will allow the user to specify Subscription information inside of the header

### **2\. Flexible Access Control**

**Problem:** Current tier system is rigid—one user, one tier.  
**Solution:** MaaSAuthPolicy allows users to belong to multiple subscriptions. A user in both "data-science-team" and "premium-customers" groups can access both subscriptions based on header information.

### **3\. Decoupled Metering Attribution**

**Problem:** Metering metadata is tightly coupled to technical access controls.  
**Solution:** meteringMetadata lives in MaaSAuthPolicy, allowing the **same subscription** (rate limits, models) to be accessed by different groups with different cost centers/organizations tracked separately.

### **4\. Dynamic Model Catalog**

**Problem:** External models (OpenAI, Anthropic) require custom integration per customer.  
**Solution:** MaaSModel CRD supports both Internal (KServe) and External providers with credential management.

| Concern | Handled By | Examples |
| :---- | :---- | :---- |
| **Commercial Limits** | MaaSSubscription | Token rate limits, quotas per model |
| **Access Control** | MaaSAuthPolicy | Who can access which subscription |
| **Metering Attribution** | MaaSAuthPolicy | Cost center, organization ID, tracking labels |
| **Model Catalog** | MaaSModel | Endpoint, provider, metadata, health status |

### **5\. Subscription vs. Policy Separation**

We separate commercial concerns from access control.

| Concern | Handled By | Examples |
| :---: | :---: | :---: |
| Commercial Limits | MaaSSubscription | Rate limits, quotas, billing rates, cost caps |
| Access Control | MaaSAuthPolicy | Who can access which models, RBAC rules, permissions |

This separation means:

* Subscription entitlements define what you can consume (based on what you pay).  
* Policies define what you’re allowed to access (based on permissions).

Both are enforced together during request evaluation.

---

## **Proposed CRDs**

### **1\. MaaSAuthPolicy**

Defines who (OIDC subjects/groups) can access a specific **Model**. 

```
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: acme-corp-enterprise-access
  namespace: opendatahubannotation:
  display-name: "My Policy"
  display-description: "My Policy Description"
spec:

  modelRefs:
    - granite-3b-instruct
    - gpt-4-turbo

  
  # Who has access (OR logic - any match grants access)
  subjects:
    groups:
      - name: "acme-corp-ai-users"
      - name: "acme-data-science"


status:
  phase: Active
```

**Backing Resources:**

* Creates an **AuthPolicy** for the MaaS API that allows authenticated users to "list" their available subscriptions.  
* When a model request is made, the `AuthPolicy` validates that the user's identity has a `MaaSAuthPolicy` linking them to the specific `subscriptionID` provided in the request header.

---

### **2\. MaaSSubscription**

Defines a subscription plan with **per-model token rate limits** and quotas, as well as billing information.

**Subscriptions** are owned by specific groups and a user must have both permission to access that model from both an AuthPolicy and Subscription perspective

User will use relevant subscription, if multiple modes we will require the user to specify a *X-MAAS-SUBSCRIPTION* header

```
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: enterprise-tier
  namespace: opendatahub
annotation:
  display-name: "Enterprise Subscription"
  display-description: "My Policy"
spec:

  owner:
   groups:
      - name: "acme-corp-ai-users"
  
  # Which models are included with per-model token rate limits
  modelRefs:
    - name: granite-3b-instruct
      # Token rate limits
      tokenRateLimits:
        # Matches required fields for Kuadrant
        - limit: 100000
          window: 24h
  	# Optional field that can be used for visulzation(dashboard) purposes
      billingRate:
        perToken: .000001
    - name: gpt-4-turbo
      # Expensive external model gets lower limits
      tokenRateLimits:
        # Allow for the creation of a refrence to an existing TokenRateLimit giving users more control
        - limit: 2000
          window: 2m
      billingRate:
        # Denote the cost per token
        perToken: .000002
    - name: gpt-4-advance
      tokenRateLimits:
        - ref: custom-rate-limit

  # Comercial Concerns
  organizationalMetadata:
    organizationId: "acme-corp"
    costCenter: "ai-r-and-d"
    labels:
      contract: "enterprise-2024"
      department: "research"
```

**Backing Resources:** Creates **TokenRateLimitPolicy** resources for each model with the subscription's token-based limits. These are enforced by Limitador at the gateway level for each model endpoint.

---

### **3\. MaaSModel**

Represents an AI/ML model endpoint—either internal (KServe) or external (OpenAI, etc.).

```
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: granite-3b-instruct
  namespace: opendatahub
annotations:
  displayName: "IBM Granite 3B Instruct"
  description: "Lightweight instruction-following model"
  contextWindow: "8192"
spec:
  # Model reference - Kind determines which fields are available
  # Similar to https://gateway-api.sigs.k8s.io/reference/spec/#backendobjectreference

  modelRef:
    kind: llmisvc  # llmisvc | ExternalModel
    name: granite-3b-instruct
    namespace: llm  # Optional, defaults to same namespace as MaaSModel

status:
  phase: Ready        # Pending | Ready | Unhealthy | Failed
  endpoint: "https://granite-3b.llm.svc.cluster.local/v1"
  conditions:
    - type: Ready
      status: "True"
```

**Backing Resources:** Creates **HTTPRoute** for the model endpoint and an **AuthPolicy** that validates the request token has access to this model via the subscription context embedded in the token. The combination of MaaSAuthPolicy, MaaSSubscription, and MaaSModel determines the final AuthPolicy rules for model access.

---

## **Evaluation Logic Flow**

Instead of a strict hierarchy, MaaS utilizes a "Dual-Gate" validation model.

* **Access Permissions** are managed via `MaaSPolicy`, which maps subjects (Users/Groups) to targets (Models).  
* **Commercial Entitlements** are managed via `MaaSSubscription`, which defines the rate limits and quotas for a set of models.

A request is only successful if the user has a valid policy for the model **AND** provides a subscription ID that commercially covers that model.

[![][image1]](https://mermaid.ai/app/projects/62029553-e504-46e0-b7bb-08a09be84ea2/diagrams/a9fa8f64-f25b-4fba-a6ab-dc0fcca56dfc/version/v0.1/edit)  
---

## **Key Flow: Request API Key**

Keys are minted at the **subscription level**, making billing and access explicit.

[![][image2]](https://mermaid.live/edit#pako:eNqFVNtu2kAQ_ZXRPlQgmTuFxKpSUdK0UUqDStpKLX1Y7AlsY7zOXpIQxL931msgFyf1g2XvzJlzZvbsrlkkY2Qh03htMY3wWPC54stpCvRwa2RqlzNU_j_jyohIZDw18F2XrZ6fHg-hcoarKJH8qvo8YWDNQiqRSqi4z7FMRLQqyRtxPoHB-PR55OxA-4APfZUGQd6gyhUF23AIJAIG0bUVWhghUzhJ5C1UPGHtA9cYF7T-7dC1o6MnDYS5YEyJnRv0mU9SaoRy4NAHLuQVplC5FWYBcyVtpstoyscQwvh8cgGNm1bjClfaI8pTX6nxgyciJrUP9Pyn0nbYIZxIdctVXOi3JBZE7No3K3hT1s8WSlV2k_8itN_AHYtADZdS5QUbvoqHF5jaIxGPoCv4hlpaFaF-gXcP_HjDE-s6H6NaCq1p23X4bqYaRz8XIlrAkryeaBAazIJeeXs8IV9gDEYCj4hEv39IwxPvdPjM9cOqPvrSAIYKnQoXci6snG5HOOJZJtJ5dQ8vHcDE5kr2WaV0-_xPmKJyjCY3n9s733YNdtSViZ39xchUi0BhhntqfeTHktC2FcERGiSjzN0HJy9xqJyreWMoaWOHVBBVtUTS7iCscx0B4F0mFOqBCdxVUtB5thB-1-v1PxtfhhaQTrK3LuwscyJtGr9G1G12nGVnIo63Jsc0ZgGbKxGz0CiLAVvStnH3y9YuZcroQC9xykL6jPGS28RM2TTdEIwumF9SLrdIMup8wcJLTvICZjN3qooLcreqiBDVkJQaFrZ6eQ0WrtkdC_uH9U77sNdr9vvdfrfTawdsRTltWm61u29bveZBu9np9TcBu89Zm_WD_tuAYSyMVCN_Mef38-YffzfMoA)  
---

## **Key Flow: Model Inference**

[![][image3]](https://mermaid.live/edit#pako:eNqFVdty2jAQ_ZUdPTRkCsHcEz-kQ0NCmUChcWg7HV6EJUATWyKSnEsz-feufElMQls9SfLZ3bOrs-snEirGiU8Mv024DPlA0LWm8UICLppYJZN4yXV23lJtRSi2VFqYAzUwN1xDHfrb7XvA0AEmlAYwpJbf00eo9BO7UVpIdfgePivgMxWJ0IHDkBuzBxkUyCBZmlCLrRVKQuVMxTHXoaDRHpuxsxmLWFjKlIbKt0RZWr9CYtntHpNJGgarE8FMqzvBXBUy2Lx2ejr0YTYNrqEu5IprVzqofOEUUT78rE36_aAWzD_7YJJljS7DWqPZyoN8VRhV3WHl0Mf5g9U0tDBiXFphMe_paHB2CB9gJ7vRoIiNFGHGdSyMcR--00gw6jDZZ7eGSG_mw9mGhzc-DBTPHqo-1CrZwobecaBpccEqsBuRp_np1cOslmXY9DyYXuJbRJG65yxPgEoG545txGNk_VcOwS6Hg1IpDoBnDvbHD97Ez6MVBLhkRTloVI6PRUsT2-Ux_hePtByax1RIIddw64RRYvK6c5F-CKQrM8mUgrg1zilPL3fvHYGJDxdK31PN4Mr1mbFQGRWyOdzFT5yfuQ-B1ZzGiDdbJQ3fBe2eSoKqAiY7U8bWXvxj1pwlKDGrbrg0sNIqLqvyPd25oWs0G6s1pFskESrkXknb_WPeFB-dQkeDEn0embydUNYh54yz_TVqN0_gWilsYvlYVMS85ZGV4T_QkhLS6CUpXFARlQnstN1oBfmcWSHMAM4EzAYY6kMeWAhTXOzyLEspp-S13HMuBcOefREkqZK1Foz4Vie8SnAUoaTwSJ4cZEHsBptlQXzcMr6iSWQXZCGf0QynzS-l4sISm3S9If6KYj5VkmwxnWIqv9ziw-KgOVOJtMRvd7upE-I_kQfiN46Pjxqdrtfw2r2G12o0q-QRQV7jqO01W92TXs_rdBud5yr5nUb1jnrNTrvZbfU6x93OSavbqhLOhFV6kv0a0j_E8x8-2dxH)  
---

## **Backing Resources Overview**

Each MaaS CRD generates specific Kubernetes/Kuadrant resources:

| MaaS CRD | Creates | Purpose |
| :---- | :---- | :---- |
| **MaaSAuthPolicy** | AuthPolicy (for MaaS API) | Controls who can request keys for a subscription; embeds billing metadata in token |
| **MaaSSubscription** | TokenRateLimitPolicy (per model) | Enforces token-based rate limits for each model in the subscription |
| **MaaSModel** | HTTPRoute \+ AuthPolicy (for model) | Routes traffic to model endpoint and validates token has subscription access |

**Combined Effect:** The three CRDs work together to create the full authorization chain:

1. MaaSAuthPolicy → Validates user has access to a model?  
2. MaaSSubscription → What token limits apply to each model?  
3. MaaSModel → Can this token access this specific model?

---

## **MaaSModel Status Controller**

A controller watches MaaSModel resources and maintains health status:

[![][image4]](https://mermaid.live/edit#pako:eNplUlFPgzAQ_ivNPeMGA4HxoFE2o8mWGOdiIuyh0tsglpaUMp1s_90OnBq9h-buu_u-u1yvhUwyhAjWXL5lOVWazB5SQYzVzctG0SonsRRaSc5R9YmjXSVPVGc5mVO6mBsFXq_I2dkFuW7vldwWDBV53FV4efihXB8L9ndCoxKU70mcxDlmr2Q2m9-JNSoUGS5QbYsMSa2pburVX_L0_USeJLdIuc5J1mngV4KgYJUshP5FjbvBpsmyYlTjz8SDf00mfWUPGKXe6d9pl7tJHpCy3XAp8q79bnhDC45sBRZsVMEg0qpBC0pUJT2G0B7pKegcS0whMi7DNW24TiEVB0OrqHiWsjwxlWw2OURrymsTNd3Mk4Kanyi_UbMqs-BYNkJD5LqdBkQtvEMUBoMgsAPH9cfu2PdCz4IdRCM3GNheaPDAsUdeOPYPFnx0Xe2B7507vj06D0LHdx0vsABZoaWa94fR3cfhE89Tqgw)  
**For Internal models:**

- Sync status from referenced LLMInferenceService  
- Propagate endpoint URL when ready

**For External models:**

- Periodic health checks to external API  
- Validate credentials are still valid

---

## **Telemetry Configuration**

Organizations have different telemetry needs. The configuration allows fine-grained control:

```
spec:
  telemetry:
    # Each dimension can be enabled/disabled independently
    captureOrganization: true   # Track org-level aggregates
    captureUser: true            # Track per-user usage
    captureGroup: true            # Track per-group usage
    captureModelUsage: true      # Track per-model usage
```

| Setting | Description | Impact on TelemetryPolicy |
| :---- | :---- | :---- |
| `captureOrganization` | Track organization-level metrics | Adds orgId label to metrics |
| `captureUser` | Track per-user metrics | Adds userId label to metrics (higher cardinality) |
| `captureTokenCounts` | Track input/output token counts | Enables token counter metrics |
| `captureLatency` | Track request latency | Enables histogram metrics |
| `captureModelUsage` | Track per-model usage | Adds modelId label to metrics |

**Note:** Each enabled dimension increases metric cardinality and storage requirements. The MaaS controller updates the **TelemetryPolicy** object based on these settings to configure what data the gateway exports.

---

## **Open Questions**

1. **Token Embedding**: What information should be embedded in subscription tokens? Current thinking: user, groups, metering metadata. Concerns about token size?  
     
2. **External Model Credentials**: How do we handle credentials rotation for external models? Should we support multiple credential rotation strategies? (probably a separate conversation)  
     
3. **Telemetry Cardinality**: What are the recommended defaults for telemetry settings? How do we guide users to avoid accidentally creating extremely high-cardinality metrics?

   

4. If a user has access to a model through a Policy but not a Subscription should the http code be 403 or 429? 