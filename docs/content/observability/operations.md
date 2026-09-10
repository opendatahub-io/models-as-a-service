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

How you remove resources depends on how they were applied. `./scripts/observability/install-observability.sh` applies TelemetryPolicy and Istio Telemetry from `deployment/base/observability/`, plus these resources when their conditions match:

| Resource | When |
|----------|------|
| Limitador `ServiceMonitor` (`limitador-metrics`) | Kuadrant is not already scraping Limitador `/metrics` |
| Authorino `ServiceMonitor` (`authorino-server-metrics`) | Authorino Service exists and nothing else scrapes `/server-metrics` |
| Gateway `Service` + `ServiceMonitor` (`istio-gateway-metrics`) | Gateway Deployment exists in `openshift-ingress` |
| PrometheusRules (`authorino-maas-metadata-evaluator-high-failure-rate`, `authorino-maas-authentication-alerts`) | Authorino Deployment exists (`kuadrant-system` or `rh-connectivity-link`) |

#### PersesDashboard ownership

| Deployment mode | How applied | Ownership | Cleanup |
|-----------------|-------------|-----------|---------|
| **Operator** (recommended) | `LifecycleReconciler.ensureUsageDashboard` (metrics Usage dashboard). Usage-log dashboards via `usageLogging` on `Config`. | Controller `ownerReference` on `Config` (`configs.maas.opendatahub.io/default`); field manager `maas-controller`. | Do not delete the CR by hand — the operator recreates it. See [Operator-managed dashboards](#operator-managed-dashboards) below. |
| **Kustomize** (development) | `kustomize build` of `deployment/components/observability/observability/dashboards/` (and optionally `usage-logs/`). | Label `app.kubernetes.io/managed-by: maas-observability` on the metrics Usage dashboard. No `Config` controller reference. | Delete `dashboard-3-maas-usage-admin` by name. For usage-logs, set `usageLogging=false` when Config exists, wait until **every** overlay object with a Config controller owner is gone, then `kubectl delete -k`. |
| **`install-observability.sh`** | TelemetryPolicy, Istio Telemetry, and the conditional monitors/rules in the table above. | `app.kubernetes.io/managed-by: maas-observability` on the kustomize base and gateway Service/ServiceMonitor. Limitador monitor and PrometheusRules do not all carry that label. | Delete the applied objects by name (see [Telemetry and ServiceMonitors](#telemetry-and-servicemonitors)). Dashboards are unaffected. |

Identify operator-owned dashboards:

```bash
kubectl get persesdashboard -A -o json | jq -r '
  .items[] | select(.metadata.ownerReferences[]? | .kind=="Config" and .controller==true)
  | "\(.metadata.namespace)/\(.metadata.name)"'
```

#### Operator-managed dashboards

Requires a running **maas-controller** (`LifecycleReconciler`), a `Config` instance, a monitoring namespace, and Perses CRDs (`PersesAvailable`). See [Setup](setup.md).

```bash
# Usage-log dashboards (dashboard-4, dashboard-5): operator deletes CRs it owns
kubectl patch configs.maas.opendatahub.io default --type=merge \
  -p '{"spec":{"usageLogging":false}}'

# Metrics Usage dashboard (dashboard-3): owned by Config.
# Disabling Tenant telemetry does not remove it.
# It is garbage-collected when Config is deleted (operator uninstall / teardown).
```

#### Kustomize dashboards

Delete only the Kustomize-applied metrics Usage dashboard, by name and namespace; if `Config` is the controller owner, do not delete the CR by hand — see [Operator-managed dashboards](#operator-managed-dashboards).

```bash
kubectl delete persesdashboard dashboard-3-maas-usage-admin -n opendatahub
```

#### Usage-log resources

Disable `usageLogging` when Config exists, wait until **every rendered overlay object** with a Config controller owner is gone, then delete leftovers.

```bash
set -euo pipefail

overlay=deployment/components/observability/usage-logs
manifests=$(mktemp)
trap 'rm -f "$manifests"' EXIT
kustomize build "$overlay" >"$manifests"

if kubectl get configs.maas.opendatahub.io default -o name >/dev/null; then
  kubectl patch configs.maas.opendatahub.io default --type=merge \
    -p '{"spec":{"usageLogging":false}}'
fi

owned_usage_log_resources() {
  kubectl get --ignore-not-found -f "$manifests" -o json | jq -r '
    (.items // [.])[]
    | select(.metadata.name != null)
    | select([.metadata.ownerReferences[]? | select(.kind=="Config" and .controller==true)] | length > 0)
    | "\(.kind)/\(.metadata.namespace // "-")/\(.metadata.name)"'
}

for _ in $(seq 1 30); do
  left=$(owned_usage_log_resources)
  [ -z "$left" ] && break
  sleep 2
done
left=$(owned_usage_log_resources)
if [ -n "$left" ]; then
  echo "operator-owned usage-log resources still present; do not delete -k:"
  echo "$left"
  exit 1
fi

kubectl delete -k "$overlay"
```

#### Telemetry and ServiceMonitors

Kustomize/development cleanup only. Do not run this in operator-managed mode. For operator-managed telemetry, update the tenant configuration and let the Tenant reconciler manage those resources.

```bash
# Always applied by the script (from deployment/base/observability/)
kubectl delete telemetrypolicy -n openshift-ingress maas-telemetry
kubectl delete telemetry -n openshift-ingress latency-per-subscription

# Conditional: Limitador ServiceMonitor (only if the script deployed it)
kubectl delete servicemonitor -n kuadrant-system limitador-metrics --ignore-not-found

# Conditional: Authorino /server-metrics ServiceMonitor
kubectl delete servicemonitor -n kuadrant-system authorino-server-metrics --ignore-not-found

# Conditional: Istio Gateway metrics
kubectl delete servicemonitor,service -n openshift-ingress istio-gateway-metrics --ignore-not-found

# Conditional: PrometheusRules (namespace is kuadrant-system or rh-connectivity-link)
kubectl delete prometheusrule -n kuadrant-system \
  authorino-maas-metadata-evaluator-high-failure-rate \
  authorino-maas-authentication-alerts --ignore-not-found
kubectl delete prometheusrule -n rh-connectivity-link \
  authorino-maas-metadata-evaluator-high-failure-rate \
  authorino-maas-authentication-alerts --ignore-not-found
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
# 1. Confirm Perses is available (ODH/RHOAI observability console)
kubectl get dscinitialization default-dsci -o json | \
  jq '.status.conditions[] | select(.type=="PersesAvailable") | {type, status}'

# 2. Confirm PersesDashboard CRs exist
kubectl get persesdashboard -A | grep maas

# 3. Run the panel query in Prometheus or Loki directly (see Setup)

# 4. Verify the time range includes when metrics or logs were generated

# 5. Check for lazily-registered metrics
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
| **Storage Check** | Weekly | Monitor Prometheus storage usage |
| **ServiceMonitor Health** | Daily | Check Target Allocator jobs and `up` series (not Thanos `/api/v1/targets`) |
| **Cardinality Review** | Monthly | Review high-cardinality metrics |
| **Dashboard Testing** | After deployment | Verify Perses dashboards load in the ODH console |
| **Backup Redis** (HA) | Daily | Backup Redis data |

## Known Limitations

### Blocked Features

| Feature | Blocker | Workaround |
|---------|---------|------------|
| **`model` label on `authorized_calls_total` / `limited_calls_total`** | Kuadrant wasm-shim doesn't pass `responseBodyJSON` context | Use `authorized_hits_total` for per-model breakdown |
| **Input/output token split** | TokenRateLimitPolicy sends single `hits_addend` | Total tokens via `authorized_hits_total`; response body has `usage.prompt_tokens` and `usage.completion_tokens` but wasm-shim doesn't split |
| **Input/output per user** | vLLM doesn't label with `user` | Total tokens per user via `authorized_hits_total{user!=""}`; vLLM prompt/gen metrics are per-model only |
| **Rate-limited in Istio metrics** | WASM plugin `sendLocalReply()` short-circuits filter chain | Use `limited_calls_total` from Limitador (has correct labels) |
| **Policy health metrics** | `kuadrant_policies_enforced`, `kuadrant_policies_total` not in RHCL 1.x | `limitador_up` and `datastore_partitioned` available now |
| **maas-api metrics** | Requires HTTPS scrape + `/metrics` get RBAC | Use ServiceMonitor `maas-api-metrics` with bearer token; grant scrapers `nonResourceURLs: ["/metrics"]` get |
| **PromQL `_total` suffix** | OTel prometheus receiver stores Limitador counters as `authorized_hits_total` (and the same for `authorized_calls` / `limited_calls`) | Query the `_total` names; Grafana panels that omit `_total` return no data |

!!! note "Total vs Split"
    Total token consumption per user **is available** via `authorized_hits_total{user!=""}`. Input/output split at gateway requires wasm-shim to send two counter updates.

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
