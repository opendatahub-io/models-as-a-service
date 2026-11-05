"""E2E for RHOAIENG-97470: switching a tenant from legacy IPP to Praxis cleans up
the legacy IPP operands and brings up praxis-extproc, leaving peers untouched.

Gated behind TENANT_EXTERNAL_MODEL_LEGACY_IPP_E2E (needs the legacy Go IPP image
pin); otherwise tenants come up on praxis directly with nothing to clean up.
"""

from __future__ import annotations

import logging
import time

import pytest
from multitenancy_helpers import (
    GATEWAY_NAMESPACE,
    PAYLOAD_PROCESSING_TYPE_IPP,
    PAYLOAD_PROCESSING_TYPE_PRAXIS,
    bootstrap_aitenant_tenant,
    cleanup_discovery_case,
    ensure_payload_processing_type_on_tenant_config,
    extproc_deployment_uses_praxis,
    get_json_or_none,
    ipp_tenant_id,
    new_named_tenant_case,
    per_tenant_ipp_names,
    require_aitenant_crd,
    require_tenant_namespace_discovery,
    wait_for_per_tenant_ipp_ready,
)
from tenant_external_model_helpers import require_legacy_ipp_coexistence_support

log = logging.getLogger(__name__)

pytestmark = [pytest.mark.xdist_group("tenant_ipp"), pytest.mark.full_suite]

SWITCH_RECONCILE_TIMEOUT = 300
POLL_INTERVAL = 5


def _wait_until_praxis(deployment_name, *, want=True, timeout=SWITCH_RECONCILE_TIMEOUT):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        last = extproc_deployment_uses_praxis(deployment_name)
        if last == want:
            return
        time.sleep(POLL_INTERVAL)
    raise AssertionError(
        f"{deployment_name} extproc_deployment_uses_praxis={last!r}, expected {want!r} within {timeout}s"
    )


def _deployment_image(name, namespace=GATEWAY_NAMESPACE):
    """Return the deployment template's first-container image, or None when the
    deployment is absent/unreadable (distinct from a readable empty image)."""
    obj = get_json_or_none("deployment", name, namespace)
    if obj is None:
        return None
    containers = (((obj.get("spec") or {}).get("template") or {}).get("spec") or {}).get("containers") or []
    if not containers:
        return None
    return containers[0].get("image")


def _deployment_available(name, namespace=GATEWAY_NAMESPACE):
    """True when the deployment exists and reports an Available replica."""
    obj = get_json_or_none("deployment", name, namespace)
    if obj is None:
        return False
    status = obj.get("status") or {}
    if status.get("availableReplicas", 0) < 1:
        return False
    return any(
        c.get("type") == "Available" and c.get("status") == "True"
        for c in status.get("conditions") or []
    )


