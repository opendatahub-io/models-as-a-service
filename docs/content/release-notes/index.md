# Release Notes

Release notes summarize user-visible changes, breaking changes, and migration requirements for each MaaS version.

## RHOAI to MaaS Release Mapping

This table maps each supported Red Hat OpenShift AI (RHOAI) release to the corresponding MaaS component version.

| RHOAI Version | MaaS Version | RHOAI Image Tag | Status | Notes |
|---------------|--------------|-----------------|--------|-------|
| 3.4           | v0.1.1       | `v3.4`          | GA     | Subscription-based access; `Tenant` CR; see [Upgrade Guide](../migration/upgrade-to-3.4.md) |
| 3.3           | v0.0.2       | `v3.3`          | Tech Preview | `ModelsAsService` CR added to DSC; operator-managed deployment |
| 3.2           | v0.0.2       | `v3.2`          | Tech Preview | Tier-based access; standalone deploy (`modelsAsService` not in DSC schema) |

**Image registries:**

- **Upstream (ODH):** `quay.io/opendatahub/maas-api:<tag>`, `quay.io/opendatahub/maas-controller:<tag>`
- **Downstream (RHOAI):** `registry.redhat.io/rhoai/odh-maas-api-rhel9:<tag>`, `registry.redhat.io/rhoai/odh-maas-controller-rhel9:<tag>`

