from __future__ import annotations

"""
Observability Tests for MaaS Platform
=====================================

These tests verify that the observability stack is correctly deployed and
that metrics are being generated with the expected labels.

How other e2e tests are implemented (not in this file):
- Smoke: smoke.sh (bash) gets token via curl from host, then pytest test_smoke.py
  hits the gateway URL from the test process (no pods).
- Validation: validate-deployment.sh (bash), kubectl + curl from host.
- Token verification: verify-tokens-metadata-logic.sh (bash).

Observability (this file): Direct metrics checks are done from inside the test
process only: port-forward each component (including Prometheus) to localhost,
then HTTP GET from the test (no exec into component pods, no helper pods).

Tests will FAIL with informative messages if:
- Kubernetes resources (TelemetryPolicy, Telemetry, ServiceMonitor) are missing
- Metrics are not available from Limitador
- Metrics are missing expected labels (user, tier, model)
- Metric names have changed or are absent

Configuration is loaded from: test/e2e/config/expected_metrics.yaml
"""

import json
import logging
import re
import ssl
import subprocess
import time
from pathlib import Path
from urllib.error import URLError
from urllib.parse import quote
from urllib.request import urlopen

import pytest
import yaml

log = logging.getLogger(__name__)

# Load expected metrics configuration
CONFIG_PATH = Path(__file__).parent.parent / "config" / "expected_metrics.yaml"


@pytest.fixture(scope="module")
def expected_metrics_config():
    """Load the expected metrics configuration from YAML."""
    if not CONFIG_PATH.exists():
        pytest.fail(f"FAIL: Metrics configuration file not found: {CONFIG_PATH}")

    with open(CONFIG_PATH, encoding="utf-8") as f:
        config = yaml.safe_load(f)

    log.info(f"[config] Loaded metrics configuration from {CONFIG_PATH}")
    return config


def _run_kubectl(args: list[str], timeout: int = 30) -> tuple[int, str, str]:
    """Run kubectl command and return (returncode, stdout, stderr)."""
    cmd = ["kubectl", *args]
    try:
        result = subprocess.run(
            cmd,
            capture_output=True,
            text=True,
            timeout=timeout,
        )
        return result.returncode, result.stdout, result.stderr
    except subprocess.TimeoutExpired:
        return -1, "", "Command timed out"
    except Exception as e:
        return -1, "", str(e)


def _metric_exists_in_text(metrics_text: str, metric_name: str) -> bool:
    """Check if a metric exists in Prometheus exposition format text.

    Matches metric_name at the start of a line followed by '{' (labels) or ' ' (value).
    Shared helper used by both TestLimitadorMetrics and TestMetricsAfterRequest.
    """
    pattern = rf"^{re.escape(metric_name)}[\{{\s]"
    return bool(re.search(pattern, metrics_text, re.MULTILINE))


def _fetch_limitador_metrics(namespace: str, port: int, path: str) -> str:
    """Fetch Limitador metrics via port-forward from the test process (works in CI and local)."""
    return _fetch_metrics_via_port_forward(
        namespace=namespace,
        pod_label_selector="app=limitador",
        port=port,
        path=path,
        scheme="http",
        component_name="Limitador",
    )


# Distinct local ports per component to avoid "address already in use" when tests run back-to-back.
LOCAL_PORT_LIMITADOR = 18590
LOCAL_PORT_ISTIO = 18591
LOCAL_PORT_VLLM = 18592
LOCAL_PORT_AUTHORINO = 18593
LOCAL_PORT_PROMETHEUS_USER = 18594
LOCAL_PORT_PROMETHEUS_PLATFORM = 18595


def _prometheus_http_via_port_forward(
    namespace: str,
    pod_name: str,
    path: str,
    local_port: int,
    timeout_sec: int = 15,
    attempts: int = 3,
) -> str | None:
    """
    Query Prometheus via port-forward + HTTP GET (no exec).
    Forwards pod:9090 to local_port, retries up to `attempts` times.
    """
    pf_cmd = [
        "kubectl", "port-forward", "-n", namespace,
        f"pod/{pod_name}", f"{local_port}:9090",
    ]
    proc = subprocess.Popen(
        pf_cmd,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
    )
    try:
        time.sleep(2)
        url = f"http://127.0.0.1:{local_port}{path}"
        last_error = None
        for attempt in range(attempts):
            try:
                with urlopen(url, timeout=timeout_sec) as resp:
                    return resp.read().decode("utf-8", errors="replace")
            except (URLError, OSError) as e:
                last_error = e
                if attempt < attempts - 1:
                    time.sleep(2)
            except Exception as e:
                last_error = e
                if attempt < attempts - 1:
                    time.sleep(2)
        log.warning(
            f"[prometheus] port-forward GET failed (ns={namespace}, "
            f"{attempts} attempts): {last_error}"
        )
        return None
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()


def _fetch_metrics_via_port_forward(
    namespace: str,
    pod_label_selector: str,
    port: int,
    path: str,
    scheme: str = "http",
    component_name: str = "pod",
    local_port: int | None = None,
) -> str:
    """
    Fetch metrics by port-forwarding the pod port to localhost, then HTTP GET from the test process.
    Uses distinct local_port per component and retries on connection errors for reliability.
    """
    if local_port is None:
        local_port = LOCAL_PORT_LIMITADOR
    pod_cmd = [
        "get", "pod", "-n", namespace,
        "-l", pod_label_selector,
        "-o", "jsonpath={.items[0].metadata.name}"
    ]
    rc, pod_name, _ = _run_kubectl(pod_cmd)
    if rc != 0 or not pod_name.strip():
        pytest.fail(
            f"FAIL: Could not find {component_name} pod in namespace '{namespace}' "
            f"(selector: {pod_label_selector}).\n"
            f"  Check: kubectl get pods -n {namespace} -l '{pod_label_selector}'"
        )
    pod_name = pod_name.strip()
    pf_cmd = [
        "kubectl", "port-forward", "-n", namespace,
        f"pod/{pod_name}", f"{local_port}:{port}"
    ]
    url = f"{scheme}://127.0.0.1:{local_port}{path}"
    proc = subprocess.Popen(
        pf_cmd,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
    )
    try:
        time.sleep(2)
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        if scheme != "https":
            ctx = None
        last_error = None
        for attempt in range(3):
            try:
                if ctx is not None:
                    with urlopen(url, timeout=15, context=ctx) as resp:
                        metrics_text = resp.read().decode("utf-8", errors="replace")
                else:
                    with urlopen(url, timeout=15) as resp:
                        metrics_text = resp.read().decode("utf-8", errors="replace")
                break
            except (URLError, OSError) as e:
                last_error = e
                if attempt < 2:
                    time.sleep(2)
            except Exception as e:
                last_error = e
                if attempt < 2:
                    time.sleep(2)
        else:
            pytest.fail(
                f"FAIL: Could not fetch metrics from {component_name} via port-forward (3 attempts).\n"
                f"  URL: {url}\n  Last error: {last_error}\n"
                f"  Pod: {pod_name} in {namespace}"
            )
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()

    log.info(f"[metrics] Fetched {len(metrics_text)} bytes from {component_name} (port-forward)")
    return metrics_text


def _can_list_servicemonitors_in(namespace: str) -> tuple[bool, str]:
    """
    Return (True, '') if the current user can list ServiceMonitors in the namespace.
    Otherwise (False, reason) so callers can skip with a clear message (edit/view often lack this).
    """
    rc, _, stderr = _run_kubectl(["get", "servicemonitor", "-n", namespace, "--no-headers"])
    if rc == 0:
        return True, ""
    err = (stderr or "").lower()
    if "forbidden" in err:
        return False, (
            f"Insufficient permissions to list ServiceMonitor in '{namespace}' "
            "(edit/view users often lack this). Run as cluster-admin to verify scraping config."
        )
    return False, (stderr or "Could not list ServiceMonitor").strip()


def _resource_exists(kind: str, name: str, namespace: str) -> bool:
    """Check if a Kubernetes resource exists."""
    rc, _, _ = _run_kubectl(["get", kind, name, "-n", namespace, "--no-headers"])
    return rc == 0


def _get_resource_condition(kind: str, name: str, namespace: str, condition_type: str) -> str | None:
    """Get the status of a specific condition from a resource."""
    jsonpath = f"{{.status.conditions[?(@.type==\"{condition_type}\")].status}}"
    rc, stdout, _ = _run_kubectl([
        "get", kind, name, "-n", namespace,
        "-o", f"jsonpath={jsonpath}"
    ])
    if rc == 0 and stdout.strip():
        return stdout.strip()
    return None


