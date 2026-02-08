# Slide Ideas for MaaS Controller Presentations

Use this document as a **slide outline** when building decks (e.g. PowerPoint, Google Slides, or Marp). Each section suggests a slide (or a short sequence); copy the bullets and diagrams into your tool and adjust for audience and time.

---

## Deck A: Executive / Product (10–15 min)

**Audience:** Product, leadership, non-engineers.  
**Goal:** What is MaaS, what problem does the controller solve, and what’s the value?

### Slide 1: Title
- **Title:** Models-as-a-Service (MaaS) Control Plane
- **Subtitle:** Subscription-style access and rate limits for AI/LLM models on OpenShift
- Optional: Your name, date, “Red Hat / Open Data Hub”

### Slide 2: The problem
- **Title:** The Problem
- **Bullets:**
  - Teams want to expose LLM models as a service (MaaS) with **who can use what** and **how much** (rate limits).
  - Doing this manually with gateway/auth/rate-limit config is error-prone and doesn’t scale.
  - We need a **single place** to declare “this model,” “these users/groups can access it,” and “these limits per subscription.”

### Slide 3: The solution in one sentence
- **Title:** MaaS Controller in One Sentence
- **One line:** A Kubernetes controller that turns high-level “model + access + subscription” definitions into gateway authentication and token rate limits, so operators manage intent instead of low-level policies.

### Slide 4: What you declare vs what runs
- **Title:** Declare Intent, Controller Does the Rest
- **Two columns:**
  - **You declare:** MaaSModel (which model), MaaSAuthPolicy (who can access), MaaSSubscription (who gets which rate limits).
  - **Controller creates:** HTTPRoutes, Kuadrant AuthPolicies, TokenRateLimitPolicies attached to those routes.
- Optional: Use the “High-Level Architecture” diagram from MAAS_CONTROLLER_OVERVIEW.md (Operator → Controller → Gateway stack).

### Slide 5: Benefits
- **Title:** Why This Matters
- **Bullets:**
  - **Single source of truth:** Model access and limits live in MaaS CRs, not scattered YAML.
  - **Consistent with Kubernetes:** CRDs, reconciliation, GitOps-friendly.
  - **Reusable stack:** Built on Gateway API and Kuadrant; works with OpenShift and existing MaaS gateway.

### Slide 6: Current status and next steps
- **Title:** Status and Next Steps
- **Bullets:**
  - Controller: Implemented (MaaSModel, MaaSAuthPolicy, MaaSSubscription reconcilers).
  - Auth: OpenShift token (e.g. `oc whoami -t`) until API token minting is ready.
  - Next: API token minting, more examples, integration with MaaS API and dashboards.

### Slide 7: Thank you / Q&A
- **Title:** Thank You — Questions?
- Contact / repo link: e.g. `github.com/opendatahub-io/models-as-a-service`

---

## Deck B: Technical Deep-Dive (20–30 min)

**Audience:** Engineers, SREs, platform maintainers.  
**Goal:** How it works under the hood and how to run it.

### Slide 1: Title
- **Title:** MaaS Controller — Technical Deep-Dive
- **Subtitle:** From MaaS CRs to Gateway API and Kuadrant

### Slide 2: Repo and components
- **Title:** What Lives in the Repo
- **Bullets:**
  - **maas-controller:** Go module; CRDs (MaaSModel, MaaSAuthPolicy, MaaSSubscription); three reconcilers; manager entrypoint.
  - **config:** CRD bases, RBAC, manager Deployment, Kustomize.
  - **examples:** Sample MaaS CRs + install script.
  - **hack:** Script to disable shared gateway-auth-policy.

### Slide 3: Architecture diagram
- **Title:** High-Level Architecture
- **Content:** Paste the “High-Level Architecture” Mermaid diagram from MAAS_CONTROLLER_OVERVIEW.md (Operator → Controller → Gateway stack → Backend).
- **Talking point:** “You apply MaaS CRs; the controller reconciles them into HTTPRoute, AuthPolicy, and TokenRateLimitPolicy that all attach to the same route and backend.”

### Slide 4: The three MaaS CRs
- **Title:** The Three MaaS Custom Resources
- **Table or bullets:**
  - **MaaSModel:** Registers a model (llmisvc or ExternalModel); controller ensures/validates HTTPRoute.
  - **MaaSAuthPolicy:** modelRefs + subjects (groups/users); controller creates one Kuadrant AuthPolicy per model.
  - **MaaSSubscription:** owner (groups/users) + modelRefs with token rate limits; controller creates one TokenRateLimitPolicy per model.

### Slide 5: Request flow
- **Title:** What Happens on an Inference Request
- **Content:** Paste the “Request Flow” sequence diagram from MAAS_CONTROLLER_OVERVIEW.md.
- **Talking point:** “Gateway → AuthPolicy validates token and attaches identity → TokenRateLimitPolicy checks limits using that identity → request hits the backend.”

