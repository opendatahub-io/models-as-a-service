# Observability

The MaaS Platform provides metrics collection, monitoring, and visualization for production deployments.

## Getting Started

1. **[Setup](setup.md)** - Prerequisites and installation
2. **[Metrics & Dashboards](metrics-and-dashboards.md)** - Available metrics and Grafana visualization
3. **[Operations](operations.md)** - High availability, maintenance, and known limitations

## Component Metrics

| Component | Metrics Endpoint | Scraped | Dashboard Panels |
|-----------|-----------------|---------|------------------|
| **Limitador** | `/metrics` | Yes (Kuadrant PodMonitor) | Token usage, rate limits |
| **Authorino** | `/metrics`, `/server-metrics` | Yes (MaaS ServiceMonitor) | Auth latency, success/deny rate |
| **Istio Gateway** | `/stats/prometheus` | Yes | Latency histograms, request counts |
| **vLLM / llm-d** | `/metrics` port 8000 | Yes | TTFT, ITL, queue depth, tokens |
| **maas-api** | **None** | No | Pod status only |

!!! note
    The observability stack will be enhanced in future releases.