For dependency version requirements (OCP, Kuadrant/RHCL, Gateway API), see [Version Compatibility](../install/prerequisites.md#version-compatibility).

---

## v0.1.2

**Release Date:** TBD

### Breaking Changes

**Gateway-level AuthPolicy (RHOAIENG-62571)**
- The `maas-controller` now creates a single `AuthPolicy/maas-gateway-auth` in the gateway namespace (`openshift-ingress`) instead of one per-model `AuthPolicy` in each model namespace.
- Per-model `AuthPolicy` objects managed by the controller are deleted on the first reconcile after upgrade.
- `status.authPolicies` now references `maas-gateway-auth / openshift-ingress` instead of per-model policy names.
- New admission webhooks (`failurePolicy=Ignore`) validate that `MaaSAuthPolicy` and `MaaSSubscription` are created in namespaces that contain a `MaasTenantConfig` CR.
- `AITenant` created outside the configured `--aitenant-namespace` are now rejected at admission instead of being accepted and later marked `Failed/InvalidPlacement` by the controller.
- `AITenant.spec.rbac` is deprecated and ignored. Existing manifests that still include it remain schema-valid, but the controller no longer creates RoleBindings from it. The controller still creates tenant-admin Roles, and platform administrators must create standard Kubernetes RoleBindings to grant access. See [Tenant RBAC](../configuration-and-management/tenant-rbac.md).
- **Minimum Kuadrant version:** v1.4.2 or later required for `spec.defaults.rules` support.
- **End-user auth behavior is unchanged** — valid API key + active subscription + allowed group still returns `200`.

### New Features

**Singleton gateway AuthPolicy**
- All per-model allowlists aggregated into one CEL expression in `maas-gateway-auth`
- Dynamic model identity extracted from `X-Gateway-Model-Name` header with `request.path` fallback
- Model-aware cache keys prevent subscription result pollution across models
- Response header injection: `X-MaaS-Subscription`, `userId`, `username`, `groups`

**Admission webhooks**
- Validating webhooks for `MaaSAuthPolicy` and `MaaSSubscription` enforce namespace tenancy requirements

### Known Limitations

- **`tenant-gateway-isolation` rule is a stub.** The gateway policy includes an always-allow placeholder for multi-gateway tenant isolation. This will be replaced with a real hostname check in a future release.

---

## v0.1.1

**Release Date:** 2026-05-01

### Breaking Changes

**Required `spec` field for MaaS CRs**
- `MaaSAuthPolicy`, `MaaSSubscription`, and `MaaSModelRef` now require the `spec` field
- CRs without `spec` are marked as `Invalid` and new CRs without `spec` are blocked
- Tenant.Spec remains optional
- **Migration:** Add a `spec` field to existing `MaaSAuthPolicy`, `MaaSSubscription`, and `MaaSModelRef` CRs that lack one (e.g., add `spec: {}` if needed)

### New Features

**MaasTenantConfig CR**
- MaaS runtime configuration centralized in the `MaasTenantConfig` CR (`maas.opendatahub.io/v1alpha1`)
- Auto-bootstrapped as `default-tenant` in `models-as-a-service` namespace
- Configurable API key lifetime and telemetry via `spec` fields
- See [MaasTenantConfig CR Configuration](../install/maas-setup.md#maastenantconfig-cr)

**Observability**
- Perses dashboards for model usage visualization
- Admin usage dashboard for token consumption tracking
- ServiceMonitor for maas-controller metrics

**OIDC Enhancements**
- OIDC token support for `/v1/models` endpoint
- Configurable cluster audience via `--cluster-audience` flag

**External Models**
- External models (introduced in v0.1.0) now included in `/v1/models` listings
- Namespace prefix added to HTTPRoute paths for LLMInferenceService parity

### Key Improvements

- Fail-close logic when Limitador is unavailable prevents rate limit bypass
- Degraded/failed subscriptions rejected at auth layer
- Token rate limit validation aligned with Kuadrant TokenRateLimitPolicy windows
- Terminating namespace handling during RHOAI upgrades
- Local Kind deployment support for development

### Known Limitations

- **Shared HTTPRoute token rate limits:** Multiple `MaaSModelRef` resources on the same `HTTPRoute` create multiple `TokenRateLimitPolicy` objects, but only one may be enforced at the gateway. See [Quota and Access Configuration](../configuration-and-management/quota-and-access-configuration.md) for workarounds.

[Full Changelog](https://github.com/opendatahub-io/models-as-a-service/compare/v0.1.0...v0.1.1)

---

## v0.1.0

**Release Date:** 2026-04-01

### Breaking Changes

**Subscription-based access model**
- Legacy tier-based access control (ConfigMap `tier-to-group-mapping`) fully removed
- All deployments must use subscription CRDs: `MaaSModelRef`, `MaaSAuthPolicy`, `MaaSSubscription`
- **Migration:** See [Migration Guide: Tier-Based to Subscription Model](../migration/tier-to-subscription.md)

**CRD Changes**
- `MaaSModel` renamed to `MaaSModelRef`
- New required CRDs: `MaaSSubscription`, `MaaSAuthPolicy`, `ExternalModel`
- Namespace scoping: MaaS API watches a configurable namespace for subscriptions

**Required `tokenRateLimits` field**
- All `MaaSSubscription` resources must include inline `tokenRateLimits` specification
- The `tokenRateLimitRef` field has been removed
- **Migration:** See [Migration Guide: Tier-Based to Subscription Model](../migration/tier-to-subscription.md) for subscription examples with inline rate limits

### New Features

**Authentication & Authorization**
- API key management: create, revoke, set expiration
- Ephemeral API keys with cleanup CronJob
- Salt-based encryption for API key hashing
- OIDC authentication integration with maas-api AuthPolicy
- RBAC aggregation for namespace users

**Model Management**
- New `ExternalModel` CRD for external model support with Istio-based egress routing
- `/v1/models` endpoint returns available models with subscription info
- `/v1/subscriptions` endpoint for subscription management
- Support for Vertex AI (Gemini) API translation

**Rate Limiting & Quotas**
- Token-based rate limiting via `tokenRateLimits` specification
- Integration with Kuadrant TokenRateLimitPolicy
- Configurable Authorino caching for AuthPolicy evaluators

**Operations**
- FIPS compliance enabled
- Auto-create `models-as-a-service` namespace on controller startup
- Multi-arch image builds
- Subscription flow E2E tests with group support

[Full Changelog](https://github.com/opendatahub-io/models-as-a-service/compare/v0.0.2...v0.1.0)

---

## v0.0.2

**Release Date:** 2026-01-22

### New Features

**Security**
- End-to-end TLS for external API traffic
- NetworkPolicy to allow Authorino access to MaaS components

**Deployment**
- Updated deploy script for new RHOAI Operator flow
- Centralized maas-api image substitution
- Flexible CSV version checking and dynamic deployment discovery

**API**
- Fixed model listing authorization to target actual API endpoint
- Corrected authorization checks for proper JWT validation

**Operations**
- GitHub Release Action automation
- Installation documentation for ODH-based deployments
- Kustomize component handling in manifest validation

[Full Changelog](https://github.com/opendatahub-io/models-as-a-service/compare/0.0.1...v0.0.2)

---

## 0.0.1

**Release Date:** 2025-11-24

*Initial release*
