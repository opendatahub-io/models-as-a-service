"""
E2E test: uninstall removes all MaaS infra when Config and parent top-level CRs are deleted.

Validates that deleting the MaaS Config CR and the parent operator's top-level custom resources
(DataScienceCluster / DSCInitialization) tears down all MaaS-owned infrastructure so clusters
do not retain stray resources that complicate upgrades, reuse, or compliance reviews.

Ordered delete sequence (documented for CI reproducibility):
  1. Delete user-created MaaS CRs with finalizers (MaaSSubscription, MaaSAuthPolicy,
     MaaSModelRef) while the controller is still running so it can process their
     finalizers (maas.opendatahub.io/*-cleanup).
  2. Delete MaaS Config CR  (configs.maas.opendatahub.io/default)
     — Kubernetes GC cascades to all operands whose ownerReferences point to Config.
  3. Delete DataScienceCluster  (default-dsc)
     — Removes the KServe / ModelsAsService component enablement.
  4. Delete DSCInitialization  (default-dsci)
     — Removes the ODH platform initialization resource.

After a bounded wait the test asserts:
  - No MaaS CRD instances survive (MaaSModelRef, MaaSAuthPolicy, MaaSSubscription,
    ExternalModel, Config, Tenant).
  - No MaaS-owned workloads survive (Deployments, CronJobs labelled app=maas-controller
    or app=maas-api in the deployment namespace).
  - No MaaS-owned routes/HTTPRoutes remain in the model namespace.
  - The MaaS subscription namespace contains no leftover MaaS CRs.

On failure the test dumps remaining objects to aid debugging.

Prerequisites:
  - MaaS deployed through the standard ODH operator enablement path
  - oc/kubectl access with cluster-admin privileges
  - DEPLOYMENT_NAMESPACE env var (default: opendatahub)
  - MAAS_SUBSCRIPTION_NAMESPACE env var (default: models-as-a-service)
  - E2E_MODEL_NAMESPACE env var (default: llm)
"""

import json
import logging
import os
import shutil
import subprocess
import time

import pytest

log = logging.getLogger(__name__)

OC_BIN = shutil.which("oc")
if not OC_BIN:
    raise RuntimeError("Required binary 'oc' not found in PATH")

DEPLOYMENT_NAMESPACE = os.environ.get("DEPLOYMENT_NAMESPACE", "opendatahub")
MAAS_SUBSCRIPTION_NAMESPACE = os.environ.get(
    "MAAS_SUBSCRIPTION_NAMESPACE", "models-as-a-service"
)
MODEL_NAMESPACE = os.environ.get("E2E_MODEL_NAMESPACE", "llm")
UNINSTALL_TIMEOUT = int(os.environ.get("E2E_UNINSTALL_TIMEOUT", "300"))
POLL_INTERVAL = 10

MAAS_CR_KINDS = [
    "configs.maas.opendatahub.io",
    "maasmodelrefs.maas.opendatahub.io",
    "maasauthpolicies.maas.opendatahub.io",
    "maassubscriptions.maas.opendatahub.io",
    "externalmodels.maas.opendatahub.io",
    "tenants.maas.opendatahub.io",
]

MAAS_WORKLOAD_LABELS = [
    "app=maas-controller",
    "app=maas-api",
]


