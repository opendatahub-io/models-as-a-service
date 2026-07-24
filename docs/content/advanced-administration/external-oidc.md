# External OIDC Configuration

Configure an external OIDC identity provider (e.g., Keycloak, Entra ID) for token-based authentication alongside OpenShift TokenReview and API keys.

!!! info "Tech Preview"
    OIDC JWT validation is optional alongside `kubernetesTokenReview`. Model routes rely on API-key auth; the typical flow is authenticate at `maas-api`, mint an API key, then use that key for discovery and inference.

!!! note "Dashboard limitation"
    External OIDC users create API keys through the MaaS API (`curl` or other HTTP clients), not through the RHOAI Dashboard. The Dashboard path uses OpenShift OAuth (see [Authentication Modes](../concepts/auth-modes.md)).

## Configure OIDC

For **AITenant-managed tenants** (including the default tenant), set OIDC on the `AITenant` CR in the infrastructure namespace (`ai-tenants` by default):

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: AITenant
metadata:
  name: models-as-a-service   # or your tenant name for additional tenants
  namespace: ai-tenants
spec:
  oidc:
    issuerUrl: "https://keycloak.example.com/realms/maas"
    clientId: maas-api
    ttl: 300  # optional: JWKS cache TTL in seconds (default 300, minimum 30)
```

| Field | Required | Description |
|-------|----------|-------------|
| `issuerUrl` | Yes | OIDC issuer URL (must be HTTPS). Must serve `/.well-known/openid-configuration`. |
| `clientId` | Yes | OAuth2 client ID. Incoming tokens must carry an `azp` claim matching this value. |
| `ttl` | No | JWKS cache duration in seconds (default `300`, minimum `30`). |

The controller propagates this configuration to the tenant's gateway-level `maas-gateway-auth` AuthPolicy in the gateway namespace. OIDC is **not** configured by patching route-level auth policies.

For **legacy unmanaged tenants** (not backed by an `AITenant`), configure `Tenant.spec.externalOIDC` with the same field semantics. During upgrade, existing `Tenant/default-tenant.spec.externalOIDC` values are migrated to `AITenant/models-as-a-service.spec.oidc` automatically.

See [Authentication Modes](../concepts/auth-modes.md) for the full field alignment between `AITenant.spec.oidc` and `Tenant.spec.externalOIDC`, and [AITenant CRD](../reference/crds/ai-tenant.md) for the authoritative schema.

### Multi-tenant OIDC

Each `AITenant` can reference its own Gateway and OIDC provider. Tokens from one tenant's IdP are validated only against that tenant's gateway AuthPolicy — a token issued by tenant A's provider must not authenticate on tenant B's gateway.

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: AITenant
metadata:
  name: team-a
  namespace: ai-tenants
spec:
  gateway:
    name: team-a
  oidc:
    issuerUrl: "https://keycloak.example.com/realms/team-a"
    clientId: team-a-client
    ttl: 60  # team-a uses aggressive JWKS refresh
```

Group names in `MaaSSubscription` and `MaaSAuthPolicy` resources must match the group claims in that tenant's OIDC tokens exactly.

See [Multi-Tenant Setup](../install/multi-tenant-setup.md) and [Multi-Tenancy](../concepts/multi-tenancy.md).

## Authentication evaluation order

The gateway-level `maas-gateway-auth` AuthPolicy evaluates authenticators in priority order. A request succeeds when **any** matching authenticator validates the token:

| Priority | Authenticator | Token pattern |
|----------|---------------|---------------|
| 0 | API keys | `Bearer sk-oai-*` |
| 1 | OIDC JWT | `Bearer` JWT (when OIDC is configured) |
| 2 | OpenShift TokenReview | Other `Bearer` tokens |

OIDC is evaluated **before** OpenShift TokenReview so external IdP JWTs are validated against JWKS rather than being sent to the Kubernetes API server TokenReview endpoint (which would fail for Keycloak tokens).

## Verify configuration