def _query_prometheus(query: str, namespace: str = "openshift-user-workload-monitoring",
                      pod: str = "prometheus-user-workload-0") -> dict | None:
    """
    Query Prometheus via port-forward + HTTP GET (no exec).
    Returns the JSON response or None if query failed.
    Uses REST so only pod get + portforward are needed (no exec).
    """
    encoded_query = quote(query, safe="")
    path = f"/api/v1/query?query={encoded_query}"
    local_port = (
        LOCAL_PORT_PROMETHEUS_PLATFORM
        if namespace == "openshift-monitoring"
        else LOCAL_PORT_PROMETHEUS_USER
    )
    body = _prometheus_http_via_port_forward(namespace, pod, path, local_port)
    if body is None:
        return None
    try:
        return json.loads(body)
    except Exception as e:
        log.warning(f"[prometheus] Failed to parse response: {e}")
        return None


def _query_prometheus_metadata(metric_name: str, namespace: str = "openshift-user-workload-monitoring",
                               pod: str = "prometheus-user-workload-0") -> str | None:
    """
    Query Prometheus metadata API via port-forward + HTTP GET (no exec).
    Returns the type string ('counter', 'gauge', 'histogram', 'summary') or None.
    """
    encoded_metric = quote(metric_name, safe="")
    path = f"/api/v1/metadata?metric={encoded_metric}"
    local_port = (
        LOCAL_PORT_PROMETHEUS_PLATFORM
        if namespace == "openshift-monitoring"
        else LOCAL_PORT_PROMETHEUS_USER
    )
    body = _prometheus_http_via_port_forward(namespace, pod, path, local_port)
    if body is None:
        return None
    try:
        data = json.loads(body)
    except Exception:
        return None
    if data.get("status") != "success":
        return None
    entries = data.get("data", {}).get(metric_name, [])
    if entries:
        return entries[0].get("type")
    return None


def _query_platform_prometheus(query: str) -> dict | None:
    """Query the platform Prometheus (openshift-monitoring) for infrastructure metrics.
    
    Istio gateway metrics, node metrics, and other platform metrics are scraped
    by the platform Prometheus, not the user-workload Prometheus.
    """
    return _query_prometheus(
        query,
        namespace="openshift-monitoring",
        pod="prometheus-k8s-0",
    )


# =============================================================================
# Resource Existence Tests
# =============================================================================

class TestObservabilityResources:
    """Tests for verifying observability Kubernetes resources are deployed."""

    def test_telemetry_policy_exists(self, expected_metrics_config):
        """Verify TelemetryPolicy resource is deployed."""
        cfg = expected_metrics_config["resources"]["telemetry_policy"]
        name = cfg["name"]
        namespace = cfg["namespace"]

        exists = _resource_exists("telemetrypolicy", name, namespace)
        assert exists, (
            f"FAIL: TelemetryPolicy '{name}' not found in namespace '{namespace}'.\n"
            f"  This resource is required for adding user/tier/model labels to Limitador metrics.\n"
            f"  Deploy with: kustomize build deployment/base/observability | kubectl apply -f -"
        )
        log.info(f"[resource] TelemetryPolicy '{name}' exists in '{namespace}'")
        print(f"[resource] TelemetryPolicy '{name}' exists in '{namespace}'")

    def test_telemetry_policy_enforced(self, expected_metrics_config):
        """Verify TelemetryPolicy is enforced (not just deployed)."""
        cfg = expected_metrics_config["resources"]["telemetry_policy"]
        name = cfg["name"]
        namespace = cfg["namespace"]
        expected = cfg["expected_condition"]

        status = _get_resource_condition("telemetrypolicy", name, namespace, expected["type"])
        assert status == expected["status"], (
            f"FAIL: TelemetryPolicy '{name}' is not enforced.\n"
            f"  Expected condition '{expected['type']}' to be '{expected['status']}', got '{status}'.\n"
            f"  Check: kubectl describe telemetrypolicy {name} -n {namespace}\n"
            f"  Common causes:\n"
            f"    - Gateway not ready\n"
            f"    - Kuadrant operator not reconciling\n"
            f"  Try: kubectl rollout restart deployment -n kuadrant-system -l control-plane=controller-manager"
        )
        log.info(f"[resource] TelemetryPolicy '{name}' is enforced")
        print(f"[resource] TelemetryPolicy '{name}' is enforced")

    def test_istio_telemetry_exists(self, expected_metrics_config):
        """Verify Istio Telemetry resource is deployed for per-user latency tracking."""
        cfg = expected_metrics_config["resources"]["istio_telemetry"]
        name = cfg["name"]
        namespace = cfg["namespace"]

        exists = _resource_exists("telemetry.telemetry.istio.io", name, namespace)
        assert exists, (
            f"FAIL: Istio Telemetry '{name}' not found in namespace '{namespace}'.\n"
            f"  This resource is required for adding 'tier' label to istio_request_duration metrics.\n"
            f"  Deploy with: kustomize build deployment/base/observability | kubectl apply -f -"
        )
        log.info(f"[resource] Istio Telemetry '{name}' exists in '{namespace}'")
        print(f"[resource] Istio Telemetry '{name}' exists in '{namespace}'")

    def test_limitador_metrics_scraping_configured(self, expected_metrics_config):
        """Verify Limitador metrics scraping is configured (ServiceMonitor or Kuadrant PodMonitor).

        When Kuadrant CR has spec.observability.enable=true, the operator creates its own
        PodMonitor (kuadrant-limitador-monitor). In that case, install-observability.sh
        skips deploying the MaaS ServiceMonitor to avoid duplicate metrics.
        Either mechanism is acceptable.
        Passes with a note for edit users who lack RBAC to list ServiceMonitor
        (already validated by admin run).
        """
        cfg = expected_metrics_config["resources"]["limitador_servicemonitor"]
        name = cfg["name"]
        namespace = cfg["namespace"]

        can_list, skip_reason = _can_list_servicemonitors_in(namespace)
        if not can_list:
            print(f"PASS: {skip_reason} (already validated by admin observability run)")
            return

        # Check for MaaS-deployed ServiceMonitor
        has_servicemonitor = _resource_exists("servicemonitor", name, namespace)
        # Check for Kuadrant-managed PodMonitor
        has_podmonitor = _resource_exists("podmonitor", "kuadrant-limitador-monitor", namespace)

        assert has_servicemonitor or has_podmonitor, (
            f"FAIL: No Limitador metrics scraping configured in namespace '{namespace}'.\n"
            f"  Expected either:\n"
            f"    - ServiceMonitor '{name}' (deployed by install-observability.sh)\n"
            f"    - PodMonitor 'kuadrant-limitador-monitor' (deployed by Kuadrant operator)\n"
            f"  Check:\n"
            f"    1. kubectl get servicemonitor,podmonitor -n {namespace}\n"
            f"    2. Deploy with: scripts/install-observability.sh"
        )
        if has_podmonitor:
            log.info(f"[resource] Kuadrant PodMonitor 'kuadrant-limitador-monitor' exists in '{namespace}'")
            print(f"[resource] Kuadrant PodMonitor 'kuadrant-limitador-monitor' exists in '{namespace}' (Kuadrant-managed)")
        else:
            log.info(f"[resource] ServiceMonitor '{name}' exists in '{namespace}'")
            print(f"[resource] ServiceMonitor '{name}' exists in '{namespace}'")


class TestLimitadorConfiguration:
    """Tests for verifying Limitador rate-limiting is properly configured."""

    def test_rate_limit_policies_enforced(self, expected_metrics_config):
        """
        Verify that rate-limiting policies exist and are enforced.
        
        This is a prerequisite for rate-limiting metrics to be generated.
        If no policies are enforced, authorized_calls/limited_calls metrics won't exist.
        """
        # Check for RateLimitPolicy or TokenRateLimitPolicy resources
        policies_found = []
        
        for policy_kind in ["ratelimitpolicy", "tokenratelimitpolicy"]:
            rc, output, _ = _run_kubectl([
                "get", policy_kind, "-A",
                "-o", "jsonpath={range .items[*]}{.metadata.namespace}/{.metadata.name}: "
                "{.status.conditions[?(@.type=='Enforced')].status}{'\\n'}{end}"
            ])
            if rc == 0 and output.strip():
                for line in output.strip().split("\n"):
                    if line.strip():
                        policies_found.append((policy_kind, line.strip()))
        
        if not policies_found:
            pytest.fail(
                "FAIL: No RateLimitPolicy or TokenRateLimitPolicy found on the cluster!\n"
                "  Rate-limiting metrics (authorized_calls, limited_calls) will NOT be generated.\n"
                "  Check:\n"
                "    1. RateLimitPolicy is deployed: kubectl get ratelimitpolicy -A\n"
                "    2. TokenRateLimitPolicy is deployed: kubectl get tokenratelimitpolicy -A\n"
                "    3. Policies target the correct Gateway"
            )
        
        # Verify at least one is enforced
        enforced = [p for p in policies_found if "True" in p[1]]
        if not enforced:
            pytest.fail(
                f"FAIL: Rate limit policies exist but none are enforced!\n"
                f"  Found policies: {policies_found}\n"
                f"  Check:\n"
                f"    1. Gateway is ready\n"
                f"    2. Kuadrant operator is reconciling\n"
                f"    3. kubectl describe ratelimitpolicy -A"
            )
        
        print(f"[limitador] Found {len(enforced)} enforced rate limit policy(ies)")
        for kind, info in enforced:
            print(f"  {kind}: {info}")


