# Quota and Access Configuration

This guide provides step-by-step instructions for configuring MaaSModelRef, MaaSAuthPolicy, and MaaSSubscription. For conceptual overview, see [Access and Quota Overview](../concepts/subscription-overview.md) and [Model Reference](../concepts/model-reference.md).

## Prerequisites

- **MaaS platform installed** — See [Install MaaS Components](../install/maas-setup.md)
- **LLMInferenceService** for your model (or external model endpoints)
- Cluster admin or equivalent permissions to create CRs in the `models-as-a-service` and model namespaces

!!! note "Deploy a sample model"
    Command to deploy simulator model as `free-model`:

    ```bash
    kustomize build 'https://github.com/opendatahub-io/models-as-a-service//docs/samples/maas-system/free/llm?ref=main' | \
      sed 's/facebook-opt-125m-simulated/free-model/g' | kubectl apply -f -
    ```

## Overview: What You'll Accomplish

Before running the configuration steps, here is the flow you will set up:

1. **Register models** — Create a MaaSModelRef for each model so MaaS knows about it and can expose it through the API. The controller reconciles each ref and populates the endpoint.

2. **Grant access** — Create MaaSAuthPolicy resources that define *which* groups or users can use *which* models. A user must match a policy to see and call a model.

3. **Define subscriptions** — Create MaaSSubscription resources that define *quota* (token rate limits) for groups or users. A user must have both access (policy) and quota (subscription) to use a model.

4. **Validate** — Confirm the CRs are reconciled, policies are enforced, and you can list models and run inference.

## Configuration Steps

Set the namespace and name of your LLMInferenceService (used in the commands below):

```bash
MODEL_NS=llm
MODEL_NAME=free-model  # From the Prerequisites deploy; or: kubectl get llminferenceservice -n $MODEL_NS -o jsonpath='{.items[0].metadata.name}'
```

### 1. Register Models (MaaSModelRef)

Create a MaaSModelRef for each model you want to expose through MaaS. The MaaSModelRef must be in the **same namespace** as the LLMInferenceService. The `spec.modelRef.name` must match the LLMInferenceService name.

```yaml
kubectl apply -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: ${MODEL_NAME}-ref
  namespace: ${MODEL_NS}
spec:
  modelRef:
    kind: LLMInferenceService
    name: ${MODEL_NAME}
EOF
```

Verify the controller has reconciled and set the endpoint:

```bash
kubectl get maasmodelref -n ${MODEL_NS}
# Check status.endpoint and status.phase
```

### 2. Grant Access (MaaSAuthPolicy)

Create an MaaSAuthPolicy to define which groups/users can access which models:

```yaml
kubectl apply -f - <<EOF
# MaaSAuthPolicy must be deployed to the models-as-a-service namespace
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: free-access
  namespace: models-as-a-service
spec:
  modelRefs:
    - name: ${MODEL_NAME}-ref
      namespace: ${MODEL_NS}
  subjects:
    groups:
      - name: free-users
    users: []
EOF
```

Verify the MaaSAuthPolicy is active (AuthPolicy created and enforced):

```bash
# List generated AuthPolicy (may take a moment for controller to reconcile)
kubectl get authpolicy -n ${MODEL_NS} -l maas.opendatahub.io/model=${MODEL_NAME}-ref

# Wait for AuthPolicy to be enforced (re-run if the controller has not reconciled yet)
AUTH_POLICY=$(kubectl get authpolicy -n ${MODEL_NS} -l maas.opendatahub.io/model=${MODEL_NAME}-ref -o jsonpath='{.items[0].metadata.name}')
[[ -n "$AUTH_POLICY" ]] && kubectl wait --for=condition=Enforced=true authpolicy/${AUTH_POLICY} -n ${MODEL_NS} --timeout=120s
```

**Multiple policies per model**: You can create multiple MaaSAuthPolicies that reference the same model. The controller aggregates them — a user matching any policy gets access.

### 3. Define Subscriptions (MaaSSubscription)

Create a MaaSSubscription to define per-model token rate limits for owner groups:

```yaml
kubectl apply -f - <<EOF
# MaaSSubscription must be deployed to the models-as-a-service namespace
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: free-subscription
  namespace: models-as-a-service
spec:
  owner:
    groups:
      - name: free-users
    users: []
  modelRefs:
    - name: ${MODEL_NAME}-ref
      namespace: ${MODEL_NS}
      tokenRateLimits:
        - limit: 100
          window: 1m
  priority: 10
EOF
```

