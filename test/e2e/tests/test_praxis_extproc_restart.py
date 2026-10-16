"""
E2E test for Praxis ExtProc pod restart / rollout recovery.

Covers RHOAIENG-97468: when a default-tenant praxis ExtProc Deployment is rolled
out (pods replaced), authenticated inference must resume on its own, without
manual CR edits or deletes.

The default praxis dataplane runs two ExtProc Deployments — payload-processing
(request ExtProc) and payload-pre-processing (BBR pre-processor). This module
exercises recovery when each is rolled out on its own and when both are rolled
out together.

Each test:
  1. Confirms the default payload-processing stack is praxis-extproc (skip otherwise).
  2. Proves authenticated inference succeeds before the disruption.
  3. `oc rollout restart`s the target Deployment(s) and waits for each rollout to
     complete and report Available with fresh pods.
  4. Proves authenticated inference recovers (200) once auth is re-enforced.

Mutates the shared default dataplane, so these are @serial tests.
"""

from __future__ import annotations

import logging

import pytest
from multitenancy_helpers import (
    GATEWAY_NAMESPACE,
    LABEL_TENANT_INSTANCE,
    _oc_run,
    extproc_deployment_uses_praxis,
    wait_for_deployment_available,
)
from test_helper import (
    _check_ipp_pods_deployed,
    _poll_status,
    _wait_for_gateway_auth_enforced,
)

log = logging.getLogger(__name__)

pytestmark = [
    pytest.mark.serial,
    pytest.mark.xdist_group("tenant_ipp"),
]

# Default-tenant praxis ExtProc Deployments (unsuffixed) in the gateway namespace.
PROCESSING_DEPLOYMENT = "payload-processing"
PRE_PROCESSING_DEPLOYMENT = "payload-pre-processing"

# Rollout + readiness budget. ExtProc pods are lightweight but the gateway/auth
# plane needs a moment to re-enforce after new pods come up.
ROLLOUT_TIMEOUT = 240


def _skip_unless_default_praxis():
    if not _check_ipp_pods_deployed():
        pytest.skip("Default payload-processing stack is not ready")
    if not extproc_deployment_uses_praxis(PROCESSING_DEPLOYMENT):
        pytest.skip("default tenant payload-processing is not praxis-extproc")


def _extproc_pod_uids(deployment: str) -> set[str]:
    """UIDs of a Deployment's current pods, so we can prove the rollout replaced them."""
    result = _oc_run(
        [
            "get",
            "pods",
            "-n",
            GATEWAY_NAMESPACE,
            "-l",
            f"{LABEL_TENANT_INSTANCE}={deployment}",
            "-o",
            "jsonpath={.items[*].metadata.uid}",
        ]
    )
    return {uid for uid in (result.stdout or "").split() if uid}


class TestPraxisExtProcRestart:
    """Default-tenant praxis ExtProc rollout recovery (RHOAIENG-97468)."""

    def _assert_rollout_recovers(self, api_key, deployments):
        """Roll out the given ExtProc Deployment(s) and assert inference recovers.

        deployments is restarted together (restarts triggered first, then waited
        on) so the "both" case reproduces a simultaneous rollout.
        """
        _skip_unless_default_praxis()

        _poll_status(api_key, 200)
        log.info("Baseline inference succeeded through praxis ExtProc")

        before_uids = {d: _extproc_pod_uids(d) for d in deployments}
        for deployment, uids in before_uids.items():
            assert uids, f"no {deployment} pods found before rollout"

        for deployment in deployments:
            log.info("Rolling out deployment/%s in %s", deployment, GATEWAY_NAMESPACE)
            _oc_run(["rollout", "restart", f"deployment/{deployment}", "-n", GATEWAY_NAMESPACE])
        for deployment in deployments:
            _oc_run(
                [
                    "rollout",
                    "status",
                    f"deployment/{deployment}",
                    "-n",
                    GATEWAY_NAMESPACE,
                    f"--timeout={ROLLOUT_TIMEOUT}s",
                ]
            )

        for deployment in deployments:
            wait_for_deployment_available(deployment, GATEWAY_NAMESPACE, timeout=ROLLOUT_TIMEOUT)
            after_uids = _extproc_pod_uids(deployment)
            # New UID, not strict replacement: the old pod lingers in Terminating
            # while the new one is already Ready.
            new_uids = after_uids - before_uids[deployment]
            assert new_uids, (
                f"{deployment} rollout produced no new pods "
                f"(before={before_uids[deployment]}, after={after_uids})"
            )
            log.info("%s rollout brought up fresh pod(s): %s", deployment, new_uids)

        _wait_for_gateway_auth_enforced()
        _poll_status(api_key, 200)
        log.info("Inference recovered after ExtProc rollout")

    def test_pre_processing_rollout_recovers_inference(self, api_key):
        """Rolling out the BBR pre-processor alone must not break inference."""
        self._assert_rollout_recovers(api_key, [PRE_PROCESSING_DEPLOYMENT])

    def test_processing_rollout_recovers_inference(self, api_key):
        """Rolling out the request ExtProc alone must not break inference."""
        self._assert_rollout_recovers(api_key, [PROCESSING_DEPLOYMENT])

    def test_both_extproc_rollout_recovers_inference(self, api_key):
        """Rolling out both ExtProc Deployments together must not break inference."""
        self._assert_rollout_recovers(
            api_key, [PROCESSING_DEPLOYMENT, PRE_PROCESSING_DEPLOYMENT]
        )