# =============================================================================
# Metrics Availability Tests (Direct Limitador)
# =============================================================================

class TestLimitadorMetrics:
    """Tests for verifying Limitador metrics are available and have correct labels.
    
    NOTE: These tests depend on make_test_request to ensure at least one request
    has been made before checking metrics.
    """

    @pytest.fixture(scope="class")
    def limitador_metrics(self, expected_metrics_config, make_test_request) -> str:
        """
        Fetch raw metrics from Limitador via port-forward.
        Returns the raw Prometheus metrics text.

        Depends on make_test_request to ensure a request has been made first.
        """
        cfg = expected_metrics_config["limitador"]["access"]
        return _fetch_limitador_metrics(cfg["namespace"], cfg["port"], cfg["path"])

    def _metric_has_label(self, metrics_text: str, metric_name: str, label_name: str) -> bool:
        """Check if a metric has a specific label."""
        pattern = rf'^{re.escape(metric_name)}\{{[^}}]*{re.escape(label_name)}="[^"]*"'
        return bool(re.search(pattern, metrics_text, re.MULTILINE))

    def _get_metric_lines(self, metrics_text: str, metric_name: str) -> list[str]:
        """Get all lines for a specific metric."""
        lines = []
        for line in metrics_text.split("\n"):
            if line.startswith(metric_name + "{") or line.startswith(metric_name + " "):
                lines.append(line)
        return lines

    def test_authorized_hits_metric_exists(self, limitador_metrics, expected_metrics_config):
        """Verify authorized_hits metric is available (from observability.md Key Metrics Reference)."""
        metric_name = "authorized_hits"

        if not _metric_exists_in_text(limitador_metrics, metric_name):
            pytest.fail(
                f"FAIL: Metric '{metric_name}' not found in Limitador after making request.\n"
                f"  Reference: observability.md Key Metrics Reference\n"
                f"  Check:\n"
                f"    1. Limitador has limits: kubectl exec -n kuadrant-system <pod> -- curl -s http://localhost:8080/limits\n"
                f"    2. TokenRateLimitPolicy enforced: kubectl get tokenratelimitpolicy -A"
            )

        print(f"[metrics] Metric '{metric_name}' exists ✓")

    def test_authorized_calls_metric_exists(self, limitador_metrics, expected_metrics_config):
        """Verify authorized_calls metric is available."""
        metric_name = "authorized_calls"
        cfg = next(
            (m for m in expected_metrics_config["limitador"]["token_metrics"] if m["name"] == metric_name),
            None
        )
        assert cfg, f"FAIL: Metric '{metric_name}' not found in configuration"

        if not _metric_exists_in_text(limitador_metrics, metric_name):
            pytest.fail(
                f"FAIL: Metric '{metric_name}' not found in Limitador after making request.\n"
                f"  Reference: observability.md Key Metrics Reference\n"
                f"  Check:\n"
                f"    1. Limitador has limits: kubectl exec -n kuadrant-system <pod> -- curl -s http://localhost:8080/limits\n"
                f"    2. TokenRateLimitPolicy enforced: kubectl get tokenratelimitpolicy -A"
            )

        print(f"[metrics] Metric '{metric_name}' exists ✓")

    def test_limited_calls_metric_exists(self, limitador_metrics, expected_metrics_config):
        """
        Verify limited_calls metric is available.
        
        This metric appears after rate limiting occurs.
        Smoke tests trigger rate limiting, so this metric MUST exist.
        """
        metric_name = "limited_calls"
        cfg = next(
            (m for m in expected_metrics_config["limitador"]["token_metrics"] if m["name"] == metric_name),
            None
        )
        assert cfg, f"FAIL: Metric '{metric_name}' not found in configuration"

        if not _metric_exists_in_text(limitador_metrics, metric_name):
            pytest.fail(
                f"FAIL: Metric '{metric_name}' not found in Limitador.\n"
                f"  Reference: observability.md Key Metrics Reference\n"
                f"  This metric should exist after smoke tests trigger rate limiting.\n"
                f"  Check:\n"
                f"    1. Smoke tests ran and hit rate limits\n"
                f"    2. Limitador has limits: kubectl exec -n kuadrant-system <pod> -- curl -s http://localhost:8080/limits\n"
                f"    3. TokenRateLimitPolicy enforced: kubectl get tokenratelimitpolicy -A"
            )

        print(f"[metrics] Metric '{metric_name}' exists ✓")


# =============================================================================
# Direct /metrics for all components (isolate endpoint vs Prometheus scraping)
# =============================================================================

class TestDirectMetricsAllComponents:
    """
    Direct endpoint tests for all components via port-forward from the test process.
    Isolates endpoint vs Prometheus scraping. No exec into pods.
    - Limitador: /metrics
    - Istio gateway: /stats/prometheus (Envoy; not /metrics)
    - vLLM: /metrics (HTTPS when configured)
    - Authorino: /server-metrics
    """

    @pytest.fixture(scope="class")
    def istio_metrics_text(self, expected_metrics_config, make_test_request) -> str:
        """Fetch Istio gateway Envoy metrics via port-forward. Endpoint: /stats/prometheus."""
        access = expected_metrics_config.get("latency_metrics", {}).get("istio_gateway", {}).get("access")
        if not access:
            pytest.skip("latency_metrics.istio_gateway.access not in expected_metrics.yaml")
        return _fetch_metrics_via_port_forward(
            namespace=access["namespace"],
            pod_label_selector=access["pod_label_selector"],
            port=access["port"],
            path=access["path"],
            scheme=access.get("scheme", "http"),
            component_name="Istio gateway",
            local_port=LOCAL_PORT_ISTIO,
        )

    def test_istio_gateway_direct_metrics(self, istio_metrics_text, expected_metrics_config):
        """Verify Istio gateway exposes istio_* metrics on /stats/prometheus (Envoy; not /metrics)."""
        for name in ("istio_requests_total", "istio_request_duration_milliseconds"):
            if _metric_exists_in_text(istio_metrics_text, name):
                print(f"[metrics] Istio gateway direct /stats/prometheus: '{name}' exists ✓")
                return
        pytest.fail(
            "FAIL: No istio_requests_total or istio_request_duration_milliseconds in Istio gateway.\n"
            "  Endpoint: /stats/prometheus (Envoy does not expose /metrics).\n"
            "  Check: kubectl get pods -n openshift-ingress -l gateway.networking.k8s.io/gateway-name=maas-default-gateway"
        )

    @pytest.fixture(scope="class")
    def vllm_metrics_text(self, expected_metrics_config, make_test_request) -> str:
        """Fetch vLLM metrics from a model pod via port-forward (after traffic so metrics are registered)."""
        access = expected_metrics_config.get("latency_metrics", {}).get("vllm", {}).get("access")
        if not access:
            pytest.skip("latency_metrics.vllm.access not in expected_metrics.yaml")
        return _fetch_metrics_via_port_forward(
            namespace=access["namespace"],
            pod_label_selector=access["pod_label_selector"],
            port=access["port"],
            path=access["path"],
            scheme=access.get("scheme", "http"),
            component_name="vLLM/model",
            local_port=LOCAL_PORT_VLLM,
        )

    def test_vllm_direct_metrics(self, vllm_metrics_text, expected_metrics_config):
        """Verify vLLM/model pod exposes vllm:* metrics on /metrics."""
        for name in ("vllm:e2e_request_latency_seconds", "vllm:request_success_total", "vllm:num_requests_running"):
            if _metric_exists_in_text(vllm_metrics_text, name):
                print(f"[metrics] vLLM direct /metrics: '{name}' exists ✓")
                return
        pytest.fail(
            "FAIL: No vllm:* metrics in model pod /metrics (traffic may not have reached the pod yet).\n"
            "  This isolates endpoint vs scraping: if this fails, the model pod is not exposing metrics.\n"
            "  Check: kubectl get pods -n llm -l app.kubernetes.io/part-of=llminferenceservice"
        )

    @pytest.fixture(scope="class")
    def authorino_metrics_text(self, expected_metrics_config, make_test_request) -> str:
        """Fetch Authorino /server-metrics via port-forward."""
        access = expected_metrics_config.get("authorino", {}).get("access")
        if not access:
            pytest.skip("authorino.access not in expected_metrics.yaml")
        return _fetch_metrics_via_port_forward(
            namespace=access["namespace"],
            pod_label_selector=access["pod_label_selector"],
            port=access["port"],
            path=access["path"],
            scheme=access.get("scheme", "http"),
            component_name="Authorino",
            local_port=LOCAL_PORT_AUTHORINO,
        )

    def test_authorino_direct_server_metrics(self, authorino_metrics_text, expected_metrics_config):
        """Verify Authorino pod exposes auth_server_* on /server-metrics."""
        for name in ("auth_server_authconfig_duration_seconds", "auth_server_authconfig_response_status"):
            if _metric_exists_in_text(authorino_metrics_text, name):
                print(f"[metrics] Authorino direct /server-metrics: '{name}' exists ✓")
                return
        pytest.fail(
            "FAIL: No auth_server_* metrics in Authorino /server-metrics.\n"
            "  This isolates endpoint vs scraping: if this fails, Authorino is not exposing server-metrics.\n"
            "  Check: kubectl get pods -n kuadrant-system -l authorino-resource=authorino"
        )