Verify the MaaSSubscription is active (TokenRateLimitPolicy created and enforced):

```bash
# List generated TokenRateLimitPolicy (may take a moment for controller to reconcile)
kubectl get tokenratelimitpolicy -n ${MODEL_NS} -l maas.opendatahub.io/model=${MODEL_NAME}-ref

# Wait for TokenRateLimitPolicy to be enforced (re-run if the controller has not reconciled yet)
TRLP=$(kubectl get tokenratelimitpolicy -n ${MODEL_NS} -l maas.opendatahub.io/model=${MODEL_NAME}-ref -o jsonpath='{.items[0].metadata.name}')
[[ -n "$TRLP" ]] && kubectl wait --for=condition=Enforced=true tokenratelimitpolicy/${TRLP} -n ${MODEL_NS} --timeout=120s
```

!!! warning "Multiple model references on one HTTPRoute"
    More than one **MaaSModelRef** on the same route can break independent per-subscription limits. Only one **TokenRateLimitPolicy** is fully effective at the gateway. For **MaaSSubscription** readiness, the controller checks each TRLP's **`Accepted`** condition; Kuadrant may still show **`Enforced`** and **`Overridden`** (or similar **`reason`**) when policies conflict on one route.
    
    **Detection:**
    
    List TRLPs that target an HTTPRoute, then inspect **`Accepted`** (controller readiness) and **`Enforced`** (gateway application):
    
    ```bash
    # List TRLPs that target an HTTPRoute (namespace/name → route name)
    kubectl get tokenratelimitpolicy -A -o json | jq -r '.items[] | select(.spec.targetRef.kind=="HTTPRoute") | "\(.metadata.namespace)/\(.metadata.name) → \(.spec.targetRef.name)"' | sort
    
    # Accepted + Enforced condition status per TRLP (needs jq; if this fails, use kubectl describe on each TRLP)
    kubectl get tokenratelimitpolicy -A -o json | jq -r '
      .items[] | select(.spec.targetRef.kind == "HTTPRoute")
      | . as $i
      | (($i.status.conditions // []) | map(select(.type == "Accepted")) | .[0]) as $a
      | (($i.status.conditions // []) | map(select(.type == "Enforced")) | .[0]) as $e
      | [
          $i.metadata.namespace,
          $i.metadata.name,
          $i.spec.targetRef.name,
          (($a // {}) | .status // "?"),
          (($e // {}) | .status // "?"),
          (($e // {}) | .reason // "")
        ] | @tsv'
    ```
    
    **How to recognize it:** Several TRLPs share the same `spec.targetRef.name`. Compare **`Accepted`** (what the MaaS controller uses for subscription readiness) and **`Enforced`** / **`reason`** (for example **`Overridden`**) on each policy—one route may show one TRLP fully effective and others superseded.
    
    **Workarounds:**
    
    - **Dedicated routes per model** — Deploy each model with its own HTTPRoute to ensure independent rate limiting
    - **Shared subscription design** — If models share an **HTTPRoute**, use **one** **MaaSSubscription** that lists every **MaaSModelRef** on that route so you are not applying **different** subscription limits to the same route. The controller may still create **one TRLP per model ref**; **prefer dedicated routes** when each subscription must enforce limits independently until **Tracking** below ships.
    - **Shared route limitations** — If **multiple** **MaaSModelRef** resources target the **same** **HTTPRoute**, you still get **multiple TRLPs**; grouping models on shared routes does **not** avoid this limitation. This pattern is **only** appropriate when **every** model on the route is meant to share **one** **MaaSSubscription** (and access policy)—**not** when different teams or subscriptions each register their own model refs on one route. If you need **separate** subscriptions with **separate** limits on the same route, use **dedicated routes per model**.
    
    **Status:** This limitation **remains in Models-as-a-Service v3.4**. The fix requiring merge strategy support for TokenRateLimitPolicy is not included. Plan your model deployment topology accordingly.
    
    **Tracking:** [opendatahub-io/models-as-a-service#585](https://github.com/opendatahub-io/models-as-a-service/pull/585) proposes the controller change for coexisting token rate limit policies on a shared route.
    
!!! note "Namespace requirements"
    Both **MaaSAuthPolicy** and **MaaSSubscription** must be installed in the `models-as-a-service` namespace. Each `modelRefs` entry must specify the `namespace` where the MaaSModelRef lives (e.g. `llm`).

!!! warning "Using the `users` field"
    The `subjects.users` (MaaSAuthPolicy) and `owner.users` (MaaSSubscription) fields should be used only for **Service Accounts** and similar programmatic identities, not for many individual human users. Having too many distinct users can cause cardinality issues in rate limiting and policy enforcement. Prefer `groups` for human users.

**Premium example** with higher limits:

```yaml
kubectl apply -f - <<EOF
# MaaSSubscription must be deployed to the models-as-a-service namespace
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: premium-subscription
  namespace: models-as-a-service
spec:
  owner:
    groups:
      - name: premium-users
    users: []
  modelRefs:
    - name: ${MODEL_NAME}-ref
      namespace: ${MODEL_NS}
      tokenRateLimits:
        - limit: 100000
          window: 24h
  priority: 20
  tokenMetadata:
    organizationId: "premium-org"
    costCenter: "ai-team"
EOF
```

### 4. Validate the Configuration

**Check CRs and generated policies:**

```bash
kubectl get maasmodelref -n ${MODEL_NS}
kubectl get maasauthpolicy,maassubscription -n models-as-a-service
kubectl get authpolicy,tokenratelimitpolicy -n ${MODEL_NS}
```

**Create groups and add users** (OpenShift example):

```bash
# Create groups if they do not exist
oc adm groups new free-users 2>/dev/null || true
oc adm groups new premium-users 2>/dev/null || true

# Add users to each group
oc adm groups add-users free-users alice@example.com
oc adm groups add-users premium-users bob@example.com
```

**Create API keys and verify access:**

Create an API key as a user in `free-users` and another as a user in `premium-users`. Follow the [Validation](../install/validation.md) guide to:

1. Get the gateway endpoint and create an API key for each user
2. List models and run inference with each API key
3. Test with both groups to confirm access and different rate limits — free-users (100 tokens/min) vs premium-users (100,000 tokens/24h)

## Adding Groups

To grant a user access to a subscription, add them to the appropriate Kubernetes group:

```bash
# Create groups if they do not exist
oc adm groups new free-users 2>/dev/null || true
oc adm groups new premium-users 2>/dev/null || true

# Add users (method depends on your IdP; OpenShift example)
oc adm groups add-users free-users alice@example.com
oc adm groups add-users premium-users bob@example.com
```

Users will get subscription access on their next request (after group membership propagates).

## Multiple Subscriptions per User

When a user belongs to multiple groups that each have a subscription, the access depends on the API key used. A subscription is bound to each API key at minting (explicit or highest priority). See [API Key Management](../user-guide/api-key-management.md).

## Troubleshooting

### 403 Forbidden: "no access to subscription"

**Cause:** User requested a subscription they do not belong to (group membership).

**Fix:** Ensure the user is in a group listed in the subscription's `spec.owner.groups`.

### 429 Too Many Requests

**Cause:** User exceeded token rate limit for the model.

**Fix:** Wait for the rate limit window to reset, or upgrade to a subscription with higher limits.

### Model not appearing in GET /v1/models

**Cause:** MaaSModelRef missing, not reconciled, or access probe failed.

**Fix:**

- Verify MaaSModelRef exists in the model namespace (e.g. `llm`) and has `status.phase: Ready`
- Check MaaSAuthPolicy in `models-as-a-service` includes the user's groups and references the MaaSModelRef with correct `name` (e.g. `${MODEL_NAME}-ref`) and `namespace`
- Ensure MaaSSubscription in `models-as-a-service` exists for the model and user's groups

### Policies not enforced

**Cause:** Kuadrant controller may need to re-sync.

**Fix:**

```bash
kubectl delete pod -l control-plane=controller-manager -n kuadrant-system
kubectl wait --for=condition=Enforced=true tokenratelimitpolicy/<policy-name> -n llm --timeout=2m
```

## Related Documentation

- [Access and Quota Overview](../concepts/subscription-overview.md) — How policies and subscriptions work together
- [Model Reference](../concepts/model-reference.md) — Conceptual overview
- [API Key Management](../user-guide/api-key-management.md)
- [Validation](../install/validation.md)
