# TLS Backend Overlay

Enables end-to-end TLS for maas-api using OpenShift serving certificates.

## Contents

| File | Purpose |
|------|---------|
| `kustomization.yaml` | References base TLS overlay and policies, applies HTTPS patches |

Authorino TLS is configured by `scripts/setup-authorino-tls.sh` (run automatically by `deploy.sh` or manually).


## Traffic Flow

**External (client → gateway → maas-api):**

```
Client :443 → Gateway (TLS termination) → DestinationRule → maas-api :8443
```

**Internal (Authorino → maas-api for API key validation and metadata):**

```
Authorino → maas-api :8443 → /internal/v1/api-keys/validate
```

## Usage

### Using Unified Deployment Script (Recommended)

```bash
# TLS is enabled by default
./scripts/deploy.sh --deployment-mode kustomize

# Or explicitly enable TLS
./scripts/deploy.sh --deployment-mode kustomize --enable-tls-backend
```

The deployment script automatically:
1. Applies the kustomize overlay
2. Configures Authorino for TLS using `scripts/setup-authorino-tls.sh`
3. Restarts deployments to pick up certificates

### Manual Deployment (Advanced)

```bash
# Apply Kustomize overlay (LoadRestrictionsNone: overlay uses ../odh/params.env)
kustomize build --load-restrictor LoadRestrictionsNone deployment/overlays/tls-backend | kubectl apply -f -

# Configure Authorino for TLS (operator-managed, can't be patched via Kustomize)
./scripts/setup-authorino-tls.sh

# Restart to pick up certificates
kubectl rollout restart deployment/maas-api -n maas-api
kubectl rollout restart deployment/authorino -n kuadrant-system
```

**Note:** `scripts/setup-authorino-tls.sh` patches Authorino's service, CR, and deployment. Use `--disable-tls-backend` with `deploy.sh` to skip if you manage Authorino TLS separately.

## Why the script?

Authorino resources are managed by the Kuadrant operator. Kustomize can't patch them because they don't exist in our manifests; they're created by the operator. The script uses `kubectl patch` to configure TLS on the live resources.

## See also

- [Securing Authorino for llm-d in RHOAI](https://github.com/opendatahub-io/kserve/tree/release-v0.15/docs/samples/llmisvc/ocp-setup-for-GA#ssl-authorino)
