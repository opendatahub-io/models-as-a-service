# Model Access Behavior

This document describes the expected behaviors and operational considerations when modifying model access (subscription) in the MaaS Platform.

## Model Access Changes During Active Usage

### Overview

When a model is removed from a subscription's access list (by updating MaaSAuthPolicy or MaaSSubscription), access revocation takes effect according to how the gateway enforces policies. This section describes the expected behaviors and considerations for administrators.

### How Model Access Removal Works

1. **Policy Update**: The administrator updates MaaSAuthPolicy or MaaSSubscription to remove access to a model
2. **Controller Processing**: The maas-controller reconciles the change and updates AuthPolicy/TokenRateLimitPolicy resources
3. **Gateway Enforcement**: The gateway (via Authorino) enforces the updated policies
4. **Access Revocation**: Users lose access to the model per the new policy

### Expected Behaviors

#### 1. Impact on Active Requests

Access revocation prevents new requests once the gateway has the updated policy.

- **New Requests**: Any request arriving after policy propagation will be denied.
- **In-Flight Requests**: Requests that have already passed the authorization gate typically complete successfully.

#### 2. Policy Propagation Delay

There may be a delay between policy update and gateway enforcement. During this window, access behavior can be inconsistent. Wait 1–2 minutes after policy updates before verifying changes.

#### 3. Model List Visibility vs. Access

The **GET /v1/models** endpoint lists models from MaaSModelRef CRs and **filters by access**: it probes each model's endpoint with the client's **Authorization** header. Only models that return 2xx or 405 are included. After access removal, a model the client can no longer access should not appear in their list.

#### 4. Token Validity vs. Model Access

API keys and tokens are per-identity, not per-model. Token validity and model access are independent. When access to a model is revoked, existing tokens simply have access to fewer models; users do not need to request new tokens.

### Recommended Practices

1. **Plan Access Changes**: Schedule changes during low-usage periods and notify affected users when possible.
2. **Verify Changes**: Wait 1–2 minutes after policy updates, then test access.
3. **Monitor for Issues**: Check maas-controller and gateway logs for policy update errors.

### Related Documentation

- [Subscription Configuration](subscription-configuration.md) - How to configure subscription access
- [Model Setup](model-setup.md) - How to configure models for MaaS
- [Token Management](token-management.md) - Understanding token lifecycle