def _oc(*args, timeout=60, check=False):
    result = subprocess.run(
        [OC_BIN, *args],
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    if check and result.returncode != 0:
        raise RuntimeError(
            f"oc {' '.join(args)} failed (rc={result.returncode}): {result.stderr.strip()}"
        )
    return result


def _resource_exists(kind, name, namespace=None):
    cmd = ["get", kind, name]
    if namespace:
        cmd += ["-n", namespace]
    result = _oc(*cmd)
    return result.returncode == 0


def _list_resources(kind, namespace=None, label=None):
    cmd = ["get", kind, "-o", "json"]
    if namespace:
        cmd += ["-n", namespace]
    else:
        cmd += ["--all-namespaces"]
    if label:
        cmd += ["-l", label]
    result = _oc(*cmd)
    if result.returncode != 0:
        stderr = result.stderr.lower()
        if "the server doesn't have a resource type" in stderr or "not found" in stderr:
            return []
        raise RuntimeError(
            f"oc {' '.join(cmd)} failed (rc={result.returncode}): {result.stderr.strip()}"
        )
    try:
        data = json.loads(result.stdout)
        return data.get("items", [])
    except json.JSONDecodeError as exc:
        raise RuntimeError(f"Invalid JSON from oc {' '.join(cmd)}") from exc


def _delete_resource(kind, name, namespace=None, timeout_sec=120):
    cmd = ["delete", kind, name, "--ignore-not-found", f"--timeout={timeout_sec}s"]
    if namespace:
        cmd += ["-n", namespace]
    log.info("Deleting %s/%s%s", kind, name, f" -n {namespace}" if namespace else "")
    result = _oc(*cmd, timeout=timeout_sec + 30)
    if result.returncode != 0:
        log.warning(
            "Delete %s/%s returned rc=%d: %s",
            kind,
            name,
            result.returncode,
            result.stderr.strip(),
        )


def _collect_remaining_maas_resources():
    """Collect all remaining MaaS resources for diagnostic dump on failure."""
    remaining = {}

    for crd_kind in MAAS_CR_KINDS:
        items = _list_resources(crd_kind)
        if items:
            remaining[crd_kind] = [
                {
                    "name": i["metadata"]["name"],
                    "namespace": i["metadata"].get("namespace", "<cluster-scoped>"),
                }
                for i in items
            ]

    for label in MAAS_WORKLOAD_LABELS:
        for wl_kind in ["deployment", "cronjob"]:
            items = _list_resources(wl_kind, namespace=DEPLOYMENT_NAMESPACE, label=label)
            if items:
                key = f"{wl_kind}({label})"
                remaining[key] = [
                    {"name": i["metadata"]["name"], "namespace": i["metadata"].get("namespace", "")}
                    for i in items
                ]

    httproutes = _list_resources("httproute", namespace=MODEL_NAMESPACE)
    maas_routes = [
        r for r in httproutes
        if "maas" in r["metadata"].get("name", "").lower()
        or any(
            "maas" in (ref.get("name", "") or "").lower()
            for ref in r.get("spec", {}).get("parentRefs", [])
        )
    ]
    if maas_routes:
        remaining["httproute(maas-owned)"] = [
            {"name": r["metadata"]["name"], "namespace": r["metadata"].get("namespace", "")}
            for r in maas_routes
        ]

    return remaining


def _format_remaining(remaining):
    lines = ["Remaining MaaS resources after uninstall:"]
    for kind, items in remaining.items():
        lines.append(f"  {kind}:")
        for item in items:
            lines.append(f"    - {item['namespace']}/{item['name']}")
    return "\n".join(lines)


FINALIZER_BEARING_KINDS = [
    "maassubscriptions.maas.opendatahub.io",
    "maasauthpolicies.maas.opendatahub.io",
    "maasmodelrefs.maas.opendatahub.io",
]


def _delete_finalizer_bearing_crs():
    """Delete MaaS CRs that carry controller-managed finalizers.

    Must run while the controller is still alive so it can process
    the finalizers and release the resources cleanly.
    """
    log.info("Step 1/4: Deleting user-created MaaS CRs (finalizer-bearing)")
    deleted_any = False
    for kind in FINALIZER_BEARING_KINDS:
        items = _list_resources(kind)
        for item in items:
            deleted_any = True
            name = item["metadata"]["name"]
            ns = item["metadata"].get("namespace")
            _delete_resource(kind, name, namespace=ns)

    if not deleted_any:
        log.info("No finalizer-bearing CRs found; nothing to wait for")
        return

    log.info("Waiting for controller to process finalizers (up to %ds)...", UNINSTALL_TIMEOUT)
    deadline = time.time() + UNINSTALL_TIMEOUT
    while time.time() < deadline:
        still_present = []
        for kind in FINALIZER_BEARING_KINDS:
            still_present.extend(_list_resources(kind))
        if not still_present:
            log.info("All finalizer-bearing CRs removed")
            return
        log.debug(
            "%d finalizer-bearing CRs still present, waiting %ds...",
            len(still_present),
            POLL_INTERVAL,
        )
        time.sleep(POLL_INTERVAL)
    log.warning(
        "Timed out waiting for finalizer-bearing CRs; %d remain",
        len(still_present),
    )


# ---------------------------------------------------------------------------
# The test runs last via pytest ordering (file sorts after other test_ files).
# It is destructive: it tears down MaaS infrastructure, so it must be the
# final test in the suite.
# ---------------------------------------------------------------------------


class TestUninstallMaaSInfrastructure:
    """Verify that deleting Config and parent CRs fully removes MaaS infrastructure."""

    @pytest.fixture(scope="class", autouse=True)
    def perform_uninstall(self):
        """
        Execute the ordered uninstall sequence before assertions run.

        Delete sequence:
          1. User-created MaaS CRs with finalizers (while controller is running)
          2. MaaS Config CR (configs.maas.opendatahub.io/default)
          3. DataScienceCluster (default-dsc)
          4. DSCInitialization (default-dsci)
        """
        log.info(
            "=== Starting MaaS uninstall sequence (timeout=%ds) ===", UNINSTALL_TIMEOUT
        )

        # Step 1: Delete user-created MaaS CRs that carry finalizers.
        # The controller must still be running to process these finalizers
        # (maas.opendatahub.io/subscription-cleanup, authpolicy-cleanup,
        # model-cleanup). Deleting Config first would GC the controller,
        # leaving these CRs stuck in Terminating.
        _delete_finalizer_bearing_crs()

        # Step 2: Delete the MaaS Config CR.
        # All operands whose ownerReferences point to Config will be garbage-collected.
        log.info("Step 2/4: Deleting MaaS Config CR (configs.maas.opendatahub.io/default)")
        _delete_resource("configs.maas.opendatahub.io", "default")

        # Step 3: Delete the DataScienceCluster.
        log.info("Step 3/4: Deleting DataScienceCluster (default-dsc)")
        _delete_resource(
            "datasciencecluster", "default-dsc", namespace=DEPLOYMENT_NAMESPACE
        )

        # Step 4: Delete DSCInitialization.
        log.info("Step 4/4: Deleting DSCInitialization (default-dsci)")
        _delete_resource(
            "dscinitializations", "default-dsci", namespace=DEPLOYMENT_NAMESPACE
        )

        # Wait for garbage collection to propagate.
        log.info(
            "Waiting up to %ds for GC to remove MaaS-owned resources...",
            UNINSTALL_TIMEOUT,
        )
        deadline = time.time() + UNINSTALL_TIMEOUT
        while time.time() < deadline:
            remaining = _collect_remaining_maas_resources()
            if not remaining:
                log.info("All MaaS resources removed successfully")
                break
            log.debug(
                "Still %d resource kinds remaining, waiting %ds...",
                len(remaining),
                POLL_INTERVAL,
            )
            time.sleep(POLL_INTERVAL)

        yield

    def test_no_maas_config_cr(self):
        """Config CR (configs.maas.opendatahub.io/default) must not exist."""
        items = _list_resources("configs.maas.opendatahub.io")
        names = [i["metadata"]["name"] for i in items]
        assert not items, f"Config CRs still exist: {names}"

    def test_no_maas_model_refs(self):
        """No MaaSModelRef instances should survive uninstall."""
        items = _list_resources("maasmodelrefs.maas.opendatahub.io")
        names = [
            f"{i['metadata'].get('namespace','')}/{i['metadata']['name']}" for i in items
        ]
        assert not items, f"MaaSModelRef CRs still exist: {names}"

    def test_no_maas_auth_policies(self):
        """No MaaSAuthPolicy instances should survive uninstall."""
        items = _list_resources("maasauthpolicies.maas.opendatahub.io")
        names = [
            f"{i['metadata'].get('namespace','')}/{i['metadata']['name']}" for i in items
        ]
        assert not items, f"MaaSAuthPolicy CRs still exist: {names}"

    def test_no_maas_subscriptions(self):
        """No MaaSSubscription instances should survive uninstall."""
        items = _list_resources("maassubscriptions.maas.opendatahub.io")
        names = [
            f"{i['metadata'].get('namespace','')}/{i['metadata']['name']}" for i in items
        ]
        assert not items, f"MaaSSubscription CRs still exist: {names}"

    def test_no_external_models(self):
        """No ExternalModel instances should survive uninstall."""
        items = _list_resources("externalmodels.maas.opendatahub.io")
        names = [
            f"{i['metadata'].get('namespace','')}/{i['metadata']['name']}" for i in items
        ]
        assert not items, f"ExternalModel CRs still exist: {names}"

    def test_no_tenant_crs(self):
        """No Tenant instances should survive uninstall."""
        items = _list_resources("tenants.maas.opendatahub.io")
        names = [i["metadata"]["name"] for i in items]
        assert not items, f"Tenant CRs still exist: {names}"

    def test_no_maas_controller_workloads(self):
        """No maas-controller Deployments should remain in the deployment namespace."""
        items = _list_resources(
            "deployment",
            namespace=DEPLOYMENT_NAMESPACE,
            label="app=maas-controller",
        )
        names = [i["metadata"]["name"] for i in items]
        assert not items, (
            f"maas-controller deployments still running in {DEPLOYMENT_NAMESPACE}: {names}"
        )

    def test_no_maas_api_workloads(self):
        """No maas-api Deployments or CronJobs should remain in the deployment namespace."""
        remaining = []
        for kind in ["deployment", "cronjob"]:
            items = _list_resources(
                kind, namespace=DEPLOYMENT_NAMESPACE, label="app=maas-api"
            )
            remaining.extend(
                f"{kind}/{i['metadata']['name']}" for i in items
            )
        assert not remaining, (
            f"maas-api workloads still running in {DEPLOYMENT_NAMESPACE}: {remaining}"
        )

    def test_no_maas_subscription_namespace_crs(self):
        """The MaaS subscription namespace should contain no leftover MaaS CRs."""
        remaining = []
        for crd_kind in [
            "maasmodelrefs.maas.opendatahub.io",
            "maasauthpolicies.maas.opendatahub.io",
            "maassubscriptions.maas.opendatahub.io",
        ]:
            items = _list_resources(crd_kind, namespace=MAAS_SUBSCRIPTION_NAMESPACE)
            remaining.extend(
                f"{crd_kind}/{i['metadata']['name']}" for i in items
            )
        assert not remaining, (
            f"MaaS CRs still exist in {MAAS_SUBSCRIPTION_NAMESPACE}: {remaining}"
        )

    def test_diagnostic_dump_on_residual(self):
        """
        Final sweep: collect any remaining MaaS resources across all checked
        kinds and fail with a full diagnostic dump if anything leaked.
        """
        remaining = _collect_remaining_maas_resources()
        assert not remaining, _format_remaining(remaining)