# =============================================================================
# End-to-End Metrics Generation Tests
# =============================================================================

def _is_gateway_authpolicy_enforced() -> tuple[bool, str]:
    """
    Check if AuthPolicy is enforced on the Gateway.
    Returns (is_enforced, reason).
    """
    # Find the Gateway namespace
    rc, output, _ = _run_kubectl([
        "get", "gateway", "-A",
        "-o", "jsonpath={.items[?(@.metadata.name=='maas-default-gateway')].metadata.namespace}"
    ])
    if rc != 0 or not output.strip():
        return False, "Could not find maas-default-gateway"
    
    gateway_ns = output.strip()
    
    # Check for AuthPolicy targeting the Gateway in the same namespace
    rc, output, _ = _run_kubectl([
        "get", "authpolicy", "-n", gateway_ns,
        "-o", "jsonpath={.items[?(@.spec.targetRef.name=='maas-default-gateway')].status.conditions[?(@.type=='Enforced')].status}"
    ])
    
    if rc != 0:
        return False, f"Could not check AuthPolicy in {gateway_ns}"
    
    if output.strip() == "True":
        return True, "AuthPolicy is enforced"
    
    # Check if AuthPolicy exists but in wrong namespace
    rc, output, _ = _run_kubectl([
        "get", "authpolicy", "-A",
        "-o", "jsonpath={range .items[?(@.spec.targetRef.name=='maas-default-gateway')]}{.metadata.namespace}/{.metadata.name}: {.status.conditions[?(@.type=='Accepted')].message}{'\\n'}{end}"
    ])
    
    if output.strip():
        return False, f"AuthPolicy exists but not enforced: {output.strip()}"
    
    return False, f"No AuthPolicy found targeting maas-default-gateway in {gateway_ns}"


# =============================================================================
# Module-level fixtures for metrics tests
# =============================================================================

@pytest.fixture(scope="module")
def authpolicy_enforced():
    """Check if AuthPolicy is enforced - FAIL if not (required for metrics labels)."""
    is_enforced, reason = _is_gateway_authpolicy_enforced()
    if not is_enforced:
        pytest.fail(
            f"FAIL: AuthPolicy not enforced on Gateway.\n"
            f"  Reason: {reason}\n"
            f"  Labels (user, tier, model) are only injected when AuthPolicy is enforced.\n"
            f"  Check:\n"
            f"    1. AuthPolicy exists: kubectl get authpolicy -n openshift-ingress\n"
            f"    2. AuthPolicy status: kubectl describe authpolicy -n openshift-ingress"
        )
    return True


@pytest.fixture(scope="module")
def make_test_request(headers, model_v1, model_name, authpolicy_enforced):  # noqa: ARG001 (authpolicy_enforced is for ordering only)
    """Make a test request to generate metrics.

    The authpolicy_enforced parameter is not used directly — it ensures the
    AuthPolicy enforcement check runs before we attempt to make requests.

    Returns:
        int: HTTP status code (200, 429, etc.) or -1 if request failed due to network error
    """
    from tests.test_helper import chat

    log.info("[e2e] Making test request to generate metrics...")
    print(f"[e2e] Making test request to model '{model_name}'...")

    try:
        response = chat("Hello, this is a test for metrics.", model_v1, headers, model_name=model_name)
        status_code = response.status_code
        log.info(f"[e2e] Test request completed with status {status_code}")
        print(f"[e2e] Test request status: {status_code}")
    except Exception as e:
        # We don't fail if the request fails - the model might not be fully ready
        # Return -1 to signal network error; downstream tests will skip gracefully
        log.warning(f"[e2e] Test request failed with exception: {e}")
        print(f"[e2e] Test request failed: {e}")
        status_code = -1

    # Give metrics time to propagate
    time.sleep(2)

    return status_code


# =============================================================================
# Limitador Metrics Label Tests
# =============================================================================

class TestMetricsAfterRequest:
    """
    Tests that verify metrics are generated with correct labels after making requests.
    These tests make actual API calls and then verify the metrics.
    
    NOTE: These tests require AuthPolicy to be enforced on the Gateway for labels
    to be injected into metrics. Tests will skip if AuthPolicy is not enforced.
    """

    @pytest.fixture(scope="class")
    def limitador_metrics_after_request(self, make_test_request, expected_metrics_config) -> str:
        """Fetch Limitador metrics after making a test request."""
        cfg = expected_metrics_config["limitador"]["access"]
        return _fetch_limitador_metrics(cfg["namespace"], cfg["port"], cfg["path"])

    def _check_metric_label(self, metrics_text: str, metric_name: str, label_name: str) -> tuple[bool, str]:
        """Check if a metric has a specific label. Returns (has_label, sample_lines)."""
        pattern = rf'^{metric_name}\{{[^}}]*{label_name}="[^"]+"'
        has_label = bool(re.search(pattern, metrics_text, re.MULTILINE))
        
        sample_lines = [line for line in metrics_text.split("\n") if line.startswith(metric_name)][:3]
        sample = "\n".join(sample_lines) if sample_lines else "(no metrics found)"
        
        return has_label, sample

    def test_token_metrics_have_user_label(self, limitador_metrics_after_request, expected_metrics_config, make_test_request):
        """
        Verify token consumption metrics have 'user' label.
        Reference: observability.md Key Metrics Reference table.
        """
        if make_test_request not in (200, 429):
            pytest.fail(f"FAIL: Test request did not succeed (status {make_test_request}). Cannot verify metrics.")

        metrics_text = limitador_metrics_after_request
        
        token_metrics = ["authorized_hits", "authorized_calls", "limited_calls"]
        metrics_verified = 0
        
        for metric_name in token_metrics:
            if not _metric_exists_in_text(limitador_metrics_after_request, metric_name):
                continue
            
            has_label, sample = self._check_metric_label(metrics_text, metric_name, "user")
            if not has_label:
                pytest.fail(
                    f"FAIL: Metric '{metric_name}' does not have 'user' label.\n"
                    f"  Reference: observability.md Key Metrics Reference\n"
                    f"  The TelemetryPolicy should inject 'user' label from auth.identity.userid.\n"
                    f"  Sample metric lines:\n{sample}\n"
                    f"  Check:\n"
                    f"    1. TelemetryPolicy is enforced: kubectl get telemetrypolicy -n openshift-ingress\n"
                    f"    2. AuthPolicy is injecting identity: kubectl get authpolicy -n openshift-ingress"
                )
            metrics_verified += 1
            print(f"[e2e] Metric '{metric_name}' has 'user' label ✓")
        
        if metrics_verified == 0:
            pytest.fail(
                f"FAIL: No token metrics found to verify 'user' label.\n"
                f"  Expected at least one of: {token_metrics}\n"
                f"  This indicates Limitador is not generating rate-limiting metrics.\n"
                f"  Check:\n"
                f"    1. Limitador has limits: kubectl exec -n kuadrant-system <pod> -- curl -s http://localhost:8080/limits\n"
                f"    2. TokenRateLimitPolicy enforced: kubectl get tokenratelimitpolicy -A"
            )

    def test_token_metrics_have_tier_label(self, limitador_metrics_after_request, expected_metrics_config, make_test_request):
        """
        Verify token consumption metrics have 'tier' label.
        Reference: observability.md Key Metrics Reference table.
        """
        if make_test_request not in (200, 429):
            pytest.fail(f"FAIL: Test request did not succeed (status {make_test_request}). Cannot verify metrics.")

        metrics_text = limitador_metrics_after_request
        token_metrics = ["authorized_hits", "authorized_calls", "limited_calls"]
        metrics_verified = 0
        
        for metric_name in token_metrics:
            if not _metric_exists_in_text(limitador_metrics_after_request, metric_name):
                continue
            
            has_label, sample = self._check_metric_label(metrics_text, metric_name, "tier")
            if not has_label:
                pytest.fail(
                    f"FAIL: Metric '{metric_name}' does not have 'tier' label.\n"
                    f"  Reference: observability.md Key Metrics Reference\n"
                    f"  The TelemetryPolicy should inject 'tier' label from auth.identity.tier.\n"
                    f"  Sample metric lines:\n{sample}\n"
                    f"  Check:\n"
                    f"    1. TelemetryPolicy is enforced: kubectl get telemetrypolicy -n openshift-ingress\n"
                    f"    2. Tier lookup is working: curl /maas-api/v1/tiers/lookup"
                )
            metrics_verified += 1
            print(f"[e2e] Metric '{metric_name}' has 'tier' label ✓")
        
        if metrics_verified == 0:
            pytest.fail(
                f"FAIL: No token metrics found to verify 'tier' label.\n"
                f"  Expected at least one of: {token_metrics}\n"
                f"  This indicates Limitador is not generating rate-limiting metrics.\n"
                f"  Check:\n"
                f"    1. Limitador has limits: kubectl exec -n kuadrant-system <pod> -- curl -s http://localhost:8080/limits\n"
                f"    2. TokenRateLimitPolicy enforced: kubectl get tokenratelimitpolicy -A"
            )

    def test_authorized_hits_has_model_label(self, limitador_metrics_after_request, expected_metrics_config, make_test_request):
        """
        Verify authorized_hits (token consumption) metric has 'model' label.
        Reference: observability.md Key Metrics Reference table.
        
        NOTE: Only authorized_hits has the 'model' label because it comes from
        TokenRateLimitPolicy which extracts the model from the response body.
        authorized_calls and limited_calls (from RateLimitPolicy) do NOT have
        the 'model' label.
        """
        if make_test_request not in (200, 429):
            pytest.fail(f"FAIL: Test request did not succeed (status {make_test_request}). Cannot verify metrics.")

        metrics_text = limitador_metrics_after_request
        metric_name = "authorized_hits"

        if not _metric_exists_in_text(metrics_text, metric_name):
            pytest.fail(
                f"FAIL: Metric '{metric_name}' not found in Limitador.\n"
                f"  Cannot verify 'model' label without the metric.\n"
                f"  Check:\n"
                f"    1. TokenRateLimitPolicy enforced: kubectl get tokenratelimitpolicy -A\n"
                f"    2. Response contains usage.total_tokens field"
            )

        has_label, sample = self._check_metric_label(metrics_text, metric_name, "model")
        if not has_label:
            pytest.fail(
                f"FAIL: Metric '{metric_name}' does not have 'model' label.\n"
                f"  Reference: observability.md Key Metrics Reference\n"
                f"  The TelemetryPolicy should inject 'model' label from responseBodyJSON('/model').\n"
                f"  Sample metric lines:\n{sample}\n"
                f"  Check:\n"
                f"    1. TelemetryPolicy is enforced: kubectl get telemetrypolicy -n openshift-ingress\n"
                f"    2. Response contains model field in JSON body"
            )

        print(f"[e2e] Metric '{metric_name}' has 'model' label ✓")


