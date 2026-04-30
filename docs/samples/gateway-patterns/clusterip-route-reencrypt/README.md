# ClusterIP Gateway with OpenShift Route (Re-encrypt)

A Gateway API pattern that uses a **ClusterIP** service (no external LoadBalancer) with an
**OpenShift Route** providing external ingress and **re-encrypt** TLS termination.

## When to use

- OpenShift clusters where you want the platform Router to handle external TLS
  (wildcard or custom certificate) and re-encrypt traffic to the Gateway Service.
- Environments where LoadBalancer services are unavailable or undesirable
  (e.g., bare-metal, restricted cloud accounts).
- Production and lab environments that already use OpenShift Routes for ingress.

## How it works

```
Client ──HTTPS──▶ OpenShift Router ──re-encrypt──▶ Gateway Service (ClusterIP:443)
                                                       │
                                                       ▼
                                                   Istio/Envoy
                                                       │
                                                   HTTPRoute
                                                       │
                                                       ▼
                                                   maas-api (8443)
```

1. **GatewayClass** registers the OpenShift gateway controller.
2. **ConfigMap `gw-options`** configures the Gateway Service as `ClusterIP` and
   requests a service-ca TLS certificate via annotation.
3. **Gateway** listens on HTTPS/443 with TLS `Terminate` mode, using the
   auto-provisioned certificate.
4. **OpenShift Route** fronts the Gateway Service with `reencrypt` TLS — the
   Router terminates the client session and opens a new TLS connection to the
   Gateway using the service-ca certificate.
5. **HTTPRoute** attaches workloads (e.g., maas-api) to the Gateway.

## Prerequisites

- OpenShift cluster with Gateway API support
- Gateway API CRDs installed (included with OpenShift)
- Kuadrant / RHCL operator installed (see [Platform Setup](../../../content/install/platform-setup.md))
- `openshift-ingress` namespace accessible

## Apply

```bash
# 1. Apply GatewayClass, ConfigMap, and Gateway
kustomize build docs/samples/gateway-patterns/clusterip-route-reencrypt | kubectl apply -f -

# 2. Wait for the Gateway to be programmed
kubectl wait --for=condition=Programmed gateway/maas-default-gateway \
  -n openshift-ingress --timeout=60s

# 3. Identify the Gateway Service name
GW_SVC=$(kubectl get svc -n openshift-ingress -l gateway.networking.k8s.io/gateway-name=maas-default-gateway -o jsonpath='{.items[0].metadata.name}')
echo "Gateway Service: ${GW_SVC}"

# 4. Create the OpenShift Route (edit openshift-route.yaml first to set your hostname and Service name)
# Replace <cluster-domain> with your cluster's apps domain:
#   CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
# Replace the Service name if it differs from maas-default-gateway-openshift-default.
kubectl apply -f docs/samples/gateway-patterns/clusterip-route-reencrypt/openshift-route.yaml

# 5. Apply HTTPRoute (edit httproute.yaml to set your application namespace)
kubectl apply -f docs/samples/gateway-patterns/clusterip-route-reencrypt/httproute.yaml
```

## Verify

```bash
# Gateway should show Programmed=True
kubectl get gateway maas-default-gateway -n openshift-ingress

# Gateway Service should be ClusterIP
kubectl get svc -n openshift-ingress -l gateway.networking.k8s.io/gateway-name=maas-default-gateway

# Route should be Admitted
kubectl get route maas-gateway-route -n openshift-ingress

# TLS certificate should be provisioned
kubectl get secret maas-gw-service-tls -n openshift-ingress
```

## Common failure modes

| Symptom | Likely cause | Fix |
|---------|-------------|-----|
| Gateway stays `NotProgrammed` | GatewayClass not accepted, or ConfigMap `gw-options` missing | Check `kubectl get gatewayclass`; verify ConfigMap exists in `openshift-ingress` |
| Route shows `HostAlreadyClaimed` | Another Route uses the same hostname | Change `spec.host` to a unique FQDN |
| `503` from the Route | Gateway Service not ready or certificate not yet provisioned | Wait for service-ca to provision `maas-gw-service-tls`; check `kubectl get secret -n openshift-ingress` |
| TLS handshake failure (re-encrypt) | Service CA cert not trusted by Router | Ensure `router.openshift.io/service-ca-certificate: "true"` is set on the Route |
| `certificateRefs` name mismatch | Secret name in Gateway does not match ConfigMap annotation | Verify both reference the same Secret name |

## Customization

- **Hostname**: Replace `maas.<cluster-domain>` in `openshift-route.yaml` with your
  actual cluster domain or a custom hostname.
- **Custom TLS certificate**: Replace the service-ca annotation with a manually
  provisioned Secret and update both the ConfigMap and Gateway `certificateRefs`.
- **Multiple listeners**: Add additional listeners to the Gateway for different
  hostnames or protocols.
- **Kuadrant labels**: The `kuadrant.io/gateway: "true"` label is required for
  Kuadrant/RHCL policy attachment (RateLimitPolicy, AuthPolicy).
