# Multi-Tenant Setup

This guide covers deploying additional tenants beyond the default `models-as-a-service` tenant. Each tenant gets its own namespace, Gateway, maas-api instance, and isolated set of MaaS resources (subscriptions, auth policies, model refs).

## Prerequisites

Before creating additional tenants:

- Default tenant is working (`oc get aitenant models-as-a-service -n ai-tenants` shows `Ready`)
- Gateway API is available (`oc get gatewayclass openshift-default`)
- cert-manager operator is installed (for TLS certificate provisioning)
- MaaS controller is running with `--enable-tenant-namespace-discovery=true`

## 1. Create a Tenant Gateway

Each AITenant requires a dedicated Gateway. Gateways cannot be shared between AITenants.

Get the cluster domain and create the Gateway:

```bash
TENANT_NAME="red-team"
CLUSTER_DOMAIN=$(oc get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
GATEWAY_HOSTNAME="${TENANT_NAME}-maas.${CLUSTER_DOMAIN}"
GATEWAY_NAMESPACE="openshift-ingress"
CERT_NAME="router-certs-default"

cat <<EOF | oc apply -f -
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: ${TENANT_NAME}
  namespace: ${GATEWAY_NAMESPACE}
  annotations:
    opendatahub.io/managed: "false"
    security.opendatahub.io/authorino-tls-bootstrap: "true"
  labels:
    app.kubernetes.io/name: maas
    app.kubernetes.io/instance: ${TENANT_NAME}
    app.kubernetes.io/component: gateway
    opendatahub.io/managed: "false"
spec:
  gatewayClassName: openshift-default
  listeners:
    - name: http
      hostname: ${GATEWAY_HOSTNAME}
      port: 80
      protocol: HTTP
      allowedRoutes:
        namespaces:
          from: All
    - name: https
      hostname: ${GATEWAY_HOSTNAME}
      port: 443
      protocol: HTTPS
      allowedRoutes:
        namespaces:
          from: All
      tls:
        mode: Terminate
        certificateRefs:
          - group: ""
            kind: Secret
            name: ${CERT_NAME}
EOF
```

Create an OpenShift Route for external access:

```bash
GATEWAY_SERVICE_NAME="${TENANT_NAME}-openshift-default"

cat <<EOF | oc apply -f -
apiVersion: route.openshift.io/v1
kind: Route
metadata:
  name: ${TENANT_NAME}-gateway
  namespace: ${GATEWAY_NAMESPACE}
  labels:
    app.kubernetes.io/name: maas
    app.kubernetes.io/instance: ${TENANT_NAME}
    gateway.networking.k8s.io/gateway-name: ${TENANT_NAME}
spec:
  host: "${GATEWAY_HOSTNAME}"
  to:
    kind: Service
    name: ${GATEWAY_SERVICE_NAME}
    weight: 100
  port:
    targetPort: https
  tls:
    termination: reencrypt
    insecureEdgeTerminationPolicy: Redirect
  wildcardPolicy: None
EOF
```

Verify the Gateway is Programmed:

```bash
oc get gateway ${TENANT_NAME} -n ${GATEWAY_NAMESPACE}
```

!!! tip "Automated script"
    The `scripts/create-ai-tenant.sh` script automates Gateway, Route, and AITenant creation:
    ```bash
    ./scripts/create-ai-tenant.sh red-team
    ```

## 2. Create the AITenant CR

AITenant resources must be created in the infrastructure namespace (`ai-tenants` by default):

```bash
cat <<EOF | oc apply -f -
apiVersion: maas.opendatahub.io/v1alpha1
kind: AITenant
metadata:
  name: ${TENANT_NAME}
  namespace: ai-tenants
EOF
```

The controller bootstraps the following resources:

| Resource | Location | Name |
|----------|----------|------|
| Namespace | Cluster | `ai-tenant-${TENANT_NAME}` |
| Tenant CR | `ai-tenant-${TENANT_NAME}` | `default-tenant` |
| maas-api Deployment | Infrastructure namespace | `maas-api-${TENANT_NAME}` |
| AuthPolicy | Gateway namespace | `${TENANT_NAME}-maas-auth` |
| tenant-admin Role | `ai-tenant-${TENANT_NAME}` | `aitenant-${TENANT_NAME}-tenant-admin` |
| object-admin Role | `ai-tenants` | `aitenant-${TENANT_NAME}-object-admin` |

### Tenant Name Constraints

- Must be a valid DNS-1123 label (lowercase alphanumeric and hyphens)
- Maximum 41 characters (to fit derived resource names within the 63-character Kubernetes limit)
- Must not conflict with existing AITenant names

### Namespace Derivation

The tenant namespace is derived from the AITenant name: `ai-tenant-<aitenant-name>`. The default tenant uses `models-as-a-service` as both the AITenant name and namespace.

## 3. Verify Bootstrap Resources

