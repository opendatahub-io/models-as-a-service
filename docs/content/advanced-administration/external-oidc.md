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

Two PrometheusRule alerts monitor gateway authentication health. They cover all auth methods (OIDC/JWT, API key, TokenReview) because Authorino's `auth_server_authconfig_response_status` does not carry evaluator-level labels. They are deployed by `scripts/observability/install-observability.sh` (not by the kustomize base).

| Alert | Condition | Severity |
|-------|-----------|----------|
| `MaaSAuthorinoAuthenticationHighFailureRate` | >10% of auth attempts return `UNAUTHENTICATED` over 5m | warning |
| `MaaSAuthorinoAuthenticationHighLatency` | P95 auth latency >2s over 5m | warning |

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

## Security controls and responsibilities

This table covers the non-functional security requirements (NFRs) raised during GA refinement. Each row states who enforces the control and what an operator needs to know.

| NFR | Enforced by | Details |
|-----|-------------|---------|
| **JWKS cache policy** | MaaS controller + Authorino | `ttl` is propagated from the Tenant CR to Authorino's `jwt.ttl` field. Minimum 30 s enforced server-side in the controller (independent of CRD admission validation). See [JWKS Cache TTL](#jwks-cache-ttl) above. |
| **Client secret handling** | Not applicable | MaaS external OIDC uses **JWT bearer validation only**. Tokens are validated against the IdP's public JWKS. No `client_secret` is required or stored at runtime. The `clientId` field in the Tenant CR is configuration, not a credential. |
| **OAuth client binding (`azp` claim)** | MaaS controller (gateway AuthPolicy) | The generated `maas-gateway-auth` AuthPolicy includes an `oidc-client-bound` rule that checks `auth.identity.azp == clientId`. Tokens issued to a different OAuth client are rejected with 403, even if they share the same issuer. An `has(auth.identity.azp)` guard ensures OpenShift TokenReview identities (which carry no `azp` claim) are unaffected. |
| **SSRF on issuer URL** | CRD validation + Authorino HTTP client | The CRD enforces `^https://\S+$` on `issuerUrl`, blocking plain HTTP and empty URLs at admission time. Blocking of private IPs and loopback addresses in JWKS fetch is the responsibility of the Authorino HTTP client (outside MaaS). Operators in restricted environments should use network policies to constrain Authorino's egress. |
| **Rate limiting on auth paths** | Kuadrant TokenRateLimitPolicy (model inference only) | The `gateway-default-deny` TRLP enforces a 0-token default on model inference routes and **explicitly excludes** `/maas-api` management paths. Authorino's internal JWKS refresh is not exposed as an external endpoint and is not subject to per-request rate limiting. If high-volume IdP calls are a concern, increase `ttl` to reduce refresh frequency. |
| **Authentication audit trail** | Authorino metrics + PrometheusRule alerts | `auth_server_authconfig_response_status` provides aggregate counts by status (`OK`, `UNAUTHENTICATED`, `UNAUTHORIZED`). The `MaaSAuthorinoAuthenticationHighFailureRate` alert fires on sustained failure rates above 10%. Per-request auth decision logging requires capturing Authorino's structured log output at debug/info level outside MaaS. |

### OAuth2 Client Credentials — out of scope

The **OAuth2 Client Credentials** (`grant_type=client_credentials`) flow is **not supported** in this GA release. MaaS external OIDC targets interactive/bearer JWT flows where end users authenticate against their corporate IdP and present access tokens. Machine-to-machine use cases should use [API keys](../concepts/api-key-authentication.md) or a broker IdP that exchanges client credentials for bearer tokens compatible with the OIDC JWT path.
