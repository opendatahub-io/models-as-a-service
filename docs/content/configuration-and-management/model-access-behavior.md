# Model Tier Access Behavior

This document describes the expected behaviors and operational considerations when modifying model tier access in the MaaS Platform Technical Preview release.

## Model Tier Access Changes During Active Usage

### Overview

When a model is removed from a tier's access list (by updating the `alpha.maas.opendatahub.io/tiers` annotation on an `LLMInferenceService` resource), access revocation takes effect immediately. This section describes the expected behaviors and considerations for administrators.

### How Model Access Removal Works

1. **Annotation Update**: The administrator updates the `alpha.maas.opendatahub.io/tiers` annotation to remove a tier from the allowed list
2. **ODH Controller Processing**: The ODH Controller detects the annotation change and updates RBAC resources
3. **RBAC Update**: The RoleBinding for the removed tier is deleted, revoking POST permissions for that tier's service accounts
4. **Access Revocation**: Users from the removed tier lose access to the model

### Expected Behaviors

#### 1. Impact on Active Requests

Access revocation prevents new requests immediately.

**Description**:

- **New Requests**: Any request arriving after the RBAC update will be denied immediately.
- **In-Flight Requests**: Requests that have already passed the authorization gate typically complete successfully. However, dependent requests or long-running sessions requiring re-authorization will fail.

**Example Scenario**:

```text
1. User starts a long-running inference request (e.g., 2-minute generation)
2. Administrator removes the tier from model annotation at 30 seconds
3. ODH Controller updates RBAC at 45 seconds
4. Request may fail at next authorization checkpoint (if any)
```

**Workaround**:

- Avoid removing tier access during peak usage periods
- Monitor active requests before making changes
- Consider using maintenance windows for tier access changes

#### 2. RBAC Propagation Delay

**Description**:

- There is a delay between annotation update and RBAC resource update by the ODH Controller
- During this window (typically seconds to minutes), access behavior is inconsistent:
  - Some requests may still succeed (if authorization was cached)
  - New requests may fail immediately
  - Model may still appear in user's model list but be inaccessible

**Example Timeline**:

```text
T+0s:  Annotation updated (remove "premium" tier)
T+5s:  ODH Controller detects change
T+10s: RoleBinding deleted
T+15s: RBAC fully propagated to API server
```

**Workaround**:

- Wait 1-2 minutes after annotation update before verifying access changes
- Monitor ODH Controller logs to confirm RBAC updates are complete
- Use `kubectl get rolebinding -n <model-namespace>` to verify RoleBinding removal

#### 3. Model List Visibility vs. Access Mismatch

**Description**:

- The `/v1/models` endpoint lists all models that are part of the MaaS instance (via gateway references)
- The endpoint does not filter models by tier access permissions
- Users may see models in the list that they can no longer access after tier removal
- Attempts to use these models will fail with `403 Forbidden` or `401 Unauthorized`

**Example**:

```json
// GET /v1/models returns:
{
  "data": [
    {"id": "model-a", "ready": true},  // Still accessible
    {"id": "model-b", "ready": true}   // No longer accessible after tier removal
  ]
}

// POST to model-b fails with 403
```

**Workaround**:

This behavior will be resolved in a future release where the model list is filtered by tier permissions (see [PR #294](https://github.com/opendatahub-io/models-as-a-service/pull/294)). In the meantime, clients should expect potential `403 Forbidden` errors if attempting to access models that appear in the list but are not permitted.

#### 4. Token Validity vs. Model Access (Expected Behavior)

Tokens are per-user (Service Account), not per-model. Token validity and model access are independent—this is by design.

**Description**:

- Service Account tokens issued before tier removal remain valid until expiration
- Model access is controlled by RBAC, which is updated independently of token validity
- When a model is removed from a tier, the RBAC change revokes access immediately
- Users do not need to request new tokens; their existing tokens simply have access to fewer models

**Example**:

```text
1. User receives token at T+0 (valid for 1 hour)
2. User has access to models A, B, C (via RBAC)
3. Model B removed from tier at T+30min (RBAC updated)
4. Token still valid, but model access changes:
   - Model A: ✅ Accessible (RBAC allows)
   - Model B: ❌ No longer accessible (RBAC denies)
   - Model C: ✅ Accessible (RBAC allows)
```

**User Communication**:

- Clearly message users when a model is being removed from a tier to set expectations regarding token validity vs. model access.

#### 5. Immediate Access Revocation

**Description**:

- The platform does not provide a "drain" mechanism to allow existing users to finish their sessions while blocking new ones.
- Revocation applies to the authorization policy immediately.
- While in-flight requests often complete (as they have passed the gate), the user experience is an immediate loss of access for any subsequent interaction.

**Workaround**:

- Monitor active requests before making changes:

  ```bash
  # Check for active connections (example)
  kubectl top pods -n <model-namespace>
  ```

- Use maintenance windows for tier access changes
- Consider implementing request draining in future releases

### Recommended Practices

1. **Plan Tier Access Changes**:
   - Schedule changes during low-usage periods
   - Notify affected users in advance when possible
   - Monitor active requests before making changes

2. **Verify Changes**:

   - Wait 1-2 minutes after annotation update
   - Verify RoleBinding removal:

     ```bash
     kubectl get rolebinding -n <model-namespace> | grep <tier-name>
     ```

   - Test access with a token from the affected tier

3. **Monitor for Issues**:
   - Check ODH Controller logs for RBAC update errors
   - Monitor API server logs for authorization failures
   - Watch for increased error rates in user applications

4. **Handle Errors Gracefully**:
   - Implement retry logic with exponential backoff
   - Provide clear error messages to end users
   - Log access denials for troubleshooting

### Future Enhancements

The following improvements are planned for future releases:

1. **Graceful Shutdown**: Implement request draining before access revocation
2. **Model List Filtering**: Filter `/v1/models` by tier permissions
3. **Real-time Notifications**: Notify users when tier access changes
4. **Audit Logging**: Enhanced logging for tier access changes

### Related Documentation

- [Tier Configuration](./tier-configuration.md) - How to configure tier access
- [Model Setup](./model-setup.md) - How to configure model tier annotations
- [Token Management](./token-management.md) - Understanding token lifecycle
