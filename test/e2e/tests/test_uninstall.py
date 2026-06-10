"""E2E test: uninstall removes all MaaS infra when Config and parent top-level CRs are deleted.

Verifies that deleting the MaaS Config CR and parent operator top-level CRs
(DataScienceCluster / DSCInitialization) in the documented order tears down all
MaaS infrastructure. No orphaned controllers, workloads, routes, or namespaced
CRs should remain after the bounded wait.

Delete sequence (documented order):
  1. Delete all user MaaS CRs (MaaSSubscription, MaaSAuthPolicy, MaaSModelRef, ExternalModel)
  2. Delete Config/default (cluster-scoped anchor -- GC cascades to Tenant, maas-controller, etc.)
  3. Delete DataScienceCluster/default-dsc
  4. Delete DSCInitialization/default-dsci

This test is DESTRUCTIVE: it removes the entire MaaS installation from the
cluster. It must run after all other E2E tests.

Environment variables:
  DEPLOYMENT_NAMESPACE       - Namespace of maas-controller and maas-api (default: opendatahub)
  MAAS_SUBSCRIPTION_NAMESPACE - Namespace of MaaS CRs and Tenant (default: models-as-a-service)
  GATEWAY_NAMESPACE          - Namespace for gateway workloads (default: openshift-ingress)
  E2E_MODEL_NAMESPACE / MODEL_NAMESPACE - Namespace of models (default: llm)
  E2E_UNINSTALL_TIMEOUT      - Max seconds to wait for resource cleanup (default: 300)
  E2E_SKIP_UNINSTALL         - Set to "true" to skip the uninstall test (default: false)
"""

import json
import logging
import os
import shutil
import subprocess
import time

import pytest

log = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

DEPLOYMENT_NAMESPACE = os.environ.get("DEPLOYMENT_NAMESPACE", "opendatahub")
MAAS_SUBSCRIPTION_NAMESPACE = os.environ.get(
    "MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service"
)
GATEWAY_NAMESPACE = os.environ.get("GATEWAY_NAMESPACE", "openshift-ingress")
MODEL_NAMESPACE = os.environ.get(
    "E2E_MODEL_NAMESPACE", os.environ.get("MODEL_NAMESPACE", "llm")
)
UNINSTALL_TIMEOUT = int(os.environ.get("E2E_UNINSTALL_TIMEOUT", "300"))
OC_TIMEOUT = int(os.environ.get("E2E_OC_TIMEOUT", "60"))

# Config CR
CONFIG_CRD = "configs.maas.opendatahub.io"
CONFIG_NAME = "default"

# Parent operator CRs
DSC_NAME = "default-dsc"
DSCI_NAME = "default-dsci"

# MaaS CRDs (plural forms for listing)
MAAS_CRDS = [
    "maassubscriptions.maas.opendatahub.io",
    "maasauthpolicies.maas.opendatahub.io",
    "maasmodelrefs.maas.opendatahub.io",
    "externalmodels.maas.opendatahub.io",
    "tenants.maas.opendatahub.io",
    "configs.maas.opendatahub.io",
    "aitenants.maas.opendatahub.io",
]

# Plural resource names for listing user CRs across namespaces
MAAS_USER_CR_PLURALS = [
    "maassubscriptions",
    "maasauthpolicies",
    "maasmodelrefs",
    "externalmodels",
]

# Resources that should not remain after uninstall.
# Each entry is (kind_plural, namespaces_to_check, label_selector_or_none).
# A None namespace means check cluster-wide.
MAAS_OWNED_RESOURCES = [
    # Deployments
    ("deployments", [DEPLOYMENT_NAMESPACE], "app.opendatahub.io/modelsasservice=true"),
    ("deployments", [GATEWAY_NAMESPACE], "app.kubernetes.io/name=payload-processing"),
    # CronJobs
    ("cronjobs", [DEPLOYMENT_NAMESPACE], "app.opendatahub.io/modelsasservice=true"),
    # HTTPRoutes
    ("httproutes", [DEPLOYMENT_NAMESPACE], "app.opendatahub.io/modelsasservice=true"),
    # Tenant CR
    ("tenants.maas.opendatahub.io", [MAAS_SUBSCRIPTION_NAMESPACE], None),
    # Config CR (cluster-scoped)
    ("configs.maas.opendatahub.io", [None], None),
]

# Resources allowed to remain after uninstall (CRDs, namespaces, etc.)
# These are not considered orphaned infrastructure.
ALLOWLIST_KINDS = {
    "customresourcedefinitions",
    "namespaces",
}


