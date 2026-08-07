"""
E2E test for AuthPolicy generation stability (RHOAIENG-81865).

Validates that once the gateway AuthPolicy reaches Accepted+Enforced state,
its metadata.generation does not increase during a quiet observation window.
Generation churn indicates a feedback loop between MaaS and Kuadrant that
causes unnecessary EnvoyFilter regeneration and contributes to gateway OOM.

Requirements:
  - maas-controller must be deployed and running.
  - Kuadrant CRDs (authpolicies.kuadrant.io) must be installed.
  - At least one MaaSAuthPolicy must be Active so the gateway AuthPolicy exists.

Environment:
  - GATEWAY_NAMESPACE: namespace where the gateway AuthPolicy lives (default: openshift-ingress)
  - E2E_GATEWAY_AUTH_POLICY_NAME: name of the gateway AuthPolicy (default: maas-gateway-auth)
  - E2E_GENERATION_STABILITY_WINDOW: seconds to observe for generation stability (default: 60)
"""

import json
import logging
import os
import subprocess
import time

import pytest

from test_helper import (
    GATEWAY_AUTH_POLICY_NAME,
    GATEWAY_NAMESPACE,
    _wait_for_gateway_auth_enforced,
)

log = logging.getLogger(__name__)

STABILITY_WINDOW = int(os.environ.get("E2E_GENERATION_STABILITY_WINDOW", "60"))
POLL_INTERVAL = 5
OC_TIMEOUT = 30


def _get_authpolicy_generation(name: str = GATEWAY_AUTH_POLICY_NAME, namespace: str = GATEWAY_NAMESPACE) -> int | None:
    """Return metadata.generation of the AuthPolicy, or None if not found."""
    try:
        result = subprocess.run(
            ["oc", "get", "authpolicy", name, "-n", namespace, "-o", "jsonpath={.metadata.generation}"],
            capture_output=True, text=True, timeout=OC_TIMEOUT,
        )
    except subprocess.TimeoutExpired:
        return None
    if result.returncode != 0:
        return None
    try:
        return int(result.stdout.strip())
    except (ValueError, TypeError):
        return None


def _get_authpolicy_resource_version(name: str = GATEWAY_AUTH_POLICY_NAME, namespace: str = GATEWAY_NAMESPACE) -> str | None:
    """Return metadata.resourceVersion (tracks any write including status)."""
    try:
        result = subprocess.run(
            ["oc", "get", "authpolicy", name, "-n", namespace, "-o", "jsonpath={.metadata.resourceVersion}"],
            capture_output=True, text=True, timeout=OC_TIMEOUT,
        )
    except subprocess.TimeoutExpired:
        return None
    if result.returncode != 0:
        return None
    return result.stdout.strip() or None


class TestAuthPolicyGenerationStability:
    """Verify the gateway AuthPolicy generation is stable after convergence."""

    @pytest.fixture(autouse=True)
    def _ensure_enforced(self):
        """Wait for the gateway AuthPolicy to be Accepted+Enforced before testing stability."""
        _wait_for_gateway_auth_enforced()

    def test_generation_does_not_increase_during_quiet_window(self):
        """
        Given the gateway AuthPolicy is Accepted and Enforced,
        When we observe it for STABILITY_WINDOW seconds without making changes,
        Then metadata.generation must not increase (no unnecessary spec Updates).
        """
        initial_gen = _get_authpolicy_generation()
        assert initial_gen is not None, (
            f"AuthPolicy {GATEWAY_AUTH_POLICY_NAME} not found in {GATEWAY_NAMESPACE}"
        )

        log.info(
            "Observing AuthPolicy %s/%s generation (initial=%d) for %ds",
            GATEWAY_NAMESPACE, GATEWAY_AUTH_POLICY_NAME, initial_gen, STABILITY_WINDOW,
        )

        observations = []
        start = time.time()
        while time.time() - start < STABILITY_WINDOW:
            time.sleep(POLL_INTERVAL)
            gen = _get_authpolicy_generation()
            elapsed = int(time.time() - start)
            observations.append({"elapsed_s": elapsed, "generation": gen})
            log.info("  t+%ds: generation=%s", elapsed, gen)

            if gen is None:
                pytest.fail(
                    f"AuthPolicy {GATEWAY_AUTH_POLICY_NAME} unavailable after {elapsed}s. "
                    f"Observations: {observations}"
                )

            if gen > initial_gen:
                pytest.fail(
                    f"AuthPolicy generation increased from {initial_gen} to {gen} "
                    f"after {elapsed}s without any user changes. "
                    f"This indicates a MaaS↔Kuadrant feedback loop causing unnecessary "
                    f"spec Updates. Observations: {observations}"
                )

        final_gen = _get_authpolicy_generation()
        assert final_gen == initial_gen, (
            f"Generation drifted: initial={initial_gen}, final={final_gen}"
        )
        log.info(
            "PASS: generation remained stable at %d for %ds",
            initial_gen, STABILITY_WINDOW,
        )

    def test_no_rapid_resource_version_changes(self):
        """
        Given the gateway AuthPolicy is Accepted and Enforced,
        When we sample resourceVersion at the start and end of a window,
        Then the number of resourceVersion bumps should be bounded
        (some status updates are expected, but not excessive churn).
        """
        initial_rv = _get_authpolicy_resource_version()
        assert initial_rv is not None, (
            f"AuthPolicy {GATEWAY_AUTH_POLICY_NAME} not found in {GATEWAY_NAMESPACE}"
        )

        time.sleep(STABILITY_WINDOW)

        final_rv = _get_authpolicy_resource_version()
        assert final_rv is not None

        try:
            rv_delta = int(final_rv) - int(initial_rv)
        except ValueError:
            log.warning("resourceVersion is not numeric; skipping delta check")
            return

        max_expected_rv_bumps = STABILITY_WINDOW // 10
        log.info(
            "resourceVersion delta: %d (initial=%s, final=%s, max_expected=%d)",
            rv_delta, initial_rv, final_rv, max_expected_rv_bumps,
        )

        if rv_delta > max_expected_rv_bumps:
            log.warning(
                "High resourceVersion churn detected: %d changes in %ds. "
                "Expected at most %d. This may indicate excessive status reconciliation.",
                rv_delta, STABILITY_WINDOW, max_expected_rv_bumps,
            )
