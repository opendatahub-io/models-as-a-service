"""
Tenant managed-resource audit and condition-value e2e tests.

Validates that all expected platform resources were applied to the cluster
by querying the maas.opendatahub.io/tenant-name tracking label, and that
Tenant status conditions report healthy values.

Catches RBAC gaps and manifest-apply failures at PR time (RHOAIENG-62458).

Tenant existence, readiness, and phase checks are in test_tenant.py.
"""

import json
import logging
import os
import subprocess
import time

import pytest

log = logging.getLogger(__name__)

TENANT_NAME = "default-tenant"
DEPLOYMENT_NAMESPACE = os.environ.get("DEPLOYMENT_NAMESPACE", "opendatahub")
TENANT_NAMESPACE = os.environ.get("MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service")
TENANT_HEALTH_TIMEOUT = int(os.environ.get("E2E_TENANT_HEALTH_TIMEOUT", "120"))
TENANT_LABEL_SELECTOR = f"maas.opendatahub.io/tenant-name={TENANT_NAME}"


def _is_transient_error(stderr):
    patterns = [
        "tls handshake timeout",
        "connection refused",
        "connection reset",
        "i/o timeout",
        "dial tcp",
        "eof",
        "temporary failure",
        "network is unreachable",
    ]
    stderr_lower = stderr.lower()
    return any(p in stderr_lower for p in patterns)


def _run_oc(args, retries=3):
    """Run an oc command with transient-error retry. Returns (stdout, stderr, returncode)."""
    for attempt in range(retries):
        try:
            result = subprocess.run(
                ["oc", *args], capture_output=True, text=True, timeout=30,
            )
        except subprocess.TimeoutExpired as exc:
            if attempt < retries - 1:
                log.warning("oc timed out (attempt %d/%d): %s", attempt + 1, retries, exc)
                time.sleep(2 * (attempt + 1))
                continue
            return "", f"oc command timed out: {exc}", 124
        if result.returncode == 0:
            return result.stdout, result.stderr, 0
        if attempt < retries - 1 and _is_transient_error(result.stderr):
            log.warning("Transient oc error (attempt %d/%d): %s", attempt + 1, retries, result.stderr.strip())
            time.sleep(2 * (attempt + 1))
            continue
        return result.stdout, result.stderr, result.returncode
    return "", "max retries exceeded", 1


def _get_tenant():
    """Fetch the default-tenant Tenant CR. Returns dict or None if not found."""
    stdout, stderr, rc = _run_oc([
        "get", "tenant", TENANT_NAME, "-n", TENANT_NAMESPACE, "-o", "json",
    ])
    if rc == 0:
        return json.loads(stdout)
    if "not found" in stderr.lower():
        return None
    raise RuntimeError(f"Failed to get tenant/{TENANT_NAME}: {stderr.strip()}")


def _get_condition(tenant, condition_type):
    """Extract a condition by type from a Tenant CR. Returns the condition dict or None."""
    for c in tenant.get("status", {}).get("conditions", []):
        if c.get("type") == condition_type:
            return c
    return None


def _wait_for_tenant_ready(timeout=None):
    """Poll until Tenant has Ready=True. Fails fast on Phase=Failed."""
    timeout = timeout or TENANT_HEALTH_TIMEOUT
    deadline = time.time() + timeout
    log.info("Waiting for Tenant %s Ready=True (timeout: %ds)...", TENANT_NAME, timeout)

    while time.time() < deadline:
        tenant = _get_tenant()
        if tenant is None:
            time.sleep(3)
            continue

        phase = tenant.get("status", {}).get("phase", "")
        if phase == "Failed":
            ready = _get_condition(tenant, "Ready")
            reason = ready.get("reason", "unknown") if ready else "unknown"
            message = ready.get("message", "no message") if ready else "no message"
            pytest.fail(f"Tenant reconcile failed: {reason} — {message}")

        ready = _get_condition(tenant, "Ready")
        if ready and ready.get("status") == "True":
            return tenant

        time.sleep(3)

    tenant = _get_tenant()
    status = tenant.get("status", {}) if tenant else {}
    ready = _get_condition(tenant, "Ready") if tenant else None
    detail = f"reason={ready.get('reason')}, message={ready.get('message')}" if ready else "no Ready condition"
    pytest.fail(
        f"Tenant {TENANT_NAME} did not become Ready within {timeout}s "
        f"(current: phase={status.get('phase', 'none')}, {detail})"
    )


def _list_resources_by_label(kind, namespace=None):
    """List resources matching the tenant tracking label. Returns list of items."""
    args = ["get", kind, "-l", TENANT_LABEL_SELECTOR, "-o", "json"]
    if namespace:
        args += ["-n", namespace]
    stdout, stderr, rc = _run_oc(args)
    if rc == 0:
        return json.loads(stdout).get("items", [])
    if "the server doesn't have a resource type" in stderr.lower():
        return None
    if "not found" in stderr.lower() or "no resources found" in stderr.lower():
        return []
    raise RuntimeError(f"Failed to list {kind} with label selector: {stderr.strip()}")


def _crd_exists(resource, api_group):
    """Check if a CRD is registered on the cluster."""
    stdout, _stderr, rc = _run_oc(["api-resources", "--api-group", api_group, "-o", "name"])
    if rc != 0:
        return False
    names = {line.strip().lower() for line in stdout.splitlines() if line.strip()}
    return resource.lower() in names


