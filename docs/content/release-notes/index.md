# Release Notes

## v3.4.0

### Major Changes

Version 3.4.0 introduces new CRDs and API resources that are not compatible with previous versions. All MaaS custom resources (`MaaSModelRef`, `MaaSAuthPolicy`, `MaaSSubscription`, `ExternalModel`) are new in this release.

**Migration:** See the overall migration plan for detailed upgrade instructions from previous versions.

### Tenant CR

The **`Tenant`** CR (`maas.opendatahub.io/v1alpha1`) is the platform configuration object for MaaS. It is self-bootstrapped by `maas-controller` on startup as `default-tenant` in the `models-as-a-service` namespace. Optional `spec` fields (`gatewayRef`, `apiKeys`, `telemetry`, `externalOIDC`) allow customizing gateway, API key lifetime, telemetry, and external OIDC. See [Tenant CR](../install/maas-setup.md#tenant-cr) for details.

### Known limitations

- **Shared HTTPRoute and token rate limits:** Multiple **MaaSModelRef** resources on the same **HTTPRoute** can yield multiple **TokenRateLimitPolicy** objects, but **only one limit set may be enforced** at the gateway until the controller change in [opendatahub-io/models-as-a-service#585](https://github.com/opendatahub-io/models-as-a-service/pull/585) is in your build. See [Subscription limitations and known issues](../configuration-and-management/subscription-known-issues.md#token-rate-limits-when-multiple-model-references-share-one-httproute).

---

## v0.1.0

*Initial release.*

<!-- Add release notes for v0.1.0 here -->
