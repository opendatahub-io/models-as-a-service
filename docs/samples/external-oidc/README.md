# External OIDC Authentication

MaaS supports external OIDC identity providers (Keycloak, Azure AD, Okta, etc.)
alongside the default OpenShift TokenReview and API key authentication.

When enabled, users can authenticate to `maas-api` with a JWT from the external
IdP, mint an `sk-oai-*` API key, and use that key for model discovery and
inference.

## Prerequisites

- MaaS deployed (maas-api, maas-controller running)
- An OIDC provider with:
  - A `.well-known/openid-configuration` endpoint served over HTTPS
  - A public or confidential OAuth2 client
  - Users with group memberships (for subscription matching)

For a development Keycloak instance, see
[Keycloak setup](../install/keycloak/README.md).

## Enabling External OIDC

Create a `MaaSPlatformAuth` CR in the namespace where `maas-api` is deployed:

```yaml
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSPlatformAuth
metadata:
  name: platform-auth
spec:
  externalOIDC:
    issuerUrl: "https://idp.example.com/realms/my-realm"
    clientId: "my-client"
    ttl: 300          # JWKS cache duration in seconds (default: 300)
```

Apply it:

```bash
kubectl apply -f docs/samples/external-oidc/platform-auth.yaml -n <maas-namespace>
```

Or with `deploy.sh`:

```bash
OIDC_ISSUER_URL=https://idp.example.com/realms/my-realm \
OIDC_CLIENT_ID=my-client \
./scripts/deploy.sh --deployment-mode operator --external-oidc
```

### What happens

The `MaaSPlatformAuth` controller:

1. Adds `opendatahub.io/managed: "false"` to the `maas-api-auth-policy`
   AuthPolicy so the ODH operator does not overwrite the OIDC rules.
2. Updates the AuthPolicy to accept OIDC JWTs (alongside existing API key
   and OpenShift TokenReview authentication).
3. Adds client-binding authorization (`azp` claim must match `clientId`).
4. Updates response headers to extract username and groups from OIDC claims.

### Verify

```bash
# CR status should be Active
kubectl get maasplatformauth platform-auth -n <maas-namespace>

# AuthPolicy should show Accepted + Enforced
kubectl get authpolicy maas-api-auth-policy -n <maas-namespace> -o jsonpath='{.status.conditions}'
```

## Disabling External OIDC

Delete the CR. The controller restores the AuthPolicy to the base state
(API keys + OpenShift only) and removes the `managed` annotation:

```bash
kubectl delete maasplatformauth platform-auth -n <maas-namespace>
```

## Spec Reference

| Field | Required | Description |
|-------|----------|-------------|
| `spec.externalOIDC.issuerUrl` | Yes | OIDC issuer URL. Must start with `https://` and serve `.well-known/openid-configuration`. |
| `spec.externalOIDC.clientId` | Yes | OAuth2 client ID. Incoming tokens must have a matching `azp` claim. |
| `spec.externalOIDC.ttl` | No | JWKS cache duration in seconds. Minimum 30, default 300. |

## Example: Keycloak

```bash
# 1. Deploy Keycloak (optional — skip if using an existing IdP)
./scripts/setup-keycloak.sh

# 2. Import a test realm (development only)
./docs/samples/install/keycloak/test-realms/apply-test-realms.sh

# 3. Enable OIDC
kubectl apply -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSPlatformAuth
metadata:
  name: platform-auth
  namespace: opendatahub
spec:
  externalOIDC:
    issuerUrl: "https://keycloak.apps.<cluster-domain>/realms/maas"
    clientId: "maas-cli"
EOF

# 4. Get a token and create an API key
TOKEN=$(curl -sk -X POST \
  "https://keycloak.apps.<cluster-domain>/realms/maas/protocol/openid-connect/token" \
  -d "grant_type=password&client_id=maas-cli&username=alice&password=letmein" \
  | jq -r '.access_token')

curl -sk -X POST "https://<gateway>/maas-api/v1/api-keys" \
  -H "Authorization: Bearer $TOKEN"
```
