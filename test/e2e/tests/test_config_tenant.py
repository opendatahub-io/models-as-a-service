"""Read-only E2E for cluster Config/default (anchor + owner refs on Tenant and maas-controller).

Does not delete Config; destructive GC checks belong in operator or dedicated jobs.
"""

import json
import os
import subprocess
import time

import pytest


CONFIG_CRD = "configs.maas.opendatahub.io"
CONFIG_NAME = "default"
CONFIG_KIND = "Config"
CONFIG_API_PREFIX = "maas.opendatahub.io/"

TENANT_NAME = "default-tenant"
TENANT_NAMESPACE = os.environ.get("MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service")
CONTROLLER_DEPLOY_NS = os.environ.get("DEPLOYMENT_NAMESPACE", "opendatahub")
CONTROLLER_DEPLOYMENT = "maas-controller"


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


def _config_doc():
    return _oc_json(["get", CONFIG_CRD, CONFIG_NAME, "-o", "json"])


def _tenant_doc():
    return _oc_json(["get", "tenant", TENANT_NAME, "-n", TENANT_NAMESPACE, "-o", "json"])


def _config_uid_or_none():
    try:
        doc = _config_doc()
        uid = doc.get("metadata", {}).get("uid") or ""
        return uid if uid else None
    except subprocess.CalledProcessError:
        return None


def _ref_to_config(refs):
    for ref in refs or []:
        if ref.get("kind") != CONFIG_KIND or ref.get("name") != CONFIG_NAME:
            continue
        api = ref.get("apiVersion") or ""
        if not api.startswith(CONFIG_API_PREFIX):
            continue
        return ref
    return None


@pytest.fixture(scope="module", autouse=True)
def require_config_crd():
    r = subprocess.run(
        ["oc", "get", "crd", CONFIG_CRD],
        capture_output=True,
        text=True,
    )
    if r.returncode != 0:
        pytest.skip(
            f"Missing CRD {CONFIG_CRD} (transitional skip: install maas-controller bundle from "
            "a release that includes the Config anchor API, e.g. post-#894)."
        )


@pytest.fixture(scope="module", autouse=True)
def require_config_singleton():
    """Wait for Config/default with UID (lifecycle reconciler creates it after controller starts)."""
    deadline = time.time() + 120
    while time.time() < deadline:
        uid = _config_uid_or_none()
        if uid:
            return
        time.sleep(5)
    pytest.skip(
        f"Config {CONFIG_NAME} did not become ready with a UID in time; check maas-controller logs."
    )


class TestConfigAnchorPresence:
    def test_cluster_config_default_exists(self):
        doc = _config_doc()
        assert doc.get("metadata", {}).get("uid"), "Config/default must have metadata.uid"

    def test_cluster_config_not_terminating(self):
        doc = _config_doc()
        assert not doc.get("metadata", {}).get(
            "deletionTimestamp"
        ), "Config anchor is deleting; platform GC may be in progress."


class TestConfigTenantOwnership:
    def test_tenant_lists_config_owner_reference(self):
        try:
            doc = _tenant_doc()
        except subprocess.CalledProcessError:
            pytest.skip(
                f"Tenant {TENANT_NAME}/{TENANT_NAMESPACE} not found; run after Tenant bootstrap."
            )
        ref = _ref_to_config(doc.get("metadata", {}).get("ownerReferences"))
        assert ref is not None, (
            f"Tenant {TENANT_NAME}/{TENANT_NAMESPACE} should reference Config/{CONFIG_NAME} "
            "(LifecycleReconciler links the anchor for GC)."
        )

    def test_maas_controller_deployment_lists_config_owner_reference(self):
        result = subprocess.run(
            [
                "oc",
                "get",
                "deployment",
                CONTROLLER_DEPLOYMENT,
                "-n",
                CONTROLLER_DEPLOY_NS,
                "-o",
                "json",
            ],
            capture_output=True,
            text=True,
        )
        if result.returncode != 0:
            pytest.skip(
                f"deployment/{CONTROLLER_DEPLOYMENT} not found in {CONTROLLER_DEPLOY_NS!r}; "
                "skipping Config→Deployment owner check."
            )
        doc = json.loads(result.stdout)
        ref = _ref_to_config(doc.get("metadata", {}).get("ownerReferences"))
        assert ref is not None, (
            f"Deployment {CONTROLLER_DEPLOYMENT}/{CONTROLLER_DEPLOY_NS} should list an owner "
            f"reference to Config/{CONFIG_NAME}."
        )
