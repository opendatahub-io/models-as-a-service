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
# WARNING: Only run in non-production or during a maintenance window.
# This will disrupt in-flight requests while pods restart.
kubectl delete pod -n kuadrant-system -l app=limitador
kubectl logs -n kuadrant-system deployment/limitador | grep -i redis
# Counters should reload from Redis, not reset
```

## Maintenance

### Grafana Datasource Token Rotation

Grafana datasource uses ServiceAccount tokens with cluster-configured expiration. Token lifetime varies by cluster (Kubernetes and OpenShift have different defaults). Check your cluster's token expiration:

```bash
# Check projected serviceAccountToken expiration in Grafana Pod
kubectl get pod -n <grafana-namespace> <grafana-pod> -o jsonpath='{.spec.volumes[?(@.projected.sources[0].serviceAccountToken)].projected.sources[0].serviceAccountToken.expirationSeconds}'

# Or check via TokenRequest API
kubectl create token <sa-name> -n <grafana-namespace> --duration=0s | kubectl get --raw /api/v1/namespaces/<grafana-namespace>/serviceaccounts/<sa-name>/token -o jsonpath='{.status.expirationTimestamp}'

# Re-deploy dashboards to rotate token
./scripts/observability/install-grafana-dashboards.sh
```

!!! tip "Production"
    Verify your cluster's token lifetime and automate rotation accordingly (e.g., CronJob or external secrets operator) to avoid outages.

### Monitor ServiceMonitor Health

The ODH/RHOAI monitoring stack scrapes `ServiceMonitor` / `PodMonitor` targets with the
OpenTelemetry Collector (Target Allocator discovers jobs; the collector scrapes; metrics
are remote-written to Prometheus). Thanos Query serves PromQL only — it has no scrape
pool, so `/api/v1/targets` on `data-science-thanos-querier-route` is empty or unimplemented.
Prometheus **Status → Targets** does not list these jobs either.

The collector's prometheus receiver sets `job` to its own scrape job
(`data-science-collector-prometheus`). The original ServiceMonitor/PodMonitor job is
preserved as `exported_job` (for example `limitador-limitador` or
`redhat-ods-applications/maas-controller-metrics`). Filter scrape health on
`exported_job`, not `job`.

Do not use the **usage-logs** collector (`usage-logs-collector`) here. That collector
ingests gateway access logs into Loki. Metrics scraping uses the **monitoring-stack**
OpenTelemetryCollector in the DSCI monitoring namespace.

```bash
# Replace <monitoring-namespace> with DSCI `spec.monitoring.namespace`
# (e.g. opendatahub or redhat-ods-monitoring).
# Replace <cluster> with your cluster's apps domain (e.g. apps.mycluster.example.com).

# 1. Confirm monitors exist (MaaS labels ServiceMonitors with monitoring.opendatahub.io/scrape=true)
kubectl get servicemonitor,podmonitor -A -l monitoring.opendatahub.io/scrape='true'

# 2. Discovery: Target Allocator job list (what should be scraped)
# The monitoring OpenTelemetryCollector is named data-science-collector, so its
# Target Allocator Service is data-science-collector-targetallocator.
kubectl -n <monitoring-namespace> port-forward svc/data-science-collector-targetallocator 8080:80
# In another terminal:
curl -s localhost:8080/jobs | jq
# Job IDs look like serviceMonitor/<namespace>/<name>/0 — inspect endpoints:
# curl -s localhost:8080/jobs/serviceMonitor%2F<namespace>%2F<name>%2F0/targets | jq
# Look for maas, limitador, authorino, kserve jobs. Missing jobs mean the monitor
# was not discovered (selector, namespace label, or NetworkPolicy).

# 3. Scrape health: `up` is emitted by the prometheus receiver and stored in Thanos.
# Match exported_job (original target). job is always data-science-collector-prometheus.
curl -s -H "Authorization: Bearer $(oc whoami -t)" --get \
  --data-urlencode 'query=up{exported_job=~".*(maas|limitador|authorino|kserve).*"}' \
  "https://data-science-thanos-querier-route-<monitoring-namespace>.<cluster>/api/v1/query" | \
  jq '.data.result[] | {exported_job: .metric.exported_job, instance: .metric.instance, up: .value[1]}'
# up==1 is the analogue of a Prometheus target UP. No series usually means the
# Target Allocator never allocated the job, not that Thanos lost the target.

# 4. If up is 0 or missing, check the monitoring-stack collector logs (exclude usage-logs-collector)
kubectl logs -n <monitoring-namespace> \
  -l 'app.kubernetes.io/component=opentelemetry-collector,app.kubernetes.io/name!=usage-logs-collector' \
  --tail=200 | grep -iE 'scrape|error|limitador|authorino|maas'
```

### Cleanup

```bash
# Remove dashboards
kubectl delete grafanadashboard -n <grafana-namespace> maas-platform-admin maas-ai-engineer

# Remove ServiceMonitors
kubectl delete servicemonitor -n <namespace> <servicemonitor-name>

# Remove telemetry
kubectl delete telemetrypolicy -n openshift-ingress maas-telemetry
kubectl delete telemetry -n openshift-ingress latency-per-subscription
```

### Troubleshooting Missing Metrics

```bash
# 1. Verify service exposes metrics
kubectl exec -n <namespace> <pod> -- curl localhost:<port>/metrics

# 2. Verify ServiceMonitor exists and labeled with: "monitoring.opendatahub.io/scrape: 'true'"
kubectl get servicemonitor -n <namespace> \
  -l 'monitoring.opendatahub.io/scrape=true'