# =============================================================================
# Prometheus Scraping Tests (run AFTER direct Limitador checks)
# =============================================================================

class TestPrometheusScrapingMetrics:
    """
    Tests for verifying metrics are being scraped by Prometheus.
    
    Limitador metrics are scraped by user-workload Prometheus.
    Istio gateway metrics are scraped by platform Prometheus (openshift-monitoring).
    """

    @staticmethod
    def _metric_exists(query_fn, metric_name: str) -> tuple[bool, str]:
        """Check if a metric exists using the given query function."""
        result = query_fn(metric_name)
        if result is None:
            return False, "Could not query Prometheus"
        if result.get("status") != "success":
            return False, f"Prometheus query failed: {result.get('error', 'unknown error')}"
        data = result.get("data", {})
        results = data.get("result", [])
        if len(results) > 0:
            return True, f"Found {len(results)} time series"
        return False, "No data found (metric not scraped or no data yet)"

    def test_prometheus_user_workload_available(self):
        """Verify Prometheus user-workload-monitoring is accessible."""
        result = _query_prometheus("up")
        
        if result is None:
            pytest.fail(
                "FAIL: Cannot reach Prometheus user-workload-monitoring.\n"
                "  Check:\n"
                "    1. User-workload-monitoring is enabled: kubectl get configmap cluster-monitoring-config -n openshift-monitoring -o yaml\n"
                "    2. Prometheus pods are running: kubectl get pods -n openshift-user-workload-monitoring"
            )
        
        assert result.get("status") == "success", (
            f"FAIL: Prometheus query failed: {result.get('error', 'unknown')}"
        )
        print("[prometheus] User-workload-monitoring Prometheus is accessible")

    def test_limitador_metrics_scraped(self, expected_metrics_config):
        """Verify Limitador metrics are being scraped by user-workload Prometheus."""
        exists, message = self._metric_exists(_query_prometheus, "limitador_up")
        
        if not exists:
            pytest.fail(
                f"FAIL: Limitador metrics are NOT being scraped by Prometheus.\n"
                f"  Query result: {message}\n"
                f"  This means the ServiceMonitor is not working correctly.\n"
                f"  Check:\n"
                f"    1. ServiceMonitor exists: kubectl get servicemonitor limitador-metrics -n kuadrant-system\n"
                f"    2. ServiceMonitor targets correct service: kubectl get svc -n kuadrant-system -l app=limitador\n"
                f"    3. Namespace has correct labels: kubectl get ns kuadrant-system --show-labels\n"
                f"    4. Prometheus targets: Check Prometheus UI targets page"
            )
        
        print(f"[prometheus] Limitador metrics are being scraped: {message}")

    def test_authorized_calls_in_prometheus(self, expected_metrics_config, make_test_request):
        """Verify authorized_calls metric appears in user-workload Prometheus after requests."""
        if make_test_request not in (200, 429):
            pytest.fail(f"FAIL: Test request did not succeed (status {make_test_request}). Cannot verify metrics.")
        
        exists, message = self._metric_exists(_query_prometheus, "authorized_calls")
        
        if not exists:
            pytest.fail(
                f"FAIL: Metric 'authorized_calls' not in Prometheus after making request.\n"
                f"  Result: {message}\n"
                f"  This means rate-limiting metrics are not being generated.\n"
                f"  Check:\n"
                f"    1. RateLimitPolicy is enforced: kubectl get ratelimitpolicy -A\n"
                f"    2. TokenRateLimitPolicy is enforced: kubectl get tokenratelimitpolicy -A\n"
                f"    3. ServiceMonitor is scraping: kubectl get servicemonitor -n kuadrant-system"
            )
        
        print(f"[prometheus] authorized_calls metric exists in Prometheus: {message}")

    def test_authorized_hits_in_prometheus(self, expected_metrics_config, make_test_request):
        """
        Verify authorized_hits metric appears in user-workload Prometheus after requests.
        Reference: observability.md Key Metrics Reference - Token Consumption.
        """
        if make_test_request not in (200, 429):
            pytest.fail(f"FAIL: Test request did not succeed (status {make_test_request}). Cannot verify metrics.")
        
        exists, message = self._metric_exists(_query_prometheus, "authorized_hits")
        
        if not exists:
            pytest.fail(
                f"FAIL: Metric 'authorized_hits' not in Prometheus after making request.\n"
                f"  Reference: observability.md Key Metrics Reference\n"
                f"  Result: {message}\n"
                f"  This metric tracks total tokens consumed.\n"
                f"  Check:\n"
                f"    1. TokenRateLimitPolicy is enforced: kubectl get tokenratelimitpolicy -A\n"
                f"    2. Response contains usage.total_tokens field\n"
                f"    3. ServiceMonitor is scraping: kubectl get servicemonitor -n kuadrant-system"
            )
        
        print(f"[prometheus] authorized_hits metric exists in Prometheus: {message}")

    def test_limited_calls_in_prometheus(self, expected_metrics_config, make_test_request):
        """
        Verify limited_calls metric exists in user-workload Prometheus.
        Reference: observability.md Key Metrics Reference - Token Consumption.
        
        Smoke tests trigger rate limiting, so this metric MUST exist.
        """
        if make_test_request not in (200, 429):
            pytest.fail(f"FAIL: Test request did not succeed (status {make_test_request}). Cannot verify metrics.")
        
        exists, message = self._metric_exists(_query_prometheus, "limited_calls")
        
        if not exists:
            pytest.fail(
                f"FAIL: Metric 'limited_calls' not in Prometheus.\n"
                f"  Reference: observability.md Key Metrics Reference\n"
                f"  Result: {message}\n"
                f"  This metric should exist after smoke tests trigger rate limiting.\n"
                f"  Check:\n"
                f"    1. Smoke tests ran and hit rate limits\n"
                f"    2. Limitador has limits configured\n"
                f"    3. ServiceMonitor is scraping: kubectl get servicemonitor -n kuadrant-system"
            )
        
        print(f"[prometheus] limited_calls metric exists in Prometheus: {message}")

    def test_istio_latency_metric_in_prometheus(self, expected_metrics_config, make_test_request):
        """
        Verify istio_request_duration_milliseconds_bucket is being scraped.
        Reference: observability.md Latency Metrics.
        
        NOTE: Istio gateway metrics are scraped by the platform Prometheus
        (openshift-monitoring), not the user-workload Prometheus.
        """
        if make_test_request not in (200, 429):
            pytest.fail(f"FAIL: Test request did not succeed (status {make_test_request}). Cannot verify metrics.")
        
        exists, message = self._metric_exists(
            _query_platform_prometheus,
            "istio_request_duration_milliseconds_bucket"
        )
        
        if not exists:
            pytest.fail(
                f"FAIL: Metric 'istio_request_duration_milliseconds_bucket' not in platform Prometheus.\n"
                f"  Reference: observability.md Latency Metrics\n"
                f"  Result: {message}\n"
                f"  This metric tracks gateway-level latency.\n"
                f"  Check:\n"
                f"    1. Istio Gateway ServiceMonitor exists: kubectl get servicemonitor -n openshift-ingress\n"
                f"    2. Gateway pods are exposing metrics: kubectl get pods -n openshift-ingress\n"
                f"    3. Platform Prometheus targets show Istio gateway"
            )
        
        print(f"[prometheus] istio_request_duration_milliseconds_bucket exists in Prometheus: {message}")

    def test_istio_requests_total_in_prometheus(self, expected_metrics_config, make_test_request):
        """
        Verify istio_requests_total is being scraped by platform Prometheus.
        Reference: observability.md Gateway Traffic Metrics.
        
        This metric is used in dashboards for error rate panels (4xx, 5xx, 401).
        Scraped by platform Prometheus (openshift-monitoring).
        """
        if make_test_request not in (200, 429):
            pytest.fail(f"FAIL: Test request did not succeed (status {make_test_request}). Cannot verify metrics.")
        
        exists, message = self._metric_exists(
            _query_platform_prometheus,
            "istio_requests_total"
        )
        
        if not exists:
            pytest.fail(
                f"FAIL: Metric 'istio_requests_total' not in platform Prometheus.\n"
                f"  Reference: observability.md Gateway Traffic Metrics\n"
                f"  Result: {message}\n"
                f"  This metric is used for error rate panels (4xx, 5xx, 401).\n"
                f"  Check:\n"
                f"    1. Istio Gateway ServiceMonitor exists: kubectl get servicemonitor -n openshift-ingress\n"
                f"    2. Gateway pods are exposing metrics: kubectl get pods -n openshift-ingress\n"
                f"    3. Platform Prometheus targets show Istio gateway"
            )
        
        print(f"[prometheus] istio_requests_total exists in Prometheus: {message}")


