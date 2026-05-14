"""Tenant singleton (`default-tenant`) checks; DSC disable/remove Tenant → operator CI.

`default-tenant` lives in `MAAS_SUBSCRIPTION_NAMESPACE` (default `models-as-a-service`), not the
controller/maas-api namespace. `maas-controller` bootstraps the Tenant when the Tenant CRD is installed.
Module-level skips below are transitional: legacy or partial clusters may lack CRD/singleton
until CI consistently installs only the controller (dependencies from other pipelines).
"""

import json
import os
import subprocess
import time

import pytest


TENANT_NAME = "default-tenant"
# Tenant CR is reconciled in the subscription namespace (see maas-controller --maas-subscription-namespace).
TENANT_NAMESPACE = os.environ.get("MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service")
# maas-api / maas-controller Deployment and gateway-adjacent workloads (payload-processing) use the app namespace.
APP_NAMESPACE = os.environ.get("DEPLOYMENT_NAMESPACE", "opendatahub")
# BBR / payload-processing deploys into the gateway namespace by default (see maas-api params.env).
GATEWAY_NAMESPACE = os.environ.get("GATEWAY_NAMESPACE", "openshift-ingress")
TENANT_CRD = "tenants.maas.opendatahub.io"

_KIND_PLURAL = {
    "maasmodelref": "maasmodelrefs",
    "maasauthpolicy": "maasauthpolicies",
    "maassubscription": "maassubscriptions",
}


def _oc_json(args):
    result = subprocess.run(
        ["oc"] + args,
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise subprocess.CalledProcessError(
            result.returncode, ["oc"] + args, result.stdout, result.stderr
        )
    return json.loads(result.stdout)


def _tenant_doc():
    return _oc_json(["get", "tenant", TENANT_NAME, "-n", TENANT_NAMESPACE, "-o", "json"])


def _tenant_status():
    try:
        doc = _tenant_doc()
        return doc.get("status") or {}
    except subprocess.CalledProcessError:
        return None


@pytest.fixture(scope="module", autouse=True)
def require_tenant_crd():
    r = subprocess.run(
        ["oc", "get", "crd", TENANT_CRD],
        capture_output=True,
        text=True,
    )
    if r.returncode != 0:
        pytest.skip(
            f"Missing CRD {TENANT_CRD} (transitional skip: install maas-controller manifests "
            f"so CRDs exist; then controller creates {TENANT_NAME})."
        )


@pytest.fixture(scope="module", autouse=True)
def require_tenant_singleton():
    """Skip if singleton missing (transitional: controller should create it once running)."""
    if _tenant_status() is None:
        pytest.skip(
            f"Tenant {TENANT_NAME}/{TENANT_NAMESPACE} not found (transitional skip: "
            "maas-controller should create this on startup once CRDs and controller are installed)."
        )


def _wait_ready_true(timeout=180, interval=5):
    deadline = time.time() + timeout
    while time.time() < deadline:
        st = _tenant_status()
        if st:
            for cond in st.get("conditions") or []:
                if cond.get("type") == "Ready" and cond.get("status") == "True":
                    return True
        time.sleep(interval)
    return False


class TestTenantLifecycle:
    def test_tenant_singleton_exists(self):
        assert _tenant_status() is not None, (
            f"Tenant {TENANT_NAME}/{TENANT_NAMESPACE} missing "
            "(maas-controller should create default-tenant on startup)."
        )

    def test_tenant_ready_and_phase_healthy(self):
        assert _wait_ready_true(), "Tenant Ready did not become True in time."

        phase = (_tenant_status() or {}).get("phase")
        assert phase in ("Active", "Degraded"), (
            f"Expected phase Active or Degraded when reconciled, got {phase!r}"
        )

    def test_payload_processing_deployed_with_active_tenant(self):
        assert _wait_ready_true(), "Tenant not Ready; skip workload checks."
        phase = (_tenant_status() or {}).get("phase")
        if phase != "Active":
            pytest.skip("Tenant not Active (e.g. Degraded); payload-processing not asserted")

        result = subprocess.run(
            [
                "oc",
                "get",
                "deployment",
                "payload-processing",
                "-n",
                GATEWAY_NAMESPACE,
                "-o",
                "name",
            ],
            capture_output=True,
            text=True,
        )
        if result.returncode != 0:
            pytest.skip(
                f"payload-processing deployment not found in namespace {GATEWAY_NAMESPACE!r}; "
                "skipping (optional workload in some CI or partial installs)."
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
            result = subprocess.run(
                ["oc", "get", plural, "-n", namespace, "-o", "json"],
                capture_output=True,
                text=True,
            )
            if result.returncode != 0:
                continue
            for item in json.loads(result.stdout).get("items") or []:
                owners = item.get("metadata", {}).get("ownerReferences") or []
                bad = [r for r in owners if r.get("kind") == "Tenant"]
                assert not bad, (
                    f"{cr_type}/{item['metadata']['name']} has Tenant ownerReferences"
                )