# ---------------------------------------------------------------------------
# oc helpers (same pattern as test_config_tenant.py / test_aitenant_lifecycle.py)
# ---------------------------------------------------------------------------


def _oc_bin():
    path = shutil.which("oc")
    if not path:
        raise RuntimeError("`oc` binary not found in PATH")
    return path


def _oc_run(args, *, timeout=None, input_text=None):
    return subprocess.run(
        [_oc_bin(), *args],
        input=input_text,
        capture_output=True,
        text=True,
        timeout=OC_TIMEOUT if timeout is None else timeout,
        stdin=subprocess.DEVNULL if input_text is None else None,
        check=False,
    )


def _oc_output_not_found(result):
    combined = (result.stderr or "") + (result.stdout or "")
    return "(NotFound)" in combined or "not found" in combined.lower()


def _oc_json(args, *, allow_not_found=False):
    result = _oc_run(args)
    if result.returncode != 0:
        if allow_not_found and _oc_output_not_found(result):
            return None
        raise subprocess.CalledProcessError(
            result.returncode,
            [_oc_bin(), *args],
            result.stdout,
            result.stderr,
        )
    return json.loads(result.stdout)


def _resource_exists(kind, name, namespace=None):
    """Check whether a specific resource exists. Returns True/False."""
    args = ["get", kind, name]
    if namespace:
        args.extend(["-n", namespace])
    result = _oc_run(args)
    return result.returncode == 0


def _list_resources(plural, namespace=None, label_selector=None):
    """List resources, returning items list. Returns [] on not-found or empty."""
    args = ["get", plural, "-o", "json"]
    if namespace:
        args.extend(["-n", namespace])
    if label_selector:
        args.extend(["-l", label_selector])
    result = _oc_run(args)
    if result.returncode != 0:
        # CRD may not exist or namespace may be gone -- treat as empty
        return []
    try:
        data = json.loads(result.stdout)
        return data.get("items", [])
    except (json.JSONDecodeError, KeyError):
        return []


def _delete_resource(kind, name, namespace=None, *, timeout_s=120):
    """Delete a resource and wait up to timeout_s. Best-effort: does not raise on not-found."""
    args = ["delete", kind, name, "--ignore-not-found", f"--timeout={timeout_s}s"]
    if namespace:
        args.extend(["-n", namespace])
    result = _oc_run(args, timeout=timeout_s + 30)
    if result.returncode != 0 and not _oc_output_not_found(result):
        log.warning(
            "Failed to delete %s/%s (ns=%s): %s",
            kind, name, namespace, result.stderr.strip(),
        )


def _delete_all_in_namespace(plural, namespace, *, timeout_s=120):
    """Delete all resources of a kind in a namespace. Best-effort."""
    args = ["delete", plural, "--all", "-n", namespace, f"--timeout={timeout_s}s"]
    result = _oc_run(args, timeout=timeout_s + 30)
    if result.returncode != 0:
        log.warning(
            "Failed to delete all %s in %s: %s",
            plural, namespace, result.stderr.strip(),
        )


def _wait_for_not_found(kind, name, namespace=None, *, timeout=None):
    """Poll until the resource is gone. Raises AssertionError on timeout."""
    timeout = timeout or UNINSTALL_TIMEOUT
    deadline = time.time() + timeout
    while time.time() < deadline:
        if not _resource_exists(kind, name, namespace):
            return
        time.sleep(5)
    ns_str = f" in {namespace}" if namespace else ""
    raise AssertionError(f"{kind}/{name}{ns_str} still exists after {timeout}s")


def _crd_exists(crd_name):
    """Check if a CRD is registered on the cluster."""
    result = _oc_run(["get", "crd", crd_name])
    return result.returncode == 0


def _dump_remaining_objects(orphans):
    """Format orphaned objects for assertion message."""
    lines = []
    for item in orphans:
        meta = item.get("metadata", {})
        kind = item.get("kind", "?")
        name = meta.get("name", "?")
        ns = meta.get("namespace", "<cluster>")
        lines.append(f"  {kind}/{name} (namespace={ns})")
    return "\n".join(lines) if lines else "  (none)"


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------


@pytest.fixture(scope="module", autouse=True)
def skip_if_disabled():
    """Allow skipping the uninstall test via E2E_SKIP_UNINSTALL=true."""
    if os.environ.get("E2E_SKIP_UNINSTALL", "").lower() == "true":
        pytest.skip("E2E_SKIP_UNINSTALL=true -- skipping uninstall test")


@pytest.fixture(scope="module", autouse=True)
def require_config_crd():
    """Skip the entire module if the Config CRD is not installed."""
    if not _crd_exists(CONFIG_CRD):
        pytest.skip(
            f"CRD {CONFIG_CRD} not found; MaaS may not be installed on this cluster."
        )


