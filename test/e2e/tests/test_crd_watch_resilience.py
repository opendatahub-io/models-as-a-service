"""
E2E tests for maas-controller dynamic CRD watch behavior (PR-1075).

Verifies that:
  - When a CRD is absent at startup, registerWatchWhenCRDAppears is used.
  - The watch registers exactly once (sync.Once) regardless of CRD update events.
  - The controller remains stable (no restarts) after dynamic watch registration.
  - When a CRD is present at startup, the watch is registered statically.

Requirements:
  - maas-controller must be deployed and running.
  - Kuadrant CRDs (authpolicies, tokenratelimitpolicies) must be installed.
  - KServe (llminferenceservices) CRD should NOT be installed at module start.

Environment:
  - DEPLOYMENT_NAMESPACE: namespace where maas-controller runs (default: opendatahub)
"""
import logging
import subprocess
import time
import os
import pytest

log = logging.getLogger(__name__)

DEPLOYMENT_NAMESPACE = os.environ.get("DEPLOYMENT_NAMESPACE", "opendatahub")
KSERVE_CRD = "llminferenceservices.serving.kserve.io"
KUADRANT_AUTHPOLICY_CRD = "authpolicies.kuadrant.io"
CONTROLLER_DEPLOYMENT = "maas-controller"
# KServe CRD is embedded as a constant to avoid network dependency in tests.
# Source: kserve/kserve v0.19.0 serving.kserve.io_llminferenceservices CRD (truncated to metadata only).
# Full CRD installed via local go module cache when available, else minimal stub.
KSERVE_CRD_STUB = """
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: llminferenceservices.serving.kserve.io
spec:
  group: serving.kserve.io
  names:
    kind: LLMInferenceService
    listKind: LLMInferenceServiceList
    plural: llminferenceservices
    singular: llminferenceservice
  scope: Namespaced
  versions:
  - name: v1alpha1
    schema:
      openAPIV3Schema:
        type: object
        x-kubernetes-preserve-unknown-fields: true
    served: true
    storage: true
"""

# ─────────────────────────────────────────────────────────────────────────────
# Helpers
# ─────────────────────────────────────────────────────────────────────────────

def _oc(*args, check=True):
    cmd = ["oc"] + list(args)
    log.debug("Running: %s", " ".join(cmd))
    return subprocess.run(cmd, capture_output=True, text=True, check=check)


def _crd_exists(crd_name):
    return _oc("get", "crd", crd_name, check=False).returncode == 0


def _current_pod_uid():
    """Return UID of the current maas-controller pod (for detecting restarts)."""
    result = _oc(
        "get", "pods", "-n", DEPLOYMENT_NAMESPACE,
        "-l", "app.kubernetes.io/name=maas-controller",
        "-o", "jsonpath={.items[0].metadata.uid}",
        check=False,
    )
    return result.stdout.strip()


def _pod_restart_count():
    """Return the restart count of the current maas-controller container."""
    result = _oc(
        "get", "pods", "-n", DEPLOYMENT_NAMESPACE,
        "-l", "app.kubernetes.io/name=maas-controller",
        "-o", "jsonpath={.items[0].status.containerStatuses[0].restartCount}",
        check=False,
    )
    try:
        return int(result.stdout.strip())
    except ValueError:
        return -1


def _pod_logs():
    """Return last 300 lines of maas-controller logs."""
    return _oc("logs", "-n", DEPLOYMENT_NAMESPACE,
               f"deployment/{CONTROLLER_DEPLOYMENT}", "--tail=300",
               check=False).stdout


def _pod_logs_since(timestamp_rfc3339):
    """Return logs since a specific RFC3339 timestamp."""
    return _oc("logs", "-n", DEPLOYMENT_NAMESPACE,
               f"deployment/{CONTROLLER_DEPLOYMENT}",
               f"--since-time={timestamp_rfc3339}",
               check=False).stdout


def _current_timestamp():
    """Return current time in RFC3339 format."""
    result = subprocess.run(
        ["date", "-u", "+%Y-%m-%dT%H:%M:%SZ"],
        capture_output=True, text=True, check=True,
    )
    return result.stdout.strip()


