# Subscription and Policy Cardinality

MaaSAuthPolicy and MaaSSubscription support both `groups` and `users` in their subject/owner configuration. Using `users` for many individual human users can cause cardinality issues in the rate-limiting and policy enforcement layer (Limitador, Authorino), which may impact performance and scalability.

**Recommendation:** Prefer `groups` for human users. Reserve the `users` field for Service Accounts and other programmatic identities where the number of distinct users remains small.

!!! note "See also"
    For configuration guidance, see [Quota and Access Configuration](../configuration-and-management/quota-and-access-configuration.md).

## TODO

- [ ] Document cardinality limits and observed impact
- [ ] Provide guidance on when `users` is appropriate vs `groups`
- [ ] Add monitoring and troubleshooting notes for cardinality-related issues
