"""E2E for RHOAIENG-97470: switching a tenant from legacy IPP to Praxis cleans up
the legacy IPP operands and brings up praxis-extproc, leaving peers untouched.

Gated behind TENANT_EXTERNAL_MODEL_LEGACY_IPP_E2E (needs the legacy Go IPP image
pin); otherwise tenants come up on praxis directly with nothing to clean up.
"""

from __future__ import annotations

import logging

import pytest
from multitenancy_helpers import (
    GATEWAY_NAMESPACE,
    PAYLOAD_PROCESSING_TYPE_IPP,
    PAYLOAD_PROCESSING_TYPE_PRAXIS,
    bootstrap_aitenant_tenant,
    cleanup_discovery_case,
    ensure_payload_processing_type_on_tenant_config,
    extproc_deployment_uses_praxis,
    ipp_tenant_id,
    new_named_tenant_case,
    per_tenant_ipp_names,
    require_aitenant_crd,
    require_tenant_namespace_discovery,
    wait_for_deployment_available,
    wait_for_json,
    wait_for_per_tenant_ipp_ready,
    wait_until,
)
from tenant_external_model_helpers import _deployment_image, require_legacy_ipp_coexistence_support

log = logging.getLogger(__name__)

pytestmark = [pytest.mark.xdist_group("tenant_ipp"), pytest.mark.full_suite]

SWITCH_RECONCILE_TIMEOUT = 300
POLL_INTERVAL = 5


def _wait_until_praxis(deployment_name, *, want=True, timeout=SWITCH_RECONCILE_TIMEOUT):
    """Wait until the deployment's extproc image flips to (or from) praxis-extproc."""
    wait_until(
        lambda: extproc_deployment_uses_praxis(deployment_name) == want,
        timeout,
        f"{deployment_name} extproc_deployment_uses_praxis == {want}",
        interval=POLL_INTERVAL,
    )


def _wait_for_rollout_complete(name, namespace=GATEWAY_NAMESPACE, *, timeout=SWITCH_RECONCILE_TIMEOUT):
    """Wait until the deployment's current generation has fully rolled out with a
    positive desired replica count (a scaled-to-zero deployment is not "rolled out",
    so the image flip alone would otherwise pass without a serving praxis pod)."""

    def _rolled_out(obj: dict) -> bool:
        spec = obj.get("spec") or {}
        status = obj.get("status") or {}
        desired = spec.get("replicas", 1)
        return (
            desired > 0
            and obj.get("metadata", {}).get("generation") == status.get("observedGeneration")
            and status.get("updatedReplicas", 0) >= desired
            and status.get("availableReplicas", 0) >= desired
            and status.get("unavailableReplicas", 0) == 0
        )

    wait_for_json("deployment", name, namespace, predicate=_rolled_out, timeout=timeout, interval=POLL_INTERVAL)


@pytest.fixture(scope="module")
def ipp_switch_cases():
    require_legacy_ipp_coexistence_support()
    require_tenant_namespace_discovery()
    require_aitenant_crd()
    switch_case = new_named_tenant_case("e2e-ipp-switch")
    sibling_case = new_named_tenant_case("e2e-ipp-keep")
    try:
        for case in (switch_case, sibling_case):
            log.info("Bootstrapping tenant %s on legacy Go IPP", case["tenant_label_name"])
            bootstrap_aitenant_tenant(case, payload_processing_type=PAYLOAD_PROCESSING_TYPE_IPP)
            wait_for_per_tenant_ipp_ready(case)
            processing = per_tenant_ipp_names(case["tenant_label_name"])["processing_deployment"]
            # Wait for a readable image so async reconciliation isn't raced.
            wait_until(
                lambda name=processing: _deployment_image(name, GATEWAY_NAMESPACE),
                SWITCH_RECONCILE_TIMEOUT,
                f"{processing} image readable",
                interval=POLL_INTERVAL,
            )
            # We bootstrapped with legacy IPP; coming up on praxis means the legacy image
            # pin is not active. That is a setup failure for this (opt-in) suite, not a skip.
            assert not extproc_deployment_uses_praxis(processing), (
                f"{processing} came up on praxis-extproc despite a legacy-IPP bootstrap; "
                "the legacy Go IPP image pin is not active"
            )
        yield switch_case, sibling_case
    finally:
        # Clean up both tenants even if the first raises, so the sibling isn't left on
        # the live cluster; re-raise the first error.
        errors = []
        for case in (switch_case, sibling_case):
            try:
                cleanup_discovery_case(case)
            except Exception as exc:  # noqa: BLE001 - best-effort cleanup, first error re-raised
                errors.append(exc)
        if errors:
            raise errors[0]