class TestTenantConditionValues:
    """Assert specific condition values beyond the shape checks in test_tenant.py."""

    def test_tenant_conditions_healthy(self):
        """Core Tenant status conditions must report healthy values.

        Checks DependenciesAvailable, MaaSPrerequisitesAvailable, and
        DeploymentsAvailable are True. Degraded is not checked because
        it may be True due to non-critical prerequisite warnings.
        """
        tenant = _wait_for_tenant_ready()

        required_true = [
            "DependenciesAvailable",
            "MaaSPrerequisitesAvailable",
            "DeploymentsAvailable",
        ]
        for cond_type in required_true:
            cond = _get_condition(tenant, cond_type)
            assert cond is not None, (
                f"Tenant is missing expected condition '{cond_type}'"
            )
            assert cond["status"] == "True", (
                f"Tenant condition {cond_type}: expected status=True, "
                f"got status={cond['status']} (reason={cond.get('reason')}, "
                f"message={cond.get('message')})"
            )


class TestTenantManagedResources:
    """Verify that expected platform resources were applied by the Tenant reconciler.

    Queries resources by the maas.opendatahub.io/tenant-name label that the
    reconciler sets on every resource it applies via Server-Side Apply.
    """

    @pytest.mark.parametrize("kind,min_count", [
        ("deployment", 1),
        ("service", 1),
        ("serviceaccount", 1),
        ("cronjob", 1),
        ("configmap", 1),
        ("networkpolicy", 1),
    ])
    def test_core_resources_exist(self, kind, min_count):
        """Core Kubernetes resources must exist with tenant tracking labels."""
        items = _list_resources_by_label(kind, namespace=DEPLOYMENT_NAMESPACE)
        assert items is not None, f"Resource type '{kind}' not available on cluster"
        names = [item["metadata"]["name"] for item in items]
        assert len(items) >= min_count, (
            f"Expected >= {min_count} {kind}(s) with label {TENANT_LABEL_SELECTOR} "
            f"in namespace {DEPLOYMENT_NAMESPACE}, found {len(items)}: {names}"
        )

    def test_httproute_exists(self):
        """HTTPRoute must exist in the deployment namespace."""
        items = _list_resources_by_label("httproute.gateway.networking.k8s.io", namespace=DEPLOYMENT_NAMESPACE)
        assert items is not None, "HTTPRoute CRD (gateway.networking.k8s.io) not available on cluster"
        names = [item["metadata"]["name"] for item in items]
        assert len(items) >= 1, (
            f"Expected >= 1 HTTPRoute with label {TENANT_LABEL_SELECTOR} "
            f"in namespace {DEPLOYMENT_NAMESPACE}, found {len(items)}: {names}"
        )

    def test_authpolicy_exists(self):
        """AuthPolicy must exist (may be in gateway namespace, not deployment namespace)."""
        items = _list_resources_by_label("authpolicy.kuadrant.io", namespace=DEPLOYMENT_NAMESPACE)
        if items:
            return
        items = _list_resources_by_label("authpolicy.kuadrant.io")
        assert items is not None, "AuthPolicy CRD (kuadrant.io) not available on cluster"
        names = [item["metadata"]["name"] for item in items]
        assert len(items) >= 1, (
            f"Expected >= 1 AuthPolicy with label {TENANT_LABEL_SELECTOR}, "
            f"found {len(items)}: {names}"
        )

    @pytest.mark.parametrize("kind,min_count", [
        ("clusterrole", 1),
        ("clusterrolebinding", 1),
    ])
    def test_rbac_resources_exist(self, kind, min_count):
        """Cluster-scoped RBAC resources must exist (no namespace filter)."""
        items = _list_resources_by_label(kind)
        assert items is not None, f"Resource type '{kind}' not available on cluster"
        names = [item["metadata"]["name"] for item in items]
        assert len(items) >= min_count, (
            f"Expected >= {min_count} {kind}(s) with label {TENANT_LABEL_SELECTOR}, "
            f"found {len(items)}: {names}"
        )

    def test_monitoring_resources_exist(self):
        """PodMonitor must exist — the exact resource type that caused the RBAC incident."""
        items = _list_resources_by_label("podmonitor.monitoring.coreos.com", namespace=DEPLOYMENT_NAMESPACE)
        assert items is not None, (
            "PodMonitor CRD (monitoring.coreos.com) not available on cluster"
        )
        names = [item["metadata"]["name"] for item in items]
        assert len(items) >= 1, (
            f"Expected >= 1 PodMonitor with label {TENANT_LABEL_SELECTOR} "
            f"in namespace {DEPLOYMENT_NAMESPACE}, found {len(items)}: {names}"
        )

    @pytest.mark.parametrize("kind,api_group,resource", [
        ("persesdashboard.perses.dev", "perses.dev", "persesdashboards"),
        ("persesdatasource.perses.dev", "perses.dev", "persesdatasources"),
    ])
    def test_optional_resources_exist(self, kind, api_group, resource):
        """Optional resources — skipped if their CRD is not registered."""
        if not _crd_exists(resource, api_group):
            pytest.skip(f"CRD {resource}.{api_group} not registered on cluster")
        items = _list_resources_by_label(kind, namespace=DEPLOYMENT_NAMESPACE)
        names = [item["metadata"]["name"] for item in items] if items else []
        assert items and len(items) >= 1, (
            f"CRD {resource}.{api_group} is registered but no {kind} found "
            f"with label {TENANT_LABEL_SELECTOR} in namespace {DEPLOYMENT_NAMESPACE}: {names}"
        )