# =============================================================================
# Istio Latency Metrics Tests
# =============================================================================

class TestIstioLatencyMetrics:
    """
    Tests for verifying Istio gateway latency metrics have correct labels.
    Reference: observability.md Latency Metrics table.
    
    NOTE: Istio gateway metrics are scraped by the platform Prometheus
    (openshift-monitoring), not the user-workload Prometheus.
    """

    @staticmethod
    def _metric_has_label(metric_name: str, label_name: str) -> tuple[bool, str]:
        """Check if a metric in platform Prometheus has a specific label."""
        result = _query_platform_prometheus(f'{metric_name}{{}}')

        if result is None:
            return False, "Could not query platform Prometheus"

        if result.get("status") != "success":
            return False, f"Query failed: {result.get('error', 'unknown')}"

        data = result.get("data", {})
        results = data.get("result", [])

        if len(results) == 0:
            return False, "No metric data found"

        for r in results:
            metric = r.get("metric", {})
            if label_name in metric:
                return True, f"Found label '{label_name}' with value '{metric[label_name]}'"

        return False, f"Label '{label_name}' not found in any time series"

    def test_istio_latency_metric_has_tier_label(self, make_test_request):
        """
        Verify istio_request_duration_milliseconds_bucket has 'tier' label.
        Reference: observability.md Latency Metrics table.
        
        This is injected by the Istio Telemetry resource (latency-per-tier)
        from the X-MaaS-Tier header set by AuthPolicy.
        """
        if make_test_request not in (200, 429):
            pytest.fail(f"FAIL: Test request did not succeed (status {make_test_request}). Cannot verify metrics.")

        metric_name = "istio_request_duration_milliseconds_bucket"
        has_label, message = self._metric_has_label(metric_name, "tier")
        
        if message == "Could not query platform Prometheus":
            pytest.fail(
                "FAIL: Cannot query platform Prometheus for Istio metrics.\n"
                "  Check:\n"
                "    1. Prometheus pods are running: kubectl get pods -n openshift-monitoring\n"
                "    2. Current user has cluster-admin privileges"
            )
        
        if not has_label:
            pytest.fail(
                f"FAIL: Metric '{metric_name}' does not have 'tier' label in Prometheus.\n"
                f"  Reference: observability.md Latency Metrics table\n"
                f"  Result: {message}\n"
                "  The Istio Telemetry resource (latency-per-tier) should inject 'tier'\n"
                "  from the X-MaaS-Tier header set by AuthPolicy.\n"
                "  Check:\n"
                "    1. Telemetry resource exists: kubectl get telemetry latency-per-tier -n openshift-ingress\n"
                "    2. AuthPolicy injects X-MaaS-Tier header\n"
                "    3. Gateway metrics are being scraped by platform Prometheus"
            )
        
        print(f"[e2e] Metric '{metric_name}' has 'tier' label ✓")

    def test_istio_latency_metric_has_destination_service_label(self, make_test_request):
        """
        Verify istio_request_duration_milliseconds_bucket has 'destination_service_name' label.
        Reference: observability.md Latency Metrics table.
        """
        if make_test_request not in (200, 429):
            pytest.fail(f"FAIL: Test request did not succeed (status {make_test_request}). Cannot verify metrics.")

        metric_name = "istio_request_duration_milliseconds_bucket"
        has_label, message = self._metric_has_label(metric_name, "destination_service_name")
        
        if message == "Could not query platform Prometheus":
            pytest.fail(
                "FAIL: Cannot query platform Prometheus for Istio metrics.\n"
                "  Check:\n"
                "    1. Prometheus pods are running: kubectl get pods -n openshift-monitoring\n"
                "    2. Current user has cluster-admin privileges"
            )
        
        if not has_label:
            pytest.fail(
                f"FAIL: Metric '{metric_name}' does not have 'destination_service_name' label.\n"
                f"  Reference: observability.md Latency Metrics table\n"
                f"  Result: {message}\n"
                "  This is a standard Istio label that should be present.\n"
                "  Check:\n"
                "    1. Istio gateway metrics are being scraped by platform Prometheus\n"
                "    2. ServiceMonitor for Istio gateway exists in openshift-ingress"
            )
        
        print(f"[e2e] Metric '{metric_name}' has 'destination_service_name' label ✓")

    def test_istio_requests_total_has_response_code_label(self, make_test_request):
        """
        Verify istio_requests_total has 'response_code' label.
        Reference: observability.md Gateway Traffic Metrics.
        
        This label is used in dashboard error rate panels to filter by
        4xx, 5xx, and 401 status codes.
        """
        if make_test_request not in (200, 429):
            pytest.fail(f"FAIL: Test request did not succeed (status {make_test_request}). Cannot verify metrics.")

        metric_name = "istio_requests_total"
        has_label, message = self._metric_has_label(metric_name, "response_code")

        if message == "Could not query platform Prometheus":
            pytest.fail(
                "FAIL: Cannot query platform Prometheus for Istio metrics.\n"
                "  Check:\n"
                "    1. Prometheus pods are running: kubectl get pods -n openshift-monitoring\n"
                "    2. Current user has cluster-admin privileges"
            )

        if not has_label:
            pytest.fail(
                f"FAIL: Metric '{metric_name}' does not have 'response_code' label.\n"
                f"  Reference: observability.md Gateway Traffic Metrics\n"
                f"  Result: {message}\n"
                "  This label is required for error rate panels (4xx, 5xx, 401).\n"
                "  Check:\n"
                "    1. Istio gateway metrics are being scraped by platform Prometheus\n"
                "    2. ServiceMonitor for Istio gateway exists in openshift-ingress"
            )

        print(f"[e2e] Metric '{metric_name}' has 'response_code' label ✓")


# =============================================================================
# vLLM Metrics Tests — ALL dashboard metrics
# =============================================================================

