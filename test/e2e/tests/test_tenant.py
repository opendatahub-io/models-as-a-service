from __future__ import annotations

import copy
import json
import os
import shutil
import subprocess
import time

import pytest

from test_helper import MAAS_API_DEPLOYMENT_NAMESPACE, _apply_cr, _ns

_OC_TIMEOUT = int(os.environ.get("E2E_OC_TIMEOUT", "60"))


def _oc_bin():
    path = shutil.which("oc")
    if not path:
        raise RuntimeError("`oc` binary not found in PATH")
    return path


def _oc_run(args, *, timeout=None):
    return subprocess.run(
        [_oc_bin(), *args],
        capture_output=True,
        text=True,
        timeout=_OC_TIMEOUT if timeout is None else timeout,
        stdin=subprocess.DEVNULL,
        check=False,
    )


def _oc_not_found(exc):
    combined = (exc.stderr or "") + (exc.stdout or "")
    return "(NotFound)" in combined


def _oc_output_not_found(result):
    combined = (result.stderr or "") + (result.stdout or "")
    return "(NotFound)" in combined or "not found" in combined.lower()


def _oc_json(args):
    result = _oc_run(args)
    if result.returncode != 0:
        raise subprocess.CalledProcessError(
            result.returncode,
            [_oc_bin(), *args],
            result.stdout,
            result.stderr,
        )
    return json.loads(result.stdout)


pytestmark = pytest.mark.xdist_group("readonly")

TENANT_NAME = "default-tenant"
GATEWAY_NAMESPACE = os.environ.get("GATEWAY_NAMESPACE", "openshift-ingress")
TENANT_CRD = "maastenantconfigs.maas.opendatahub.io"

_KIND_PLURAL = {
    "maasmodelref": "maasmodelrefs",
    "maasauthpolicy": "maasauthpolicies",
    "maassubscription": "maassubscriptions",
}


def _tenant_doc():
    return _oc_json(["get", "maastenantconfig", TENANT_NAME, "-n", _ns(), "-o", "json"])


def _tenant_status():
    try:
        doc = _tenant_doc()
        return doc.get("status") or {}
    except subprocess.CalledProcessError as exc:
        if _oc_not_found(exc):
            return None
        raise


@pytest.fixture(scope="module", autouse=True)
def require_tenant_crd():
    r = _oc_run(["get", "crd", TENANT_CRD])
    if r.returncode != 0:
        if _oc_output_not_found(r):
            pytest.skip(
                f"Missing CRD {TENANT_CRD} (transitional skip: install maas-controller manifests "
                f"so CRDs exist; then controller creates {TENANT_NAME})."
            )
        combined = (r.stderr or "") + (r.stdout or "")
        pytest.fail(f"`oc get crd {TENANT_CRD}` failed: {combined.strip()}")


@pytest.fixture(scope="module", autouse=True)
def require_tenant_singleton():
    if _tenant_status() is None:
        pytest.skip(
            f"MaasTenantConfig {TENANT_NAME}/{_ns()} not found (transitional skip: "
            "maas-controller should create this on startup once CRDs and controller are installed)."
        )


def _wait_tenant_ready(timeout=180, interval=5):
    deadline = time.time() + timeout
    while time.time() < deadline:
        st = _tenant_status()
        if st:
            for cond in st.get("conditions") or []:
                if cond.get("type") == "Ready" and cond.get("status") == "True":
                    return st
        time.sleep(interval)
    return None


_MAAS_API_OVERRIDE_REPLICAS = 2
_MAAS_API_OVERRIDE_REQUESTS = {"memory": "320Mi", "cpu": "150m"}
_MAAS_API_OVERRIDE_LIMITS = {"memory": "768Mi", "cpu": "750m"}


def _maas_api_deployment(namespace: str) -> dict | None:
    try:
        return _oc_json(["get", "deployment", "maas-api", "-n", namespace, "-o", "json"])
    except subprocess.CalledProcessError as exc:
        if _oc_not_found(exc):
            return None
        raise


def _maas_api_container_resources(namespace: str) -> dict | None:
    deployment = _maas_api_deployment(namespace)
    if deployment is None:
        return None
    containers = deployment.get("spec", {}).get("template", {}).get("spec", {}).get("containers") or []
    for container in containers:
        if container.get("name") == "maas-api":
            return container.get("resources") or {}
    return {}


