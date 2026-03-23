# Keycloak Configuration for MaaS External OIDC

This directory contains examples for configuring Keycloak as an identity provider for MaaS external OIDC authentication.

## Prerequisites

Deploy Keycloak first:

```bash
./scripts/setup-keycloak.sh
```

## Access Admin Console

Get credentials:

```bash
# Username
kubectl get secret maas-keycloak-initial-admin -n keycloak-system \
  -o jsonpath='{.data.username}' | base64 -d

# Password
kubectl get secret maas-keycloak-initial-admin -n keycloak-system \
  -o jsonpath='{.data.password}' | base64 -d
```

Get the URL:

```bash
kubectl get httproute keycloak-route -n keycloak-system \
  -o jsonpath='{.spec.hostnames[0]}'
```

Navigate to `https://{hostname}` and log in.

## Realm Configuration

### Method 1: Import Test Realms (Development)

For quick testing, import pre-configured test realms:

> **Warning:** Test realms contain hardcoded passwords. Not for production.

```bash
./docs/samples/keycloak/test-realms/apply-test-realms.sh
```

This creates a `maas` realm with:
- Client: `maas-cli` (public, direct-access grants)
- Users: `alice/letmein` (premium-group), `erin/letmein` (enterprise-group), `ada/letmein` (admin-group)
- Groups mapper on access, ID, and userinfo tokens

### Method 2: Admin Console (Production)

1. **Create a Realm** — click "Create Realm" in the top-left dropdown.
2. **Configure Groups** — must match MaaS subscription owner groups (e.g. `engineering`, `data-science`).
3. **Add Users** — assign to groups.
4. **Create OIDC Client:**
   - Client type: OpenID Connect
   - Client authentication: ON (or OFF for public clients)
   - Authentication flow: Standard flow + Direct access grants
   - Valid redirect URIs: `https://*.{cluster-domain}/*`
5. **Add Group Mapper** — in the client's dedicated scope:
   - Mapper type: Group Membership
   - Token Claim Name: `groups`
   - Full group path: OFF
   - Add to ID token, access token, userinfo: ON

## Configure MaaS for OIDC

After Keycloak is configured, deploy MaaS with external OIDC:

```bash
OIDC_ISSUER_URL="https://keycloak.{cluster-domain}/realms/{realm}" \
OIDC_CLIENT_ID="{client-id}" \
./scripts/deploy.sh --external-oidc
```

Or use the combined flag to deploy both:

```bash
./scripts/deploy.sh --enable-keycloak --external-oidc
```

Then import test realms and set the OIDC variables:

```bash
./docs/samples/keycloak/test-realms/apply-test-realms.sh

CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
OIDC_ISSUER_URL="https://keycloak.${CLUSTER_DOMAIN}/realms/maas"
OIDC_CLIENT_ID="maas-cli"
```

## Verify Token Generation

```bash
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')

curl -sk -X POST \
  "https://keycloak.${CLUSTER_DOMAIN}/realms/maas/protocol/openid-connect/token" \
  -d "grant_type=password" \
  -d "client_id=maas-cli" \
  -d "username=alice" \
  -d "password=letmein" | jq -r '.access_token'
```

Decode the token to verify the `groups` claim:

```bash
TOKEN="<paste-token-here>"
echo "$TOKEN" | cut -d'.' -f2 | base64 -d 2>/dev/null | jq '{sub, preferred_username, azp, groups}'
```

## Cleanup

```bash
./scripts/cleanup-keycloak.sh --force
```
