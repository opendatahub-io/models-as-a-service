# Gateway Resource Tuning

The MaaS gateway pod (`maas-default-gateway-openshift-default`) runs an `istio-proxy` container whose CPU and memory limits are set by the OpenShift Gateway API / Sail Operator. The defaults (2 CPU cores, 1Gi memory) can be too low for high-concurrency workloads.

This page covers how to raise those limits vertically and how to scale the gateway horizontally.

## Symptoms

- Gateway pod `OOMKilled` (exit code 137) shortly after startup or under load
- CPU throttling causing latency spikes and 503 errors at moderate RPS
- `oc get pods -n openshift-ingress` shows repeated restarts on `maas-default-gateway-openshift-default`

## Vertical scaling: ConfigMap + parametersRef

!!! note
    Customizing gateway pod resources through `spec.infrastructure.parametersRef` is not directly supported in OpenShift yet. This is a field workaround that relies on Istio's ConfigMap merge behavior for Gateway deployments.

The Gateway API `spec.infrastructure.parametersRef` field allows referencing a ConfigMap that Istio merges into the generated Deployment. Use this rather than patching the Deployment directly, as direct patches do not persist across operator reconciliation.

### 1. Create and apply the ConfigMap

Save the following as `gateway-resource-config.yaml` and apply it. The `deployment` key contains a strategic-merge patch for the istio-proxy container resources:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: maas-default-gateway-config
  namespace: openshift-ingress
data:
  deployment: |
    spec:
      template:
        spec:
          containers:
          - name: istio-proxy
            resources:
              requests:
                cpu: 200m
                memory: 512Mi
              limits:
                cpu: "4"
                memory: 2Gi
```

```bash
kubectl apply -f gateway-resource-config.yaml
```

Adjust CPU and memory values based on your workload.

### 2. Wire parametersRef on the Gateway

Patch the MaaS gateway to reference the ConfigMap:

```bash
kubectl patch gateway maas-default-gateway -n openshift-ingress --type=merge \
  -p '{
    "spec": {
      "infrastructure": {
        "parametersRef": {
          "group": "",
          "kind": "ConfigMap",
          "name": "maas-default-gateway-config"
        }
      }
    }
  }'
```

If your Gateway already has an `infrastructure.parametersRef` (e.g. for ClusterIP service type via `gw-options`), you need to merge both configurations into a single ConfigMap. A Gateway can only reference one ConfigMap. See [Gateway Patterns](../configuration-and-management/gateway-patterns.md) for existing parametersRef usage.

### 3. Verify

```bash
# Confirm parametersRef is set on the Gateway
kubectl get gateway maas-default-gateway -n openshift-ingress -o json \
  | jq '.spec.infrastructure.parametersRef'

# Verify the istio-proxy container picked up the new resource limits
kubectl get deploy maas-default-gateway-openshift-default -n openshift-ingress -o json \
  | jq '.spec.template.spec.containers[] | select(.name=="istio-proxy") | .resources'

# Confirm the gateway pod is running without restarts
kubectl get pods -n openshift-ingress \
  -l gateway.networking.k8s.io/gateway-name=maas-default-gateway
```

## Horizontal scaling: HPA

!!! warning
    Do not create a standalone HorizontalPodAutoscaler for the gateway deployment. Recent OpenShift releases provision an HPA for managed gateways automatically, and a second HPA targeting the same deployment leaves both controllers fighting over the replica count. The autoscaler reports `AmbiguousSelector` and stops scaling.

Check whether one already exists before doing anything else:

```bash
kubectl get hpa -n openshift-ingress \
  -o custom-columns='NAME:.metadata.name,TARGET:.spec.scaleTargetRef.name,MIN:.spec.minReplicas,MAX:.spec.maxReplicas'
```

Either way, tune the gateway's autoscaling through the same `parametersRef` ConfigMap used for resources. The `horizontalPodAutoscaler` key overlays the generated HPA when one is managed for you, and creates one when it is not, so a single procedure works on both.

### Add autoscaling to the ConfigMap

Extend `gateway-resource-config.yaml` with the `horizontalPodAutoscaler` key:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: maas-default-gateway-config
  namespace: openshift-ingress
data:
  deployment: |
    spec:
      template:
        spec:
          containers:
          - name: istio-proxy
            resources:
              requests:
                cpu: 200m
                memory: 512Mi
              limits:
                cpu: "4"
                memory: 2Gi
  horizontalPodAutoscaler: |
    spec:
      minReplicas: 2
      maxReplicas: 5
      metrics:
      - type: Resource
        resource:
          name: cpu
          target:
            type: Utilization
            averageUtilization: 80
      - type: Resource
        resource:
          name: memory
          target:
            type: Utilization
            averageUtilization: 80
  podDisruptionBudget: |
    spec:
      minAvailable: 1
```

```bash
kubectl apply -f gateway-resource-config.yaml
```

Adjust `maxReplicas` and utilization thresholds based on your traffic patterns. `minReplicas: 2` keeps the gateway available during rolling updates and node disruptions.

Two things to keep in mind:

- `averageUtilization` is a percentage of the container's resource **requests**, so the `deployment` key above must set `requests` for CPU and memory. Without them the HPA has no denominator and reports `<unknown>` for the metric.
- Keep the PodDisruptionBudget `minAvailable` below `minReplicas`. If they are equal, voluntary evictions such as node drains are blocked entirely.

### Verify

```bash
# Confirm the HPA reflects the values from the ConfigMap
kubectl get hpa -n openshift-ingress

# Check that metrics are being read, not reported as <unknown>
kubectl describe hpa -n openshift-ingress \
  -l gateway.networking.k8s.io/gateway-name=maas-default-gateway
```

An `AmbiguousSelector` condition in the `describe` output means more than one HPA targets the gateway deployment. Remove the one you created manually and keep the ConfigMap-driven configuration.

### If the ConfigMap approach is unavailable

On releases where the gateway implementation does not honour the `horizontalPodAutoscaler` key, scale the gateway manually instead by setting `spec.replicas` in the `deployment` key of the same ConfigMap. Do not mix manual replicas with an active HPA.

## References

- [Istio Gateway API automated deployment](https://istio.io/latest/docs/tasks/traffic-management/ingress/gateway-api/#automated-deployment)
- [Gateway Patterns](../configuration-and-management/gateway-patterns.md)
