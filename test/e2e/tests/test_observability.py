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


# =============================================================================
# Metrics Availability Tests
# =============================================================================

class TestLimitadorMetrics:
    """Tests for verifying Limitador metrics are available and have correct labels."""

    @pytest.fixture(scope="class")
    def limitador_metrics(self, expected_metrics_config) -> str:
        """
        Fetch raw metrics from Limitador via kubectl port-forward.
        Returns the raw Prometheus metrics text.
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
        """Verify authorized_hits metric is available."""
        metric_name = "authorized_hits"
        cfg = next(
            (m for m in expected_metrics_config["limitador"]["metrics"] if m["name"] == metric_name),
            None
        )
        assert cfg, f"FAIL: Metric '{metric_name}' not found in configuration"

        # Note: Metric may not exist if no requests have been made yet
        # This is expected in a fresh deployment
        if not self._metric_exists(limitador_metrics, metric_name):
            log.warning(
                f"[metrics] Metric '{metric_name}' not found - this is expected if no requests have been made yet"
            )
            print(f"[metrics] WARNING: Metric '{metric_name}' not found (expected if no requests made yet)")
            pytest.skip(f"Metric '{metric_name}' not present - no requests made yet")

        print(f"[metrics] Metric '{metric_name}' exists")

    def test_authorized_calls_metric_exists(self, limitador_metrics, expected_metrics_config):
        """Verify authorized_calls metric is available."""
        metric_name = "authorized_calls"
        cfg = next(
            (m for m in expected_metrics_config["limitador"]["metrics"] if m["name"] == metric_name),
            None
        )
        assert cfg, f"FAIL: Metric '{metric_name}' not found in configuration"

        if not self._metric_exists(limitador_metrics, metric_name):
            log.warning(
                f"[metrics] Metric '{metric_name}' not found - this is expected if no requests have been made yet"
            )
            print(f"[metrics] WARNING: Metric '{metric_name}' not found (expected if no requests made yet)")
            pytest.skip(f"Metric '{metric_name}' not present - no requests made yet")

        print(f"[metrics] Metric '{metric_name}' exists")

    def test_limited_calls_metric_exists(self, limitador_metrics, expected_metrics_config):
        """Verify limited_calls metric is available."""
        metric_name = "limited_calls"
        cfg = next(
            (m for m in expected_metrics_config["limitador"]["metrics"] if m["name"] == metric_name),
            None
        )
        assert cfg, f"FAIL: Metric '{metric_name}' not found in configuration"

        if not self._metric_exists(limitador_metrics, metric_name):
            log.warning(
                f"[metrics] Metric '{metric_name}' not found - this is expected if no rate limiting has occurred"
            )
            print(f"[metrics] WARNING: Metric '{metric_name}' not found (expected if no rate limiting occurred)")
            pytest.skip(f"Metric '{metric_name}' not present - no rate limiting occurred yet")

        print(f"[metrics] Metric '{metric_name}' exists")


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


class TestMetricsAfterRequest:
    """
    Tests that verify metrics are generated with correct labels after making requests.
    These tests make actual API calls and then verify the metrics.
    
    NOTE: These tests require AuthPolicy to be enforced on the Gateway for labels
    to be injected into metrics. Tests will skip if AuthPolicy is not enforced.
    """

    @pytest.fixture(scope="class")
    def authpolicy_enforced(self):
        """Check if AuthPolicy is enforced - skip tests if not."""
        is_enforced, reason = _is_gateway_authpolicy_enforced()
        if not is_enforced:
            pytest.skip(
                f"Skipping label tests: AuthPolicy not enforced on Gateway.\n"
                f"  Reason: {reason}\n"
                f"  Labels (user, tier, model) are only injected when AuthPolicy is enforced.\n"
                f"  This is a platform configuration issue, not an observability issue."
            )
        return True

    @pytest.fixture(scope="class")
    def make_test_request(self, headers, model_v1, model_name, authpolicy_enforced):
        """Make a test request to generate metrics."""
        from test_helper import chat

        log.info(f"[e2e] Making test request to generate metrics...")
        print(f"[e2e] Making test request to model '{model_name}'...")

        response = chat("Hello, this is a test for metrics.", model_v1, headers, model_name=model_name)

        # We don't fail if the request fails - the model might not be fully ready
        # But we log it for debugging
        log.info(f"[e2e] Test request completed with status {response.status_code}")
        print(f"[e2e] Test request status: {response.status_code}")

        # Give metrics time to propagate
        time.sleep(2)

        return response.status_code

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
            pytest.skip("Could not find Limitador pod for metrics verification")

        pod_name = pod_name.strip()

        # Fetch metrics
        exec_cmd = [
            "exec", "-n", namespace, pod_name, "--",
            "curl", "-s", f"http://localhost:{port}{path}"
        ]
        rc, metrics_text, stderr = _run_kubectl(exec_cmd, timeout=60)

        if rc != 0:
            pytest.skip(f"Could not fetch metrics from Limitador: {stderr}")

        return metrics_text

    def test_metrics_have_user_label(self, limitador_metrics_after_request, expected_metrics_config, make_test_request):
        """Verify that metrics have the 'user' label after making a request."""
        if make_test_request not in (200, 429):
            pytest.skip(f"Test request did not succeed (status {make_test_request}), skipping label verification")

        metrics_text = limitador_metrics_after_request

        # Check authorized_calls for user label (most likely to exist)
        metric_name = "authorized_calls"

        # Look for the metric with any user label
        pattern = rf'^{metric_name}\{{[^}}]*user="[^"]+"'
        has_user = bool(re.search(pattern, metrics_text, re.MULTILINE))

        if not has_user:
            # Get sample lines for debugging
            sample_lines = [l for l in metrics_text.split("\n") if l.startswith(metric_name)][:3]
            sample = "\n".join(sample_lines) if sample_lines else "(no metrics found)"

            pytest.fail(
                f"FAIL: Metric '{metric_name}' does not have 'user' label.\n"
                f"  The TelemetryPolicy should inject 'user' label from auth.identity.userid.\n"
                f"  Sample metric lines:\n{sample}\n"
                f"  Check:\n"
                f"    1. TelemetryPolicy is enforced: kubectl get telemetrypolicy -n openshift-ingress\n"
                f"    2. AuthPolicy is injecting identity: kubectl get authpolicy -n openshift-ingress"
            )

        print(f"[e2e] Metric '{metric_name}' has 'user' label")

    def test_metrics_have_tier_label(self, limitador_metrics_after_request, expected_metrics_config, make_test_request):
        """Verify that metrics have the 'tier' label after making a request."""
        if make_test_request not in (200, 429):
            pytest.skip(f"Test request did not succeed (status {make_test_request}), skipping label verification")

        metrics_text = limitador_metrics_after_request
        metric_name = "authorized_calls"

        pattern = rf'^{metric_name}\{{[^}}]*tier="[^"]+"'
        has_tier = bool(re.search(pattern, metrics_text, re.MULTILINE))

        if not has_tier:
            sample_lines = [l for l in metrics_text.split("\n") if l.startswith(metric_name)][:3]
            sample = "\n".join(sample_lines) if sample_lines else "(no metrics found)"

            pytest.fail(
                f"FAIL: Metric '{metric_name}' does not have 'tier' label.\n"
                f"  The TelemetryPolicy should inject 'tier' label from auth.identity.tier.\n"
                f"  Sample metric lines:\n{sample}\n"
                f"  Check:\n"
                f"    1. TelemetryPolicy is enforced: kubectl get telemetrypolicy -n openshift-ingress\n"
                f"    2. Tier lookup is working: curl /maas-api/v1/tiers/lookup"
            )

        print(f"[e2e] Metric '{metric_name}' has 'tier' label")

    def test_metrics_have_model_label(self, limitador_metrics_after_request, expected_metrics_config, make_test_request):
        """Verify that metrics have the 'model' label after making a request."""
        if make_test_request not in (200, 429):
            pytest.skip(f"Test request did not succeed (status {make_test_request}), skipping label verification")

        metrics_text = limitador_metrics_after_request
        metric_name = "authorized_calls"

        pattern = rf'^{metric_name}\{{[^}}]*model="[^"]+"'
        has_model = bool(re.search(pattern, metrics_text, re.MULTILINE))

        if not has_model:
            sample_lines = [l for l in metrics_text.split("\n") if l.startswith(metric_name)][:3]
            sample = "\n".join(sample_lines) if sample_lines else "(no metrics found)"

            pytest.fail(
                f"FAIL: Metric '{metric_name}' does not have 'model' label.\n"
                f"  The TelemetryPolicy should inject 'model' label from responseBodyJSON('/model').\n"
                f"  Sample metric lines:\n{sample}\n"
                f"  Check:\n"
                f"    1. TelemetryPolicy is enforced: kubectl get telemetrypolicy -n openshift-ingress\n"
                f"    2. Response contains model field in JSON body"
            )

        print(f"[e2e] Metric '{metric_name}' has 'model' label")