class TestLegacyIppCleanupOnPraxisSwitch:
    """Switching a tenant from legacy IPP to Praxis cleans up its legacy IPP
    operands and brings up praxis-extproc, without touching peer tenants."""

    def test_switch_removes_legacy_ipp_and_brings_up_praxis(self, ipp_switch_cases):
        switch_case, sibling_case = ipp_switch_cases
        names = per_tenant_ipp_names(switch_case["tenant_label_name"])
        sibling = per_tenant_ipp_names(sibling_case["tenant_label_name"])

        assert ipp_tenant_id(switch_case["tenant_label_name"]) != ""
        assert ipp_tenant_id(sibling_case["tenant_label_name"]) != ""

        # The fixture already guaranteed the processing deployment starts on legacy IPP.
        # It does not cover the pre-processing deployment, so assert that one starts
        # legacy too, so the switch exercises a legacy->Praxis transition for both operands.
        assert _deployment_image(names["pre_processing_deployment"], GATEWAY_NAMESPACE), (
            f"{names['pre_processing_deployment']} must exist with a readable image"
        )
        assert not extproc_deployment_uses_praxis(names["pre_processing_deployment"]), (
            f"{names['pre_processing_deployment']} must start on legacy Go IPP"
        )

        # Capture the default stack's pre-switch images to prove they are untouched later.
        default_names = ("payload-processing", "payload-pre-processing")
        default_before = {name: _deployment_image(name, GATEWAY_NAMESPACE) for name in default_names}
        assert default_before["payload-processing"], (
            "default payload-processing deployment must exist before the switch"
        )

        ensure_payload_processing_type_on_tenant_config(
            switch_case["tenant_ns"], PAYLOAD_PROCESSING_TYPE_PRAXIS
        )
        log.info("Switched %s to praxis; waiting for cleanup + praxis reconcile", switch_case["tenant_label_name"])

        # Praxis reuses the legacy names, so the switch shows up as the deployments
        # flipping to praxis-extproc rather than the names disappearing. Shared-name
        # operands (e.g. the plugins ConfigMap) are adopted by ai-gateway-controller,
        # so cleanup leaves them intact; the image flip + rollout is the reliable signal.
        _wait_until_praxis(names["processing_deployment"])
        _wait_until_praxis(names["pre_processing_deployment"])
        _wait_for_rollout_complete(names["processing_deployment"])
        _wait_for_rollout_complete(names["pre_processing_deployment"])

        log.info("Asserting sibling %s stays legacy and default stack is untouched", sibling_case["tenant_label_name"])
        # A deleted sibling would false-pass the praxis check (read failure -> False),
        # so require it to still exist and be available before checking it stayed legacy.
        wait_for_deployment_available(sibling["processing_deployment"], GATEWAY_NAMESPACE)
        assert not extproc_deployment_uses_praxis(sibling["processing_deployment"]), (
            f"sibling {sibling['processing_deployment']} must remain legacy Go IPP"
        )

        for name in default_names:
            after = _deployment_image(name, GATEWAY_NAMESPACE)
            assert after == default_before[name], (
                f"default stack {name} image changed across the tenant switch "
                f"(before={default_before[name]!r}, after={after!r})"
            )
            if default_before[name]:  # existed before the switch -> must still be available
                wait_for_deployment_available(name, GATEWAY_NAMESPACE)
