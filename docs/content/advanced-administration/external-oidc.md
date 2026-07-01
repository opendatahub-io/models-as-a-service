# External OIDC Configuration

Configure an external OIDC identity provider (e.g., Keycloak, Entra ID) for token-based authentication alongside OpenShift TokenReview and API keys.

!!! info "Tech Preview"
    OIDC JWT validation is optional alongside `kubernetesTokenReview`. Model routes rely on API-key auth; the typical flow is authenticate at `maas-api`, mint an API key, then use that key for discovery and inference.

## JWKS Cache TTL

Authorino validates OIDC tokens by fetching the IdP's JWKS (JSON Web Key Set). The `ttl` field controls how long Authorino caches the key set before re-fetching.

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: Tenant
metadata:
  name: default-tenant
spec:
  externalOIDC:
    issuerUrl: "https://keycloak.example.com/realms/maas"
    clientId: maas-api
    ttl: 300  # seconds (default)
```

| Field | Default | Minimum | Description |
|-------|---------|---------|-------------|
| `ttl` | 300 | 30 | JWKS cache duration in seconds. CRD validation enforces the minimum. |

**Choosing a TTL value:**

- **Lower TTL** (30-60s): faster key rotation propagation, more frequent JWKS fetches.
- **Default TTL** (300s): balanced for most deployments.
- **Higher TTL** (600-3600s): reduced load on the IdP, but key rotations take longer to propagate.

### IdP Outage Behavior

When the IdP becomes unreachable:

- Authorino continues using the **last successfully cached JWKS** indefinitely. Existing tokens signed with cached keys keep working.
- The `ttl` controls refresh frequency, not cache expiration. Authorino does not evict cached keys on TTL expiry if the refresh fails.
- Tokens signed with keys that were **never cached** (e.g., a key added to the IdP after the last successful fetch) will fail validation until the IdP is reachable again.

### Multi-Tenant TTL

In multi-tenant deployments, each tenant configures TTL independently via its own Tenant CR. The controller applies the per-tenant TTL to that tenant's gateway-level AuthPolicy:

```yaml
# Tenant in ai-tenant-team-a namespace
apiVersion: maas.opendatahub.io/v1alpha1
kind: Tenant
metadata:
  name: default-tenant
  namespace: ai-tenant-team-a
spec:
  externalOIDC:
    issuerUrl: "https://keycloak.example.com/realms/team-a"
    clientId: team-a-client
    ttl: 60  # team-a uses aggressive refresh
```

See [Tenant CRD reference](../reference/crds/tenant.md) for all fields.

## Monitoring

Two PrometheusRule alerts monitor OIDC authentication health. They are deployed by `scripts/observability/install-observability.sh` (not by the kustomize base).

| Alert | Condition | Severity |
|-------|-----------|----------|
| `MaaSAuthorinoOIDCAuthenticationHighFailureRate` | >10% of auth attempts return `UNAUTHENTICATED` over 5m | warning |
| `MaaSAuthorinoOIDCAuthenticationHighLatency` | P95 auth latency >2s over 5m | warning |

**Common causes of high failure rate:**

- IdP (Keycloak/OIDC provider) is down or unreachable
- JWKS endpoint unreachable (network policy, DNS)
- Expired or revoked tokens in client applications
- Incorrect `clientId` in the Tenant CR

**Common causes of high latency:**

- Slow IdP response times
- Network latency to JWKS endpoint
- Consider increasing `ttl` if the IdP is slow but reliable

See [Metrics & Dashboards](../observability/metrics-and-dashboards.md) for all Authorino metrics.