# 3. Verify ODH monitoring stack enabled
kubectl get maastenantconfig default-tenant -n models-as-a-service  -o json | jq '.status.conditions[] | select(.type=="Degraded" or .type=="MaaSPrerequisitesAvailable") | {type, status, message}'

kubectl get dscinitialization default-dsci -o json | jq '.status.conditions[] | select(.type=="MonitoringReady" or .type=="MonitoringStackAvailable") | {type, status}'

# 4. Confirm the Target Allocator discovered the monitor and `up` is 1
# (see Monitor ServiceMonitor Health above). Do not use Thanos /api/v1/targets.

# 5. Query stored samples via Thanos (PromQL only — this is not a scrape-target API)
# Replace <monitoring-namespace> with DSCI `spec.monitoring.namespace` (e.g., opendatahub)
# Replace <cluster> with your cluster's apps domain (e.g., apps.mycluster.example.com)
# For example: https://data-science-thanos-querier-route-redhat-ods-monitoring.apps.mycluster.example.com/api/v1/query?query=authorized_hits_total
curl -s -H "Authorization: Bearer $(oc whoami -t)" --get \
  --data-urlencode 'query=<metric_name>' \
  "https://data-science-thanos-querier-route-<monitoring-namespace>.<cluster>/api/v1/query"
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
kubectl exec prometheus-data-science-monitoringstack-0 -n <monitoring-namespace> -- \
  df -h /prometheus

# View retention
kubectl get monitoringstack data-science-monitoringstack -n <monitoring-namespace> -o yaml | \
  grep -A 5 retention
```

**Metric cardinality:**

```bash
# Thanos Query has no local TSDB, so /api/v1/status/tsdb is not available.
# Count active series per MaaS metric via PromQL instead.
# Replace <monitoring-namespace> with DSCI `spec.monitoring.namespace` (e.g., opendatahub)
# Replace <cluster> with your cluster's apps domain (e.g., apps.mycluster.example.com)
curl -s -H "Authorization: Bearer $(oc whoami -t)" --get \
  --data-urlencode 'query=count by (__name__) ({__name__=~"authorized_hits_total|authorized_calls_total|limited_calls_total"})' \
  "https://data-science-thanos-querier-route-<monitoring-namespace>.<cluster>/api/v1/query" | \
  jq '.data.result[] | {metric: .metric.__name__, series: .value[1]}'
```

Watch: `authorized_hits_total{user!=""}`, `authorized_calls_total{user!=""}`, `istio_request_duration_milliseconds_bucket{subscription!=""}`.

### Regular Maintenance Tasks

| Task | Frequency | Action |
|------|-----------|--------|
| **Token Rotation** | Per cluster token TTL | Rotate Grafana datasource token before expiration (verify cluster-specific lifetime) |
| **Storage Check** | Weekly | Monitor Prometheus storage usage |
| **ServiceMonitor Health** | Daily | Check Target Allocator jobs and `up` series (not Thanos `/api/v1/targets`) |
| **Cardinality Review** | Monthly | Review high-cardinality metrics |
| **Dashboard Testing** | After deployment | Verify dashboards load |
| **Backup Redis** (HA) | Daily | Backup Redis data |

## Known Limitations

### Blocked Features

| Feature | Blocker | Workaround |
|---------|---------|------------|
| **`model` label on `authorized_calls_total` / `limited_calls_total`** | Kuadrant wasm-shim doesn't pass `responseBodyJSON` context | Use `authorized_hits_total` for per-model breakdown |
| **Input/output token split** | TokenRateLimitPolicy sends single `hits_addend` | Total tokens via `authorized_hits_total`; response body has `usage.prompt_tokens` and `usage.completion_tokens` but wasm-shim doesn't split |
| **Input/output per user** | vLLM doesn't label with `user` | Total tokens per user via `authorized_hits_total{user}`; vLLM prompt/gen metrics are per-model only |
| **Rate-limited in Istio metrics** | WASM plugin `sendLocalReply()` short-circuits filter chain | Use `limited_calls_total` from Limitador (has correct labels) |
| **Policy health metrics** | `kuadrant_policies_enforced`, `kuadrant_policies_total` not in RHCL 1.x | `limitador_up` and `datastore_partitioned` available now |
| **maas-api metrics** | Requires HTTPS scrape + `/metrics` get RBAC | Use ServiceMonitor `maas-api-metrics` with bearer token; grant scrapers `nonResourceURLs: ["/metrics"]` get |
| **PromQL `_total` suffix** | OTel prometheus receiver stores Limitador counters as `authorized_hits_total` (and the same for `authorized_calls` / `limited_calls`) | Query the `_total` names; Grafana panels that omit `_total` return no data |

!!! note "Total vs Split"
    Total token consumption per user **is available** via `authorized_hits_total{user}`. Input/output split at gateway requires wasm-shim to send two counter updates.

### Available Metrics

| Feature | Metric | Label |
|---------|--------|-------|
| **Latency per subscription** | `istio_request_duration_milliseconds_bucket` | `subscription` |
| **Tokens per user** | `authorized_hits_total` | `user` |
| **Tokens per subscription** | `authorized_hits_total` | `subscription` |
| **Tokens per model** | `authorized_hits_total` | `model` |
| **Requests per user** | `authorized_calls_total` | `user` |
| **Requests per subscription** | `authorized_calls_total` | `subscription` |
| **Rate limited per user** | `limited_calls_total` | `user` |
| **Rate limited per subscription** | `limited_calls_total` | `subscription` |

## Reporting Issues

1. Check [Setup](setup.md) prerequisites
2. Review troubleshooting procedures above
3. Search [GitHub Issues](https://github.com/opendatahub-io/models-as-a-service/issues)
4. Report with: MaaS version, failing query/panel, expected vs actual, relevant logs
