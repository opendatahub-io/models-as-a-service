# HTTP Backend Overlay

This overlay deploys the MaaS API with HTTP (no TLS) and includes all gateway-level policies.

## What's Included

- `base/maas-api` — Deployment, Service, HTTPRoute, RBAC, maas-api-auth-policy
- maas-controller provides gateway-level auth and rate limit policies

## Usage

```bash
kustomize build --load-restrictor LoadRestrictionsNone deployment/overlays/http-backend | kubectl apply -f -
```

`LoadRestrictionsNone` is required because this overlay references `../odh/params.env` outside the overlay root.

## When to Use

- Development environments
- When TLS is handled at the ingress/mesh layer
- Testing without certificate complexity

For production with end-to-end TLS, use `overlays/tls-backend` instead.

