# Controller Performance Tuning

The maas-controller processes subscription and auth policy reconciliation within a single leader pod. By default, reconciliation is parallelized across 5 concurrent workers. This page describes how to tune concurrency for large-scale deployments.

## `--max-concurrent-reconciles`

Controls the number of concurrent reconciliation goroutines for the MaaSSubscription and MaaSAuthPolicy controllers. Other controllers (AITenant, MaaSModelRef, Tenant, Lifecycle) always use 1 to avoid conflicts on shared resources.

| Parameter | Value |
|---|---|
| Flag | `--max-concurrent-reconciles` |
| Default | 5 |
| Range | 1–10 |
| Applies to | MaaSSubscription, MaaSAuthPolicy controllers |

## Benchmark Results

Tested on RHOAI 3.5 cluster with 300 MaaSSubscriptions created simultaneously:

| MaxConcurrentReconciles | Time to all Active | Speedup |
|---|---|---|
| 1 | 236s | baseline |
| 5 (default) | 60s | 3.9x |
| 10 | 67s | 3.5x |

At default pod resource limits, values above 5 show diminishing returns due to API server contention.

## Scaling Guidance

| Deployment Size | Recommended Value | Resource Changes |
|---|---|---|
| Small (< 100 subscriptions) | 5 (default) | None |
| Medium (100–500 subscriptions) | 5 | None |
| Large (500+ subscriptions) | 5–10 | Increase controller CPU and memory |

To increase the value beyond 5, update the controller deployment args and scale resources:

```bash
# Increase concurrency
oc patch deploy maas-controller -n <controller-namespace> --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--max-concurrent-reconciles=10"}]'

# Scale resources to support higher concurrency
oc patch deploy maas-controller -n <controller-namespace> --type=json \
  -p '[{"op":"replace","path":"/spec/template/spec/containers/0/resources/limits/memory","value":"512Mi"},
      {"op":"replace","path":"/spec/template/spec/containers/0/resources/limits/cpu","value":"1"}]'
```

## Leader Election

The maas-controller uses Kubernetes leader election (`--leader-elect`). Only the leader pod runs reconcilers — additional replicas are standby for high-availability failover, not parallel processing. `MaxConcurrentReconciles` is the mechanism for parallelizing reconciliation within the single leader.
