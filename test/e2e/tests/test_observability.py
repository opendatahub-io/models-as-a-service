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
import re
import subprocess
import time
from pathlib import Path
from urllib.parse import quote

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


def _query_prometheus(query: str, namespace: str = "openshift-user-workload-monitoring",
                      pod: str = "prometheus-user-workload-0",
                      container: str = "prometheus") -> dict | None:
    """
    Query Prometheus via kubectl exec.
    Returns the JSON response or None if query failed.
    
    Args:
        query: PromQL query string
        namespace: Prometheus pod namespace
        pod: Prometheus pod name
        container: Container name within the pod
    """
    # URL-encode the query to avoid shell interpretation of special chars like {}
    encoded_query = quote(query, safe="")
    exec_cmd = [
        "exec", "-n", namespace,
        pod, "-c", container, "--",
        "curl", "-s", f"http://localhost:9090/api/v1/query?query={encoded_query}"
    ]
    rc, stdout, stderr = _run_kubectl(exec_cmd, timeout=30)

    if rc != 0:
        log.warning(f"[prometheus] Query failed (ns={namespace}): {stderr}")
        return None

    try:
        return json.loads(stdout)
    except Exception as e:
        log.warning(f"[prometheus] Failed to parse response: {e}")
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
        container="prometheus",
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

        if not self._metric_exists(metrics_text, metric_name):
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
                f"FAIL: Cannot query platform Prometheus for Istio metrics.\n"
                f"  Check:\n"
                f"    1. Prometheus pods are running: kubectl get pods -n openshift-monitoring\n"
                f"    2. Current user has cluster-admin privileges"
            )
        
        if not has_label:
            pytest.fail(
                f"FAIL: Metric '{metric_name}' does not have 'tier' label in Prometheus.\n"
                f"  Reference: observability.md Latency Metrics table\n"
                f"  Result: {message}\n"
                f"  The Istio Telemetry resource (latency-per-tier) should inject 'tier'\n"
                f"  from the X-MaaS-Tier header set by AuthPolicy.\n"
                f"  Check:\n"
                f"    1. Telemetry resource exists: kubectl get telemetry latency-per-tier -n openshift-ingress\n"
                f"    2. AuthPolicy injects X-MaaS-Tier header\n"
                f"    3. Gateway metrics are being scraped by platform Prometheus"
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
                f"FAIL: Cannot query platform Prometheus for Istio metrics.\n"
                f"  Check:\n"
                f"    1. Prometheus pods are running: kubectl get pods -n openshift-monitoring\n"
                f"    2. Current user has cluster-admin privileges"
            )
        
        if not has_label:
            pytest.fail(
                f"FAIL: Metric '{metric_name}' does not have 'destination_service_name' label.\n"
                f"  Reference: observability.md Latency Metrics table\n"
                f"  Result: {message}\n"
                f"  This is a standard Istio label that should be present.\n"
                f"  Check:\n"
                f"    1. Istio gateway metrics are being scraped by platform Prometheus\n"
                f"    2. ServiceMonitor for Istio gateway exists in openshift-ingress"
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
                f"FAIL: Cannot query platform Prometheus for Istio metrics.\n"
                f"  Check:\n"
                f"    1. Prometheus pods are running: kubectl get pods -n openshift-monitoring\n"
                f"    2. Current user has cluster-admin privileges"
            )

        if not has_label:
            pytest.fail(
                f"FAIL: Metric '{metric_name}' does not have 'response_code' label.\n"
                f"  Reference: observability.md Gateway Traffic Metrics\n"
                f"  Result: {message}\n"
                f"  This label is required for error rate panels (4xx, 5xx, 401).\n"
                f"  Check:\n"
                f"    1. Istio gateway metrics are being scraped by platform Prometheus\n"
                f"    2. ServiceMonitor for Istio gateway exists in openshift-ingress"
            )

        print(f"[e2e] Metric '{metric_name}' has 'response_code' label ✓")


# =============================================================================
# vLLM Metrics Tests
# =============================================================================

class TestVLLMMetrics:
    """
    Tests for verifying vLLM model metrics have correct labels.
    Reference: observability.md Model Latency Metrics table.
    
    vLLM metrics are scraped by user-workload Prometheus.
    Both real vLLM models and the simulator (v0.7.1+) expose these metrics.
    
    NOTE: Histogram metrics must be queried with a suffix (_count, _bucket, _sum)
    because Prometheus does not return results for the base histogram name.
    """

    def test_vllm_latency_metric_exists(self):
        """
        Verify vllm:e2e_request_latency_seconds metric is being scraped.
        Reference: observability.md Model Latency Metrics table.
        
        Queries _count suffix since Prometheus histograms require a suffix.
        """
        metric_name = "vllm:e2e_request_latency_seconds_count"
        result = _query_prometheus(metric_name)
        
        if result is None:
            pytest.fail("FAIL: Could not query Prometheus for vLLM metrics.")
        
        if result.get("status") != "success":
            pytest.fail(f"FAIL: Prometheus query failed: {result.get('error', 'unknown')}")
        
        data = result.get("data", {})
        results = data.get("result", [])
        
        if len(results) == 0:
            pytest.fail(
                f"FAIL: Metric '{metric_name}' not found in Prometheus.\n"
                f"  Both real vLLM and simulator v0.7.1+ expose this metric.\n"
                f"  Check:\n"
                f"    1. Model pods are running: kubectl get pods -n llm\n"
                f"    2. ServiceMonitor exists: kubectl get servicemonitor -n llm\n"
                f"    3. Traffic has been sent to generate metrics (lazily registered)"
            )
        
        print(f"[e2e] Metric '{metric_name}' exists in Prometheus ✓")

    def test_vllm_latency_metric_has_model_name_label(self):
        """
        Verify vllm:e2e_request_latency_seconds has 'model_name' label.
        Reference: observability.md Model Latency Metrics table.
        """
        metric_name = "vllm:e2e_request_latency_seconds_count"
        result = _query_prometheus(metric_name)
        
        if result is None or result.get("status") != "success":
            pytest.fail("FAIL: Could not query Prometheus for vLLM metrics.")
        
        data = result.get("data", {})
        results = data.get("result", [])
        
        if len(results) == 0:
            pytest.fail(
                f"FAIL: Metric '{metric_name}' not found in Prometheus.\n"
                f"  Cannot verify 'model_name' label without metric data."
            )
        
        # Check for model_name label
        for r in results:
            metric = r.get("metric", {})
            if "model_name" in metric:
                print(f"[e2e] Metric '{metric_name}' has 'model_name' label: '{metric['model_name']}' ✓")
                return
        
        pytest.fail(
            f"FAIL: Metric '{metric_name}' does not have 'model_name' label.\n"
            f"  Reference: observability.md Model Latency Metrics table\n"
            f"  This label should be present on vLLM metrics."
        )
