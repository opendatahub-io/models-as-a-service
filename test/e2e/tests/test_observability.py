"""
Observability Tests for MaaS Platform
=====================================

These tests verify that the observability stack is correctly deployed and
that metrics are being generated with the expected labels.

Tests will FAIL with informative messages if:
- Kubernetes resources (TelemetryPolicy, Telemetry, ServiceMonitor) are missing
- Metrics are not available from Limitador
- Metrics are missing expected labels (user, tier, model)
- Metric names have changed or are absent

Configuration is loaded from: test/e2e/config/expected_metrics.yaml
"""

import json
import logging
import os
import re
import subprocess
import time
from pathlib import Path

import pytest
import requests
import yaml

log = logging.getLogger(__name__)

# Load expected metrics configuration
CONFIG_PATH = Path(__file__).parent.parent / "config" / "expected_metrics.yaml"


@pytest.fixture(scope="module")
def expected_metrics_config():
    """Load the expected metrics configuration from YAML."""
    if not CONFIG_PATH.exists():
        pytest.fail(f"FAIL: Metrics configuration file not found: {CONFIG_PATH}")

    with open(CONFIG_PATH) as f:
        config = yaml.safe_load(f)

    log.info(f"[config] Loaded metrics configuration from {CONFIG_PATH}")
    return config


def _run_kubectl(args: list[str], timeout: int = 30) -> tuple[int, str, str]:
    """Run kubectl command and return (returncode, stdout, stderr)."""
    cmd = ["kubectl"] + args
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
            f"  This resource is required for adding 'user' label to istio_request_duration metrics.\n"
            f"  Deploy with: kustomize build deployment/base/observability | kubectl apply -f -"
        )
        log.info(f"[resource] Istio Telemetry '{name}' exists in '{namespace}'")
        print(f"[resource] Istio Telemetry '{name}' exists in '{namespace}'")

    def test_limitador_servicemonitor_exists(self, expected_metrics_config):
        """Verify ServiceMonitor for Limitador is deployed."""
        cfg = expected_metrics_config["resources"]["limitador_servicemonitor"]
        name = cfg["name"]
        namespace = cfg["namespace"]

        exists = _resource_exists("servicemonitor", name, namespace)
        assert exists, (
            f"FAIL: ServiceMonitor '{name}' not found in namespace '{namespace}'.\n"
            f"  This resource is required for Prometheus to scrape Limitador metrics.\n"
            f"  Deploy with: kustomize build deployment/base/observability | kubectl apply -f -"
        )
        log.info(f"[resource] ServiceMonitor '{name}' exists in '{namespace}'")
        print(f"[resource] ServiceMonitor '{name}' exists in '{namespace}'")


