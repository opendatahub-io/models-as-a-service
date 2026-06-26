# Infrastructure Namespace Separation

## Overview

MaaS can separate infrastructure services (maas-api deployment and maas-db-config secret) from controller components into dedicated namespaces. 

**Note:** PostgreSQL itself can be external (e.g., AWS RDS, Azure Database). Only the maas-api deployment and the database connection secret (`maas-db-config`) move to the infrastructure namespace.

For details on the full namespace architecture, see [Controller Architecture](../architecture-internals/controller-architecture.md).

## Default Behavior (No Separation)

Infrastructure runs in the controller namespace. This works on all clusters including **ODH on ROSA** (which has webhook restrictions on namespace creation).

## Enabling Namespace Separation

### Quick Start

!!! warning "ROSA Restriction"
    **ODH on ROSA does not support namespace separation yet** due to OpenShift webhook restrictions that block namespace creation. The `INFRA_NAMESPACE=AUTO` deployment will fail when trying to create the infrastructure namespace.
    
    Until the ROSA restriction is lifted, **use the default behavior** (no separation) on ROSA clusters.

```bash
export INFRA_NAMESPACE=AUTO
./scripts/deploy.sh
```

**What happens:**
- maas-api deployed to `odh-ai-gateway-infra` (or `redhat-ai-gateway-infra` for RHOAI)
- `maas-db-config` secret created in infrastructure namespace
- Old maas-api in controller namespace **automatically cleaned up**
- Controller deployment automatically patched

**PostgreSQL:** If using the built-in PostgreSQL (development only), it deploys to the infrastructure namespace. If using external PostgreSQL, the connection string in `maas-db-config` points to wherever your database actually lives.

### Alternative: Kustomize Component

Add to your overlay `kustomization.yaml`:
```yaml
components:
- ../../components/infra-namespace-separation
```

Then redeploy via operator or `kustomize build . | kubectl apply -f -`.

### Custom Namespace

```bash
export INFRA_NAMESPACE=my-custom-namespace
./scripts/deploy.sh
```

## Migration & Cleanup

Migration is **automatic** when enabling separation:

1. Scripts detect existing PostgreSQL (if present) in controller namespace
2. `maas-db-config` secret copied to new infra namespace
3. New maas-api deployed to infra namespace  
4. **Controller automatically deletes old maas-api** from controller namespace
5. Services use FQDN for cross-namespace communication

## Verification

```bash
# Check separation is active
kubectl get pods -n odh-ai-gateway-infra  # Should show maas-api (and postgres if using built-in)

# Old namespace should only have controller
kubectl get pods -n opendatahub  # Should NOT show maas-api

# Validate everything works
./scripts/validate-deployment.sh
```

## References

- Component: `deployment/components/infra-namespace-separation/`
- Auto-derivation logic: mirrors Go code in `main.go` and scripts
- Namespace architecture: [Controller Architecture](../architecture-internals/controller-architecture.md)
