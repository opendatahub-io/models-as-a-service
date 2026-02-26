# Persisting Limitador Metric Counts

By default, Limitador stores its rate-limiting counters in memory. This provides high performance but has a significant drawback: if a Limitador pod restarts, scales down, or is rescheduled, all hit counts are lost.

For persistent, production-ready rate limiting where counts are maintained across pod lifecycles, you must configure Limitador to use an external Redis backend.

!!! warning
    **Production Considerations**: The basic Redis setup script provided in this document is intended for local development and validation only. For production deployments, follow the official Red Hat documentation for proper Redis configuration and high availability.

---

## Table of Contents

. [Requirements for Persistent Counts](#requirements-for-persistent-counts)
. [Example Limitador CR Configuration](#example-limitador-cr-configuration)
. [Local Validation Script](#local-validation-script-basic-redis)
. [How to Validate Persistence](#how-to-validate-persistence)
. [Related Documentation](#related-documentation)

---

## Requirements for Persistent Counts

To enable persistence, two conditions must be met:

. **A Running Redis Instance**: A Redis instance must be deployed and network-accessible from within the Kubernetes cluster.

. **Limitador Custom Resource (CR) Configuration**: The Limitador CR that manages your deployment must be updated to point to the running Redis instance by specifying the storage configuration in its spec.

---

## Example Limitador CR Configuration

To configure Limitador to use Redis for persistent storage, you need to:

. **Create a Kubernetes Secret** containing the Redis connection URL:

   ```bash
   kubectl create secret generic redis-config \
     --from-literal=URL=redis://redis-service.redis-limitador.svc:6379 \
     --namespace=<your-limitador-namespace>
   ```

. **Update your Limitador CR** to reference the secret:

   ```yaml
   apiVersion: limitador.kuadrant.io/v1alpha1
   kind: Limitador
   metadata:
     name: limitador
   spec:
     storage:
       redis:
         configSecretRef:
           name: redis-config
   ```

   Edit your existing Limitador CR:
   
   ```bash
   kubectl edit limitador <your-instance-name> -n <your-limitador-namespace>
   ```

For detailed, official instructions on production Redis setup, refer to the Red Hat documentation:

- [Red Hat Connectivity Link - Configure Redis](https://docs.redhat.com/en/documentation/red_hat_connectivity_link/1.2/html/installing_on_openshift_container_platform/configure-redis_connectivity-link)

---

## Local Validation Script (Basic Dev-only Redis)

A basic Redis setup script is provided for local development and validation. This script deploys a non-production Redis instance.

**Script Location:** [`scripts/setup-redis.sh`](https://github.com/opendatahub-io/models-as-a-service/blob/main/scripts/setup-redis.sh)

### Namespace Selection

The script uses a simple namespace selection logic:

- **`NAMESPACE` environment variable** (if set)
- **Default: `redis-limitador`** (created automatically if it doesn't exist)

This opinionated default simplifies troubleshooting and ensures consistent deployments.

### Usage

```bash
# Make the script executable
chmod +x scripts/setup-redis.sh

# Run with default namespace (redis-limitador)
./scripts/setup-redis.sh

# Or override with environment variable
NAMESPACE=my-namespace ./scripts/setup-redis.sh
```

The script will:

- Create the namespace if it doesn't exist (for default `redis-limitador` namespace)
- Deploy a Redis Deployment and Service
- Wait for Redis to be ready
- Output instructions for creating a Secret and configuring your Limitador CR

!!! note
    **Single Source of Truth**: The script content is maintained only in `scripts/setup-redis.sh`. Any updates to the script are automatically reflected when users download and run it.

---

## How to Validate Persistence

. **Run the script**: `./scripts/setup-redis.sh`

   This will deploy Redis to the `redis-limitador` namespace by default (or use your `NAMESPACE` env var).

. **Follow the output instructions** to create the Secret and configure your Limitador CR with the Redis storage configuration.

. **Send traffic** against a rate-limited route until you have a non-zero hit count.

   You can verify metrics in Prometheus:

   ```bash
   # Port-forward to Prometheus (adjust namespace as needed)
   kubectl port-forward -n monitoring svc/prometheus-k8s 9090:9091

   # Query for authorized_hits metric
   # Open http://localhost:9090 and search for: authorized_hits
   ```

. **Find your Limitador pod**:

   ```bash
   kubectl get pods -l app=limitador
   ```

. **Delete the pod** to force a restart:

   ```bash
   kubectl delete pod <limitador-pod-name>
   ```

. **Wait for the new pod** to become Running:

   ```bash
   kubectl get pods -l app=limitador -w
   ```

. **Send another request** to the same route. You will see that the metric count continues from its previous value instead of resetting to 1.

---

## Related Documentation

- [Red Hat Connectivity Link - Configure Redis](https://docs.redhat.com/en/documentation/red_hat_connectivity_link/1.2/html/installing_on_openshift_container_platform/configure-redis_connectivity-link) - Official Red Hat documentation for production Redis setup
