# Keycloak Configuration for MaaS External OIDC

This directory contains examples and guides for configuring Keycloak as an identity provider for MaaS external OIDC authentication.

## Prerequisites

Keycloak must be deployed first:

```bash
./scripts/setup-keycloak.sh
```

## Quick Start: Access Admin Console

### 1. Get Admin Credentials

```bash
# Get username
kubectl get secret maas-keycloak-initial-admin -n keycloak-system \
  -o jsonpath='{.data.username}' | base64 -d

# Get password
kubectl get secret maas-keycloak-initial-admin -n keycloak-system \
  -o jsonpath='{.data.password}' | base64 -d
```

### 2. Access Keycloak Admin Console

```bash
# Get the URL (auto-detected during deployment)
kubectl get httproute keycloak-route -n keycloak-system \
  -o jsonpath='{.spec.hostnames[0]}'
```

Navigate to: `https://{hostname-from-above}`

### 3. Login

Use the username and password from step 1.

## Realm Configuration Methods

### Method 1: Admin Console (Recommended)

The easiest way to configure realms is through the web UI:

1. **Create a Realm**
   - Click "Create Realm" in the top-left dropdown
   - Enter a realm name (e.g., `my-company`)
   - Click "Create"

2. **Configure Groups** (must match MaaS subscription groups)
   - Navigate to: Groups
   - Click "Create group"
   - Add groups that match your MaaS subscription owner groups
   - Example: `engineering`, `data-science`, `premium-users`

3. **Add Users**
   - Navigate to: Users → Add user
   - Set username, email, first/last name
   - Click "Create"
   - Go to "Credentials" tab → Set password
   - Go to "Groups" tab → Join groups

4. **Create OIDC Client for MaaS**
   - Navigate to: Clients → Create client
   - **Client type:** OpenID Connect
   - **Client ID:** `maas`
   - Click "Next"
   - **Client authentication:** ON
   - **Authorization:** OFF
   - **Authentication flow:** Standard flow, Direct access grants
   - Click "Next"
   - **Valid redirect URIs:** `https://maas.apps.{your-cluster-domain}/*` (restrict to the MaaS gateway host)
   - **Web origins:** `https://maas.apps.{your-cluster-domain}` (restrict to the MaaS gateway host)
   - Click "Save"

5. **Configure Group Mapper**
   - In the client, go to "Client scopes" tab
   - Click the dedicated scope (e.g., `maas-dedicated`)
   - Click "Add mapper" → "By configuration" → "Group Membership"
   - **Name:** `groups`
   - **Token Claim Name:** `groups`
   - **Full group path:** OFF (recommended)
   - **Add to ID token:** ON
   - **Add to access token:** ON
   - **Add to userinfo:** ON
   - Click "Save"

6. **Get Client Credentials**
   - Navigate to: Clients → maas → Credentials tab
   - Copy the "Client secret"
   - Save for MaaS OIDC configuration

### Method 2: Import Test Realms (Development Only)

For quick testing and development, you can import pre-configured test realms:

**⚠️ WARNING: Test realms contain hardcoded passwords and wildcard redirects. NOT for production use.**

See: [test-realms/README.md](test-realms/README.md)

```bash
# Deploy test realms
./docs/samples/install/keycloak/test-realms/apply-test-realms.sh
```

### Method 3: Custom Realm Template

Start from a minimal template and customize:

```bash
# Copy template (when available)
cp docs/samples/install/keycloak/custom-realm-template.json my-realm.json

# Edit my-realm.json with your configuration

# Import via Admin Console:
# Realm dropdown → Create Realm → Browse → my-realm.json → Create
```

## OIDC Configuration for MaaS

Once you have a realm configured with groups and users, you'll need to configure MaaS to use Keycloak as an OIDC provider.

### Required Information

From your Keycloak configuration, you'll need:

1. **Issuer URL:** `https://{keycloak-hostname}/realms/{realm-name}`
2. **Client ID:** The client you created (e.g., `maas`)
3. **Client Secret:** From the client credentials tab
4. **Groups Claim:** `groups` (from the mapper configuration)

### Example Configuration

```yaml
# This will be configured in MaaS (implementation pending)
oidc:
  issuer: https://keycloak.apps.my-cluster.example.com/realms/my-company
  clientId: maas
  clientSecret: {from-keycloak-client-credentials}
  groupsClaim: groups
  usernameClaim: preferred_username  # or "sub" for user ID
```

## Verifying Realm Configuration

### Test OIDC Token Generation

You can test that your realm is configured correctly using the Keycloak token endpoint:

```bash
# Get cluster domain
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
KEYCLOAK_HOST="keycloak.${CLUSTER_DOMAIN}"
REALM_NAME="my-realm"
CLIENT_ID="maas"
CLIENT_SECRET="your-client-secret"
USERNAME="testuser"
PASSWORD="testpass"

# Get token
curl -k -X POST \
  "https://${KEYCLOAK_HOST}/realms/${REALM_NAME}/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=password" \
  -d "client_id=${CLIENT_ID}" \
  -d "client_secret=${CLIENT_SECRET}" \
  -d "username=${USERNAME}" \
  -d "password=${PASSWORD}" \
  | jq .

# Decode the access token to verify groups claim
# Copy the access_token from above output
TOKEN="eyJhbGci..."

# Decode (requires jq and base64)
echo $TOKEN | cut -d'.' -f2 | base64 -d 2>/dev/null | jq .
```

Look for the `groups` array in the decoded token payload.

## Troubleshooting

### Can't Access Admin Console

```bash
# Check Keycloak pod status
kubectl get pods -n keycloak-system

# Check HTTPRoute
kubectl get httproute keycloak-route -n keycloak-system

# Check logs
kubectl logs -n keycloak-system -l app=keycloak
```

### Groups Not Appearing in Token

1. Verify group mapper is configured in the client scope
2. Check that "Add to ID token" and "Add to access token" are enabled
3. Verify users are actually members of the groups
4. Test token generation (see "Verifying Realm Configuration" above)

### Forgot Admin Password

The admin password is always available in the Kubernetes secret:

```bash
# Retrieve admin username
kubectl get secret maas-keycloak-initial-admin -n keycloak-system \
  -o jsonpath='{.data.username}' | base64 -d

# Retrieve admin password
kubectl get secret maas-keycloak-initial-admin -n keycloak-system \
  -o jsonpath='{.data.password}' | base64 -d
```

The secret persists as long as the Keycloak instance exists, so you can retrieve credentials at any time.

## Next Steps

After configuring Keycloak realms:

1. Configure MaaS to use Keycloak as OIDC provider (implementation pending)
2. Create MaaSSubscription resources with groups matching Keycloak groups
3. Create MaaSAuthPolicy resources to grant access to models
4. Test authentication with OIDC tokens

## Additional Resources

- [Keycloak Documentation](https://www.keycloak.org/docs/latest/server_admin/)
- [OpenID Connect Core Specification](https://openid.net/specs/openid-connect-core-1_0.html)
