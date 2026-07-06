import json
import os
import shutil
import subprocess
import time

import pytest

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


TENANT_NAME = "default-tenant"
TENANT_NAMESPACE = os.environ.get("MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service")
GATEWAY_NAMESPACE = os.environ.get("GATEWAY_NAMESPACE", "openshift-ingress")
TENANT_CRD = "tenants.maas.opendatahub.io"

_KIND_PLURAL = {
    "maasmodelref": "maasmodelrefs",
    "maasauthpolicy": "maasauthpolicies",
    "maassubscription": "maassubscriptions",
}


def _tenant_doc():
    return _oc_json(["get", "tenant", TENANT_NAME, "-n", TENANT_NAMESPACE, "-o", "json"])


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
            f"Tenant {TENANT_NAME}/{TENANT_NAMESPACE} not found (transitional skip: "
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


class TestTenantLifecycle:
    def test_tenant_ready_and_phase_healthy(self):
        st = _wait_tenant_ready()
        assert st is not None, "Tenant Ready did not become True in time."

        phase = st.get("phase")
        assert phase in ("Active", "Degraded"), (
            f"Expected phase Active or Degraded when reconciled, got {phase!r}"
        )

    def test_payload_processing_deployed_with_active_tenant(self):
        st = _wait_tenant_ready()
        assert st is not None, "Tenant not Ready; skip workload checks."
        phase = st.get("phase")
        if phase != "Active":
            pytest.skip("Tenant not Active (e.g. Degraded); payload-processing not asserted")

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
                bad = [r for r in owners if r.get("kind") == "Tenant"]
                assert not bad, (
                    f"{cr_type}/{item['metadata']['name']} has Tenant ownerReferences"
                )
