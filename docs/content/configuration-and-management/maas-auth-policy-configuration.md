# MaaSAuthPolicy Configuration

MaaSAuthPolicy defines **who** (OIDC subjects/groups/users) can access **which models**. It creates Kuadrant AuthPolicies that validate API keys via the MaaS API callback and perform subscription selection.

## Example

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: data-science-access
  namespace: opendatahub
spec:
  modelRefs:
    - granite-3b-instruct
    - gpt-4-turbo
  subjects:
    groups:
      - name: data-science-team
    users:
      - name: service-account-a
```

## Fields

| Field | Description |
|-------|-------------|
| **modelRefs** | List of model names (MaaSModelRef `metadata.name`) this policy grants access to |
| **subjects** | Groups and/or users; **OR logic** — any match grants access |
| **meteringMetadata** | Optional billing and tracking labels |

## Multiple Policies per Model

You can create multiple MaaSAuthPolicies that reference the same model. The controller aggregates them — a user matching any policy gets access.

## Related Documentation

- [Access Configuration Overview](access-configuration-overview.md) — High-level access concepts
- [Access and Quota Overview](subscription-overview.md) — How policies and subscriptions work together
- [MaaSAuthPolicy CRD](../reference/crds/maas-auth-policy.md) — Full CRD schema reference
