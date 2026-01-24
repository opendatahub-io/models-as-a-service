# Keycloak IDP-Based Token Minting PoC - Summary

## What Was Created

This PoC demonstrates replacing Kubernetes ServiceAccount token minting with Keycloak IDP-based token minting.

### Files Created/Modified

#### New Files
1. **Keycloak Deployment**
   - `deployment/components/keycloak/keycloak-deployment.yaml` - Keycloak server deployment
   - `deployment/components/keycloak/kustomization.yaml` - Kustomize config

2. **Keycloak Integration Code**
   - `maas-api/internal/token/keycloak.go` - Keycloak token manager implementation
   - `deployment/base/policies/auth-policies/gateway-auth-policy-keycloak.yaml` - OIDC AuthPolicy

3. **Scripts**
   - `scripts/setup-keycloak-poc.sh` - Keycloak setup and configuration
   - `scripts/deploy-keycloak-poc.sh` - Full PoC deployment
   - `scripts/test-keycloak-poc.sh` - PoC testing script

4. **Documentation**
   - `docs/keycloak-poc/README.md` - Full documentation
   - `docs/keycloak-poc/QUICKSTART.md` - Quick start guide
   - `docs/keycloak-poc/SUMMARY.md` - This file

#### Modified Files
1. `maas-api/internal/token/manager.go` - Added Keycloak support
2. `maas-api/internal/config/config.go` - Added Keycloak configuration
3. `maas-api/cmd/main.go` - Initialize Keycloak manager when enabled
4. `maas-api/internal/token/handler.go` - Extract OpenShift token for Keycloak exchange

## Key Features

### 1. Hybrid Token Manager
- Supports both ServiceAccount and Keycloak modes
- Switches based on `KEYCLOAK_ENABLED` environment variable
- Backward compatible with existing ServiceAccount implementation

### 2. Keycloak Integration
- Token exchange from OpenShift tokens to Keycloak tokens
- OIDC-based authentication in AuthPolicy
- Configurable via environment variables

### 3. Easy Deployment
- Automated setup scripts
- One-command deployment
- Automated testing

## How It Works

### Token Issuance Flow
1. User authenticates with OpenShift token
2. maas-api receives request with OpenShift token
3. maas-api exchanges OpenShift token for Keycloak token
4. Keycloak token returned to user

### Model Access Flow
1. User presents Keycloak token to Gateway
2. Authorino validates token via OIDC discovery
3. Token claims extracted (username, groups)
4. Tier lookup and RBAC checks performed
5. Request authorized and forwarded to model

## Deployment

### Quick Deploy
```bash
./scripts/setup-keycloak-poc.sh      # Setup Keycloak
./scripts/deploy-keycloak-poc.sh     # Deploy MaaS with Keycloak
./scripts/test-keycloak-poc.sh       # Test the PoC
```

### Configuration
Set these environment variables on maas-api deployment:
- `KEYCLOAK_ENABLED=true`
- `KEYCLOAK_BASE_URL=http://keycloak.keycloak.svc.cluster.local:8080`
- `KEYCLOAK_REALM=maas`
- `KEYCLOAK_CLIENT_ID=maas-api`
- `KEYCLOAK_CLIENT_SECRET=maas-api-secret`
- `KEYCLOAK_AUDIENCE=maas-model-access`

## Testing

### Manual Test
```bash
# Get token
OC_TOKEN=$(oc whoami -t)
TOKEN_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer ${OC_TOKEN}" \
  -H "Content-Type: application/json" \
  -X POST -d '{"expiration": "1h"}' \
  "https://maas.${CLUSTER_DOMAIN}/maas-api/v1/tokens")

TOKEN=$(echo "$TOKEN_RESPONSE" | jq -r .token)

# Use token
curl -sSk \
  -H "Authorization: Bearer ${TOKEN}" \
  "https://maas.${CLUSTER_DOMAIN}/maas-api/v1/models"
```

## PoC Limitations

1. **Simplified Token Exchange**: Uses client credentials flow. Production would need proper token exchange API.
2. **User Mapping**: No automatic mapping of OpenShift users to Keycloak users.
3. **Token Claims**: May not include all required user claims (groups, etc.).
4. **Revocation**: Token revocation not fully implemented.

## Production Considerations

1. **User Mapping**: Implement automatic mapping of OpenShift users to Keycloak users
2. **Token Exchange**: Use Keycloak's token exchange API properly
3. **Claims**: Configure Keycloak mappers to include required claims
4. **Revocation**: Implement proper token revocation mechanism
5. **Multi-Tenant**: Support multiple Keycloak realms for multi-tenant deployments
6. **Security**: Proper TLS, audience validation, token expiration

## Rollback

To rollback to ServiceAccount mode:
```bash
kubectl set env deployment/maas-api -n maas-api KEYCLOAK_ENABLED=false
kubectl rollout restart deployment/maas-api -n maas-api
kubectl apply -f deployment/base/policies/auth-policies/gateway-auth-policy.yaml
```

## Next Steps

1. Test the PoC on your OpenShift cluster
2. Verify token flow end-to-end
3. Test with different user groups and tiers
4. Evaluate production readiness
5. Plan migration strategy if proceeding

## Questions Answered

### Can we maintain the same security posture with IDP tokens?
✅ Yes, with proper configuration (TLS, audience validation, expiration)

### How do we handle tier-based access without Kubernetes namespaces?
✅ Still use tier namespaces, but tokens come from Keycloak instead of ServiceAccounts

### What's the operational overhead of managing IDP clients vs. Service Accounts?
⚠️ Similar - both require configuration and management

### Can we support both IDP and SA tokens during migration?
✅ Yes - the hybrid manager supports both modes

### How do we handle token revocation in IDP vs. Kubernetes?
⚠️ Keycloak supports revocation, but requires implementation

### What's the impact on multi-tenant deployments?
⚠️ Would need multiple Keycloak realms or proper tenant isolation