class TestLimitadorConfiguration:
    """Tests for verifying Limitador rate-limiting is properly configured."""

    def test_limitador_has_limits_configured(self, expected_metrics_config):
        """
        Verify that Limitador has rate limits configured.
        
        This is a prerequisite for rate-limiting metrics to be generated.
        If no limits are configured, authorized_calls/limited_calls metrics won't exist.
        """
        cfg = expected_metrics_config["limitador"]["access"]
        namespace = cfg["namespace"]
        
        # Get Limitador pod
        pod_cmd = [
            "get", "pod", "-n", namespace,
            "-l", "app=limitador",
            "-o", "jsonpath={.items[0].metadata.name}"
        ]
        rc, pod_name, _ = _run_kubectl(pod_cmd)
        if rc != 0 or not pod_name.strip():
            pytest.fail(
                f"FAIL: Could not find Limitador pod in namespace '{namespace}'.\n"
                f"  Check: kubectl get pods -n {namespace} -l app=limitador"
            )
        
        pod_name = pod_name.strip()
        
        # Check /limits endpoint
        exec_cmd = [
            "exec", "-n", namespace, pod_name, "--",
            "curl", "-s", "http://localhost:8080/limits"
        ]
        rc, limits_output, stderr = _run_kubectl(exec_cmd, timeout=30)
        
        if rc != 0:
            pytest.fail(f"FAIL: Could not query Limitador limits: {stderr}")
        
        # Parse limits - should be a JSON array
        limits_output = limits_output.strip()
        
        if not limits_output or limits_output == "[]" or limits_output == "null":
            pytest.fail(
                "FAIL: Limitador has NO rate limits configured!\n"
                "  This means rate-limiting is not active despite policies being 'Enforced'.\n"
                "  Rate-limiting metrics (authorized_calls, limited_calls) will NOT be generated.\n"
                "  Check:\n"
                "    1. RateLimitPolicy or TokenRateLimitPolicy is deployed\n"
                "    2. Policy targets the correct Gateway\n"
                "    3. Kuadrant operator logs: kubectl logs -n kuadrant-system -l control-plane=controller-manager\n"
                "    4. Limitador logs: kubectl logs -n kuadrant-system -l app=limitador"
            )
        
        try:
            limits = json.loads(limits_output)
            if isinstance(limits, list) and len(limits) > 0:
                print(f"[limitador] Found {len(limits)} rate limit(s) configured")
                log.info(f"[limitador] Limits: {limits}")
            else:
                pytest.fail(
                    f"FAIL: Limitador limits response is empty or invalid: {limits_output[:200]}"
                )
        except json.JSONDecodeError:
            # Not JSON - might be empty or error
            pytest.fail(
                f"FAIL: Limitador /limits returned invalid response: {limits_output[:200]}"
            )


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
        Fetch raw metrics from Limitador via kubectl port-forward.
        Returns the raw Prometheus metrics text.
        
        Depends on make_test_request to ensure a request has been made first.
        """
        cfg = expected_metrics_config["limitador"]["access"]
        service = cfg["service"]
        namespace = cfg["namespace"]
        port = cfg["port"]
        path = cfg["path"]

        # Use kubectl exec to curl metrics from within the cluster
        # This avoids needing port-forward which can be flaky in CI
        pod_cmd = [
            "get", "pod", "-n", namespace,
            "-l", "app=limitador",
            "-o", "jsonpath={.items[0].metadata.name}"
        ]
        rc, pod_name, _ = _run_kubectl(pod_cmd)
        if rc != 0 or not pod_name.strip():
            pytest.fail(
                f"FAIL: Could not find Limitador pod in namespace '{namespace}'.\n"
                f"  Check: kubectl get pods -n {namespace} -l app=limitador"
            )

        pod_name = pod_name.strip()

        # Curl metrics from localhost within the pod
        exec_cmd = [
            "exec", "-n", namespace, pod_name, "--",
            "curl", "-s", f"http://localhost:{port}{path}"
        ]
        rc, metrics_text, stderr = _run_kubectl(exec_cmd, timeout=60)

        if rc != 0:
            pytest.fail(
                f"FAIL: Could not fetch metrics from Limitador pod '{pod_name}'.\n"
                f"  Error: {stderr}\n"
                f"  Check: kubectl exec -n {namespace} {pod_name} -- curl -s http://localhost:{port}{path}"
            )

        log.info(f"[metrics] Fetched {len(metrics_text)} bytes from Limitador")
        return metrics_text

    def _metric_exists(self, metrics_text: str, metric_name: str) -> bool:
        """Check if a metric exists in the Prometheus metrics text."""
        # Match metric name at start of line, followed by { or space or newline
        pattern = rf"^{re.escape(metric_name)}[\{{\s]"
        return bool(re.search(pattern, metrics_text, re.MULTILINE))

    def _metric_has_label(self, metrics_text: str, metric_name: str, label_name: str) -> bool:
        """Check if a metric has a specific label."""
        # Match metric_name{...label_name="..."}
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

        if not self._metric_exists(limitador_metrics, metric_name):
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

        if not self._metric_exists(limitador_metrics, metric_name):
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

        if not self._metric_exists(limitador_metrics, metric_name):
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
def make_test_request(headers, model_v1, model_name, authpolicy_enforced):
    """Make a test request to generate metrics.
    
    Returns:
        int: HTTP status code (200, 429, etc.) or -1 if request failed due to network error
    """
    from tests.test_helper import chat

    log.info(f"[e2e] Making test request to generate metrics...")
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
        namespace = cfg["namespace"]
        port = cfg["port"]
        path = cfg["path"]

        # Get pod name
        pod_cmd = [
            "get", "pod", "-n", namespace,
            "-l", "app=limitador",
            "-o", "jsonpath={.items[0].metadata.name}"
        ]
        rc, pod_name, _ = _run_kubectl(pod_cmd)
        if rc != 0 or not pod_name.strip():
            pytest.fail(
                f"FAIL: Could not find Limitador pod for metrics verification.\n"
                f"  Check: kubectl get pods -n {namespace} -l app=limitador"
            )

        pod_name = pod_name.strip()

        # Fetch metrics
        exec_cmd = [
            "exec", "-n", namespace, pod_name, "--",
            "curl", "-s", f"http://localhost:{port}{path}"
        ]
        rc, metrics_text, stderr = _run_kubectl(exec_cmd, timeout=60)

        if rc != 0:
            pytest.fail(f"FAIL: Could not fetch metrics from Limitador: {stderr}")

        return metrics_text

    def _metric_exists(self, metrics_text: str, metric_name: str) -> bool:
        """Check if a metric exists in Prometheus format text."""
        for line in metrics_text.split("\n"):
            if line.startswith(metric_name + "{") or line.startswith(metric_name + " "):
                return True
        return False

    def _check_metric_label(self, metrics_text: str, metric_name: str, label_name: str) -> tuple[bool, str]:
        """Check if a metric has a specific label. Returns (has_label, sample_lines)."""
        pattern = rf'^{metric_name}\{{[^}}]*{label_name}="[^"]+"'
        has_label = bool(re.search(pattern, metrics_text, re.MULTILINE))
        
        sample_lines = [l for l in metrics_text.split("\n") if l.startswith(metric_name)][:3]
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
        
        # Check all token metrics for user label (from docs)
        token_metrics = ["authorized_hits", "authorized_calls", "limited_calls"]
        metrics_verified = 0
        
        for metric_name in token_metrics:
            if not self._metric_exists(limitador_metrics_after_request, metric_name):
                continue  # Skip if metric doesn't exist yet
            
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
        
        # Fail if no metrics were found to verify
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
            if not self._metric_exists(limitador_metrics_after_request, metric_name):
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
        
        # Fail if no metrics were found to verify
        if metrics_verified == 0:
            pytest.fail(
                f"FAIL: No token metrics found to verify 'tier' label.\n"
                f"  Expected at least one of: {token_metrics}\n"
                f"  This indicates Limitador is not generating rate-limiting metrics.\n"
                f"  Check:\n"
                f"    1. Limitador has limits: kubectl exec -n kuadrant-system <pod> -- curl -s http://localhost:8080/limits\n"
                f"    2. TokenRateLimitPolicy enforced: kubectl get tokenratelimitpolicy -A"
            )

    def test_token_metrics_have_model_label(self, limitador_metrics_after_request, expected_metrics_config, make_test_request):
        """
        Verify token consumption metrics have 'model' label.
        Reference: observability.md Key Metrics Reference table.
        """
        if make_test_request not in (200, 429):
            pytest.fail(f"FAIL: Test request did not succeed (status {make_test_request}). Cannot verify metrics.")

        metrics_text = limitador_metrics_after_request
        token_metrics = ["authorized_hits", "authorized_calls", "limited_calls"]
        metrics_verified = 0
        
        for metric_name in token_metrics:
            if not self._metric_exists(limitador_metrics_after_request, metric_name):
                continue
            
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
            metrics_verified += 1
            print(f"[e2e] Metric '{metric_name}' has 'model' label ✓")
        
        # Fail if no metrics were found to verify
        if metrics_verified == 0:
            pytest.fail(
                f"FAIL: No token metrics found to verify 'model' label.\n"
                f"  Expected at least one of: {token_metrics}\n"
                f"  This indicates Limitador is not generating rate-limiting metrics.\n"
                f"  Check:\n"
                f"    1. Limitador has limits: kubectl exec -n kuadrant-system <pod> -- curl -s http://localhost:8080/limits\n"
                f"    2. TokenRateLimitPolicy enforced: kubectl get tokenratelimitpolicy -A"
            )


# =============================================================================
# Prometheus Scraping Tests (run AFTER direct Limitador checks)
# =============================================================================

class TestPrometheusScrapingMetrics:
    """
    Tests for verifying metrics are being scraped by Prometheus.
    
    These tests run AFTER TestLimitadorMetrics and TestMetricsAfterRequest
    to ensure we first verify the source (Limitador) has metrics, then
    verify Prometheus is scraping them.
    """

    def _query_prometheus(self, query: str) -> dict | None:
        """
        Query Prometheus via kubectl exec.
        Returns the JSON response or None if query failed.
        """
        exec_cmd = [
            "exec", "-n", "openshift-user-workload-monitoring",
            "prometheus-user-workload-0", "-c", "prometheus", "--",
            "curl", "-s", f"http://localhost:9090/api/v1/query?query={query}"
        ]
        rc, stdout, stderr = _run_kubectl(exec_cmd, timeout=30)
        
        if rc != 0:
            log.warning(f"[prometheus] Query failed: {stderr}")
            return None
        
        try:
            return json.loads(stdout)
        except Exception as e:
            log.warning(f"[prometheus] Failed to parse response: {e}")
            return None

    def _metric_exists_in_prometheus(self, metric_name: str) -> tuple[bool, str]:
        """
        Check if a metric exists in Prometheus.
        Returns (exists, message).
        """
        result = self._query_prometheus(metric_name)
        
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
        result = self._query_prometheus("up")
        
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
        """Verify Limitador metrics are being scraped by Prometheus."""
        exists, message = self._metric_exists_in_prometheus("limitador_up")
        
        if not exists:
            exists_any, msg_any = self._metric_exists_in_prometheus("{__name__=~\"limitador.*\"}")
            
            pytest.fail(
                f"FAIL: Limitador metrics are NOT being scraped by Prometheus.\n"
                f"  Query result: {message}\n"
                f"  Any limitador metrics: {msg_any}\n"
                f"  This means the ServiceMonitor is not working correctly.\n"
                f"  Check:\n"
                f"    1. ServiceMonitor exists: kubectl get servicemonitor limitador-metrics -n kuadrant-system\n"
                f"    2. ServiceMonitor targets correct service: kubectl get svc -n kuadrant-system -l app=limitador\n"
                f"    3. Namespace has correct labels: kubectl get ns kuadrant-system --show-labels\n"
                f"    4. Prometheus targets: Check Prometheus UI targets page"
            )
        
        print(f"[prometheus] Limitador metrics are being scraped: {message}")

    def test_authorized_calls_in_prometheus(self, expected_metrics_config, make_test_request):
        """Verify authorized_calls metric appears in Prometheus after requests."""
        if make_test_request not in (200, 429):
            pytest.fail(f"FAIL: Test request did not succeed (status {make_test_request}). Cannot verify metrics.")
        
        exists, message = self._metric_exists_in_prometheus("authorized_calls")
        
        if not exists:
            pytest.fail(
                f"FAIL: Metric 'authorized_calls' not in Prometheus after making request.\n"
                f"  Result: {message}\n"
                f"  This means rate-limiting metrics are not being generated.\n"
                f"  Check:\n"
                f"    1. Limitador has limits configured: kubectl exec -n kuadrant-system <limitador-pod> -- curl -s http://localhost:8080/limits\n"
                f"    2. TokenRateLimitPolicy is enforced: kubectl get tokenratelimitpolicy -A\n"
                f"    3. ServiceMonitor is scraping: kubectl get servicemonitor -n kuadrant-system"
            )
        
        print(f"[prometheus] authorized_calls metric exists in Prometheus: {message}")

    def test_authorized_hits_in_prometheus(self, expected_metrics_config, make_test_request):
        """
        Verify authorized_hits metric appears in Prometheus after requests.
        Reference: observability.md Key Metrics Reference - Token Consumption.
        """
        if make_test_request not in (200, 429):
            pytest.fail(f"FAIL: Test request did not succeed (status {make_test_request}). Cannot verify metrics.")
        
        exists, message = self._metric_exists_in_prometheus("authorized_hits")
        
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
        Verify limited_calls metric exists in Prometheus.
        Reference: observability.md Key Metrics Reference - Token Consumption.
        
        Smoke tests trigger rate limiting, so this metric MUST exist.
        """
        if make_test_request not in (200, 429):
            pytest.fail(f"FAIL: Test request did not succeed (status {make_test_request}). Cannot verify metrics.")
        
        exists, message = self._metric_exists_in_prometheus("limited_calls")
        
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
        """
        if make_test_request not in (200, 429):
            pytest.fail(f"FAIL: Test request did not succeed (status {make_test_request}). Cannot verify metrics.")
        
        exists, message = self._metric_exists_in_prometheus("istio_request_duration_milliseconds_bucket")
        
        if not exists:
            pytest.fail(
                f"FAIL: Metric 'istio_request_duration_milliseconds_bucket' not in Prometheus.\n"
                f"  Reference: observability.md Latency Metrics\n"
                f"  Result: {message}\n"
                f"  This metric tracks gateway-level latency.\n"
                f"  Check:\n"
                f"    1. Istio Gateway ServiceMonitor exists\n"
                f"    2. Gateway pods are exposing metrics: kubectl get svc -n openshift-ingress\n"
                f"    3. Prometheus targets show Istio gateway"
            )
        
        print(f"[prometheus] istio_request_duration_milliseconds_bucket exists in Prometheus: {message}")


# =============================================================================
# Istio Latency Metrics Tests
# =============================================================================

class TestIstioLatencyMetrics:
    """
    Tests for verifying Istio gateway latency metrics have correct labels.
    Reference: observability.md Latency Metrics table.
    """

    def _query_prometheus(self, query: str) -> dict | None:
        """Query Prometheus for metrics."""
        exec_cmd = [
            "exec", "-n", "openshift-user-workload-monitoring",
            "prometheus-user-workload-0", "-c", "prometheus", "--",
            "curl", "-s", f"http://localhost:9090/api/v1/query?query={query}"
        ]
        rc, stdout, stderr = _run_kubectl(exec_cmd, timeout=30)
        
        if rc != 0:
            log.warning(f"[prometheus] Query failed: {stderr}")
            return None
        
        try:
            return json.loads(stdout)
        except Exception as e:
            log.warning(f"[prometheus] Failed to parse response: {e}")
            return None

    def _metric_has_label_in_prometheus(self, metric_name: str, label_name: str) -> tuple[bool, str]:
        """Check if a metric in Prometheus has a specific label."""
        # Query for the metric with the label
        result = self._query_prometheus(f'{metric_name}{{}}')
        
        if result is None:
            return False, "Could not query Prometheus"
        
        if result.get("status") != "success":
            return False, f"Query failed: {result.get('error', 'unknown')}"
        
        data = result.get("data", {})
        results = data.get("result", [])
        
        if len(results) == 0:
            return False, "No metric data found"
        
        # Check if any result has the label
        for r in results:
            metric = r.get("metric", {})
            if label_name in metric:
                return True, f"Found label '{label_name}' with value '{metric[label_name]}'"
        
        return False, f"Label '{label_name}' not found in any time series"

    def test_istio_latency_metric_has_user_label(self, make_test_request):
        """
        Verify istio_request_duration_milliseconds_bucket has 'user' label.
        Reference: observability.md Latency Metrics table.
        
        This is injected by the Istio Telemetry resource from X-MaaS-Username header.
        """
        if make_test_request not in (200, 429):
            pytest.fail(f"FAIL: Test request did not succeed (status {make_test_request}). Cannot verify metrics.")

        metric_name = "istio_request_duration_milliseconds_bucket"
        has_label, message = self._metric_has_label_in_prometheus(metric_name, "user")
        
        if message == "Could not query Prometheus":
            pytest.fail(
                f"FAIL: Cannot query Prometheus for Istio metrics.\n"
                f"  Check:\n"
                f"    1. Prometheus pods are running: kubectl get pods -n openshift-user-workload-monitoring\n"
                f"    2. User-workload-monitoring is enabled"
            )
        
        if not has_label:
            pytest.fail(
                f"FAIL: Metric '{metric_name}' does not have 'user' label in Prometheus.\n"
                f"  Reference: observability.md Latency Metrics table\n"
                f"  Result: {message}\n"
                f"  The Istio Telemetry resource should inject 'user' from X-MaaS-Username header.\n"
                f"  Check:\n"
                f"    1. Telemetry resource exists: kubectl get telemetry -n openshift-ingress\n"
                f"    2. AuthPolicy injects X-MaaS-Username header\n"
                f"    3. Gateway metrics are being scraped by Prometheus"
            )
        
        print(f"[e2e] Metric '{metric_name}' has 'user' label ✓")

    def test_istio_latency_metric_has_destination_service_label(self, make_test_request):
        """
        Verify istio_request_duration_milliseconds_bucket has 'destination_service_name' label.
        Reference: observability.md Latency Metrics table.
        """
        if make_test_request not in (200, 429):
            pytest.fail(f"FAIL: Test request did not succeed (status {make_test_request}). Cannot verify metrics.")

        metric_name = "istio_request_duration_milliseconds_bucket"
        has_label, message = self._metric_has_label_in_prometheus(metric_name, "destination_service_name")
        
        if message == "Could not query Prometheus":
            pytest.fail(
                f"FAIL: Cannot query Prometheus for Istio metrics.\n"
                f"  Check:\n"
                f"    1. Prometheus pods are running: kubectl get pods -n openshift-user-workload-monitoring\n"
                f"    2. User-workload-monitoring is enabled"
            )
        
        if not has_label:
            pytest.fail(
                f"FAIL: Metric '{metric_name}' does not have 'destination_service_name' label.\n"
                f"  Reference: observability.md Latency Metrics table\n"
                f"  Result: {message}\n"
                f"  This is a standard Istio label that should be present.\n"
                f"  Check:\n"
                f"    1. Istio gateway metrics are being scraped\n"
                f"    2. ServiceMonitor for Istio gateway exists"
            )
        
        print(f"[e2e] Metric '{metric_name}' has 'destination_service_name' label ✓")


# =============================================================================
# vLLM Metrics Tests
# =============================================================================

class TestVLLMMetrics:
    """
    Tests for verifying vLLM model metrics have correct labels.
    Reference: observability.md Latency Metrics table.
    
    NOTE: These tests only apply to real vLLM models (not simulators).
    The simulator model (llm-d-inference-sim) does NOT expose vLLM metrics.
    """

    def _query_prometheus(self, query: str) -> dict | None:
        """Query Prometheus for metrics."""
        exec_cmd = [
            "exec", "-n", "openshift-user-workload-monitoring",
            "prometheus-user-workload-0", "-c", "prometheus", "--",
            "curl", "-s", f"http://localhost:9090/api/v1/query?query={query}"
        ]
        rc, stdout, stderr = _run_kubectl(exec_cmd, timeout=30)
        
        if rc != 0:
            return None
        
        try:
            return json.loads(stdout)
        except Exception:
            return None

    def test_vllm_latency_metric_exists(self):
        """
        Verify vllm:e2e_request_latency_seconds metric is being scraped.
        Reference: observability.md Latency Metrics table.
        
        NOTE: This test only applies to real vLLM models.
        Simulator models (llm-d-inference-sim) do NOT expose these metrics.
        """
        metric_name = "vllm:e2e_request_latency_seconds"
        result = self._query_prometheus(f'{metric_name}{{}}')
        
        if result is None:
            pytest.skip("Could not query Prometheus for vLLM metrics")
        
        if result.get("status") != "success":
            pytest.skip(f"Prometheus query failed: {result.get('error', 'unknown')}")
        
        data = result.get("data", {})
        results = data.get("result", [])
        
        if len(results) == 0:
            pytest.skip(
                f"Metric '{metric_name}' not found in Prometheus.\n"
                f"  This is EXPECTED if only simulator models are deployed.\n"
                f"  Simulator (llm-d-inference-sim) does NOT expose vLLM metrics.\n"
                f"  For real vLLM models, ensure ServiceMonitor is deployed:\n"
                f"    kubectl apply -f docs/samples/observability/kserve-llm-models-servicemonitor.yaml"
            )
        
        print(f"[e2e] Metric '{metric_name}' exists in Prometheus ✓")

    def test_vllm_latency_metric_has_model_name_label(self):
        """
        Verify vllm:e2e_request_latency_seconds has 'model_name' label.
        Reference: observability.md Latency Metrics table.
        
        NOTE: This test only applies to real vLLM models.
        """
        metric_name = "vllm:e2e_request_latency_seconds"
        result = self._query_prometheus(f'{metric_name}{{}}')
        
        if result is None or result.get("status") != "success":
            pytest.skip("Could not query Prometheus for vLLM metrics")
        
        data = result.get("data", {})
        results = data.get("result", [])
        
        if len(results) == 0:
            pytest.skip(
                f"Metric '{metric_name}' not found.\n"
                f"  Expected if only simulator models are deployed (simulators don't expose vLLM metrics)."
            )
        
        # Check for model_name label
        for r in results:
            metric = r.get("metric", {})
            if "model_name" in metric:
                print(f"[e2e] Metric '{metric_name}' has 'model_name' label ✓")
                return
        
        pytest.fail(
            f"FAIL: Metric '{metric_name}' does not have 'model_name' label.\n"
            f"  Reference: observability.md Latency Metrics table\n"
            f"  This label should be present on vLLM metrics."
        )
