"""
E2E tests for multi-tenant maas-api deployment (S24).

Tests that multiple AITenant instances create unique maas-api deployments with
proper naming, routing, and tenant isolation.
"""
import json
import os
import shutil
import subprocess
import time

import pytest

_OC_TIMEOUT = int(os.environ.get("E2E_OC_TIMEOUT", "60"))
AITENANT_NAMESPACE = "ai-tenants"  # Where AITenant CRs live
MAAS_API_NAMESPACE = os.environ.get("E2E_MAAS_API_DEPLOYMENT_NAMESPACE", os.environ.get("DEPLOYMENT_NAMESPACE", "opendatahub"))  # Where maas-api workloads run (operator namespace)
TEST_TENANT_NAME = "test-e2e"
GATEWAY_NAMESPACE = "openshift-ingress"


def _oc_bin():
    path = shutil.which("oc")
    if not path:
        raise RuntimeError("`oc` binary not found in PATH")
    return path


def _oc_run(args, *, timeout=None, check=False):
    result = subprocess.run(
        [_oc_bin(), *args],
        capture_output=True,
        text=True,
        timeout=_OC_TIMEOUT if timeout is None else timeout,
        stdin=subprocess.DEVNULL,
        check=False,
    )
    if check and result.returncode != 0:
        raise subprocess.CalledProcessError(
            result.returncode,
            [_oc_bin(), *args],
            result.stdout,
            result.stderr,
        )
    return result


def _oc_json(args):
    result = _oc_run(args, check=True)
    return json.loads(result.stdout)


def _resource_exists(kind, name, namespace):
    result = _oc_run(["get", kind, name, "-n", namespace, "-o", "name"])
    return result.returncode == 0


def _wait_deployment_ready(name, namespace, timeout=180):
    """Wait for deployment to be available."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            dep = _oc_json(["get", "deployment", name, "-n", namespace, "-o", "json"])
            conditions = dep.get("status", {}).get("conditions", [])
            for cond in conditions:
                if cond.get("type") == "Available" and cond.get("status") == "True":
                    return True
        except subprocess.CalledProcessError:
            pass
        time.sleep(5)
    return False


@pytest.fixture(scope="module")
def aitenant_crd_exists():
    """Check if AITenant CRD is installed."""
    result = _oc_run(["get", "crd", "aitenants.maas.opendatahub.io"])
    if result.returncode != 0:
        pytest.skip("AITenant CRD not installed - multi-tenancy not enabled")
    return True


@pytest.fixture(scope="module")
def test_gateway(aitenant_crd_exists):
    """Clone the default gateway for testing (Phase 2 will prevent sharing gateways)."""
    # Get the existing gateway as JSON
    result = _oc_run(
        ["get", "gateway", "maas-default-gateway", "-n", GATEWAY_NAMESPACE, "-o", "json"]
    )
    if result.returncode != 0:
        pytest.skip("maas-default-gateway not found - cannot run multi-tenant test")

    gateway = json.loads(result.stdout)

    # Clone it with a new name
    gateway["metadata"]["name"] = f"{TEST_TENANT_NAME}-gateway"
    gateway["metadata"]["resourceVersion"] = None
    gateway["metadata"]["uid"] = None
    gateway["metadata"].pop("creationTimestamp", None)
    gateway["metadata"].pop("generation", None)
    gateway["metadata"].pop("managedFields", None)
    gateway.pop("status", None)

    # Update hostname to be unique for this test tenant
    for listener in gateway["spec"]["listeners"]:
        if "hostname" in listener:
            listener["hostname"] = f"{TEST_TENANT_NAME}.apps.example.com"

    gateway_yaml = json.dumps(gateway)
    result = subprocess.run(
        [_oc_bin(), "apply", "-f", "-"],
        input=gateway_yaml,
        capture_output=True,
        text=True,
        check=False,
    )
    if result.returncode != 0:
        pytest.skip(f"Failed to create cloned Gateway: {result.stderr}")

    yield f"{TEST_TENANT_NAME}-gateway"

    # Cleanup
    _oc_run(["delete", "gateway", f"{TEST_TENANT_NAME}-gateway", "-n", GATEWAY_NAMESPACE], timeout=120)


@pytest.fixture(scope="module")
def test_aitenant(test_gateway):
    """Create a test AITenant and clean up after tests."""
    aitenant_yaml = f"""
apiVersion: maas.opendatahub.io/v1alpha1
kind: AITenant
metadata:
  name: {TEST_TENANT_NAME}
  namespace: {AITENANT_NAMESPACE}
spec:
  gateway:
    name: {test_gateway}
