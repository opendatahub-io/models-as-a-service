# Keycloak OAuth Integration Proposal

This document summarizes how Keycloak OAuth integrates with MaaS for token minting and inference access.

## Goals

- Use Keycloak as the identity provider (IDP) for MaaS.
- Validate user identity and group membership at the gateway.
- Mint MaaS service tokens scoped to tier namespaces.
- Keep the flow reproducible on redeploy via manifests and scripts.

## Components

- **Keycloak** (deployment + route): hosts the `maas` realm and `maas-cli` client.
- **Kuadrant AuthPolicy** on `maas-api-route`: validates Keycloak JWTs and injects identity headers.
- **MaaS API**: maps groups to tiers and mints ServiceAccount tokens.
- **Tier mapping ConfigMap**: defines which groups map to which tiers.

## Auth Flow (End-to-End)

1. User authenticates to Keycloak and receives an **access token**.
2. Request hits the gateway `maas-api-route` with `Authorization: Bearer <kc-access-token>`.
3. AuthPolicy validates the JWT and injects identity headers:
   - `X-MaaS-Username` from `auth.identity.preferred_username`
   - `X-MaaS-Group` from `auth.identity.groups.@tostr` (JSON array)
4. MaaS API reads those headers, maps groups to a tier, and mints a **ServiceAccount token**.
5. Client uses the MaaS token for inference calls through the gateway.

## How a User Maps to OpenShift Objects

This is the object-level mapping for a user request:

1. **Keycloak user** logs in and receives an access token with `preferred_username` and `groups`.
2. **AuthPolicy** validates the token and injects:
   - `X-MaaS-Username` = `preferred_username`
   - `X-MaaS-Group` = JSON array of group names
3. **Tier mapping** resolves groups to a tier using `tier-to-group-mapping` ConfigMap.
4. **Tier namespace** is created or reused:
   - `maas-default-gateway-tier-<tier>` (e.g., `maas-default-gateway-tier-free`)
5. **ServiceAccount** is created or reused in that tier namespace:
   - Name is a sanitized username plus hash.
6. **TokenRequest** is created for that ServiceAccount.
7. The resulting **ServiceAccount token** is returned to the client as the MaaS token.

## Tiering and Groups (Current Behavior)

Tiering is derived from group membership. The MaaS API expects `X-MaaS-Group` and uses it to select a tier. If you remove groups from the flow, MaaS API will not be able to determine a tier and token minting fails. OAuth is only used to authenticate identity; groups are the input to tier selection.

## Token Types

- **Keycloak access token**: OAuth JWT issued by Keycloak for user identity.
- **MaaS token**: Kubernetes ServiceAccount token minted per user and tier namespace.
  - Issued via `TokenRequest` and returned to client.
  - Not stored in MaaS; only API key metadata is stored (if enabled).

## Configuration Summary

### Keycloak Realm

- Realm: `maas`
- Client: `maas-cli` (confidential, direct grant enabled)
- Protocol mapper: `groups` claim in access token
- Users: `freeuser1`, `premiumuser1` (password: `password123`)

### AuthPolicy (OIDC via JWKS)

Use JWKS from the in-cluster Keycloak service to avoid TLS trust issues with the Route cert:

```
spec:
  rules:
    authentication:
      keycloak-oidc:
        jwt:
          jwksUrl: http://keycloak.keycloak-system.svc.cluster.local:8080/realms/maas/protocol/openid-connect/certs
        credentials:
          authorizationHeader:
            prefix: Bearer
```

### Header Mapping

```
spec:
  rules:
    response:
      success:
        headers:
          X-MaaS-Username:
            plain:
              selector: auth.identity.preferred_username
          X-MaaS-Group:
            plain:
              selector: auth.identity.groups.@tostr
```

### Tier Mapping

The tier mapping must include Keycloak group names:

```
free: free-users, system:authenticated
premium: premium-users
enterprise: enterprise-users, admins
```

## Deployment Hooks

The deployment scripts should:

- Create Keycloak and import the realm.
- Patch AuthPolicy to use JWKS URL from the in-cluster Keycloak service.
- Ensure the tier mapping ConfigMap includes Keycloak group names.

## Known Caveats

- If AuthPolicy uses `issuerUrl` with the Keycloak Route, Authorino may fail OIDC discovery due to TLS trust.
- Validation scripts should look for `maas-api-route` in the namespace where MaaS is deployed (typically `opendatahub`).

## Reference Files

- AuthPolicy: `deployment/base/maas-api/policies/auth-policy.yaml`
- Tier map: `deployment/base/maas-api/resources/tier-mapping-configmap.yaml`
- Deployment script: `scripts/deploy-rhoai-stable.sh`
