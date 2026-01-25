# Keycloak PoC Quick Start

Bare-minimum guide to get Keycloak-based token minting up and running.

## Prerequisites

- OpenShift cluster
- `oc` and `kubectl` configured
- `jq` installed

## Deployment

### Step 1: Deploy Base Platform

```bash
./scripts/deploy-rhoai-stable.sh --operator-type odh --operator-catalog quay.io/opendatahub/opendatahub-operator-catalog:latest --channel fast
./scripts/deploy-openshift.sh
```

### Step 2: Deploy Keycloak Overlay

```bash
./scripts/deploy-keycloak-poc.sh
```

That's it. The script handles:
- Keycloak deployment and configuration
- Realm, clients, and test users setup
- AuthPolicy updates
- maas-api configuration

## Quick Test

```bash
# Get Keycloak token
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
KEYCLOAK_URL=$(kubectl get route keycloak -n keycloak -o jsonpath='{.spec.host}')
TOKEN=$(curl -sSk -X POST "https://${KEYCLOAK_URL}/realms/maas/protocol/openid-connect/token" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -d "username=free-user-1" \
  -d "password=password" \
  -d "grant_type=password" \
  -d "client_id=maas-api" \
  -d "client_secret=maas-api-secret" | jq -r '.access_token')

# Mint MaaS token
HOST="maas.${CLUSTER_DOMAIN}"
MAAS_TOKEN=$(curl -sSk -X POST "https://${HOST}/maas-api/v1/tokens" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"expiration": "1h"}' | jq -r '.token')

# List models
curl -sSk -H "Authorization: Bearer ${MAAS_TOKEN}" \
  "https://${HOST}/maas-api/v1/models" | jq .
```

## Test Users

All passwords: `password`

- **Free**: `free-user-1`, `free-user-2`
- **Premium**: `premium-user-1`, `premium-user-2`
- **Enterprise**: `enterprise-user-1`, `enterprise-user-2`

## Troubleshooting

**Keycloak not accessible:**
```bash
kubectl get route keycloak -n keycloak
kubectl logs -n keycloak deployment/keycloak
```

**Token minting fails:**
```bash
kubectl logs -n opendatahub deployment/maas-api | grep -i keycloak
```

**Model access returns 401:**
```bash
kubectl describe authpolicy gateway-auth-policy -n openshift-ingress
```

## Next Steps

- See [Architecture-Deep-Dive.md](./Architecture-Deep-Dive.md) for detailed flow
- See [Migration-Comparison.md](./Migration-Comparison.md) for before/after comparison