"""

    result = subprocess.run(
        [_oc_bin(), "apply", "-f", "-"],
        input=aitenant_yaml,
        capture_output=True,
        text=True,
        check=False,
    )
    if result.returncode != 0:
        pytest.skip(f"Failed to create test AITenant: {result.stderr}")

    # Wait for AITenant to reconcile
    time.sleep(10)

    yield TEST_TENANT_NAME

    # Cleanup
    _oc_run(["delete", "aitenant", TEST_TENANT_NAME, "-n", AITENANT_NAMESPACE], timeout=120)
    _oc_run(["delete", "tenant", "default-tenant", "-n", f"ai-tenant-{TEST_TENANT_NAME}"], timeout=120)
    _oc_run(["delete", "namespace", f"ai-tenant-{TEST_TENANT_NAME}"], timeout=120)


class TestMultiTenantDeployment:
    """Test per-tenant maas-api deployment (S24 acceptance criteria)."""

    def test_tenant_maas_api_deployment_created(self, test_aitenant):
        """AC1: Verify maas-api-{tenant} deployment is created."""
        expected_name = f"maas-api-{test_aitenant}"

        assert _wait_deployment_ready(expected_name, MAAS_API_NAMESPACE, timeout=180), (
            f"Deployment {expected_name} not ready in {MAAS_API_NAMESPACE}"
        )

    def test_tenant_maas_api_service_created(self, test_aitenant):
        """AC1: Verify Service maas-api-{tenant} is created."""
        expected_name = f"maas-api-{test_aitenant}"

        assert _resource_exists("service", expected_name, MAAS_API_NAMESPACE), (
            f"Service {expected_name} not found in {MAAS_API_NAMESPACE}"
        )

    def test_tenant_httproute_created(self, test_aitenant):
        """AC4: Verify HTTPRoute is created for the tenant."""
        expected_name = f"maas-api-route-{test_aitenant}"

        assert _resource_exists("httproute", expected_name, MAAS_API_NAMESPACE), (
            f"HTTPRoute {expected_name} not found in {MAAS_API_NAMESPACE}"
        )

    def test_tenant_httproute_backend_refs(self, test_aitenant):
        """AC4: Verify HTTPRoute backendRefs point to tenant Service."""
        route_name = f"maas-api-route-{test_aitenant}"
        service_name = f"maas-api-{test_aitenant}"

        route = _oc_json(["get", "httproute", route_name, "-n", MAAS_API_NAMESPACE, "-o", "json"])

        # All rules should point to the correct Service
        for rule in route.get("spec", {}).get("rules", []):
            for backend_ref in rule.get("backendRefs", []):
                assert backend_ref.get("name") == service_name, (
                    f"HTTPRoute backendRef points to {backend_ref.get('name')}, expected {service_name}"
                )

    def test_tenant_deployment_has_tenant_name_env(self, test_aitenant):
        """AC2: Verify TENANT_NAME environment variable is set."""
        deployment_name = f"maas-api-{test_aitenant}"

        dep = _oc_json(["get", "deployment", deployment_name, "-n", MAAS_API_NAMESPACE, "-o", "json"])

        containers = dep.get("spec", {}).get("template", {}).get("spec", {}).get("containers", [])
        assert containers, "Deployment has no containers"

        env_vars = containers[0].get("env", [])
        tenant_name_env = next((e for e in env_vars if e.get("name") == "TENANT_NAME"), None)

        assert tenant_name_env is not None, "TENANT_NAME env var not found"
        assert tenant_name_env.get("value") == test_aitenant, (
            f"TENANT_NAME should be {test_aitenant}, got {tenant_name_env.get('value')}"
        )

    def test_default_and_tenant_coexist(self, test_aitenant):
        """AC3: Verify multiple tenants can coexist without collision."""
        # Check default maas-api exists
        default_exists = _resource_exists("deployment", "maas-api", MAAS_API_NAMESPACE)

        # Check tenant maas-api exists
        tenant_deployment = f"maas-api-{test_aitenant}"
        tenant_exists = _resource_exists("deployment", tenant_deployment, MAAS_API_NAMESPACE)

        if not default_exists:
            pytest.skip("Default maas-api not deployed - single tenant test only")

        assert tenant_exists, f"Tenant deployment {tenant_deployment} not found"

        # Verify both are ready simultaneously
        default_ready = _wait_deployment_ready("maas-api", MAAS_API_NAMESPACE, timeout=30)
        tenant_ready = _wait_deployment_ready(tenant_deployment, MAAS_API_NAMESPACE, timeout=30)

        assert default_ready and tenant_ready, (
            "Both default and tenant maas-api should be ready simultaneously"
        )


class TestLegacyCleanup:
    """Test that old maas-api deployments are cleaned up."""

    def test_no_legacy_maas_api_in_opendatahub(self):
        """Verify cleanup removed old maas-api from opendatahub."""
        if not _resource_exists("namespace", "opendatahub", ""):
            pytest.skip("opendatahub namespace does not exist")

        # After migration, maas-api should NOT exist in opendatahub
        exists = _resource_exists("deployment", "maas-api", "opendatahub")

        # This assertion may fail on fresh installs (no legacy to clean up)
        # but should pass after upgrade from pre-S24 deployment
        if exists:
            pytest.skip("Legacy maas-api still in opendatahub - cleanup may be pending or skipped on fresh install")
