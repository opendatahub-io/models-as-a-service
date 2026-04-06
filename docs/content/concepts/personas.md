# Personas and responsibilities

To understand how MaaS fits into Open Data Hub (ODH), it helps to know **who** owns **which** objects. The diagram below is the resource model: personas on the left, deployable stack on the right (top to bottom).

## Resource model

![Personas resource model](../assets/concepts/personas-resource-model-light.png#only-light)
![Personas resource model](../assets/concepts/personas-resource-model-dark.png#only-dark)

**How to read it**

- **Cluster operators** install and lifecycle the **ODH Operator** (platform install/upgrade).
- **ODH administrators** configure **MaaSAuthPolicy** and **MaaSSubscription** (who may use which models and how much).
- **Data scientists** (model owners) work with **MaaSModelRef** and the **model server** workload in their namespace—typically one stack per model line in the diagram.
- **MaaSSubscription** ties subscriptions to model references; two parallel **MaaSModelRef → ModelServer** branches illustrate multiple models under the same subscription pattern.

---

## Cluster operators

**Who:** Platform engineers who install and maintain the **ODH Operator** on the cluster (and related catalog/subscriptions).

**Owns:** Operator install, upgrades, and health of the operator that reconciles ODH / MaaS components—not day-to-day MaaS CR content.

**Does not usually own:** Individual `MaaSAuthPolicy`, `MaaSSubscription`, or `MaaSModelRef` objects (those fall to administrators and model owners).

---

## ODH administrators

**Who:** OpenShift or ODH **administrators** who govern access and quota for MaaS.

**Owns:** **`MaaSAuthPolicy`** and **`MaaSSubscription`**, shared **Gateway** and routes, and the policy stack (**Authorino**, **Limitador**, **maas-api** integration). RBAC for who may create or edit MaaS CRs in which namespaces.

**Does not usually own:** Inference server images, model weights, or **`MaaSModelRef`** content in application namespaces (that stays with model owners).

---

## Data scientists / model service owners

**Who:** ML engineers, data scientists, or service owners for a model namespace.

**Owns:** **`MaaSModelRef`** (same namespace as the backend) and the **model server** (for example KServe `LLMInferenceService` or your inference Deployment)—the “ModelServer” box in the diagram.

**Does not usually own:** Cluster-wide **`MaaSAuthPolicy`** or **`MaaSSubscription`** CRs, or end-user **API keys** (unless your org merges roles).

---

## API consumers (end users)

**Who:** Application developers, automation, or anyone calling inference with an **`sk-oai-*`** key.

**Owns:** **Self-service** access—authenticate to **`maas-api`**, mint keys, and call model routes within subscription limits.

**Does not usually own:** The CRs in the diagram above; this persona is **not** drawn on the resource-model figure, which focuses on install and configuration roles.

---