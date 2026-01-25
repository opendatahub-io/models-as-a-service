# Keycloak PoC Quick Start

This is a quick start guide for the Keycloak IDP-based token minting PoC.

## Quick Deploy

```bash
# 1. Deploy base platform
./scripts/deploy-rhoai-stable.sh --operator-type odh --operator-catalog quay.io/opendatahub/opendatahub-operator-catalog:latest --channel fast
./scripts/deploy-openshift.sh

# 2. Deploy Keycloak overlay (handles everything)
./scripts/deploy-keycloak-poc.sh

# 3. Test the PoC
./scripts/test-keycloak-poc.sh
```

## What Changed

### Code Changes

1. **New Keycloak Token Manager** (`maas-api/internal/token/keycloak.go`)
   - Handles token exchange with Keycloak
   - Replaces ServiceAccount token creation

2. **Updated Token Manager** (`maas-api/internal/token/manager.go`)
   - Supports both ServiceAccount and Keycloak modes
   - Switches based on `KEYCLOAK_ENABLED` environment variable

3. **Updated Configuration** (`maas-api/internal/config/config.go`)
   - Added Keycloak configuration options
   - Environment variable support

4. **Updated Main** (`maas-api/cmd/main.go`)
   - Initializes Keycloak manager when enabled
   - Falls back to ServiceAccount mode when disabled

### Deployment Changes

1. **Keycloak Deployment** (`deployment/components/keycloak/`)
   - Keycloak server deployment
   - Route configuration

2. **Updated AuthPolicy** (`deployment/base/policies/auth-policies/gateway-auth-policy-keycloak.yaml`)
   - OIDC authentication instead of Kubernetes TokenReview
   - Validates Keycloak tokens

## Verification

After deployment, verify:

1. **Keycloak is running:**
```bash
kubectl get pods -n keycloak
```

2. **maas-api has Keycloak config:**
```bash
kubectl get deployment maas-api -n maas-api -o jsonpath='{.spec.template.spec.containers[0].env}' | jq '.[] | select(.name | startswith("KEYCLOAK"))'
```

3. **AuthPolicy uses OIDC:**
```bash
kubectl get authpolicy gateway-auth-policy -n openshift-ingress -o jsonpath='{.spec.rules.authentication}' | jq .
```

## Testing Token Flow

The maas-api now uses Keycloak for authentication. You need to get a Keycloak token directly from Keycloak, not from maas-api.

```bash
# Get cluster domain and Keycloak route
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
HOST="maas.${CLUSTER_DOMAIN}"
KEYCLOAK_ROUTE=$(kubectl get route keycloak -n keycloak -o jsonpath='{.spec.host}')

# Get Keycloak token for premium-user-1
# All tier users have password: "password"
TOKEN=$(curl -sSk -X POST "https://${KEYCLOAK_ROUTE}/realms/maas/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "username=premium-user-1" \
  -d "password=password" \
  -d "grant_type=password" \
  -d "client_id=maas-api" \
  -d "client_secret=maas-api-secret" | jq -r '.access_token')

# Use the Keycloak token to access maas-api
curl -sSk \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  "${HOST}/maas-api/v1/models"
```

### Available Test Users

All users have password: `password`

- **Free tier**: `free-user-1`, `free-user-2`
- **Premium tier**: `premium-user-1`, `premium-user-2`
- **Enterprise tier**: `enterprise-user-1`, `enterprise-user-2`

To use a different user, replace `premium-user-1` with any of the above usernames.

## Rollback to ServiceAccount Mode

```bash
# Disable Keycloak
kubectl set env deployment/maas-api -n maas-api KEYCLOAK_ENABLED=false

# Restart deployment
kubectl rollout restart deployment/maas-api -n maas-api

# Restore original AuthPolicy
kubectl apply -f deployment/base/policies/auth-policies/gateway-auth-policy.yaml
```

## Troubleshooting

### Keycloak not accessible
```bash
# Check Keycloak route
kubectl get route keycloak -n keycloak

# Check Keycloak logs
kubectl logs -n keycloak deployment/keycloak
```

### Token minting fails
```bash
# Check maas-api logs
kubectl logs -n maas-api deployment/maas-api | grep -i keycloak

# Verify Keycloak configuration
kubectl exec -n maas-api deployment/maas-api -- env | grep KEYCLOAK
```

### Model access returns 401
```bash
# Check AuthPolicy status
kubectl describe authpolicy gateway-auth-policy -n openshift-ingress

# Check Authorino logs
kubectl logs -n kuadrant-system deployment/authorino-operator | grep -i oidc
```

## Next Steps

1. Review the [full README](./README.md) for detailed documentation
2. Test with different user groups and tiers
3. Verify token claims include required information
4. Consider production improvements (user mapping, token exchange, etc.)
