# Limitador Persistence

By default, Limitador stores rate-limit counters in memory. Counters reset when pods restart, scale, or are rescheduled. For production deployments, configure Limitador to use Redis for persistent storage.

!!! info "Other Storage Options"
    Limitador supports additional storage backends. See the [Limitador configuration documentation](https://github.com/Kuadrant/limitador/blob/main/doc/server/configuration.md) for details. This guide focuses on Redis.

---

## Configuration

To configure Limitador with Redis persistent storage:

1. Deploy Redis according to your environment's requirements
2. Create a Kubernetes Secret with the Redis connection URL
3. Update the Limitador CR to reference the Secret

**For complete configuration steps**, see [Red Hat Connectivity Link - Configure Redis](https://docs.redhat.com/en/documentation/red_hat_connectivity_link/1.3/html/installing_on_openshift_container_platform/rhcl-install-on-ocp#configure-redis_installing-rhcl-on-ocp).

---

## Local Development Setup

For local development and testing, use the provided Redis setup script:

**Script:** [`scripts/setup-redis.sh`](https://github.com/opendatahub-io/models-as-a-service/blob/main/scripts/setup-redis.sh)

```bash
# Default namespace: redis-limitador
./scripts/setup-redis.sh

# Or specify custom namespace
NAMESPACE=my-namespace ./scripts/setup-redis.sh
```

The script deploys a basic Redis instance and outputs instructions for configuring your Limitador CR.

!!! warning "Development Only"
    This script deploys a non-HA Redis instance for local development. For production, follow the [Red Hat Connectivity Link documentation](https://docs.redhat.com/en/documentation/red_hat_connectivity_link/1.3/html/installing_on_openshift_container_platform/rhcl-install-on-ocp#configure-redis_installing-rhcl-on-ocp).

---

## Related Documentation

- [Limitador Configuration](https://github.com/Kuadrant/limitador/blob/main/doc/server/configuration.md) - Storage backend options and configuration
- [Red Hat Connectivity Link - Configure Redis](https://docs.redhat.com/en/documentation/red_hat_connectivity_link/1.3/html/installing_on_openshift_container_platform/rhcl-install-on-ocp#configure-redis_installing-rhcl-on-ocp) - Production Redis setup
