# Infrastructure Namespace Separation

## Overview

MaaS separates infrastructure services (maas-api deployment and maas-db-config secret) from controller components into dedicated namespaces **by default**.

**Note:** PostgreSQL itself can be external (e.g., AWS RDS, Azure Database). Only the maas-api deployment and the database connection secret (`maas-db-config`) move to the infrastructure namespace.

For details on the full namespace architecture, see [Controller Architecture](../architecture-internals/controller-architecture.md).

## Default Behavior (Namespace Separation Enabled)

By default, MaaS automatically derives the infrastructure namespace from the controller namespace:

- **ODH**: `opendatahub` (controller) → `odh-ai-gateway-infra` (infrastructure)
- **RHOAI**: `redhat-ods-applications` (controller) → `redhat-ai-gateway-infra` (infrastructure)

This provides better security isolation and simpler upgrades.

### What Gets Deployed Where

**Infrastructure namespace** (`odh-ai-gateway-infra` or `redhat-ai-gateway-infra`):
- maas-api Deployment
- maas-db-config Secret
- PostgreSQL (if using built-in, development only)

**Controller namespace** (`opendatahub` or `redhat-ods-applications`):
- maas-controller Deployment
- CRDs and RBAC
- Webhook

## Disabling Namespace Separation

!!! warning "Clusters with Namespace Creation Restrictions"
    Some clusters (including ROSA) have webhook restrictions that block namespace creation.
    
    On such clusters, you **must disable** namespace separation by setting `INFRA_NAMESPACE=""` (empty string).

### Disable via Script

```bash
export INFRA_NAMESPACE=""
./scripts/deploy.sh
```

### Disable via Kustomize

In your overlay's `kustomization.yaml`, add a patch:

```yaml
patches:
- target:
    kind: Deployment
    name: maas-controller
  patch: |-
    - op: replace
      path: /spec/template/spec/containers/0/env
      value:
      - name: INFRA_NAMESPACE
        value: ""
```

**What happens when disabled:**
- maas-api deploys to controller namespace (same as controller)
- No separate infrastructure namespace created
- Works on ROSA clusters with namespace creation restrictions

## Custom Infrastructure Namespace

You can override the auto-derived namespace with a custom value:

```bash
export INFRA_NAMESPACE=my-custom-namespace
./scripts/deploy.sh
```

Or via kustomize patch (similar to ROSA disable above, but set `value: "my-custom-namespace"`).

## Migration & Cleanup

Migration happens **automatically** when switching between modes:

1. Scripts detect existing PostgreSQL (if present) in controller namespace
2. `maas-db-config` secret copied to new infra namespace
3. New maas-api deployed to infra namespace  
4. **Controller automatically deletes old maas-api** from controller namespace
5. Services use FQDN for cross-namespace communication

!!! warning "Credential Rotation After Migration"
    After migration, the `maas-db-config` secret in the **infrastructure namespace** is the
    source of truth. If you rotate database credentials, you must update the secret in the
    infrastructure namespace (e.g., `odh-ai-gateway-infra` or `redhat-ai-gateway-infra`).

    The original secret in the controller namespace is **not** synced. Updating only the
    controller-namespace copy will cause maas-api to crash-loop with authentication failures.

    To check which namespace is active for a tenant:

    ```bash
    kubectl get maastenantconfig default-tenant -o jsonpath='{.status.infraNamespace}'
    ```

## Verification

### With Namespace Separation (Default)

```bash
# Check infrastructure namespace has maas-api
kubectl get pods -n odh-ai-gateway-infra  # Should show maas-api (and postgres if using built-in)

# Controller namespace should NOT have maas-api
kubectl get pods -n opendatahub  # Should only show maas-controller

# Validate everything works
./scripts/validate-deployment.sh
```

### Without Namespace Separation (ROSA)

```bash
# Check controller namespace has both controller and maas-api
kubectl get pods -n opendatahub  # Should show both maas-controller and maas-api

# No separate infrastructure namespace
kubectl get namespace odh-ai-gateway-infra  # Should not exist (or be empty)

# Validate everything works
INFRA_NAMESPACE="" ./scripts/validate-deployment.sh
```

## References

- Auto-derivation logic: `resolveInfraNamespace()` in `main.go` and `derive_infra_namespace()` in scripts
- Namespace architecture: [Controller Architecture](../architecture-internals/controller-architecture.md)