### Slide 6: The “string trick”
- **Title:** Passing Groups from AuthPolicy to TokenRateLimitPolicy
- **Bullets:**
  - **Problem:** TokenRateLimitPolicy CEL predicates need a reliable way to use “allowed groups” from AuthPolicy.
  - **Approach:** AuthPolicy writes **groups_str** = comma-separated allowed groups (e.g. `filter(...).join(",")`).
  - **Usage:** TokenRateLimitPolicy uses `auth.identity.groups_str.split(",").exists(g, g == "group")` for subscription matching.
- Optional: One-line example of each side (AuthPolicy expression vs TRLP predicate).

### Slide 7: What the controller creates (runtime)
- **Title:** Generated Resources (Runtime)
- **Content:** Use the “What the Controller Creates” flowchart from MAAS_CONTROLLER_OVERVIEW.md, or a simple table: MaaSModel → HTTPRoute; MaaSAuthPolicy → AuthPolicy (per model); MaaSSubscription → TokenRateLimitPolicy (per model). All labeled `app.kubernetes.io/managed-by: maas-controller`.

### Slide 8: Controller internals
- **Title:** Controller Internals
- **Content:** “Component Diagram” from MAAS_CONTROLLER_OVERVIEW.md (Manager + three reconcilers, watches, CRDs, RBAC).
- **Bullets:** Single binary; watches MaaS CRs, Gateway API, Kuadrant, LLMInferenceService; uses unstructured client for Kuadrant resources.

### Slide 9: Prerequisites and install
- **Title:** Prerequisites and Install
- **Bullets:**
  - OpenShift (or K8s) with Gateway API and Kuadrant; Open Data Hub operator (for opendatahub ns); optionally KServe.
  - Disable shared gateway-auth-policy: `./hack/disable-gateway-auth-policy.sh`.
  - Install controller: `./scripts/install-maas-controller.sh`.
  - Optional: `./scripts/install-examples.sh` for sample CRs and simulator model.

### Slide 10: Example YAML (one resource)
- **Title:** Example: MaaSAuthPolicy
- **Content:** Short snippet from `examples/maas-auth-policy.yaml` (modelRefs + subjects.groups).
- **Talking point:** “This one resource drives one Kuadrant AuthPolicy per model; no need to edit AuthPolicy YAML by hand.”

### Slide 11: Authentication today
- **Title:** Authentication (Current)
- **Bullets:**
  - No MaaS API token minting yet; use OpenShift token: `oc whoami -t`.
  - AuthPolicy uses Kubernetes TokenReview to validate the token and derive user/groups.
  - When token minting is added, the same controller-generated AuthPolicy/TRLP structure can be reused.

### Slide 12: Summary and links
- **Title:** Summary
- **Bullets:**
  - MaaS Controller = control plane for MaaSModel, MaaSAuthPolicy, MaaSSubscription.
  - Produces HTTPRoute, AuthPolicy, TokenRateLimitPolicy; uses groups_str “string trick” for rate-limit identity.
  - Install: disable gateway-auth-policy, then install-maas-controller.sh.
- **Links:** Repo, README, MAAS_CONTROLLER_OVERVIEW.md.

---

## Deck C: Lightning / Demo (5–7 min)

**Audience:** Mixed; goal is “what is it and how do I try it?”

### Slide 1: Title
- **Title:** MaaS Controller in 5 Minutes

### Slide 2: One-slide picture
- **Title:** One Picture
- **Content:** The “High-Level Architecture” diagram only (Operator → Controller → Gateway stack). One sentence: “You declare models, access, and subscriptions; the controller creates the gateway and policy resources.”

### Slide 3: Three CRs, one sentence each
- **Title:** Three CRs
- MaaSModel = “this model is in MaaS.”
- MaaSAuthPolicy = “these users/groups can use these models.”
- MaaSSubscription = “these users/groups get these token limits per model.”

### Slide 4: How to try it
- **Title:** Try It
- **Bullets:**
  - `./hack/disable-gateway-auth-policy.sh`
  - `./scripts/install-maas-controller.sh`
  - `./scripts/install-examples.sh`
  - Call inference with `Authorization: Bearer $(oc whoami -t)`.

### Slide 5: Q&A
- **Title:** Questions?

---

## Diagram Source Reference

All Mermaid diagrams used in these ideas are in **MAAS_CONTROLLER_OVERVIEW.md**. You can:

- Render them in GitHub/GitLab (Markdown with Mermaid).
- Export as images using [Mermaid Live Editor](https://mermaid.live) or `mmdc` (Mermaid CLI) and paste into slides.
- Redraw in draw.io / Lucidchart / PowerPoint using the same structure.

---

## Tips for Building Slides

1. **Executive deck:** Lead with problem → one-sentence solution → “declare vs create” and benefits; keep technical details to one diagram max.
2. **Technical deck:** Use the same diagrams as the overview doc; add one example YAML and the install commands; include the “string trick” for audiences that care about Kuadrant/identity.
3. **Lightning:** One architecture diagram + three one-liners for the CRs + three install steps; no deep dive.
4. **Consistency:** Use the same diagram (e.g. “High-Level Architecture”) across decks so the story is recognizable in every talk.
