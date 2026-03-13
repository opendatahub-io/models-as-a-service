# Access Configuration Overview

Access control in MaaS is configured via **MaaSAuthPolicy**. A policy defines **who** (subjects: groups/users) can access **which models** (by MaaSModelRef name). Users must match a policy's subjects to pass the access gate; they also need a matching subscription for quota.

## Key Concepts

- **modelRefs** — List of model names (MaaSModelRef `metadata.name`) this policy grants access to
- **subjects** — Groups and/or users; **OR logic** — any match grants access
- **Multiple policies per model** — You can create multiple MaaSAuthPolicies that reference the same model. The controller aggregates them; a user matching any policy gets access.

## Related Documentation

For detailed configuration steps and examples, see [MaaSAuthPolicy Configuration](maas-auth-policy-configuration.md).