def _maas_api_replica_count(namespace: str) -> int | None:
    deployment = _maas_api_deployment(namespace)
    if deployment is None:
        return None
    replicas = deployment.get("spec", {}).get("replicas")
    if replicas is None:
        return None
    return int(replicas)


def _resources_match(resources: dict, expected_requests: dict, expected_limits: dict) -> bool:
    requests = resources.get("requests") or {}
    limits = resources.get("limits") or {}
    return all(requests.get(key) == value for key, value in expected_requests.items()) and all(
        limits.get(key) == value for key, value in expected_limits.items()
    )


def _wait_maas_api_resources(
    namespace: str,
    expected_requests: dict,
    expected_limits: dict,
    *,
    timeout: int = 180,
    interval: int = 5,
) -> dict | None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        resources = _maas_api_container_resources(namespace)
        if resources is not None and _resources_match(resources, expected_requests, expected_limits):
            return resources
        time.sleep(interval)
    return None


def _wait_maas_api_replicas(
    namespace: str,
    expected_replicas: int,
    *,
    timeout: int = 180,
    interval: int = 5,
) -> int | None:
    deadline = time.time() + timeout
    while time.time() < deadline:
        replicas = _maas_api_replica_count(namespace)
        if replicas == expected_replicas:
            return replicas
        time.sleep(interval)
    return None


def _restore_tenant_spec(baseline: dict, original_spec: dict) -> None:
    restore_doc = {
        "apiVersion": baseline["apiVersion"],
        "kind": baseline["kind"],
        "metadata": {
            "name": baseline["metadata"]["name"],
            "namespace": baseline["metadata"]["namespace"],
        },
        "spec": original_spec,
    }
    _apply_cr(restore_doc)


class TestTenantLifecycle:
    def test_tenant_ready_and_phase_healthy(self):
        st = _wait_tenant_ready()
        assert st is not None, "MaasTenantConfig Ready did not become True in time."

        phase = st.get("phase")
        assert phase in ("Active", "Degraded"), (
            f"Expected phase Active or Degraded when reconciled, got {phase!r}"
        )

    @pytest.mark.serial
    def test_payload_processing_deployed_with_active_tenant(self):
        """Active MaasTenantConfig should reconcile tenant platform workloads.

        Verifies payload-processing exists and that spec.maasApi replicas/resources
        overrides are applied to the default maas-api Deployment (then restored).
        """
        st = _wait_tenant_ready()
        assert st is not None, "MaasTenantConfig not Ready; skip workload checks."
        phase = st.get("phase")
        if phase not in ("Active", "Degraded"):
            pytest.skip(f"Tenant phase {phase!r}; workload checks require Active or Degraded")

        deployments_ready = any(
            cond.get("type") == "DeploymentsAvailable" and cond.get("status") == "True"
            for cond in (st.get("conditions") or [])
        )
        if not deployments_ready:
            pytest.skip("Tenant DeploymentsAvailable is not True; skipping workload checks")

        result = _oc_run(
            [
                "get",
                "deployment",
                "payload-processing",
                "-n",
                GATEWAY_NAMESPACE,
                "-o",
                "name",
            ]
        )
        if result.returncode != 0:
            if _oc_output_not_found(result):
                pytest.skip(
                    f"payload-processing deployment not found in namespace {GATEWAY_NAMESPACE!r}; "
                    "skipping (optional workload in some CI or partial installs)."
                )
            combined = (result.stderr or "") + (result.stdout or "")
            pytest.fail(
                f"`oc get deployment payload-processing -n {GATEWAY_NAMESPACE}` failed: "
                f"{combined.strip()}"
            )
        assert result.stdout.strip(), "payload-processing deployment get succeeded but returned no name"

        maas_api_result = _oc_run(
            [
                "get",
                "deployment",
                "maas-api",
                "-n",
                MAAS_API_DEPLOYMENT_NAMESPACE,
                "-o",
                "name",
            ]
        )
        if maas_api_result.returncode != 0:
            if _oc_output_not_found(maas_api_result):
                pytest.skip(
                    f"maas-api deployment not found in namespace {MAAS_API_DEPLOYMENT_NAMESPACE!r}; "
                    "skipping maasApi override check."
                )
            combined = (maas_api_result.stderr or "") + (maas_api_result.stdout or "")
            pytest.fail(
                f"`oc get deployment maas-api -n {MAAS_API_DEPLOYMENT_NAMESPACE}` failed: "
                f"{combined.strip()}"
            )

        baseline = _tenant_doc()
        original_spec = copy.deepcopy(baseline.get("spec") or {})
        patch = {
            "spec": {
                "maasApi": {
                    "replicas": _MAAS_API_OVERRIDE_REPLICAS,
                    "resources": {
                        "requests": _MAAS_API_OVERRIDE_REQUESTS,
                        "limits": _MAAS_API_OVERRIDE_LIMITS,
                    },
                }
            }
        }
        patch_result = _oc_run(
            [
                "patch",
                "maastenantconfig",
                TENANT_NAME,
                "-n",
                _ns(),
                "--type=merge",
                "-p",
                json.dumps(patch),
            ]
        )
        if patch_result.returncode != 0:
            combined = (patch_result.stderr or "") + (patch_result.stdout or "")
            if "maasApi" in combined or "unknown field" in combined.lower():
                pytest.skip(
                    "MaasTenantConfig spec.maasApi not supported by installed CRD/controller; "
                    f"skipping maasApi override check: {combined.strip()}"
                )
            pytest.fail(
                f"`oc patch maastenantconfig/{TENANT_NAME}` failed: {combined.strip()}"
            )

        try:
            matched_replicas = _wait_maas_api_replicas(
                MAAS_API_DEPLOYMENT_NAMESPACE,
                _MAAS_API_OVERRIDE_REPLICAS,
            )
            assert matched_replicas is not None, (
                "maas-api Deployment replicas did not match MaasTenantConfig override within timeout; "
                f"expected replicas={_MAAS_API_OVERRIDE_REPLICAS}, "
                f"last observed={_maas_api_replica_count(MAAS_API_DEPLOYMENT_NAMESPACE)!r}"
            )

            matched_resources = _wait_maas_api_resources(
                MAAS_API_DEPLOYMENT_NAMESPACE,
                _MAAS_API_OVERRIDE_REQUESTS,
                _MAAS_API_OVERRIDE_LIMITS,
            )
            assert matched_resources is not None, (
                "maas-api Deployment resources did not match MaasTenantConfig override within timeout; "
                f"expected requests={_MAAS_API_OVERRIDE_REQUESTS!r} limits={_MAAS_API_OVERRIDE_LIMITS!r}, "
                f"last observed={_maas_api_container_resources(MAAS_API_DEPLOYMENT_NAMESPACE)!r}"
            )
        finally:
            _restore_tenant_spec(baseline, original_spec)


