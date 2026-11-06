# Test Realms for Development and Testing

⚠️ **WARNING: These test realms are for development and testing ONLY. NOT for production use.**

## What Are These?

These are pre-configured Keycloak realm definitions for multi-tenant testing scenarios. They demonstrate:
- Multi-realm setup for tenant isolation
- Group-based access control
- OIDC client configuration with group mappers
- User assignments to groups

## Security Warning

**DO NOT use these realms in production environments.**

These realms contain security issues intentional for testing:
- **Hardcoded passwords:** All users have password `letmein`
- **Wildcard redirects:** `["*"]` allows any redirect URI
- **Public clients:** Client authentication is disabled
- **No email verification:** Email addresses are not validated
- **Overly permissive CORS:** Web origins set to `["*"]`

## Contents

### Tenant-A Realm

**Groups:**
- Engineering
- Site-Reliability
- Project-Alpha
- `ai-grid-admins`
- `42c3e46f-2477-44d7-a85b-d3b43f6b31fa/reserved-ai-grid-admins`

**Users:**
- `alice_lead` (password: `letmein`)
  - Member of: Engineering, Project-Alpha
- `bob_sre` (password: `letmein`)
  - Member of: Site-Reliability
- `grid_admin` (password: `letmein`)
  - Member of: `ai-grid-admins`
  - Organization ID: `42c3e46f-2477-44d7-a85b-d3b43f6b31fa`
  - Organization name: `coke`
  - Organization role: `admins`
- `grid_no_org` (password: `letmein`)
  - Member of: no groups
- `grid_reserved_group` (password: `letmein`)
  - Member of: `42c3e46f-2477-44d7-a85b-d3b43f6b31fa/reserved-ai-grid-admins`
  - Has no organization claims

**Client:**
- Client ID: `test-client`
- Type: Public (no authentication)
- Groups claim configured in tokens
- Organization claims are flat: `org_id`, `org_name`, and `roles`
- Access tokens include `aud: maas-api` and `token_use: access` for the strict MaaS OIDC contract

### Tenant-B Realm

**Groups:**
- Product-Security
- Project-Omega

**Users:**
- `charlie_sec_lead` (password: `letmein`)
  - Member of: Product-Security, Project-Omega
- `grace_dev` (password: `letmein`)
  - Member of: Project-Omega

**Client:**
- Client ID: `test-client`
- Type: Public (no authentication)
- Groups claim configured in tokens
- Access tokens include `aud: maas-api` and `token_use: access` for the strict MaaS OIDC contract

## How to Deploy

### Prerequisites

1. Keycloak must be deployed:
   ```bash
   ./scripts/setup-keycloak.sh
   ```

2. Keycloak must be running and accessible

### Deploy Test Realms

```bash
# From repository root
./docs/samples/install/keycloak/test-realms/apply-test-realms.sh
```

This script will:
1. Create a ConfigMap with both realm JSONs
2. Patch the Keycloak instance to mount the ConfigMap
3. Restart Keycloak to trigger realm import
4. Wait for Keycloak to be ready

### Verify Deployment

```bash
# Check Keycloak pods are running
kubectl get pods -n keycloak-system

# Access admin console and verify realms exist
# You should see "tenant-a" and "tenant-b" in the realm dropdown
```

## Testing with Test Realms

### Get Access Token

```bash
# Get cluster domain
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
KEYCLOAK_HOST="keycloak.${CLUSTER_DOMAIN}"

# Test with tenant-a user
curl -X POST \
  "https://${KEYCLOAK_HOST}/realms/tenant-a/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=password" \
  -d "client_id=test-client" \
  -d "username=alice_lead" \
  -d "password=letmein" \
  | jq .

# Test with tenant-b user
curl -X POST \
  "https://${KEYCLOAK_HOST}/realms/tenant-b/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=password" \
  -d "client_id=test-client" \
  -d "username=charlie_sec_lead" \
  -d "password=letmein" \
  | jq .
```

### Verify Groups in Token

```bash
# Decode the access token to see groups claim
TOKEN="<paste-access-token-from-above>"
echo $TOKEN | cut -d'.' -f2 | base64 -d 2>/dev/null | jq .

# Look for the "groups" array in the output
```

### Expected Groups in Tokens

**alice_lead** should have:
```json
{
  "groups": ["Engineering", "Project-Alpha"]
}
```

**bob_sre** should have:
```json
{
  "groups": ["Site-Reliability"]
}
```

**charlie_sec_lead** should have:
```json
{
  "groups": ["Product-Security", "Project-Omega"]
}
```

**grace_dev** should have:
```json
{
  "groups": ["Project-Omega"]
}
```

**grid_admin** should have:
```json
{
  "groups": ["ai-grid-admins"],
  "org_id": "42c3e46f-2477-44d7-a85b-d3b43f6b31fa",
  "org_name": "coke",
  "roles": ["admins"]
}
```

The `grid_reserved_group` user has a UUID-prefixed group but no `org_id`. Its group must not grant access to organization-bound resources.

## Use Cases

These test realms are useful for:
- **Local development:** Quick OIDC provider without manual configuration
- **Integration testing:** Automated tests with predictable users/groups
- **Multi-tenant POC:** Demonstrating tenant isolation patterns
- **Learning:** Understanding Keycloak realm structure and OIDC configuration

## Cleanup

To remove test realms:

```bash
# Remove the ConfigMap
kubectl delete configmap keycloak-test-realms -n keycloak-system

# Restart Keycloak to reset (optional)
kubectl rollout restart statefulset/maas-keycloak -n keycloak-system
```

Or completely remove Keycloak:

```bash
./scripts/cleanup-keycloak.sh
```

## Creating Production Realms

For production use, create proper realms via:
1. Keycloak Admin Console (recommended)
2. Export a properly configured realm and use as template
3. Use Keycloak's realm configuration tools

See: [../README.md](../README.md) for production configuration guide.
