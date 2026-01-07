# Tier Configuration

This guide provides step-by-step instructions for configuring and managing tiers in the MaaS Platform.

## Configuration Steps

### 1. Configure Tier Mapping

Update `tier-to-group-mapping` ConfigMap:

To add a new tier, save the current ConfigMap, modify it, and reapply:

```bash
# 1. Edit ConfigMap (use example below as a guide)
kubectl edit configmap tier-to-group-mapping -n maas-api

# Example: Add this tier entry to the end of the tiers list:
#   - name: stier
#     description: S tier user
#     level: 99
#     groups:
#     - fox
```

Verify the updated ConfigMap:

```bash
kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: tier-to-group-mapping
  namespace: maas-api
data:
  tiers: |
    - name: free
      description: Free tier for basic users
      level: 1
      groups:
      - system:authenticated
    - name: premium
      description: Premium tier
      level: 10
      groups:
      - premium-users
    - name: enterprise
      description: Enterprise tier
      level: 20
      groups:
      - enterprise-users
EOF
```

Restart the MaaS API to pick up the new configuration:

```bash
kubectl rollout restart deployment/maas-api -n maas-api
```

!!!Note "Adding Users to Tiers"
    To add a user to a tier, add them to the appropriate Kubernetes group. For example, to add a user to the `fox` group (which maps to the `stier` tier in this example):

    ```bash
    # Add a user to the fox group
    kubectl patch group fox -p '{"users": ["username"]}' --type merge
    ```

    Replace `username` with the actual username. Users will automatically be assigned to the tier when they request a new token.

### 2. Configure Tier Access

Grant tier-specific access to models by annotating the `LLMInferenceService` resource with the `alpha.maas.opendatahub.io/tiers` annotation:

```bash
kubectl annotate llminferenceservice <model-name> -n llm \
  alpha.maas.opendatahub.io/tiers='["stier","premium","enterprise"]' \
  --overwrite
```

**Annotation Behavior:**

- **List of tier names**: Grant access to specific tiers (e.g., `["stier","premium","enterprise"]`)
- **Empty list `[]`**: Grant access to **all** tiers
- **Missing annotation**: **No** tiers have access by default

**Example - Grant access to stier and premium tiers:**

```bash
kubectl annotate llminferenceservice qwen3 -n llm \
  alpha.maas.opendatahub.io/tiers='["stier","premium"]' \
  --overwrite
```

This annotation automatically sets up the necessary RBAC (Role and RoleBinding) for the specified tiers to access the model via MaaS tokens.

!!!Note "Manual RBAC Setup"
    For reference, here's what the automatic RBAC setup looks like behind the scenes if you need to configure access manually:

    ```yaml
    ---
    apiVersion: rbac.authorization.k8s.io/v1
    kind: Role
    metadata:
      name: model-post-access
      namespace: <model-namespace>
    rules:
      - apiGroups: ["serving.kserve.io"]
        resources: ["llminferenceservices"]
        verbs: ["post"]
    ---
    apiVersion: rbac.authorization.k8s.io/v1
    kind: RoleBinding
    metadata:
      name: model-post-access-tier-binding
      namespace: <model-namespace>
    subjects:
      - kind: Group
        name: system:serviceaccounts:maas-default-gateway-tier-<tier>
        apiGroup: rbac.authorization.k8s.io
    roleRef:
      kind: Role
      name: model-post-access
      apiGroup: rbac.authorization.k8s.io
    ```

!!!info "Why the custom `post` verb?"
    We intentionally use a custom verb (`post`) instead of standard Kubernetes verbs like `get` or `create`. This is the **only** RBAC permission required for model access. By using a non-standard verb that doesn't exist in Kubernetes' built-in authorization, we minimize the security surface - these service accounts cannot accidentally read, modify, or delete any cluster resources.

### 3. Configure Rate Limiting

Add tier-specific rate limits by patching the existing `gateway-token-rate-limits` TokenRateLimitPolicy:

```bash
kubectl patch tokenratelimitpolicy gateway-token-rate-limits -n openshift-ingress --type merge --patch-file=/dev/stdin <<'EOF'
spec:
  limits:
    stier-user-tokens: # 1
      rates:
        - limit: 999 # 2
          window: 1m # 3
      when:
        - predicate: auth.identity.tier == "stier" # 4
      counters:
        - expression: auth.identity.userid # 5
EOF
```

**Rate Limit Policy Configuration Explained:**

