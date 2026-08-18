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

Get the cluster domain and create the Gateway.

The Gateway uses a per-tenant label selector for `allowedRoutes` so only explicitly
labelled namespaces can attach HTTPRoutes. The controller automatically labels
the infrastructure namespace with the gateway's `matchLabels` (see
[Automatic infrastructure namespace labeling](../configuration-and-management/gateway-patterns.md#automatic-infrastructure-namespace-labeling)).
Model namespaces still require manual labeling.

```bash
TENANT_NAME="red-team"
CLUSTER_DOMAIN=$(oc get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
GATEWAY_HOSTNAME="${TENANT_NAME}-maas.${CLUSTER_DOMAIN}"
GATEWAY_NAMESPACE="openshift-ingress"
CERT_NAME="router-certs-default"
GATEWAY_ACCESS_LABEL="maas.opendatahub.io/gateway-access-${TENANT_NAME}"

# Label model namespaces that need to attach HTTPRoutes to this tenant Gateway.
# The infrastructure namespace is labeled automatically by the controller.
# oc label namespace llm "${GATEWAY_ACCESS_LABEL}=true" --overwrite  # repeat for model namespaces

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
    - name: https
      hostname: ${GATEWAY_HOSTNAME}
      port: 443
      protocol: HTTPS
      allowedRoutes:
        namespaces:
          from: Selector
          selector:
            matchLabels:
              ${GATEWAY_ACCESS_LABEL}: "true"
      tls:
        mode: Terminate
        certificateRefs:
          - group: ""
            kind: Secret
            name: ${CERT_NAME}
EOF
```

!!! note "Route auto-provisioning"
    On OpenShift with `gatewayClassName: openshift-default`, the Gateway controller typically auto-provisions a Route for external access. Check whether a Route was created automatically before creating one manually:
    ```bash
    oc get route -n ${GATEWAY_NAMESPACE} -l gateway.networking.k8s.io/gateway-name=${TENANT_NAME}
    ```
    If no Route was auto-provisioned, create one manually:

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
    The `scripts/create-ai-tenant.sh` script automates Gateway and AITenant creation.
    For multi-tenant deployments use the per-gateway label selector:
    ```bash
    NAMESPACE_SELECTOR_LABELS="maas.opendatahub.io/gateway-access-red-team=true" \
      ./scripts/create-ai-tenant.sh red-team
    ```
    The controller labels the infrastructure namespace automatically. Label any
    model namespaces manually:
    ```bash
    oc label namespace llm maas.opendatahub.io/gateway-access-red-team=true --overwrite
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
spec:
  gateway:
    name: ${TENANT_NAME}
  # Optional: configure an external OIDC provider for this tenant.
  # Each tenant can use a different IdP realm/client. Omit to rely on OpenShift TokenReview only.
  # oidc:
  #   issuerUrl: "https://keycloak.example.com/realms/${TENANT_NAME}"
  #   clientId: ${TENANT_NAME}-maas
  #   ttl: 300
EOF
```

For OIDC configuration details, per-tenant isolation, and verification steps, see [External OIDC Configuration](../advanced-administration/external-oidc.md).

The controller bootstraps the following resources:

| Resource | Location | Name |
|----------|----------|------|
| Namespace | Cluster | `ai-tenant-${TENANT_NAME}` |
| MaasTenantConfig CR | `ai-tenant-${TENANT_NAME}` | `default-tenant` |
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

Verify the MaasTenantConfig CR exists:

```bash
oc get maastenantconfig default-tenant -n ai-tenant-${TENANT_NAME}
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

Create the MaaSModelRef in the **model namespace** (co-located with the backend resource) and use `tenantRef` to associate it with the tenant's gateway. MaaSAuthPolicy and MaaSSubscription must be created in the **tenant namespace** (where the MaasTenantConfig CR lives).

```bash
TENANT_NS="ai-tenant-${TENANT_NAME}"
MODEL_NS="llm"   # namespace where the LLMInferenceService runs

# The model namespace must carry the tenant Gateway's access label
# so the controller-generated HTTPRoute can attach.
oc label namespace "${MODEL_NS}" "maas.opendatahub.io/gateway-access-${TENANT_NAME}=true" --overwrite

# Create a MaaSModelRef in the model namespace.
# tenantRef tells the controller to resolve the gateway from this AITenant
# instead of using namespace-based inference (which defaults to the default tenant).
cat <<EOF | oc apply -f -
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: my-model
  namespace: ${MODEL_NS}
spec:
  modelRef:
    kind: LLMInferenceService
    name: my-llm-inference-service
  tenantRef: ${TENANT_NAME}
EOF

# Create a MaaSAuthPolicy in the tenant namespace.
# modelRefs point to the MaaSModelRef by name and namespace.
cat <<EOF | oc apply -f -
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: my-model-access
  namespace: ${TENANT_NS}
spec:
  modelRefs:
    - name: my-model
      namespace: ${MODEL_NS}
  subjects:
    groups:
      - name: system:authenticated
    users: []
EOF

# Create a MaaSSubscription in the tenant namespace.
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
      namespace: ${MODEL_NS}
      tokenRateLimits:
        - limit: 1000
          window: 1m
EOF
```

!!! note
    MaaSAuthPolicy and MaaSSubscription must be created in a namespace that contains a `MaasTenantConfig` CR. The admission webhook rejects them otherwise.

!!! tip "MaaSModelRef for the default tenant"
    For models that belong to the default tenant, `tenantRef` can be omitted. The controller falls back to namespace-based gateway resolution. See [MaaSModelRef CRD Reference](../reference/crds/maas-model-ref.md#multi-tenant-models) for details.

## Webhook Validation

The AITenant admission webhook enforces two rules:

1. **Namespace restriction**: AITenant must be created in the configured infrastructure namespace (default: `ai-tenants`). Creating it in any other namespace is rejected.

2. **Gateway uniqueness**: Each AITenant must reference a unique Gateway. Two AITenants cannot use the same Gateway. The webhook checks all existing AITenants and rejects duplicates.

## Operator Behavior

### Self-Bootstrap

On startup, the controller automatically creates `AITenant/models-as-a-service` in the infrastructure namespace for the default tenant. This AITenant bootstraps the default tenant namespace and `MaasTenantConfig` CR.

### Namespace Discovery

When `--enable-tenant-namespace-discovery=true` is set, the controller watches for namespaces with the `ai-gateway.opendatahub.io/tenant` label. Changes to this label trigger tenant reconciliation.

### Controller Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--aitenant-namespace` | `ai-tenants` | Namespace for AITenant CRs |
| `--enable-tenant-namespace-discovery` | `false` | Watch namespaces for tenant label changes |
| `--gateway-namespace` | `openshift-ingress` | Namespace where Gateways are deployed |
| `--gateway-name` | `maas-default-gateway` | Default Gateway name for the default tenant |
| `--infra-namespace` | `AUTO` | Infrastructure namespace for maas-api and maas-db-config. See [Infrastructure Namespace Migration](../configuration-and-management/infra-namespace-migration.md) |

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
    Use the `scripts/delete-ai-tenant.sh` script for full cleanup including Gateway:
    ```bash
    ./scripts/delete-ai-tenant.sh red-team
    ```

## Known Limitations

!!! warning "External models are only supported for the default tenant"
    External models work for the default tenant only. Non-default tenant IPP instances have the ExternalModel controller disabled to prevent HTTPRoute conflicts. See [External Model Setup — Multi-Tenant Limitation](external-model-setup.md#multi-tenant-limitation) for details.

## See Also

- [AITenant CRD Reference](../reference/crds/ai-tenant.md)
- [MaasTenantConfig CRD Reference](../reference/crds/tenant.md)
- [Tenant RBAC](../configuration-and-management/tenant-rbac.md)
- [Multi-Tenant Validation](multi-tenant-validation.md)
- [API Reference](../reference/api-reference.md)
