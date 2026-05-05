# Operations

## High Availability

For production deployments, configure Limitador with Redis backend for metric persistence across pod restarts.

### Why HA Matters

Default in-memory storage means:

- All hit counts lost on pod restart
- Metrics reset on reschedule or scale down
- No persistence across cluster maintenance

### Configure Redis Persistence

See [Configuring Redis storage for rate limiting](https://docs.redhat.com/en/documentation/red_hat_connectivity_link/1.2/html/installing_on_openshift_container_platform/rhcl-install-on-ocp#configure-redis_installing-rhcl-on-ocp).

For local development: [Limitador Persistence](../advanced-administration/limitador-persistence.md).

**Production considerations:**

- **HA**: Use Redis Sentinel or Cluster
- **Persistence**: Configure RDB snapshots or AOF logs
- **Monitoring**: Monitor memory and connection pool
- **Backup**: Implement regular backups
- **Scaling**: Size for expected metric volume

**Verify connection:**

```bash
# Check Limitador logs
kubectl logs -n kuadrant-system deployment/limitador | grep -i redis

# Test persistence across restart
kubectl delete pod -n kuadrant-system -l app=limitador
kubectl logs -n kuadrant-system deployment/limitador | grep -i redis
# Counters should reload from Redis, not reset
```

## Maintenance

### Grafana Datasource Token Rotation

Grafana datasource uses ServiceAccount token valid for **30 days**.

```bash
# Re-deploy dashboards to rotate token
./scripts/observability/install-grafana-dashboards.sh
```

!!! tip "Production"
    Automate token rotation using CronJob or external secrets operator to avoid outages.

### Monitor ServiceMonitor Health

```bash
# Check ServiceMonitor status
kubectl get servicemonitor -A

# View targets in Prometheus UI: Status → Targets
# Look for maas-*, kserve-*, authorino-*, limitador-* targets (should be UP)

# Query Prometheus directly
curl -sk -H "Authorization: Bearer $(oc whoami -t)" \
  "https://thanos-querier-openshift-monitoring.<cluster>/api/v1/targets" | \
  jq '.data.activeTargets[] | select(.labels.job | contains("maas"))'
```

### Cleanup

```bash
# Remove dashboards
kubectl delete grafanadashboard -n <grafana-namespace> maas-platform-admin maas-ai-engineer

# Remove ServiceMonitors
kubectl delete servicemonitor -n <namespace> <servicemonitor-name>

# Remove telemetry
kubectl delete telemetrypolicy -n openshift-ingress gateway-telemetry-policy
kubectl delete telemetry -n openshift-ingress latency-per-subscription
```

### Troubleshooting Missing Metrics

```bash
# 1. Verify service exposes metrics
kubectl exec -n <namespace> <pod> -- curl localhost:<port>/metrics

# 2. Verify ServiceMonitor exists
kubectl get servicemonitor -n <namespace>

# 3. Verify User Workload Monitoring enabled
kubectl get pods -n openshift-user-workload-monitoring

# 4. Check Prometheus targets (UI → Status → Targets)

# 5. Query Prometheus directly
curl -sk -H "Authorization: Bearer $(oc whoami -t)" \
  "https://thanos-querier-openshift-monitoring.<cluster>/api/v1/query?query=<metric_name>"
```

### Troubleshooting Dashboard Issues

```bash
# 1. Verify Grafana → Prometheus connection
# In Grafana: Configuration → Data Sources → Test

# 2. Check query syntax
# Edit panel → View query in Prometheus directly

# 3. Verify time range includes when metrics were generated

# 4. Check for lazily-registered metrics
# Some metrics appear only after first event (e.g., queue_time after first queued request)
```

### Capacity Planning

**Prometheus storage:**

```bash
# Check storage size
kubectl exec -n openshift-user-workload-monitoring prometheus-user-workload-0 -- \
  df -h /prometheus

# View retention
kubectl get prometheus -n openshift-user-workload-monitoring -o yaml | \
  grep -A 5 retention
```

**Metric cardinality:**

```bash
# Check high-cardinality metrics
curl -sk -H "Authorization: Bearer $(oc whoami -t)" \
  "https://thanos-querier-openshift-monitoring.<cluster>/api/v1/status/tsdb" | \
  jq '.data.seriesCountByMetricName[] | select(.value > 1000)'
```

Watch: `authorized_hits{user}`, `authorized_calls{user}`, `istio_request_duration_milliseconds_bucket{subscription}`.

### Regular Maintenance Tasks

| Task | Frequency | Action |
|------|-----------|--------|
| **Token Rotation** | Every 30 days | Rotate Grafana datasource token |
| **Storage Check** | Weekly | Monitor Prometheus storage usage |
| **ServiceMonitor Health** | Daily | Check Prometheus targets |
| **Cardinality Review** | Monthly | Review high-cardinality metrics |
| **Dashboard Testing** | After deployment | Verify dashboards load |
| **Backup Redis** (HA) | Daily | Backup Redis data |

## Known Limitations

### Blocked Features

| Feature | Blocker | Workaround |
|---------|---------|------------|
| **`model` label on `authorized_calls` / `limited_calls`** | Kuadrant wasm-shim doesn't pass `responseBodyJSON` context | Use `authorized_hits` for per-model breakdown |
| **Input/output token split** | TokenRateLimitPolicy sends single `hits_addend` | Total tokens via `authorized_hits`; response body has `usage.prompt_tokens` and `usage.completion_tokens` but wasm-shim doesn't split |
| **Input/output per user** | vLLM doesn't label with `user` | Total tokens per user via `authorized_hits{user}`; vLLM prompt/gen metrics are per-model only |
| **Rate-limited in Istio metrics** | WASM plugin `sendLocalReply()` short-circuits filter chain | Use `limited_calls` from Limitador (has correct labels) |
| **Policy health metrics** | `kuadrant_policies_enforced`, `kuadrant_policies_total` not in RHCL 1.x | `limitador_up` and `datastore_partitioned` available now |
| **maas-api metrics** | No `/metrics` endpoint | No workaround; requires adding Prometheus instrumentation |
| **PromQL warnings** | Counter names don't end in `_total` | Cosmetic only; all queries work correctly |

!!! note "Total vs Split"
    Total token consumption per user **is available** via `authorized_hits{user}`. Input/output split at gateway requires wasm-shim to send two counter updates.

### Available Metrics

| Feature | Metric | Label |
|---------|--------|-------|
| **Latency per subscription** | `istio_request_duration_milliseconds_bucket` | `subscription` |
| **Tokens per user** | `authorized_hits` | `user` |
| **Tokens per subscription** | `authorized_hits` | `subscription` |
| **Tokens per model** | `authorized_hits` | `model` |
| **Requests per user** | `authorized_calls` | `user` |
| **Requests per subscription** | `authorized_calls` | `subscription` |
| **Rate limited per user** | `limited_calls` | `user` |
| **Rate limited per subscription** | `limited_calls` | `subscription` |

### Requirements Status

| Requirement | Status |
|-------------|--------|
| **Usage dashboards** (token per user/subscription/model) | ✅ Met |
| **Latency by subscription** (P50/P95/P99) | ✅ Met |
| **Request tracking** (per user/subscription) | ✅ Met |
| **Chargeback export** (CSV/API) | ❌ RFE - No `/maas-api/v1/usage` endpoint |
| **Input/output token split** | ❌ Requires upstream Kuadrant |
| **`model` on all counters** | ⚠️ Partial - only on `authorized_hits` |
| **Policy health** | ⏳ Future RHCL release |
| **Auth evaluation metrics** | ✅ Met via `authorino-server-metrics` |
| **maas-api metrics** | ❌ Gap - no instrumentation |

### Future Enhancements

1. Input/output token split (upstream Kuadrant)
2. Model label on all counters
3. maas-api Prometheus instrumentation
4. Chargeback API endpoint (`/maas-api/v1/usage`)
5. Full policy health metrics in RHCL

### Reporting Issues

1. Check [Setup](setup.md) prerequisites
2. Review troubleshooting procedures above
3. Search [GitHub Issues](https://github.com/opendatahub-io/models-as-a-service/issues)
4. Report with: MaaS version, failing query/panel, expected vs actual, relevant logs