1. **Tier definition** - Each tier (free, premium, enterprise) gets its own configuration block (this is just a naming convention, it is not used for the actual tier resolution)
2. **Token limit** - Maximum number of total tokens allowed per time window
3. **Time window** - Duration after which the request counter resets
4. **Predicate condition** - Determines when this tier's limits apply based on user authentication
5. **Counter expression** - Tracks token consumption per user ID (globally)

!!!Warning "Important"
    The predicate condition (not the Tier Definition) is used to determine when this tier's limits apply based on user authentication. It is a CEL expression that is evaluated by the Authorino policy engine.

Validate the TokenRateLimitPolicy has been updated and enforced:

```bash
# Delete the Kuadrant operator pod to trigger a re-sync
kubectl delete pod -l control-plane=controller-manager -n kuadrant-system

# Wait for the TokenRateLimitPolicy to be enforced
kubectl wait --for=condition=Enforced=true tokenratelimitpolicy/gateway-token-rate-limits -n openshift-ingress --timeout=2m
```

### 4. Validate the Configuration

Configuration can be validated by logging in as a user belonging to the appropriate group and running through the manual validation steps in the [deployment scripts documentation](../install/validation.md), or by using the automated validation script.

```bash
# Validate the configuration with 20 requests and a max tokens limit of 500
./scripts/validate-deployment.sh --rate-limit-requests 20 --max-tokens 500
```

**Example Output:**

```bash
🔍 Checking: Token information
ℹ️  Token subject: system:serviceaccount:maas-default-gateway-tier-stier:jland-78028f6d
✅ PASS: User tier: stier <--- Important
🔍 Checking: Models endpoint
✅ PASS: Models endpoint returns 200 OK
...
🔍 Checking: Rate limiting
ℹ️  Sending 20 rapid requests to test rate limiting...
✅ PASS: Rate limiting is working (5 successful, 15 rate limited) <--- Important
```

## Troubleshooting

### General Tips

**Authentication errors (403/401):**
Check Authorino logs for detailed error messages:

```bash
kubectl logs -n openshift-ingress -l app.kubernetes.io/name=authorino --tail=50
```

**Token retrieval issues:**
Check MaaS API logs during the token request:

```bash
kubectl logs -n maas-api -l app=maas-api --tail=50
```

**Policy enforcement issues:**
Restart the Kuadrant operator to trigger policy re-sync:

```bash
kubectl delete pod -l control-plane=controller-manager -n kuadrant-system
```

### Common Issues

#### 403 Forbidden: "not authorized: unknown reason"

**Possible Cause:** Added new tier to ConfigMap but didn't update `gateway-token-rate-limits` TokenRateLimitPolicy.

**Fix:** Validate/Update the TokenRateLimitPolicy as documented in [Configure Rate Limiting](#3-configure-rate-limiting), then restart the Kuadrant operator:

```bash
kubectl patch tokenratelimitpolicy gateway-token-rate-limits -n openshift-ingress --type merge --patch-file=/dev/stdin <<'EOF'
spec:
  limits:
    <tier-name>-user-tokens:
      rates:
        - limit: 999
          window: 1m
      when:
        - predicate: auth.identity.tier == "<tier-name>"
      counters:
        - expression: auth.identity.userid
EOF

kubectl delete pod -l control-plane=controller-manager -n kuadrant-system
```

!!!Warning "Modifying Tiers During Active Usage"
    Modifying the tier definitions (ConfigMap) while users have active requests may cause side effects due to caching and eventual consistency. See [Tier Modification Known Issues](./tier-modification-known-issues.md) for details on:

    - Propagation delays for group changes
    - Tier name immutability
    - Monitoring inconsistencies
    - Service interruptions on tier deletion

!!!Warning "Removing Group Membership During Active Usage"
    Removing a user from a group while they have active tokens may not immediately revoke access. See [Group Membership Known Issues](./group-membership-known-issues.md) for details on:

    - Existing tokens remaining valid until expiration
    - Rate limiting continuing at the old tier
    - Service Account persistence after group removal
    - Recommended practices for group membership changes

!!!info "Model Tier Access Changes"
    Removing a model from a tier's access list (by updating the `alpha.maas.opendatahub.io/tiers` annotation) takes effect immediately. See [Model Tier Access Behavior](./model-access-behavior.md#model-tier-access-changes-during-active-usage) for details on:

    - Expected behaviors when access is revoked
    - RBAC propagation timing
    - Recommended practices for tier access changes
