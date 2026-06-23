# MaaS API TLS Overlay

Enables HTTPS for the maas-api service using OpenShift serving certificates.

## Features

- Configures maas-api to serve HTTPS on port 8443
- Uses OpenShift's `service.beta.openshift.io/serving-cert-secret-name` annotation
- OpenShift automatically provisions and rotates TLS certificates
- Includes DestinationRule for gateway→maas-api TLS origination

## Resources

| Resource | Purpose |
|----------|---------|
| `deployment-patch.yaml` | Configure maas-api container for TLS |
| `service-patch.yaml` | Add serving-cert annotation, expose port 8443 |
| `destinationrule.yaml` | Configure gateway TLS to maas-api backend |

## Why DestinationRule?

DestinationRule is the current workaround for backend TLS origination in this deployment.

**The problem:** Gateway API's HTTPRoute doesn't tell the gateway implementation "use TLS to the backend". [BackendTLSPolicy](https://gateway-api.sigs.k8s.io/api-types/backendtlspolicy/) is the standard Gateway API resource for this, but MaaS currently uses the managed `openshift-default` GatewayClass and keeps an Istio-native policy object to configure TLS origination.

**The solution:** DestinationRule tells the gateway's Envoy proxy how to talk to the backend:
- TLS origination from gateway → maas-api over HTTPS
- Controls TLS/mTLS settings for traffic leaving the gateway proxy

```
Client → Gateway (TLS termination) → [DestinationRule] → maas-api:8443 (TLS origination)
```

> **Future:** Once the managed Gateway provider supports BackendTLSPolicy for this deployment, this DestinationRule can be replaced with a standard Gateway API resource.

## Usage

### Standalone (maas-api with TLS only)

```bash
kustomize build deployment/base/maas-api/overlays/tls | kubectl apply -f -
```

### As part of Tenant reconciler

This overlay is referenced by `maas-api/deploy/overlays/odh` (the Tenant reconciler overlay).
The Tenant reconciler also applies gateway policies and configures DestinationRule namespace
via PostRender.

## Certificate Management

OpenShift's service-ca controller automatically:
1. Creates `maas-api-serving-cert` secret when service is annotated
2. Rotates certificates before expiration
3. Updates the secret in-place (pods need restart to pick up new certs)