def _assert_vllm_metric_exists(metric_query: str, display_name: str, dashboard: str):
    """Helper: assert a vLLM metric exists in user-workload Prometheus."""
    result = _query_prometheus(metric_query)

    if result is None:
        pytest.fail(f"FAIL: Could not query Prometheus for '{metric_query}'.")

    if result.get("status") != "success":
        pytest.fail(f"FAIL: Prometheus query failed for '{metric_query}': {result.get('error', 'unknown')}")

    data = result.get("data", {})
    results = data.get("result", [])

    if len(results) == 0:
        pytest.fail(
            f"FAIL: Metric '{metric_query}' not found in Prometheus.\n"
            f"  Dashboard panel: {dashboard}\n"
            f"  Both real vLLM and simulator v0.7.1+ expose this metric.\n"
            f"  Check:\n"
            f"    1. Model pods are running: kubectl get pods -n llm\n"
            f"    2. ServiceMonitor exists: kubectl get servicemonitor -n llm\n"
            f"    3. Traffic has been sent to generate metrics (lazily registered)"
        )

    print(f"[e2e] Metric '{display_name}' exists in Prometheus ✓")
    return results


def _assert_vllm_metric_has_label(metric_query: str, label_name: str, display_name: str):
    """Helper: assert a vLLM metric has a specific label."""
    result = _query_prometheus(metric_query)

    if result is None or result.get("status") != "success":
        pytest.fail(f"FAIL: Could not query Prometheus for '{metric_query}'.")

    data = result.get("data", {})
    results = data.get("result", [])

    if len(results) == 0:
        pytest.fail(
            f"FAIL: Metric '{metric_query}' not found in Prometheus.\n"
            f"  Cannot verify '{label_name}' label without metric data."
        )

    for r in results:
        metric = r.get("metric", {})
        if label_name in metric:
            print(f"[e2e] Metric '{display_name}' has '{label_name}' label: '{metric[label_name]}' ✓")
            return

    pytest.fail(
        f"FAIL: Metric '{metric_query}' does not have '{label_name}' label.\n"
        f"  This label should be present on vLLM metrics."
    )


class TestVLLMMetrics:
    """
    Tests for ALL vLLM metrics used in Grafana dashboards.
    Reference: observability.md vLLM Metrics table + dashboard JSON.

    vLLM metrics are scraped by user-workload Prometheus.
    Both real vLLM models and the simulator (v0.7.1+) expose these metrics.

    NOTE: Histogram metrics must be queried with a suffix (_count, _bucket, _sum)
    because Prometheus does not return results for the base histogram name.

    DEPENDENCY: These tests rely on the module-scoped ``make_test_request`` fixture
    (triggered by TestMetricsAfterRequest / TestPrometheusScrapingMetrics) to ensure
    at least one inference request has been made so vLLM metrics are registered.
    If running this class in isolation, ensure traffic has been sent first.
    """

    # --- vllm:e2e_request_latency_seconds (histogram) ---
    # Dashboard: Platform Admin (Model Inference Latency), AI Engineer (Inference Success Rate)

    def test_vllm_e2e_latency_exists(self):
        """Verify vllm:e2e_request_latency_seconds is being scraped."""
        _assert_vllm_metric_exists(
            "vllm:e2e_request_latency_seconds_count",
            "vllm:e2e_request_latency_seconds",
            "Platform Admin → Model Inference Latency",
        )

    def test_vllm_e2e_latency_has_model_name(self):
        """Verify vllm:e2e_request_latency_seconds has 'model_name' label."""
        _assert_vllm_metric_has_label(
            "vllm:e2e_request_latency_seconds_count",
            "model_name",
            "vllm:e2e_request_latency_seconds",
        )

    # --- vllm:request_success_total (counter) ---
    # Dashboard: Platform Admin (Inference Success Rate), AI Engineer (Inference Success Rate)

    def test_vllm_request_success_total_exists(self):
        """Verify vllm:request_success_total is being scraped."""
        _assert_vllm_metric_exists(
            "vllm:request_success_total",
            "vllm:request_success_total",
            "Platform Admin → Inference Success Rate",
        )

    # --- vllm:num_requests_running (gauge) ---
    # Dashboard: Platform Admin (Requests Running, Model Queue Depth)

    def test_vllm_num_requests_running_exists(self):
        """Verify vllm:num_requests_running is being scraped."""
        _assert_vllm_metric_exists(
            "vllm:num_requests_running",
            "vllm:num_requests_running",
            "Platform Admin → Requests Running",
        )

    def test_vllm_num_requests_running_has_model_name(self):
        """Verify vllm:num_requests_running has 'model_name' label."""
        _assert_vllm_metric_has_label(
            "vllm:num_requests_running",
            "model_name",
            "vllm:num_requests_running",
        )

    # --- vllm:num_requests_waiting (gauge) ---
    # Dashboard: Platform Admin (Requests Waiting, Model Queue Depth)

    def test_vllm_num_requests_waiting_exists(self):
        """Verify vllm:num_requests_waiting is being scraped."""
        _assert_vllm_metric_exists(
            "vllm:num_requests_waiting",
            "vllm:num_requests_waiting",
            "Platform Admin → Requests Waiting",
        )

    def test_vllm_num_requests_waiting_has_model_name(self):
        """Verify vllm:num_requests_waiting has 'model_name' label."""
        _assert_vllm_metric_has_label(
            "vllm:num_requests_waiting",
            "model_name",
            "vllm:num_requests_waiting",
        )

    # --- vllm:kv_cache_usage_perc (gauge) ---
    # Dashboard: Platform Admin (GPU Cache Usage)

    def test_vllm_kv_cache_usage_exists(self):
        """Verify vllm:kv_cache_usage_perc is being scraped."""
        _assert_vllm_metric_exists(
            "vllm:kv_cache_usage_perc",
            "vllm:kv_cache_usage_perc",
            "Platform Admin → GPU Cache Usage",
        )

    # --- vllm:request_prompt_tokens (histogram) ---
    # Dashboard: Platform Admin (Tokens 1h, Token Throughput, Prompt vs Generation Ratio)

    def test_vllm_prompt_tokens_exists(self):
        """Verify vllm:request_prompt_tokens is being scraped."""
        _assert_vllm_metric_exists(
            "vllm:request_prompt_tokens_sum",
            "vllm:request_prompt_tokens",
            "Platform Admin → Token Throughput",
        )

    # --- vllm:request_generation_tokens (histogram) ---
    # Dashboard: Platform Admin (Token Throughput, Prompt vs Generation Ratio)

    def test_vllm_generation_tokens_exists(self):
        """Verify vllm:request_generation_tokens is being scraped."""
        _assert_vllm_metric_exists(
            "vllm:request_generation_tokens_sum",
            "vllm:request_generation_tokens",
            "Platform Admin → Token Throughput",
        )

    # --- vllm:time_to_first_token_seconds (histogram) ---
    # Dashboard: Platform Admin (TTFT panel)

    def test_vllm_ttft_exists(self):
        """Verify vllm:time_to_first_token_seconds is being scraped."""
        _assert_vllm_metric_exists(
            "vllm:time_to_first_token_seconds_count",
            "vllm:time_to_first_token_seconds",
            "Platform Admin → Time to First Token (TTFT)",
        )

    # --- vllm:inter_token_latency_seconds (histogram) ---
    # Dashboard: Platform Admin (ITL panel)

    def test_vllm_itl_exists(self):
        """Verify vllm:inter_token_latency_seconds is being scraped."""
        _assert_vllm_metric_exists(
            "vllm:inter_token_latency_seconds_count",
            "vllm:inter_token_latency_seconds",
            "Platform Admin → Inter-Token Latency (ITL)",
        )

    # --- vllm:request_queue_time_seconds (histogram) ---
    # NOTE: vllm:request_queue_time_seconds is in the Platform Admin dashboard
    # but is NOT exposed by the simulator. Only real vLLM backends produce it.
    # Skipped from CI validation.


# =============================================================================
# Authorino Auth Server Metrics Tests — dashboard metrics
# =============================================================================