Wait for the AITenant to become Ready:

```bash
oc get aitenant ${TENANT_NAME} -n ai-tenants -w
```

Verify the tenant namespace was created with correct labels:

```bash
oc get namespace ai-tenant-${TENANT_NAME} --show-labels
```

Expected labels:

- `ai-gateway.opendatahub.io/tenant=<tenant-name>`
- `maas.opendatahub.io/managed-by-aitenant=true`

Verify the Tenant CR exists:

```bash
oc get tenant default-tenant -n ai-tenant-${TENANT_NAME}
```

Verify the maas-api deployment is running in the infrastructure namespace:

```bash
INFRA_NS=$(oc get deployment -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name --no-headers | grep "maas-api-${TENANT_NAME}" | awk '{print $1}')
oc get deployment maas-api-${TENANT_NAME} -n ${INFRA_NS}
```

## 4. Grant Tenant-Admin Access

The controller creates Roles but does not create RoleBindings. Grant access with standard Kubernetes RoleBindings:

```bash
oc create rolebinding ${TENANT_NAME}-tenant-admin \
  --role=aitenant-${TENANT_NAME}-tenant-admin \
  --group=red-team-admins \
  -n ai-tenant-${TENANT_NAME}
```

See [Tenant RBAC](../configuration-and-management/tenant-rbac.md) for examples with users, groups, and ServiceAccounts.

## 5. Configure Models

Create MaaS resources in the tenant namespace to expose models:

```bash
TENANT_NS="ai-tenant-${TENANT_NAME}"

# Create a MaaSModelRef
cat <<EOF | oc apply -f -
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: my-model
  namespace: ${TENANT_NS}
spec:
  modelRef:
    name: my-llm-inference-service
    namespace: llm
EOF

# Create a MaaSAuthPolicy
cat <<EOF | oc apply -f -
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: my-model-access
  namespace: ${TENANT_NS}
spec:
  modelRefs:
    - name: my-model
      namespace: ${TENANT_NS}
  subjects:
    groups:
      - name: system:authenticated
    users: []
EOF

# Create a MaaSSubscription
cat <<EOF | oc apply -f -
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: my-subscription
  namespace: ${TENANT_NS}
spec:
  owner:
    groups:
      - name: system:authenticated
    users: []
  modelRefs:
    - name: my-model
      namespace: ${TENANT_NS}
      tokenRateLimits:
        - limit: 1000
          window: 1m
EOF
```

!!! note
    MaaSAuthPolicy and MaaSSubscription must be created in a namespace that contains a `Tenant` CR. The admission webhook rejects them otherwise.

## Webhook Validation

The AITenant admission webhook enforces two rules:

1. **Namespace restriction**: AITenant must be created in the configured infrastructure namespace (default: `ai-tenants`). Creating it in any other namespace is rejected.

2. **Gateway uniqueness**: Each AITenant must reference a unique Gateway. Two AITenants cannot use the same Gateway. The webhook checks all existing AITenants and rejects duplicates.

## Operator Behavior

### Self-Bootstrap

On startup, the controller automatically creates `AITenant/models-as-a-service` in the infrastructure namespace for the default tenant. This AITenant bootstraps the default tenant namespace and Tenant CR.

### Namespace Discovery

When `--enable-tenant-namespace-discovery=true` is set, the controller watches for namespaces with the `ai-gateway.opendatahub.io/tenant` label. Changes to this label trigger tenant reconciliation.

### Controller Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--aitenant-namespace` | `ai-tenants` | Infrastructure namespace for AITenant CRs |
| `--enable-tenant-namespace-discovery` | `false` | Watch namespaces for tenant label changes |
| `--gateway-namespace` | `openshift-ingress` | Namespace where Gateways are deployed |
| `--gateway-name` | `maas-default-gateway` | Default Gateway name for the default tenant |

## Delete a Tenant

To remove a tenant:

```bash
oc delete aitenant ${TENANT_NAME} -n ai-tenants
```

The controller finalizer cleans up:

- Tenant CR in the tenant namespace
- Controller-created Roles and RoleBindings
- Namespace labels and annotations (namespace itself is preserved)

!!! warning
    User-created RoleBindings are **not** deleted. Remove them manually before or after deleting the AITenant. Stale RoleBindings that reference recreated Roles can re-enable access.

!!! tip "Automated cleanup"
    Use the `scripts/delete-ai-tenant.sh` script for full cleanup including Gateway and Route:
    ```bash
    ./scripts/delete-ai-tenant.sh red-team
    ```

## See Also

- [AITenant CRD Reference](../reference/crds/ai-tenant.md)
- [Tenant CRD Reference](../reference/crds/tenant.md)
- [Tenant RBAC](../configuration-and-management/tenant-rbac.md)
- [Multi-Tenant Validation](multi-tenant-validation.md)
- [API Reference](../reference/api-reference.md)