def _rollout_restart_and_wait(timeout=120):
    """Restart maas-controller and wait for it to be ready. Returns new pod UID."""
    _oc("rollout", "restart", f"deployment/{CONTROLLER_DEPLOYMENT}",
        "-n", DEPLOYMENT_NAMESPACE)
    _oc("rollout", "status", f"deployment/{CONTROLLER_DEPLOYMENT}",
        "-n", DEPLOYMENT_NAMESPACE, f"--timeout={timeout}s")
    # Brief stabilization
    time.sleep(5)
    return _current_pod_uid()


def _wait_for_log(pattern, timeout=60, check_since=None):
    """Poll logs until pattern appears. Returns True if found within timeout."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        logs = _pod_logs_since(check_since) if check_since else _pod_logs()
        if pattern in logs:
            return True
        time.sleep(5)
    return False


def _delete_crd(crd_name):
    _oc("delete", "crd", crd_name, "--ignore-not-found", check=False)
    # Wait for deletion to propagate
    deadline = time.time() + 30
    while time.time() < deadline:
        if not _crd_exists(crd_name):
            return
        time.sleep(3)


def _install_kserve_crd():
    """Install KServe LLMInferenceService CRD using local go module cache or embedded stub."""
    import subprocess as sp
    local_crd = os.path.expanduser(
        "~/go/pkg/mod/github.com/kserve/kserve@v0.19.0"
        "/charts/kserve-llmisvc-crd/templates"
        "/serving.kserve.io_llminferenceservices.yaml"
    )
    if os.path.exists(local_crd):
        sp.run(["oc", "apply", "--server-side", "-f", local_crd],
               capture_output=True, text=True, check=False)
    else:
        # Fall back to minimal stub CRD
        sp.run(["oc", "apply", "--server-side", "-f", "-"],
               input=KSERVE_CRD_STUB, capture_output=True, text=True, check=False)
    # Wait for establishment
    deadline = time.time() + 60
    while time.time() < deadline:
        if _crd_exists(KSERVE_CRD):
            return True
        time.sleep(5)
    return False


# ─────────────────────────────────────────────────────────────────────────────
# Module-level fixtures
# ─────────────────────────────────────────────────────────────────────────────

@pytest.fixture(scope="module", autouse=True)
def skip_if_not_ready():
    """Skip module if controller or Kuadrant CRDs are not present."""
    if _oc("get", "deployment", CONTROLLER_DEPLOYMENT,
           "-n", DEPLOYMENT_NAMESPACE, check=False).returncode != 0:
        pytest.skip(f"maas-controller not found in {DEPLOYMENT_NAMESPACE}")
    if not _crd_exists(KUADRANT_AUTHPOLICY_CRD):
        pytest.skip("Kuadrant AuthPolicy CRD not installed — required for controller")


@pytest.fixture(scope="module")
def fresh_state_without_kserve():
    """
    Module-scoped: ensure KServe CRD is absent and controller is freshly started.
    Yields the pod UID after restart. Cleans up KServe CRD after module.
    """
    _delete_crd(KSERVE_CRD)
    pod_uid = _rollout_restart_and_wait()
    yield pod_uid
    # Cleanup
    _delete_crd(KSERVE_CRD)


# ─────────────────────────────────────────────────────────────────────────────
# Tests
# ─────────────────────────────────────────────────────────────────────────────

class TestDynamicCRDWatch:
    """Tests for registerWatchWhenCRDAppears + sync.Once behavior."""

    def test_controller_stable_without_kserve_crd(self, fresh_state_without_kserve):
        """
        Controller must remain stable (no restarts) when KServe CRD is absent.
        The crd-watcher log must confirm dynamic watch was registered.
        """
        pod_uid = fresh_state_without_kserve
        assert not _crd_exists(KSERVE_CRD), "Pre-condition: KServe CRD must be absent"
        assert pod_uid == _current_pod_uid(), "Pre-condition: pod must not have restarted"

        restart_before = _pod_restart_count()

        # Wait to confirm controller is stable (not crash-looping)
        time.sleep(30)

        # Pod must not have been replaced
        assert pod_uid == _current_pod_uid(), (
            "Pod was replaced — controller crash-looped with KServe CRD absent"
        )
        assert _pod_restart_count() == restart_before, (
            "Controller restarted — expected 0 restarts with KServe CRD absent"
        )

        # Crd-watcher startup log must be present for KServe
        logs = _pod_logs()
        assert "will register watch dynamically" in logs or "CRD not yet registered at startup" in logs, (
            "Expected crd-watcher startup message for llminferenceservices in logs"
        )

        # CRITICAL: registerWatchWhenCRDAppears must only fire for the correct CRD.
        # Kuadrant CRDs (already installed) generate CRD events that the watcher sees,
        # but the name filter (crd.Name != crdName) must prevent them from triggering
        # the KServe watch registration.
        assert "CRD appeared; watch registered dynamically" not in logs, (
            "KServe watch was spuriously triggered — crd.Name filter not working correctly. "
            "Unrelated CRD events (Kuadrant) should not trigger the KServe watcher."
        )
        log.info("✓ Controller stable; no spurious watch registration from unrelated CRD events")

    def test_dynamic_watch_fires_exactly_once_on_crd_install(self, fresh_state_without_kserve):
        """
        After KServe CRD is installed, the dynamic watch must fire exactly once.
        Triggering 3 additional CRD update events must not produce new registrations.
        sync.Once guarantees exactly one call to c.Watch(makeSource()).
        """
        pod_uid = fresh_state_without_kserve
        assert not _crd_exists(KSERVE_CRD), "Pre-condition: KServe CRD must be absent"
        assert pod_uid == _current_pod_uid(), "Pre-condition: same pod must be running"

        # Capture timestamp BEFORE installing CRD — only count messages after this point
        t_before_install = _current_timestamp()
        time.sleep(1)  # ensure log timestamp is strictly after t_before_install

        # Install KServe CRD
        assert _install_kserve_crd(), "Failed to install KServe CRD"

        # Wait for the dynamic watch to fire
        found = _wait_for_log("CRD appeared; watch registered dynamically",
                              timeout=60, check_since=t_before_install)
        assert found, "Dynamic watch did not fire within 60s after KServe CRD install"

        # Wait 3s to ensure the "appeared" log timestamp is strictly before t_after_watch.
        # kubectl --since-time has second granularity; if the log and timestamp share the
        # same second, the log would be included. 3s ensures clear separation.
        time.sleep(3)
        t_after_watch = _current_timestamp()
        time.sleep(1)

        # Trigger 3 CRD update events via annotations
        for i in range(1, 4):
            _oc("annotate", "crd", KSERVE_CRD,
                f"test.maas.io/sync-once-test={i}", "--overwrite", check=False)

        time.sleep(15)  # Let events propagate

        # Count "CRD appeared" messages AFTER the first registration
        logs_after = _pod_logs_since(t_after_watch)
        appeared_count = logs_after.count("CRD appeared; watch registered dynamically")

        assert appeared_count == 0, (
            f"sync.Once FAILED: 'CRD appeared; watch registered dynamically' appeared "
            f"{appeared_count} more time(s) after 3 CRD annotation events — expected 0"
        )

        # Controller must not have restarted
        assert pod_uid == _current_pod_uid(), "Controller restarted during test"

        log.info("✓ Dynamic watch fired once; sync.Once prevented %d duplicate registrations", 3)

    def test_static_watch_registered_when_crd_present_at_startup(self):
        """
        When controller restarts with KServe CRD already present, the watch
        is registered statically via the builder — registerWatchWhenCRDAppears
        must NOT be called for KServe.
        """
        assert _crd_exists(KSERVE_CRD), (
            "Pre-condition: KServe CRD must be installed (run after test_dynamic_watch_fires)"
        )

        # Restart controller — KServe CRD is now present at startup
        t_before_restart = _current_timestamp()
        time.sleep(1)
        new_uid = _rollout_restart_and_wait()
        time.sleep(10)

        logs = _pod_logs_since(t_before_restart)

        # Static watch registered — crd-watcher should NOT have been set up for KServe
        # Both strings together indicate the dynamic watcher was incorrectly set up.
        # Neither should appear in fresh logs when CRD is already present at startup.
        assert not (
            "crdName\":\"llminferenceservices.serving.kserve.io\"" in logs and
            "will register watch dynamically" in logs
        ), "Controller used dynamic watch for KServe even though CRD was present at startup"
        # Confirm controller is running
        assert _current_pod_uid() == new_uid, "Controller unexpectedly restarted"

        log.info("✓ Static watch registered at startup when KServe CRD is present")
