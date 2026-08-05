# Gateway Resource Tuning

The MaaS gateway pod (`maas-default-gateway-openshift-default`) runs an `istio-proxy` container whose CPU and memory limits are set by the OpenShift Gateway API / Sail Operator. The defaults (2 CPU cores, 1Gi memory) can cause problems under load.

PSAP performance testing ([PSAP-2514](https://issues.redhat.com/browse/PSAP-2514)) found that:

- **CPU**: 2 cores throttle at ~500 RPS. 4+ cores recommended for high-concurrency workloads.
- **Memory**: 1Gi leaves little headroom once RHCL Wasm filters (Kuadrant AuthPolicy, RateLimitPolicy) compile at startup. 2Gi minimum recommended with RHCL enabled.

Related: [RHOAIENG-68589](https://issues.redhat.com/browse/RHOAIENG-68589) (gateway pod OOMKilled at 1Gi with RHCL Wasm EnvoyFilters).

## Symptoms

- Gateway pod `OOMKilled` (exit code 137) shortly after startup or under load
- CPU throttling causing latency spikes and 503 errors at moderate RPS
- `oc get pods -n openshift-ingress` shows repeated restarts on `maas-default-gateway-openshift-default`

## Why `oc patch deployment` does not work

The Sail Operator (`istiod-openshift-gateway`) reconciles the gateway Deployment with hardcoded resource limits. Any manual `oc patch deployment` is reverted within seconds.

## Workaround: ConfigMap + parametersRef

!!! warning "Field Workaround"
    This procedure is a **community/field workaround**, not Red Hat supported for production MaaS installs. It relies on Istio's ConfigMap merge behavior for Gateway deployments. A supported path will be available when [RFE-9090](https://issues.redhat.com/browse/RFE-9090) (Gateway API: Support Gateway service customization) ships in a future OCP/Istio release.

The Gateway API `spec.infrastructure.parametersRef` field allows referencing a ConfigMap that Istio merges into the generated Deployment. Unlike direct deployment patches, this survives operator reconciliation.

### 1. Create the ConfigMap

Create a ConfigMap in `openshift-ingress` with a `deployment` key containing a strategic-merge patch for the istio-proxy container resources:

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

Adjust CPU and memory values based on your workload. The values above follow PSAP-2514 recommendations for high-concurrency deployments with RHCL Wasm filters.

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
kubectl get gateway maas-default-gateway -n openshift-ingress \
  -o jsonpath='{.spec.infrastructure.parametersRef}' | jq .

# Verify the istio-proxy container picked up the new resource limits
kubectl get deploy maas-default-gateway-openshift-default -n openshift-ingress \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="istio-proxy")].resources}' | jq .

# Confirm the gateway pod is running without restarts
kubectl get pods -n openshift-ingress \
  -l gateway.networking.k8s.io/gateway-name=maas-default-gateway
```

## Sizing guidance

| Workload | CPU (limit) | Memory (limit) | Notes |
|----------|-------------|----------------|-------|
| Low volume (< 100 RPS) | `2` (default) | `1Gi` (default) | Default limits are sufficient |
| Medium (100-500 RPS) | `2`-`4` | `1.5Gi`-`2Gi` | Raise memory if RHCL Wasm filters are enabled |
| High concurrency (500+ RPS) | `4`+ | `2Gi`+ | PSAP-2514 tested up to 4 cores; scale further based on observed throttling |

## Supported path (future)

[RFE-9090](https://issues.redhat.com/browse/RFE-9090) (Approved) will add supported Gateway Deployment customization to the OpenShift Gateway API / Istio integration. Once that ships, the ConfigMap workaround can be replaced with the officially supported mechanism.

Track the RFE for availability in your OCP version.

## References

- [PSAP-2514](https://issues.redhat.com/browse/PSAP-2514) - Performance findings for MaaS gateway CPU and memory
- [RHOAIENG-68589](https://issues.redhat.com/browse/RHOAIENG-68589) - Gateway pod OOMKilled at 1Gi
- [RFE-9090](https://issues.redhat.com/browse/RFE-9090) - Gateway API: Support Gateway service customization
- [#wg-maas Slack thread](https://redhat-internal.slack.com/archives/C094HF5KD6E/p1781514371299959?thread_ts=1781514371.299959) - Original OOM report and workaround discussion
