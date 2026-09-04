# Gateway Resource Tuning

The MaaS gateway pod (`maas-default-gateway-openshift-default`) runs an `istio-proxy` container whose CPU and memory limits are set by the OpenShift Gateway API / Sail Operator. The defaults (2 CPU cores, 1Gi memory) can cause problems under load.

Performance testing found that:

- **CPU**: 2 cores throttle at ~500 RPS. 4+ cores recommended for high-concurrency workloads.
- **Memory**: 1Gi leaves little headroom once RHCL Wasm filters (Kuadrant AuthPolicy, RateLimitPolicy) compile at startup. 2Gi minimum recommended with RHCL enabled.

## Symptoms

- Gateway pod `OOMKilled` (exit code 137) shortly after startup or under load
- CPU throttling causing latency spikes and 503 errors at moderate RPS
- `oc get pods -n openshift-ingress` shows repeated restarts on `maas-default-gateway-openshift-default`

## Vertical scaling: ConfigMap + parametersRef

!!! note
    Customizing gateway pod resources through `spec.infrastructure.parametersRef` is not directly supported in OpenShift yet. This is a field workaround that relies on Istio's ConfigMap merge behavior for Gateway deployments.

The Gateway API `spec.infrastructure.parametersRef` field allows referencing a ConfigMap that Istio merges into the generated Deployment. Unlike direct `oc patch deployment` commands (which the Sail Operator reverts on reconciliation), this approach persists across operator restarts.

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

Adjust CPU and memory values based on your workload. See [Sizing guidance](#sizing-guidance) below for recommendations.

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

On OCP 4.22+, Gateway API supports autoscaling the gateway deployment via a standard Kubernetes HorizontalPodAutoscaler. This allows the gateway to scale out under load rather than relying solely on vertical resource increases.

### Create and apply an HPA for the gateway

Save the following as `gateway-hpa.yaml` and apply it:

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: maas-default-gateway-hpa
  namespace: openshift-ingress
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: maas-default-gateway-openshift-default
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
```

```bash
kubectl apply -f gateway-hpa.yaml
```

Adjust `maxReplicas` and utilization thresholds based on your traffic patterns. `minReplicas: 2` is recommended for availability during rolling updates and node disruptions. For the HPA `averageUtilization` metric to work, the gateway pod must have CPU and memory `requests` set (see the ConfigMap approach above).

For production deployments, consider adding a PodDisruptionBudget to prevent all gateway replicas from being evicted simultaneously during cluster maintenance:

```yaml
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: maas-default-gateway-pdb
  namespace: openshift-ingress
spec:
  minAvailable: 1
  selector:
    matchLabels:
      gateway.networking.k8s.io/gateway-name: maas-default-gateway
```

### Verify HPA

```bash
# Check HPA status and current replica count
kubectl get hpa maas-default-gateway-hpa -n openshift-ingress

# Watch scaling events
kubectl describe hpa maas-default-gateway-hpa -n openshift-ingress
```

!!! note
    Gateway HPA support requires OCP 4.22 or later. On earlier versions, scale the gateway manually by combining the ConfigMap approach above with a `replicas` field in the deployment patch.

## Sizing guidance

| Workload | CPU (limit) | Memory (limit) | Notes |
|----------|-------------|----------------|-------|
| Low volume (< 100 RPS) | `2` (default) | `1Gi` (default) | Default limits are sufficient |
| Medium (100-500 RPS) | `2`-`4` | `1.5Gi`-`2Gi` | Raise memory if RHCL Wasm filters are enabled |
| High concurrency (500+ RPS) | `4`+ | `2Gi`+ | Consider combining vertical scaling with HPA |

## References

- [Istio Gateway API automated deployment](https://istio.io/latest/docs/tasks/traffic-management/ingress/gateway-api/#automated-deployment)
- [Gateway Patterns](../configuration-and-management/gateway-patterns.md)
