# Shared Patches Component

## Overview

This is a **Kustomize Component** that provides shared configuration used by all MaaS deployment overlays. It eliminates duplication by centralizing common patches and replacements that apply regardless of deployment mode (TLS backend, HTTP backend, ODH operator).

## Purpose

The shared-patches component solves a specific problem: all MaaS overlays need the same base transformations:
- Environment variables for the maas-api deployment
- Image replacements from params.env
- Gateway configuration (namespace, name)
- **AuthPolicy URL placeholder replacement** (`maas-api.placehold.svc` → `maas-api.<namespace>.svc`)

Without this component, these transformations would be duplicated across `tls-backend`, `http-backend`, and `odh` overlays, violating the DRY principle.

## How It Works

### Kustomize Components

Components are a Kustomize feature for reusable configuration that can be included in multiple overlays:

```yaml
# In an overlay's kustomization.yaml
components:
  - ../../components/shared-patches
```

When an overlay includes this component, it inherits:
- All `resources` (the common ConfigMap)
- All `patches` (environment variables)
- All `replacements` (image URLs, gateway config, placeholder URL fix)

The overlay can then add its own overlay-specific patches/replacements on top.

## What's Included

### Resources
- `../../overlays/common` - ConfigMap with parameters from `params.env`:
  - `maas-api-image`
  - `maas-controller-image`
  - `gateway-namespace`
  - `gateway-name`
  - `app-namespace`
  - `api-key-max-expiration-days`

### Patches
- **maas-api environment variables**:
  - `GATEWAY_NAMESPACE` (from ConfigMap)
  - `GATEWAY_NAME` (from ConfigMap)
  - `API_KEY_MAX_EXPIRATION_DAYS` (from ConfigMap, optional)

### Replacements

All replacements use the ConfigMap (`maas-parameters`) as the source:

1. **maas-api image**: Replaces container image in maas-api Deployment
2. **maas-controller image**: Replaces container image in maas-controller Deployment
3. **gateway-namespace**: Replaces namespace in HTTPRoute parentRefs
4. **gateway-name**: Replaces gateway name in HTTPRoute parentRefs
5. **app-namespace**: **Fixes AuthPolicy placeholder URL**
   - From: `https://maas-api.placehold.svc.cluster.local:8443/...`
   - To: `https://maas-api.<app-namespace>.svc.cluster.local:8443/...`

## Usage

### In Overlays

Overlays reference this component in their `kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
  - ../../base/maas-api/overlays/tls
  - ../../base/maas-controller/default

components:
  - ../../components/shared-patches  # ← Includes shared configuration

# Overlay-specific replacements can be added here
replacements:
  - source: ...  # Overlay-specific only
```

### Current Users

This component is used by:
- `deployment/overlays/tls-backend/` - Kustomize deployment mode with TLS
- `deployment/overlays/http-backend/` - Kustomize deployment mode without TLS
- `deployment/overlays/odh/` - ODH operator deployment

## Configuration

The component reads all parameter values from `deployment/overlays/common/params.env`. See that file for the current configuration values.

To change these values, edit `params.env` - all overlays will pick up the changes automatically.

### Security Best Practice: Image Digest Pinning

For production deployments, use **immutable image digests** instead of mutable tags like `:latest`:

```env
# ❌ Mutable tag (development/testing only)
maas-api-image=quay.io/opendatahub/maas-api:latest

# ✅ Immutable digest (production recommended)
maas-api-image=quay.io/opendatahub/maas-api:v1.2.3@sha256:abc123...
```

**Why?**
- Mutable tags (`:latest`, `:v1.0`) can point to different images over time
- Digest pinning (`@sha256:...`) ensures exact image reproducibility
- Prevents supply chain attacks where tags are overwritten

**How to get digests:**
```bash
# Pull and inspect the image
podman pull quay.io/opendatahub/maas-api:latest
podman inspect quay.io/opendatahub/maas-api:latest | grep Digest
```

## Testing

Verify the component works correctly:

```bash
# Build an overlay that uses this component
kustomize build deployment/overlays/tls-backend

# Check that placeholder URL is replaced
kustomize build deployment/overlays/tls-backend | grep "api-keys/validate"
# Should output:
# url: https://maas-api.opendatahub.svc.cluster.local:8443/internal/v1/api-keys/validate

# Check that images are replaced
kustomize build deployment/overlays/tls-backend | grep "image:" | grep maas-api
# Should output:
# image: quay.io/opendatahub/maas-api:latest
```

## Design

This component uses Kustomize's native composition features to share configuration across deployment overlays. Components provide:
- Idiomatic Kustomize solution compatible with GitOps tools (ArgoCD, Flux)
- Single source of truth for shared configuration
- Declarative and version controlled
- Testable with `kustomize build`

## Troubleshooting

### Component not found error
```text
Error: unable to find one of 'kustomization.yaml', 'kustomization.yml' or 'Kustomization'
```

**Solution**: Check that the relative path to the component is correct. From an overlay, it should be `../../components/shared-patches`.

### Replacement not working
```yaml
url: https://maas-api.placehold.svc...  # Still has placeholder!
```

**Solution**:
1. Check that `params.env` has `app-namespace=<your-namespace>`
2. Verify the overlay includes the component: `components: [../../components/shared-patches]`
3. Check Kustomize version (needs v5.7.0+)

### Kustomize version too old
```text
Error: components not supported in this version
```

**Solution**: Upgrade to Kustomize v5.7.0 or later. OpenShift 4.x includes a modern version.

## Related Files

- `deployment/overlays/common/params.env` - Parameter values
- `deployment/overlays/tls-backend/kustomization.yaml` - TLS overlay using this component
- `deployment/overlays/http-backend/kustomization.yaml` - HTTP overlay using this component
- `deployment/overlays/odh/kustomization.yaml` - ODH overlay using this component
- `deployment/base/maas-api/policies/auth-policy.yaml` - Contains the placeholder URL

## References

- [Kustomize Components Documentation](https://kubectl.docs.kubernetes.io/guides/config_management/components/)
- [Kustomize Replacements](https://kubectl.docs.kubernetes.io/references/kustomize/kustomization/replacements/)
