# Subscription Configuration

This guide provides step-by-step instructions for configuring and managing subscriptions in the MaaS Platform.

## Prerequisites

- **MaaS platform installed** including the MaaS controller
- **MaaSModelRef** CRD and controller deployed
- **LLMInferenceService** resources for your models (or external model endpoints)
- Cluster admin or equivalent permissions to create CRs in the MaaS namespace

## Configuration Steps

### 1. Register Models (MaaSModelRef)

Create a MaaSModelRef for each model you want to expose through MaaS:

```bash
kubectl apply -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: granite-3b-instruct
  namespace: opendatahub
spec:
  modelRef:
    kind: LLMInferenceService
    name: granite-3b-instruct
    namespace: llm
EOF
```

Verify the controller has reconciled and set the endpoint:

```bash
kubectl get maasmodelref -n opendatahub
# Check status.endpoint and status.phase
```

### 2. Grant Access (MaaSAuthPolicy)

Create an MaaSAuthPolicy to define which groups/users can access which models:

```bash
kubectl apply -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: free-access
  namespace: opendatahub
spec:
  modelRefs:
    - granite-3b-instruct
  subjects:
    groups:
      - name: system:authenticated
    users: []
EOF
```

**Multiple policies per model**: You can create multiple MaaSAuthPolicies that reference the same model. The controller aggregates them — a user matching any policy gets access.

### 3. Define Subscriptions (MaaSSubscription)

Create a MaaSSubscription to define per-model token rate limits for owner groups:

```bash
kubectl apply -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: free-subscription
  namespace: opendatahub
spec:
  owner:
    groups:
      - name: system:authenticated
    users: []
  modelRefs:
    - name: granite-3b-instruct
      tokenRateLimits:
        - limit: 100
          window: 1m
EOF
```

**Premium example** with higher limits:

```bash
kubectl apply -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: premium-subscription
  namespace: opendatahub
spec:
  owner:
    groups:
      - name: premium-users
    users: []
  modelRefs:
    - name: granite-3b-instruct
      tokenRateLimits:
        - limit: 100000
          window: 24h
  tokenMetadata:
    organizationId: "premium-org"
    costCenter: "ai-team"
EOF
```

### 4. Validate the Configuration

**Check CRs and generated policies:**

```bash
kubectl get maasmodelref,maasauthpolicy,maassubscription -n opendatahub
kubectl get authpolicy,tokenratelimitpolicy -n llm
```

**Test model access** (see [Validation](../install/validation.md)):

```bash
# Create an API key
curl -sSk -X POST "${HOST}/maas-api/v1/api-keys" \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -d '{"name": "test-key"}' | jq .

# List models
curl -sSk "${HOST}/maas-api/v1/models" \
  -H "Authorization: Bearer ${API_KEY}" | jq .

# Inference (add X-MaaS-Subscription if user has multiple subscriptions)
curl -sSk -H "Authorization: Bearer ${API_KEY}" \
  -H "Content-Type: application/json" \
  -H "X-MaaS-Subscription: free-subscription" \
  -d '{"model": "granite-3b-instruct", "prompt": "Hello", "max_tokens": 10}' \
  "${MODEL_URL}"
```

## Adding Users to Subscriptions

To grant a user access to a subscription, add them to the appropriate Kubernetes group:

```bash
# Example: Add user to premium-users group (method depends on your IdP)
# For OpenShift/LDAP groups:
oc adm groups add-users premium-users alice@example.com
```

Users will get subscription access on their next request (after group membership propagates).

## Multiple Subscriptions per User

When a user belongs to multiple groups that each have a subscription:

1. **Single subscription** — No `X-MaaS-Subscription` header needed
2. **Multiple subscriptions** — Client **must** send `X-MaaS-Subscription: <subscription-name>` to specify which subscription's rate limits apply

Example for a user in both `system:authenticated` and `premium-users`:

```bash
# Use free subscription limits
curl -H "X-MaaS-Subscription: free-subscription" ...

# Use premium subscription limits
curl -H "X-MaaS-Subscription: premium-subscription" ...
```

## Troubleshooting

### 403 Forbidden: "must specify X-MaaS-Subscription"

**Cause:** User has multiple subscriptions and did not send the header.

**Fix:** Add `X-MaaS-Subscription: <subscription-name>` to the request.

### 403 Forbidden: "no access to subscription"

**Cause:** User requested a subscription they do not belong to (group membership).

**Fix:** Ensure the user is in a group listed in the subscription's `spec.owner.groups`.

### 429 Too Many Requests

**Cause:** User exceeded token rate limit for the model.

**Fix:** Wait for the rate limit window to reset, or upgrade to a subscription with higher limits.

### Model not appearing in GET /v1/models

**Cause:** MaaSModelRef missing, not reconciled, or access probe failed.

**Fix:**

- Verify MaaSModelRef exists and has `status.phase: Ready`
- Check MaaSAuthPolicy includes the user's groups
- Ensure MaaSSubscription exists for the model and user's groups

### Policies not enforced

**Cause:** Kuadrant controller may need to re-sync.

**Fix:**

```bash
kubectl delete pod -l control-plane=controller-manager -n kuadrant-system
kubectl wait --for=condition=Enforced=true tokenratelimitpolicy/<policy-name> -n llm --timeout=2m
```

## Related Documentation

- [Subscription Overview](subscription-overview.md)
- [Subscription Concepts](subscription-concepts.md)
- [Token Management](token-management.md)
- [Validation](../install/validation.md)