class TestAuthorinoMetrics:
    """
    Tests for Authorino auth server metrics used in the Platform Admin dashboard.

    These metrics are scraped from Authorino's /server-metrics endpoint via the
    authorino-server-metrics ServiceMonitor deployed by install-observability.sh.

    DEPENDENCY: These tests rely on the module-scoped ``make_test_request`` fixture
    (triggered by earlier test classes) to ensure auth evaluations have occurred.
    If running this class in isolation, ensure traffic has been sent first.
    """

    def test_authorino_server_metrics_scraping_configured(self):
        """Verify Authorino /server-metrics scraping is configured.

        Passes with a note for edit users who lack RBAC to list ServiceMonitor
        (already validated by admin run).
        """
        namespace = "kuadrant-system"
        can_list, skip_reason = _can_list_servicemonitors_in(namespace)
        if not can_list:
            print(f"PASS: {skip_reason} (already validated by admin observability run)")
            return

        # Check for MaaS-deployed ServiceMonitor
        has_maas_sm = _resource_exists(
            "servicemonitor", "authorino-server-metrics", "kuadrant-system"
        )
        # Check if Kuadrant already scrapes /server-metrics by inspecting
        # the actual endpoint paths in monitor specs (not just names).
        has_kuadrant_sm = False
        rc, output, _ = _run_kubectl([
            "get", "servicemonitor,podmonitor", "-n", "kuadrant-system",
            "-o", "json"
        ])
        if rc == 0:
            try:
                monitors = json.loads(output)
                for item in monitors.get("items", []):
                    endpoints = item.get("spec", {}).get("endpoints", [])
                    endpoints += item.get("spec", {}).get("podMetricsEndpoints", [])
                    for ep in endpoints:
                        if ep.get("path") == "/server-metrics":
                            has_kuadrant_sm = True
                            break
                    if has_kuadrant_sm:
                        break
            except (json.JSONDecodeError, KeyError):
                pass

        assert has_maas_sm or has_kuadrant_sm, (
            "FAIL: No Authorino /server-metrics scraping configured.\n"
            "  The Platform Admin dashboard needs auth evaluation metrics.\n"
            "  Check:\n"
            "    1. kubectl get servicemonitor authorino-server-metrics -n kuadrant-system\n"
            "    2. Deploy with: scripts/install-observability.sh"
        )
        print("[resource] Authorino /server-metrics scraping configured ✓")

    def test_auth_evaluation_latency_exists(self):
        """
        Verify auth_server_authconfig_duration_seconds is being scraped.
        Dashboard: Platform Admin → Auth Evaluation Latency (P50/P95/P99).
        """
        metric_name = "auth_server_authconfig_duration_seconds_count"
        result = _query_prometheus(metric_name)

        if result is None:
            pytest.fail("FAIL: Could not query Prometheus for Authorino metrics.")

        if result.get("status") != "success":
            pytest.fail(f"FAIL: Prometheus query failed: {result.get('error', 'unknown')}")

        data = result.get("data", {})
        results = data.get("result", [])

        if len(results) == 0:
            pytest.fail(
                f"FAIL: Metric '{metric_name}' not found in Prometheus.\n"
                f"  Dashboard: Platform Admin → Auth Evaluation Latency\n"
                f"  This metric requires the authorino-server-metrics ServiceMonitor.\n"
                f"  Check:\n"
                f"    1. ServiceMonitor exists: kubectl get servicemonitor authorino-server-metrics -n kuadrant-system\n"
                f"    2. Authorino pods are running: kubectl get pods -n kuadrant-system -l authorino-resource=authorino\n"
                f"    3. Traffic has been sent to trigger auth evaluations"
            )

        print(f"[e2e] Metric '{metric_name}' exists in Prometheus ✓")

    def test_auth_response_status_exists(self):
        """
        Verify auth_server_authconfig_response_status is being scraped.
        Dashboard: Platform Admin → Auth Success / Deny Rate.
        """
        metric_name = "auth_server_authconfig_response_status"
        result = _query_prometheus(metric_name)

        if result is None:
            pytest.fail("FAIL: Could not query Prometheus for Authorino metrics.")

        if result.get("status") != "success":
            pytest.fail(f"FAIL: Prometheus query failed: {result.get('error', 'unknown')}")

        data = result.get("data", {})
        results = data.get("result", [])

        if len(results) == 0:
            pytest.fail(
                f"FAIL: Metric '{metric_name}' not found in Prometheus.\n"
                f"  Dashboard: Platform Admin → Auth Success / Deny Rate\n"
                f"  This metric requires the authorino-server-metrics ServiceMonitor.\n"
                f"  Check:\n"
                f"    1. ServiceMonitor exists: kubectl get servicemonitor authorino-server-metrics -n kuadrant-system\n"
                f"    2. Authorino pods are running: kubectl get pods -n kuadrant-system -l authorino-resource=authorino\n"
                f"    3. Traffic has been sent to trigger auth evaluations"
            )

        print(f"[e2e] Metric '{metric_name}' exists in Prometheus ✓")

    def test_auth_response_status_has_status_label(self):
        """
        Verify auth_server_authconfig_response_status has 'status' label.
        Dashboard: Platform Admin → Auth Success / Deny Rate (grouped by status).
        """
        metric_name = "auth_server_authconfig_response_status"
        result = _query_prometheus(metric_name)

        if result is None or result.get("status") != "success":
            pytest.fail("FAIL: Could not query Prometheus for Authorino metrics.")

        data = result.get("data", {})
        results = data.get("result", [])

        if len(results) == 0:
            pytest.fail(
                f"FAIL: Metric '{metric_name}' not found. Cannot verify 'status' label."
            )

        for r in results:
            metric = r.get("metric", {})
            if "status" in metric:
                print(f"[e2e] Metric '{metric_name}' has 'status' label: '{metric['status']}' ✓")
                return

        pytest.fail(
            f"FAIL: Metric '{metric_name}' does not have 'status' label.\n"
            f"  Dashboard: Platform Admin → Auth Success / Deny Rate\n"
            f"  The 'status' label (OK, PERMISSION_DENIED, etc.) is required for the dashboard."
        )


# =============================================================================
# Metric Type Validation — data-driven from expected_metrics.yaml
# =============================================================================

def _collect_all_metrics_with_type(config: dict) -> list[tuple[str, str, str]]:
    """
    Walk expected_metrics.yaml and collect all (metric_name, expected_type, prometheus_source)
    tuples where 'type' is declared.

    prometheus_source is 'user-workload' or 'platform' to indicate which Prometheus to query.
    """
    entries = []

    # Limitador metrics → user-workload Prometheus
    for section in ("token_metrics", "health_metrics"):
        for m in config.get("limitador", {}).get(section, []):
            if "type" in m:
                entries.append((m["name"], m["type"], "user-workload"))

    # Istio gateway metrics → platform Prometheus
    for m in config.get("latency_metrics", {}).get("istio_gateway", {}).get("metrics", []):
        if "type" in m:
            entries.append((m["name"], m["type"], "platform"))

    # vLLM metrics → user-workload Prometheus
    for m in config.get("latency_metrics", {}).get("vllm", {}).get("metrics", []):
        if "type" in m:
            entries.append((m["name"], m["type"], "user-workload"))

    # Authorino metrics → user-workload Prometheus
    for m in config.get("authorino", {}).get("metrics", []):
        if "type" in m:
            entries.append((m["name"], m["type"], "user-workload"))

    return entries


class TestMetricTypes:
    """
    Data-driven test that validates every metric's type in Prometheus
    matches the type declared in expected_metrics.yaml.

    Uses the Prometheus /api/v1/metadata endpoint.
    """

    @pytest.fixture(scope="class")
    def metrics_with_types(self, expected_metrics_config):
        """Collect all metrics that declare a type."""
        entries = _collect_all_metrics_with_type(expected_metrics_config)
        if not entries:
            pytest.skip("No metrics with 'type' declared in config")
        return entries

    def test_all_metric_types_match(self, metrics_with_types):
        """Verify every metric's type in Prometheus matches expected_metrics.yaml."""
        mismatches = []
        skipped = []

        for metric_name, expected_type, source in metrics_with_types:
            if source == "platform":
                actual_type = _query_prometheus_metadata(
                    metric_name,
                    namespace="openshift-monitoring",
                    pod="prometheus-k8s-0",
                )
            else:
                actual_type = _query_prometheus_metadata(metric_name)

            if actual_type is None:
                skipped.append(metric_name)
                continue

            if actual_type != expected_type:
                mismatches.append(
                    f"  {metric_name}: expected '{expected_type}', got '{actual_type}'"
                )
            else:
                print(f"[type] {metric_name}: {actual_type} ✓")

        if skipped:
            print(f"[type] Skipped (no metadata): {', '.join(skipped)}")

        # Guard: if most metrics were skipped, Prometheus metadata may not be available yet
        total = len(metrics_with_types)
        skip_ratio = len(skipped) / total if total else 0
        if skip_ratio > 0.5 and total > 2:
            pytest.fail(
                f"FAIL: {len(skipped)}/{total} metrics had no Prometheus metadata "
                f"({skip_ratio:.0%} skipped). Prometheus may not have scraped targets yet.\n"
                f"  Skipped: {', '.join(skipped)}"
            )

        if mismatches:
            pytest.fail(
                "FAIL: Metric type mismatches found:\n"
                + "\n".join(mismatches)
                + "\n  Update expected_metrics.yaml or fix the metric source."
            )