# ---------------------------------------------------------------------------
# Test class
# ---------------------------------------------------------------------------


class TestUninstallCleanup:
    """Verify that deleting Config and parent CRs tears down all MaaS infra."""

    def test_config_exists_before_uninstall(self):
        """Precondition: Config/default must exist before we start the uninstall."""
        assert _resource_exists(
            CONFIG_CRD, CONFIG_NAME
        ), "Config/default does not exist -- MaaS may not be installed"

    def test_delete_user_maas_crs(self):
        """Step 1: Delete all user MaaS CRs (subscriptions, auth policies, model refs, external models).

        This ensures user-created CRs are cleaned up before tearing down
        infrastructure. Finalizers on these CRs may need the controller to
        still be running.
        """
        namespaces_to_clean = [MAAS_SUBSCRIPTION_NAMESPACE, MODEL_NAMESPACE]

        for plural in MAAS_USER_CR_PLURALS:
            if not _crd_exists(f"{plural}.maas.opendatahub.io"):
                log.info("CRD %s.maas.opendatahub.io not found, skipping", plural)
                continue
            for ns in namespaces_to_clean:
                items = _list_resources(plural, namespace=ns)
                if items:
                    log.info(
                        "Deleting %d %s in %s", len(items), plural, ns,
                    )
                    _delete_all_in_namespace(plural, ns, timeout_s=120)

        # Wait for user CRs to be gone
        deadline = time.time() + UNINSTALL_TIMEOUT
        while time.time() < deadline:
            remaining = []
            for plural in MAAS_USER_CR_PLURALS:
                if not _crd_exists(f"{plural}.maas.opendatahub.io"):
                    continue
                for ns in namespaces_to_clean:
                    remaining.extend(_list_resources(plural, namespace=ns))
            if not remaining:
                log.info("All user MaaS CRs deleted")
                return
            log.info("Still %d user CRs remaining, waiting...", len(remaining))
            time.sleep(5)

        pytest.fail(
            f"User MaaS CRs not fully deleted within {UNINSTALL_TIMEOUT}s:\n"
            + _dump_remaining_objects(remaining)
        )

    def test_delete_config_anchor(self):
        """Step 2: Delete Config/default (cluster-scoped anchor).

        Kubernetes GC should cascade to Tenant, maas-controller Deployment,
        and other resources linked via ownerReferences.
        """
        if not _resource_exists(CONFIG_CRD, CONFIG_NAME):
            log.info("Config/default already absent, skipping delete")
            return

        log.info("Deleting Config/default (cluster-scoped anchor)")
        _delete_resource(CONFIG_CRD, CONFIG_NAME, timeout_s=120)

        # Wait for Config to be fully gone
        _wait_for_not_found(CONFIG_CRD, CONFIG_NAME, timeout=UNINSTALL_TIMEOUT)
        log.info("Config/default deleted")

    def test_delete_datasciencecluster(self):
        """Step 3: Delete DataScienceCluster/default-dsc.

        This tells the ODH operator to remove the modelsAsService component.
        """
        dsc_crd = "datascienceclusters.datasciencecluster.opendatahub.io"
        if not _crd_exists(dsc_crd):
            log.info("DSC CRD not found, skipping DSC delete")
            return
        if not _resource_exists("datasciencecluster", DSC_NAME):
            log.info("DSC/%s already absent, skipping delete", DSC_NAME)
            return

        log.info("Deleting DataScienceCluster/%s", DSC_NAME)
        _delete_resource("datasciencecluster", DSC_NAME, timeout_s=180)
        _wait_for_not_found("datasciencecluster", DSC_NAME, timeout=UNINSTALL_TIMEOUT)
        log.info("DataScienceCluster/%s deleted", DSC_NAME)

    def test_delete_dscinitialisation(self):
        """Step 4: Delete DSCInitialization/default-dsci."""
        dsci_crd = "dscinitializations.dscinitialization.opendatahub.io"
        if not _crd_exists(dsci_crd):
            log.info("DSCI CRD not found, skipping DSCI delete")
            return
        if not _resource_exists("dsciinitialization", DSCI_NAME):
            log.info("DSCI/%s already absent, skipping delete", DSCI_NAME)
            return

        log.info("Deleting DSCInitialization/%s", DSCI_NAME)
        _delete_resource("dsciinitialization", DSCI_NAME, timeout_s=180)
        _wait_for_not_found("dsciinitialization", DSCI_NAME, timeout=UNINSTALL_TIMEOUT)
        log.info("DSCInitialization/%s deleted", DSCI_NAME)

    def test_no_maas_workloads_remain(self):
        """Assertion: no MaaS-owned Deployments, CronJobs, or HTTPRoutes remain.

        Fails with a dump of remaining objects on regression.
        """
        orphans = []
        deadline = time.time() + UNINSTALL_TIMEOUT

        while time.time() < deadline:
            orphans = []
            for plural, namespaces, label_sel in MAAS_OWNED_RESOURCES:
                for ns in namespaces:
                    items = _list_resources(plural, namespace=ns, label_selector=label_sel)
                    orphans.extend(items)

            if not orphans:
                log.info("No MaaS-owned workloads remain")
                return
            log.info("Still %d MaaS-owned resources remaining, waiting...", len(orphans))
            time.sleep(10)

        pytest.fail(
            f"MaaS-owned resources still present after {UNINSTALL_TIMEOUT}s:\n"
            + _dump_remaining_objects(orphans)
        )

    def test_no_maas_tenant_remains(self):
        """Assertion: Tenant/default-tenant should be gone after Config deletion."""
        tenant_crd = "tenants.maas.opendatahub.io"
        if not _crd_exists(tenant_crd):
            log.info("Tenant CRD not found, skipping check")
            return

        deadline = time.time() + UNINSTALL_TIMEOUT
        while time.time() < deadline:
            items = _list_resources(
                "tenants.maas.opendatahub.io",
                namespace=MAAS_SUBSCRIPTION_NAMESPACE,
            )
            if not items:
                log.info("No Tenant CRs remain in %s", MAAS_SUBSCRIPTION_NAMESPACE)
                return
            time.sleep(5)

        pytest.fail(
            f"Tenant CRs still present in {MAAS_SUBSCRIPTION_NAMESPACE} "
            f"after {UNINSTALL_TIMEOUT}s:\n"
            + _dump_remaining_objects(items)
        )

    def test_no_maas_controller_deployment_remains(self):
        """Assertion: maas-controller Deployment should be gone."""
        deadline = time.time() + UNINSTALL_TIMEOUT
        while time.time() < deadline:
            if not _resource_exists(
                "deployment", "maas-controller", namespace=DEPLOYMENT_NAMESPACE
            ):
                log.info("maas-controller deployment is gone")
                return
            time.sleep(5)

        pytest.fail(
            f"deployment/maas-controller still exists in {DEPLOYMENT_NAMESPACE} "
            f"after {UNINSTALL_TIMEOUT}s"
        )

    def test_no_maas_api_deployment_remains(self):
        """Assertion: maas-api Deployment should be gone."""
        deadline = time.time() + UNINSTALL_TIMEOUT
        while time.time() < deadline:
            if not _resource_exists(
                "deployment", "maas-api", namespace=DEPLOYMENT_NAMESPACE
            ):
                log.info("maas-api deployment is gone")
                return
            time.sleep(5)

        pytest.fail(
            f"deployment/maas-api still exists in {DEPLOYMENT_NAMESPACE} "
            f"after {UNINSTALL_TIMEOUT}s"
        )

    def test_no_payload_processing_deployment_remains(self):
        """Assertion: payload-processing Deployment in the gateway namespace should be gone."""
        deadline = time.time() + UNINSTALL_TIMEOUT
        while time.time() < deadline:
            if not _resource_exists(
                "deployment", "payload-processing", namespace=GATEWAY_NAMESPACE
            ):
                log.info("payload-processing deployment is gone")
                return
            time.sleep(5)

        pytest.fail(
            f"deployment/payload-processing still exists in {GATEWAY_NAMESPACE} "
            f"after {UNINSTALL_TIMEOUT}s"
        )

    def test_no_maas_user_crs_remain(self):
        """Assertion: no MaaSSubscription, MaaSAuthPolicy, MaaSModelRef, or ExternalModel CRs remain."""
        namespaces_to_check = [MAAS_SUBSCRIPTION_NAMESPACE, MODEL_NAMESPACE]
        orphans = []

        deadline = time.time() + UNINSTALL_TIMEOUT
        while time.time() < deadline:
            orphans = []
            for plural in MAAS_USER_CR_PLURALS:
                if not _crd_exists(f"{plural}.maas.opendatahub.io"):
                    continue
                for ns in namespaces_to_check:
                    orphans.extend(_list_resources(plural, namespace=ns))
            if not orphans:
                log.info("No MaaS user CRs remain")
                return
            time.sleep(5)

        pytest.fail(
            f"MaaS user CRs still present after {UNINSTALL_TIMEOUT}s:\n"
            + _dump_remaining_objects(orphans)
        )
