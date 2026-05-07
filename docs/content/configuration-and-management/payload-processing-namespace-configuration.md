# Payload Processing Namespace Configuration

This reference documents the namespace-related configuration for the Inference Payload Processor (IPP) component from the [`ai-gateway-payload-processing`](https://github.com/opendatahub-io/ai-gateway-payload-processing) chart. Use this when wiring IPP into an ODH or RHOAI operator overlay or when customizing the gateway namespace.

## Gateway namespace constraint

!!! warning "IPP must run in the gateway namespace"
    The payload-processing component **must** be deployed in the same namespace as the Gateway. This is a hard Istio requirement.

    The EnvoyFilter uses `targetRefs` to reference the Gateway by name. Istio requires the EnvoyFilter to be in the same namespace as the targeted Gateway for the reference to resolve.

    The EnvoyFilter's `cluster_name` and the DestinationRule's `spec.host` both embed the gateway namespace as a fully qualified service name:

    ```
    outbound|9004||payload-processing.<gateway-namespace>.svc.cluster.local
    ```

    If the payload-processing Service is in a different namespace, Envoy cannot resolve the cluster and ext_proc calls fail silently (requests pass through without payload processing).

## Default namespace

The gateway namespace defaults to `openshift-ingress` on both ODH and RHOAI. This is configurable via the Tenant CR or kustomize parameters.

| Platform | Default gateway namespace | Configurable via |
|----------|--------------------------|------------------|
| ODH | `openshift-ingress` | `params.env` or `Tenant.spec.gatewayRef.namespace` |
| RHOAI | `openshift-ingress` | `Tenant.spec.gatewayRef.namespace` |

## Payload-processing resources

Six payload-processing resources must be created in the gateway namespace. The ClusterRoleBinding is cluster-scoped, but its `subjects[0].namespace` must reference the gateway namespace. The kustomize overlay and Tenant reconciler handle this automatically via the `gateway-namespace` parameter.

| Resource Kind | Resource Name | Namespace field |
|--------------|---------------|-----------------|
| Deployment | `payload-processing` | `metadata.namespace` |
| Service | `payload-processing` | `metadata.namespace` |
| ServiceAccount | `payload-processing` | `metadata.namespace` |
| ClusterRoleBinding | `payload-processing-reader` | `subjects[0].namespace` (cluster-scoped) |
| EnvoyFilter | `payload-processing` | `metadata.namespace` |
| DestinationRule | `payload-processing` | `metadata.namespace` |
| ConfigMap | `payload-processing-plugins` | `metadata.namespace` |

Additionally, two resources embed the gateway namespace inside field values:

| Resource | Field | Value pattern |
|----------|-------|---------------|
| EnvoyFilter | `...grpc_service.envoy_grpc.cluster_name` | `outbound\|9004\|\|payload-processing.<gateway-ns>.svc.cluster.local` |
| DestinationRule | `spec.host` | `payload-processing.<gateway-ns>.svc.cluster.local` |

## Changing the gateway namespace

=== "Kustomize overlay"

    Edit `deployment/overlays/odh/params.env`:

    ```
    gateway-namespace=<your-namespace>
    gateway-name=<your-gateway-name>
    ```

    The overlay's kustomize replacements remap all seven resource namespaces and both embedded FQDNs from this single parameter.

=== "Tenant CR"

    Set `spec.gatewayRef` on the Tenant CR:

    ```yaml
    apiVersion: maas.opendatahub.io/v1alpha1
    kind: Tenant
    metadata:
      name: default-tenant
      namespace: models-as-a-service
    spec:
      gatewayRef:
        namespace: <your-namespace>
        name: <your-gateway-name>
    ```

    The controller handles all remapping automatically.

The Gateway resource must already exist in the target namespace.

## Troubleshooting

| Symptom | Likely cause | Fix |
|---------|-------------|-----|
| `payload-processing` pod not starting | Deployed in wrong namespace; ServiceAccount not found | Verify all 7 resources are in the gateway namespace |
| EnvoyFilter has no effect; ext_proc not invoked | EnvoyFilter namespace does not match Gateway namespace | Move EnvoyFilter to the gateway namespace; check `targetRefs[0].name` matches the Gateway |
| `503` on external model inference | DestinationRule `spec.host` FQDN does not match actual Service location | Verify `cluster_name` and `spec.host` contain the correct gateway namespace |

## Related documentation

- [Controller Overview](maas-controller-overview.md)
- [External Model Setup (Tech Preview)](../install/external-model-setup.md)
- [Namespace User Permissions (RBAC)](namespace-rbac.md)
- [Architecture](../concepts/architecture.md)
