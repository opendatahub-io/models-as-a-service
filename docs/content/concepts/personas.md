# Personas and responsibilities

This page follows the same idea as the [Gateway API personas](https://gateway-api.sigs.k8s.io/#personas): short **who** and **what they own**, focused on **MaaS day-to-day operation** (not cluster install). Anything not listed as in scope is out of scope for that persona.

## Resource model


![Personas resource model](../assets/concepts/personas-resource-model-light.png#only-light)
![Personas resource model](../assets/concepts/personas-resource-model-dark.png#only-dark)

**How to read it**

- **Model owners** deploy **`MaaSModelRef`** and the **model server** workload in their namespace (often one stack per model line).
- **ODH administrators** configure **`MaaSAuthPolicy`** and **`MaaSSubscription`** so the right callers and quotas apply to those models.
- **`MaaSSubscription`** ties subscriptions to model references; parallel **MaaSModelRef → model server** branches can represent multiple models under one subscription pattern.
- **API consumers** call inference through the **Gateway** with an **`sk-oai-*`** key and use **maas-api** for self-service key minting—they do not manage **`MaaSAuthPolicy`**, **`MaaSSubscription`**, or **`MaaSModelRef`** (those sit with administrators and model owners).

---

## Model owners

**Who:** Teams that ship and operate a model in their namespace—often **model owners**, ML engineers, or project admins (not a special “data scientist” role required by MaaS).

**Owns:** **`MaaSModelRef`** in the same namespace as the **model server** (for example KServe `LLMInferenceService` or your inference `Deployment`)—the serving workload the reference points at.

---

## ODH administrators

**Who:** OpenShift or ODH **administrators** who govern access and quota for MaaS.

**Owns:** **`MaaSAuthPolicy`**, **`MaaSSubscription`**, and the **Gateway** / **HTTPRoute** surface that exposes MaaS to users—at the level of **MaaS and Gateway API resources**, not the inference images or weights in application namespaces.

---

## API consumers

**Who:** Application developers, automation, or anyone calling inference with an **`sk-oai-*`** key.

**Owns:** **Self-service** use of **maas-api** (mint and manage keys within policy) and **inference** through the **Gateway**, subject to **`MaaSSubscription`** limits—shown on the **inference** arc in the diagram above.

---