1. Obtain a **user access token** from your IdP (authorization code, device, or password grant in lab environments). Do **not** use the OAuth2 client credentials grant — see [OAuth2 Client Credentials — out of scope](#oauth2-client-credentials-out-of-scope) below.

2. Call the MaaS management API:

```bash
curl -H "Authorization: Bearer <user-access-token>" \
  https://<maas-gateway-url>/maas-api/v1/models
```

If authentication succeeds, the API returns models available to the groups in your token. An empty list usually means the token is valid but the user has no matching subscription groups.

For Keycloak-specific IdP setup and token generation examples, see [Keycloak Configuration](../../samples/install/keycloak/README.md).

## JWKS Cache TTL

Authorino validates OIDC tokens by fetching the IdP's JWKS (JSON Web Key Set). The `ttl` field controls how long Authorino caches the key set before re-fetching.

| Field | Default | Minimum | Description |
|-------|---------|---------|-------------|
| `ttl` | 300 | 30 | JWKS cache duration in seconds. CRD validation enforces the minimum; the controller also rejects values below 30 at reconcile time. |

**Choosing a TTL value:**

- **Lower TTL** (30-60s): faster key rotation propagation, more frequent JWKS fetches.
- **Default TTL** (300s): balanced for most deployments.
- **Higher TTL** (600-3600s): reduced load on the IdP, but key rotations take longer to propagate.

### IdP Outage Behavior

When the IdP becomes unreachable:

- Authorino continues using the **last successfully cached JWKS** indefinitely. Existing tokens signed with cached keys keep working.
- The `ttl` controls refresh frequency, not cache expiration. Authorino does not evict cached keys on TTL expiry if the refresh fails.
- Tokens signed with keys that were **never cached** (e.g., a key added to the IdP after the last successful fetch) will fail validation until the IdP is reachable again.

In multi-tenant deployments, each tenant configures `ttl` independently on its owning `AITenant`. The controller applies the per-tenant TTL to that tenant's gateway-level AuthPolicy.

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
- Incorrect `clientId` in the `AITenant` CR
- Token `azp` claim does not match configured `clientId`

**Common causes of high latency:**

- Slow IdP response times
- Network latency to JWKS endpoint
- Consider increasing `ttl` if the IdP is slow but reliable

See [Metrics & Dashboards](../observability/metrics-and-dashboards.md) for all Authorino metrics.

## Security controls and responsibilities

This table covers the non-functional security requirements (NFRs) raised during GA refinement. Each row states who enforces the control and what an operator needs to know.

| NFR | Enforced by | Details |
|-----|-------------|---------|
| **JWKS cache policy** | MaaS controller + Authorino | `ttl` is propagated from the `AITenant` (or legacy `Tenant`) CR to Authorino's `jwt.ttl` field. Minimum 30 s enforced server-side in the controller (independent of CRD admission validation). See [JWKS Cache TTL](#jwks-cache-ttl) above. |
| **Client secret handling** | Not applicable | MaaS external OIDC uses **JWT bearer validation only**. Tokens are validated against the IdP's public JWKS. No `client_secret` is required or stored at runtime. The `clientId` field is configuration, not a credential. |
| **OAuth client binding (`azp` claim)** | MaaS controller (gateway AuthPolicy) | The generated `maas-gateway-auth` AuthPolicy includes an `oidc-client-bound` rule that checks `auth.identity.azp == clientId`. Tokens issued to a different OAuth client are rejected with 403, even if they share the same issuer. An `has(auth.identity.azp)` guard ensures OpenShift TokenReview identities (which carry no `azp` claim) are unaffected. |
| **OIDC group claim sanitization** | MaaS controller (gateway AuthPolicy) | The `oidc-groups-safe` authorization rule rejects OIDC tokens whose `groups` claim contains characters outside `[A-Za-z0-9:._/-]`. This prevents malformed group names from reaching subscription selection or being serialized into identity headers. Ensure IdP group names match this pattern and your `MaaSSubscription` / `MaaSAuthPolicy` group entries. |
| **SSRF on issuer URL** | CRD validation + Authorino HTTP client | The CRD enforces `^https://\S+$` on `issuerUrl`, blocking plain HTTP and empty URLs at admission time. Blocking of private IPs and loopback addresses in JWKS fetch is the responsibility of the Authorino HTTP client (outside MaaS). Operators in restricted environments should use network policies to constrain Authorino's egress. |
| **Rate limiting on auth paths** | Kuadrant TokenRateLimitPolicy (model inference only) | The `gateway-default-deny` TRLP enforces a 0-token default on model inference routes and **explicitly excludes** `/maas-api` management paths. Authorino's internal JWKS refresh is not exposed as an external endpoint and is not subject to per-request rate limiting. If high-volume IdP calls are a concern, increase `ttl` to reduce refresh frequency. |
| **Authentication audit trail** | Authorino metrics + PrometheusRule alerts | `auth_server_authconfig_response_status` provides aggregate counts by status (`OK`, `UNAUTHENTICATED`, `UNAUTHORIZED`). The `MaaSAuthorinoAuthenticationHighFailureRate` alert fires on sustained failure rates above 10%. Per-request auth decision logging requires capturing Authorino's structured log output at debug/info level outside MaaS. |

### OAuth2 Client Credentials — out of scope

The **OAuth2 Client Credentials** (`grant_type=client_credentials`) flow is **not supported** in this GA release. MaaS external OIDC targets interactive/bearer JWT flows where end users authenticate against their corporate IdP and present access tokens. Machine-to-machine use cases should use [API keys](../concepts/api-key-authentication.md) or a broker IdP that exchanges client credentials for bearer tokens compatible with the OIDC JWT path.

## See Also

- [Authentication Modes](../concepts/auth-modes.md) — Dashboard vs standalone OIDC paths
- [Keycloak Configuration](../../samples/install/keycloak/README.md) — IdP setup and token generation
- [AITenant CRD](../reference/crds/ai-tenant.md) — Platform context including OIDC
- [API Key Management](../user-guide/api-key-management.md) — Minting keys after OIDC authentication
