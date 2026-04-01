# Gateway Setup

This guide covers creating the MaaS Gateway, which serves as the single entry point for
all model inference traffic and MaaS API requests.

!!! info "What is the MaaS Gateway?"
    The `maas-default-gateway` is a standard Kubernetes [Gateway API](https://gateway-api.sigs.k8s.io/)
    resource managed by OpenShift's built-in gateway controller. It handles TLS termination
    and hostname-based routing. The Gateway itself is independent of Kuadrant/RHCL — it is
    purely an OpenShift networking resource. Kuadrant/RHCL policies (authentication, rate
    limiting) attach to the Gateway later but are not required to create it.

## Prerequisites

Before creating the Gateway, ensure you have:

- OpenShift 4.19.9+ cluster with admin access
- [Operator Setup](platform-setup.md) completed (ODH or RHOAI operator installed)
- GatewayClass `openshift-default` exists and is accepted

```bash
kubectl get gatewayclass openshift-default
```

```text
NAME                CONTROLLER                           ACCEPTED   AGE
openshift-default   openshift.io/gateway-controller/v1   True       5m
```

If the GatewayClass does not exist, create it:

```yaml
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: openshift-default
spec:
  controllerName: "openshift.io/gateway-controller/v1"
EOF
```

Wait until `ACCEPTED` shows `True` before proceeding:

```bash
kubectl wait --for=jsonpath='{.status.conditions[?(@.type=="Accepted")].status}'=True \
  gatewayclass/openshift-default --timeout=60s
```

## Step 1: Determine Your Cluster Values

You need two values to create the Gateway: your **cluster domain** and your **TLS certificate secret name**.

### Cluster Domain

```bash
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
echo "Cluster domain: ${CLUSTER_DOMAIN}"
```

The Gateway hostname will be `maas.${CLUSTER_DOMAIN}`. This hostname must be resolvable from
wherever you plan to send inference requests. On most OpenShift clusters, `*.apps.<cluster>`
has a wildcard DNS entry, so `maas.apps.<cluster>` resolves automatically.

### TLS Certificate

The HTTPS listener requires a TLS certificate secret in the `openshift-ingress` namespace.
The default OpenShift ingress certificate is a wildcard cert (`*.apps.<cluster>`) that
already covers the `maas.apps.<cluster>` hostname.

Detect the default ingress certificate name:

```bash
CERT_NAME=$(kubectl get ingresscontroller default -n openshift-ingress-operator \
  -o jsonpath='{.spec.defaultCertificate.name}' 2>/dev/null)
[[ -z "$CERT_NAME" ]] && CERT_NAME="router-certs-default"
echo "TLS secret: ${CERT_NAME}"
```

Verify the secret exists:

```bash
kubectl get secret "${CERT_NAME}" -n openshift-ingress
```

??? note "Secret not found?"
    If the secret does not exist, list all secrets in the `openshift-ingress` namespace and
    look for a TLS secret:

    ```bash
    kubectl get secrets -n openshift-ingress
    ```

    Set `CERT_NAME` to the name of the TLS secret you find:

    ```bash
    CERT_NAME="<your-secret-name>"
    ```

??? note "Using a custom TLS certificate?"
    If you have your own TLS certificate, ensure the certificate's Subject Alternative Name
    (SAN) covers `maas.<cluster-domain>` (or uses a matching wildcard). Create the secret:

    ```bash
    kubectl create secret tls maas-tls-cert \
      -n openshift-ingress \
      --cert=path/to/tls.crt \
      --key=path/to/tls.key

    CERT_NAME="maas-tls-cert"
    ```

### Validate Before Proceeding

Confirm both values are set:

```bash
echo "CLUSTER_DOMAIN=${CLUSTER_DOMAIN}"
echo "CERT_NAME=${CERT_NAME}"
```

Both values must be non-empty. If either is blank, revisit the steps above before continuing.

## Step 2: Create the Gateway

```yaml
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: maas-default-gateway
  namespace: openshift-ingress
  annotations:
    security.opendatahub.io/authorino-tls-bootstrap: "true"
    opendatahub.io/managed: "false"
spec:
  gatewayClassName: openshift-default
  listeners:
   - name: http
     hostname: maas.${CLUSTER_DOMAIN}
     port: 80
     protocol: HTTP
     allowedRoutes:
       namespaces:
         from: All
   - name: https
     hostname: maas.${CLUSTER_DOMAIN}
     port: 443
     protocol: HTTPS
     allowedRoutes:
       namespaces:
         from: All
     tls:
       certificateRefs:
       - group: ""
         kind: Secret
         name: ${CERT_NAME}
       mode: Terminate
EOF
```

| Field | Description |
|-------|-------------|
| `hostname` | `maas.<cluster-domain>` — all MaaS traffic is routed through this hostname |
| `listeners` | HTTP (port 80) and HTTPS (port 443), accepting routes from all namespaces |
| `tls.certificateRefs` | References the TLS secret in the `openshift-ingress` namespace |
| `tls.mode: Terminate` | TLS is terminated at the Gateway; backend traffic is handled separately (see [TLS Configuration](../configuration-and-management/tls-configuration.md)) |

| Annotation | Description |
|------------|-------------|
| `security.opendatahub.io/authorino-tls-bootstrap` | When `"true"`, the ODH Model Controller creates an EnvoyFilter for Gateway-to-Authorino TLS. This is processed once modelsAsService is enabled in the DataScienceCluster. |
| `opendatahub.io/managed` | When `"false"`, disables automatic AuthPolicy creation so MaaS manages its own policies |

!!! note "HTTPS only"
    If you do not need an HTTP listener (port 80), remove the `http` listener block.

## Step 3: Verify the Gateway

Wait for the Gateway to reach `Programmed` state:

```bash
kubectl wait --for=condition=Programmed gateway/maas-default-gateway \
  -n openshift-ingress --timeout=300s
```

```bash
kubectl get gateway maas-default-gateway -n openshift-ingress
```

```text
NAME                   CLASS               ADDRESS        PROGRAMMED   AGE
maas-default-gateway   openshift-default   <ip-or-host>   True         30s
```

!!! warning "Gateway not Programmed?"
    Inspect the status conditions for the specific reason:

    ```bash
    kubectl describe gateway maas-default-gateway -n openshift-ingress
    ```

    **Common causes:**

    | Condition | Cause | Fix |
    |-----------|-------|-----|
    | `ResolvedRefs=False` | TLS secret not found or not a valid TLS secret | Verify: `kubectl get secret ${CERT_NAME} -n openshift-ingress -o jsonpath='{.type}'` — must be `kubernetes.io/tls` |
    | `Accepted=False` | GatewayClass not accepted or does not exist | Check: `kubectl get gatewayclass openshift-default` |
    | No conditions at all | Gateway controller not running | Verify OpenShift ingress operator is healthy: `kubectl get pods -n openshift-ingress-operator` |

## Next Steps

The Gateway is now ready. Continue with:

1. [Install MaaS Components](maas-setup.md) — Database, DataScienceCluster configuration
2. [Model Setup (On Cluster)](model-setup.md) — Deploy your first model
3. [Validation](validation.md) — End-to-end verification
