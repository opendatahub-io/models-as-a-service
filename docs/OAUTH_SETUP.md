# OAuth Setup Guide (Keycloak OIDC)

This guide describes how to configure Keycloak so MaaS can accept Keycloak access tokens directly for `/maas-api/v1/tokens`.

## Keycloak Configuration

1. Create a realm for MaaS (example: `maas`).
2. Create an OIDC client for MaaS token requests:
   - **Client ID**: `maas-cli` (example)
   - **Client authentication**: On (confidential client)
   - **Standard flow**: On
   - **Direct access grants**: On (required if using the password grant in CLI examples)
3. Ensure tokens include a username and groups:
   - **preferred_username**: enabled by default in Keycloak tokens.
   - **groups**: add a client scope with a **Group Membership** mapper and attach it to the client.

MaaS expects:
- `preferred_username` for user identity
- `groups` as an array for tier mapping

If you use different claim names, adjust the MaaS AuthPolicy response headers accordingly.

## MaaS AuthPolicy Configuration

Update the MaaS API AuthPolicy JWKS URL to point at your realm:

```shell
KEYCLOAK_REALM="maas"

kubectl patch authpolicy maas-api-auth-policy -n maas-api --type=merge --patch-file <(cat <<EOF
spec:
  rules:
    authentication:
      keycloak-oidc:
        jwt:
          jwksUrl: "http://keycloak.keycloak-system.svc.cluster.local:8080/realms/${KEYCLOAK_REALM}/protocol/openid-connect/certs"
EOF
)
```

## Request a MaaS Token with a Keycloak Access Token

```shell
KEYCLOAK_URL="https://keycloak.${CLUSTER_DOMAIN}"
KEYCLOAK_REALM="maas"
KEYCLOAK_CLIENT_ID="maas-cli"
KEYCLOAK_USERNAME="user@example.com"
KEYCLOAK_PASSWORD="your-password"

KEYCLOAK_RESPONSE=$(curl -sSk \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "grant_type=password" \
  -d "client_id=${KEYCLOAK_CLIENT_ID}" \
  -d "username=${KEYCLOAK_USERNAME}" \
  -d "password=${KEYCLOAK_PASSWORD}" \
  "${KEYCLOAK_URL}/realms/${KEYCLOAK_REALM}/protocol/openid-connect/token")

KEYCLOAK_ACCESS_TOKEN=$(echo $KEYCLOAK_RESPONSE | jq -r .access_token)

MAAS_API_URL="https://maas.${CLUSTER_DOMAIN}"
TOKEN_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer ${KEYCLOAK_ACCESS_TOKEN}" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{"expiration": "24h"}' \
  "${MAAS_API_URL}/maas-api/v1/tokens")

echo $TOKEN_RESPONSE | jq -r .
```