class TestTenantContract:
    def test_status_has_phase_and_conditions(self):
        st = _tenant_status()
        assert st is not None
        assert "phase" in st
        assert "conditions" in st and isinstance(st["conditions"], list)

    def test_spec_is_well_formed(self):
        doc = _tenant_doc()
        assert "spec" in doc and isinstance(doc["spec"], dict)

    def test_conditions_use_kubernetes_metav1_shape(self):
        st = _tenant_status()
        assert st is not None
        required_keys = ("type", "status", "reason", "message", "lastTransitionTime")
        for cond in st.get("conditions") or []:
            for key in required_keys:
                assert key in cond, f"condition {cond.get('type')!r} missing {key!r}"


class TestTenantNoFalseOwnership:
    def test_maas_user_crs_not_owned_by_tenant(self):
        checks = [
            (
                "maasmodelref",
                os.environ.get("E2E_MODEL_NAMESPACE", os.environ.get("MODEL_NAMESPACE", "llm")),
            ),
            ("maasauthpolicy", os.environ.get("MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service")),
            ("maassubscription", os.environ.get("MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service")),
        ]
        for cr_type, namespace in checks:
            plural = _KIND_PLURAL[cr_type]
            result = _oc_run(["get", plural, "-n", namespace, "-o", "json"])
            if result.returncode != 0:
                if _oc_output_not_found(result):
                    continue
                combined = (result.stderr or "") + (result.stdout or "")
                pytest.fail(f"`oc get {plural} -n {namespace}` failed: {combined.strip()}")
            for item in json.loads(result.stdout).get("items") or []:
                owners = item.get("metadata", {}).get("ownerReferences") or []
                bad = [r for r in owners if r.get("kind") in ("Tenant", "MaasTenantConfig")]
                assert not bad, (
                    f"{cr_type}/{item['metadata']['name']} has tenant config ownerReferences"
                )