def _wait_for_deployment_image(name, *, timeout=SWITCH_RECONCILE_TIMEOUT):
    """Poll until the deployment's image is readable, so async reconciliation isn't
    raced and a momentarily failed read isn't mistaken for a real image."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        image = _deployment_image(name)
        if image:
            return image
        time.sleep(POLL_INTERVAL)
    raise AssertionError(f"{name} image never became readable within {timeout}s")


def _wait_for_rollout_complete(name, namespace=GATEWAY_NAMESPACE, *, timeout=SWITCH_RECONCILE_TIMEOUT):
    """Wait until the current generation has fully rolled out and is available."""
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        obj = get_json_or_none("deployment", name, namespace)
        if obj is not None:
            spec = obj.get("spec") or {}
            status = obj.get("status") or {}
            last = status
            desired = spec.get("replicas", 1)
            if (
                obj.get("metadata", {}).get("generation") == status.get("observedGeneration")
                and status.get("updatedReplicas", 0) >= desired
                and status.get("availableReplicas", 0) >= desired
                and status.get("unavailableReplicas", 0) == 0
            ):
                return
        time.sleep(POLL_INTERVAL)
    raise AssertionError(f"{name} rollout did not complete within {timeout}s (status={last!r})")


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
            names = per_tenant_ipp_names(case["tenant_label_name"])
            # Poll for a readable image so async reconciliation isn't raced; only a
            # confirmed praxis image skips, never a momentarily failed read.
            _wait_for_deployment_image(names["processing_deployment"])
            if extproc_deployment_uses_praxis(names["processing_deployment"]):
                pytest.skip(
                    f"{names['processing_deployment']} came up on praxis-extproc; "
                    "legacy Go IPP image pin not active, nothing to clean up"
                )
        yield switch_case, sibling_case
    finally:
        cleanup_discovery_case(switch_case)
        cleanup_discovery_case(sibling_case)


class TestLegacyIppCleanupOnPraxisSwitch:
    """Switching a tenant from legacy IPP to Praxis cleans up its legacy IPP
    operands and brings up praxis-extproc, without touching peer tenants."""

    def test_switch_removes_legacy_ipp_and_brings_up_praxis(self, ipp_switch_cases):
        switch_case, sibling_case = ipp_switch_cases
        names = per_tenant_ipp_names(switch_case["tenant_label_name"])
        sibling = per_tenant_ipp_names(sibling_case["tenant_label_name"])

        assert ipp_tenant_id(switch_case["tenant_label_name"]) != ""
        assert ipp_tenant_id(sibling_case["tenant_label_name"]) != ""

        assert not extproc_deployment_uses_praxis(names["processing_deployment"]), (
            f"{names['processing_deployment']} must start on legacy Go IPP"
        )
        # Assert the pre-processing deployment exists AND is on the legacy image, so
        # the test actually exercises a legacy->Praxis transition for both operands.
        assert _deployment_image(names["pre_processing_deployment"]) is not None, (
            f"{names['pre_processing_deployment']} must exist with a readable image"
        )
        assert not extproc_deployment_uses_praxis(names["pre_processing_deployment"]), (
            f"{names['pre_processing_deployment']} must start on legacy Go IPP"
        )

        # Capture the default stack's pre-switch state to prove it is untouched later.
        default_names = ("payload-processing", "payload-pre-processing")
        default_before = {
            name: (_deployment_image(name), _deployment_available(name))
            for name in default_names
        }
        assert default_before["payload-processing"][0] is not None, (
            "default payload-processing deployment must exist before the switch"
        )

        ensure_payload_processing_type_on_tenant_config(
            switch_case["tenant_ns"], PAYLOAD_PROCESSING_TYPE_PRAXIS
        )
        log.info("Switched %s to praxis; waiting for cleanup + praxis reconcile", switch_case["tenant_label_name"])

        # Praxis reuses the legacy names, so the switch shows up as the deployments
        # flipping to praxis-extproc rather than the names disappearing. Shared-name
        # operands (e.g. the plugins ConfigMap) are adopted by ai-gateway-controller,
        # so cleanup leaves them intact; the image flip is the reliable signal.
        _wait_until_praxis(names["processing_deployment"])
        _wait_until_praxis(names["pre_processing_deployment"])
        # The image flip only confirms the template; wait for the new generation to
        # actually roll out so praxis-extproc is serving, not merely declared.
        _wait_for_rollout_complete(names["processing_deployment"])
        _wait_for_rollout_complete(names["pre_processing_deployment"])

        log.info("Asserting sibling %s stays legacy and default stack is untouched", sibling_case["tenant_label_name"])
        # A deleted sibling would false-pass the praxis check (read failure -> False),
        # so require it to still exist and be available on its legacy image.
        assert _deployment_available(sibling["processing_deployment"]), (
            f"sibling {sibling['processing_deployment']} must stay deployed and available"
        )
        assert not extproc_deployment_uses_praxis(sibling["processing_deployment"]), (
            f"sibling {sibling['processing_deployment']} must remain legacy Go IPP"
        )

        for name in default_names:
            before = default_before[name]
            after = (_deployment_image(name), _deployment_available(name))
            assert after == before, (
                f"default stack {name} changed across the tenant switch "
                f"(before={before}, after={after})"
            )
