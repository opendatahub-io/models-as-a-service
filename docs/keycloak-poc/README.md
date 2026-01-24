# Keycloak IDP-Based Token Minting PoC

This Proof of Concept (PoC) demonstrates using Keycloak for IDP-based token minting instead of Kubernetes Service Account tokens.

## Overview

The PoC replaces the ServiceAccount token minting mechanism with Keycloak token exchange, allowing model access tokens to be issued by an external Identity Provider (IDP) rather than Kubernetes.

## Architecture

### Current Architecture (ServiceAccount-based)
```
User → OpenShift Token → maas-api → ServiceAccount Token → Model Access
```

### PoC Architecture (Keycloak-based)
```
User → OpenShift Token → maas-api → Keycloak Token Exchange → Keycloak Token → Model Access
```

## Components

### 1. Keycloak Deployment
- Deployed in `keycloak` namespace
- Realm: `maas`
- Client: `maas-api` (service account enabled)
- Audience: `maas-model-access`

### 2. Token Manager Updates
- `token/keycloak.go`: Keycloak token exchange implementation
- `token/manager.go`: Hybrid manager supporting both ServiceAccount and Keycloak modes
- Configuration via environment variables

### 3. AuthPolicy Updates
- `gateway-auth-policy-keycloak.yaml`: OIDC-based authentication
- Validates Keycloak tokens via OIDC discovery

## Deployment

### Prerequisites
- OpenShift cluster
- `oc` CLI configured
- `kubectl` access
- `jq` installed

### Step 1: Deploy Keycloak and Configure

```bash
./scripts/setup-keycloak-poc.sh
```

This script:
1. Deploys Keycloak to the cluster
2. Creates the `maas` realm
3. Configures the `maas-api` client
4. Sets up a test user

### Step 2: Deploy MaaS with Keycloak Integration

```bash
./scripts/deploy-keycloak-poc.sh
```

This script:
1. Deploys the base MaaS platform
2. Configures maas-api with Keycloak environment variables
3. Updates AuthPolicy to use OIDC authentication

### Step 3: Test the PoC

```bash
./scripts/test-keycloak-poc.sh
```

## Configuration

### Environment Variables

The following environment variables configure Keycloak integration:

```bash
KEYCLOAK_ENABLED=true
KEYCLOAK_BASE_URL=http://keycloak.keycloak.svc.cluster.local:8080
KEYCLOAK_REALM=maas
KEYCLOAK_CLIENT_ID=maas-api
KEYCLOAK_CLIENT_SECRET=maas-api-secret
KEYCLOAK_AUDIENCE=maas-model-access
```

### Keycloak Client Configuration

The `maas-api` client is configured with:
- Service Account enabled
- Direct Access Grants enabled
- Token lifespan: 4 hours (14400 seconds)

## Token Flow

### Token Issuance

1. User authenticates with OpenShift token
2. maas-api receives token and extracts user context
3. maas-api exchanges OpenShift token for Keycloak token
4. Keycloak token is returned to user

### Model Access

1. User presents Keycloak token to Gateway
2. Authorino validates token via OIDC discovery
3. Token claims (username, groups) are extracted
4. Tier lookup and RBAC checks are performed
5. Request is authorized and forwarded to model

## Limitations and Known Issues

### PoC Limitations

1. **User Mapping**: The PoC uses a simplified token exchange. In production, you'd need to:
   - Map OpenShift users to Keycloak users
   - Use Keycloak's token exchange API properly
   - Configure user mappers in Keycloak

2. **Token Claims**: The current implementation may not include all required user claims. Production would need:
   - Keycloak user mappers to add groups
   - Proper audience configuration
   - Custom claims for tier information

3. **Revocation**: Token revocation is not fully implemented. Options:
   - Use short-lived tokens (current approach)
   - Implement token tracking in database
   - Use Keycloak's logout/revocation endpoints

### Security Considerations

1. **Token Validation**: AuthPolicy validates tokens via OIDC, but ensure:
   - Proper TLS configuration
   - Correct audience validation
   - Token expiration checks

2. **RBAC Integration**: Current implementation still uses Kubernetes SubjectAccessReview. Consider:
   - Mapping Keycloak groups to Kubernetes groups
   - Using Keycloak's authorization services
   - Custom authorization logic

## Testing

### Manual Testing

1. **Get a token:**
```bash
OC_TOKEN=$(oc whoami -t)
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
HOST="maas.${CLUSTER_DOMAIN}"

TOKEN_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer ${OC_TOKEN}" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{"expiration": "1h"}' \
  "${HOST}/maas-api/v1/tokens")

TOKEN=$(echo "$TOKEN_RESPONSE" | jq -r .token)
```

2. **Use the token:**
```bash
curl -sSk \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  "${HOST}/maas-api/v1/models"
```

3. **Verify token format:**
```bash
echo "$TOKEN" | cut -d. -f2 | base64 -d | jq .
```

## Troubleshooting

### Token Minting Fails

1. Check Keycloak is running:
```bash
kubectl get pods -n keycloak
kubectl logs -n keycloak deployment/keycloak
```

2. Verify Keycloak configuration:
```bash
kubectl get deployment maas-api -n maas-api -o jsonpath='{.spec.template.spec.containers[0].env}' | jq .
```

3. Check maas-api logs:
```bash
kubectl logs -n maas-api deployment/maas-api | grep -i keycloak
```

### Model Access Fails (401)

1. Verify AuthPolicy is using OIDC:
```bash
kubectl get authpolicy gateway-auth-policy -n openshift-ingress -o yaml
```

2. Check Authorino logs:
```bash
kubectl logs -n kuadrant-system deployment/authorino-operator | grep -i oidc
```

3. Verify token audience matches:
```bash
echo "$TOKEN" | cut -d. -f2 | base64 -d | jq .aud
```

## Migration Path

### From ServiceAccount to Keycloak

1. **Phase 1**: Deploy Keycloak alongside existing system
2. **Phase 2**: Enable Keycloak mode for new tokens (feature flag)
3. **Phase 3**: Migrate existing users gradually
4. **Phase 4**: Disable ServiceAccount mode

### Rollback

To rollback to ServiceAccount mode:

```bash
kubectl set env deployment/maas-api -n maas-api KEYCLOAK_ENABLED=false
kubectl rollout restart deployment/maas-api -n maas-api
kubectl apply -f deployment/base/policies/auth-policies/gateway-auth-policy.yaml
```

## Future Work

1. **Proper Token Exchange**: Implement Keycloak's token exchange API
2. **User Mapping**: Automatic mapping of OpenShift users to Keycloak users
3. **Token Revocation**: Implement proper revocation mechanism
4. **Multi-Tenant Support**: Keycloak realm per tenant
5. **Audit Logging**: Track token issuance and usage
6. **Token Refresh**: Support refresh token flow

## References

- [Keycloak Token Exchange](https://www.keycloak.org/docs/latest/securing_apps/#_token-exchange)
- [Authorino OIDC Authentication](https://docs.kuadrant.io/authorino/docs/features/#openid-connect-oidc)
- [JIRA Spike: IDP-Based Token Minting](./jira-spike.md)
