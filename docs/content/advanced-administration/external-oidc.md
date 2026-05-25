# External OIDC Configuration

This guide documents JWKS caching behavior and operator alerting for external OIDC authentication in MaaS.

## Overview

MaaS supports external OIDC identity providers for JWT-based authentication. When configured via `Tenant.spec.externalOIDC`, Authorino validates tokens by:

1. Discovering the OIDC provider's configuration from `{issuerUrl}/.well-known/openid-configuration`
2. Fetching JSON Web Key Sets (JWKS) from the provider's `jwks_uri` endpoint
3. Caching JWKS for the configured refresh interval
4. Validating JWT signatures using cached keys

## Configuration

Add external OIDC settings to the Tenant CR:

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: Tenant
metadata:
  name: default-tenant
  namespace: models-as-a-service
spec:
  externalOIDC:
    issuerUrl: "https://keycloak.example.com/realms/my-realm"
    clientId: maas-client
    ttl: 300  # JWKS refresh interval in seconds
```

For field descriptions and validation rules, see [Tenant CRD - TenantExternalOIDCConfig](../reference/crds/tenant.md#tenantexternaloidcconfig).

## JWKS Caching Behavior

### What TTL Controls

The `ttl` field controls **how often Authorino fetches fresh JWKS** from the IdP when it is available. It does not control cache expiration.

- Default: 300 seconds (5 minutes)
- Minimum: 30 seconds
- Authorino refreshes JWKS at the TTL interval to pick up rotated keys
- Cached validation is faster than fetching on every request

### Behavior During IdP Outages

When the IdP JWKS endpoint becomes unreachable:

- **Authentication continues** using the last successfully cached JWKS
- **Cached keys persist indefinitely** until the IdP recovers
- **Tokens signed with never-cached keys will fail** validation
- **No automatic cache expiration** based on TTL

This design provides resilience during temporary IdP outages but requires the IdP to be available when new keys are introduced.

## Monitoring and Alerts

MaaS deploys PrometheusRules to alert operators when OIDC authentication issues occur. These alerts fire when prolonged JWKS unavailability affects authentication.

**Alerts:**

- **MaaSAuthorinoOIDCAuthenticationHighFailureRate** — fires when >10% of auth attempts fail for 5 minutes
- **MaaSAuthorinoOIDCAuthenticationHighLatency** — fires when P95 latency exceeds 2 seconds for 5 minutes

For alert configuration, remediation steps, and metrics queries, see [OIDC Authentication Alerts](../observability/metrics-and-dashboards.md#oidc-authentication-alerts).

## Troubleshooting

### Authentication fails during IdP outage

**Cause:** JWKS was never cached, or IdP rotated keys during the outage.

**What happens:**

- Tokens signed with cached keys continue working
- Tokens signed with new (never-cached) keys fail with 401 Unauthorized
- The `MaaSAuthorinoOIDCAuthenticationHighFailureRate` alert fires

**Resolution:**

Restore IdP connectivity. Authorino will fetch fresh JWKS on the next TTL interval.

### Authentication fails after Authorino restart

**Cause:** JWKS cache is empty after pod restart.

**Resolution:**

Authorino fetches JWKS on the first authentication request. If IdP is unreachable, restart Authorino after IdP recovery:

```bash
kubectl rollout restart deployment/authorino -n rh-connectivity-link
```

### Tokens fail after key rotation

**Cause:** New key not in Authorino's cache yet.

**Resolution:**

Wait for the next TTL refresh interval, or restart Authorino to force immediate fetch.

## Related Documentation

- [Tenant CRD Reference](../reference/crds/tenant.md)
- [Observability - OIDC Authentication Alerts](../observability/metrics-and-dashboards.md#oidc-authentication-alerts)
- [Authorino JWT Authentication](https://docs.kuadrant.io/latest/authorino/docs/features/#json-web-token-jwt-verification-authenticationjwt)
