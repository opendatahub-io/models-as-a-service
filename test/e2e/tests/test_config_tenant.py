"""Read-only E2E for cluster Config/default anchor.

Verifies that the Config singleton exists and is healthy. Lifecycle coordination between Config
and namespace-scoped resources (Tenant, Deployment) happens through controller watches rather
than owner references (cross-scope owner refs are invalid in Kubernetes).

Does not delete Config; destructive GC checks belong in operator or dedicated jobs.
"""

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


CONFIG_CRD = "configs.maas.opendatahub.io"
CONFIG_NAME = "default"
CONFIG_KIND = "Config"
CONFIG_API_PREFIX = "maas.opendatahub.io/"

TENANT_NAME = "default-tenant"
TENANT_NAMESPACE = os.environ.get("MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service")
CONTROLLER_DEPLOY_NS = os.environ.get("DEPLOYMENT_NAMESPACE", "opendatahub")
CONTROLLER_DEPLOYMENT = "maas-controller"


def _config_doc():
    return _oc_json(["get", CONFIG_CRD, CONFIG_NAME, "-o", "json"])


def _tenant_doc():
    return _oc_json(["get", "tenant", TENANT_NAME, "-n", TENANT_NAMESPACE, "-o", "json"])


def _config_uid_or_none():
    try:
        doc = _config_doc()
        uid = doc.get("metadata", {}).get("uid") or ""
        return uid if uid else None
    except subprocess.CalledProcessError as exc:
        if _oc_not_found(exc):
            return None
        raise


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
    r = _oc_run(["get", "crd", CONFIG_CRD])
    if r.returncode != 0:
        if _oc_output_not_found(r):
            pytest.skip(
                f"Missing CRD {CONFIG_CRD} (transitional skip: install maas-controller bundle from "
                "a release that includes the Config anchor API, e.g. post-#894)."
            )
        combined = (r.stderr or "") + (r.stdout or "")
        pytest.fail(f"`oc get crd {CONFIG_CRD}` failed: {combined.strip()}")


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
