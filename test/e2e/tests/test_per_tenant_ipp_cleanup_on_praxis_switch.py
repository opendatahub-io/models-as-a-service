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

pytestmark = pytest.mark.xdist_group("tenant_ipp")

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
        assert get_json_or_none("deployment", names["pre_processing_deployment"], GATEWAY_NAMESPACE) is not None

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

        log.info("Asserting sibling %s stays legacy and default stack is untouched", sibling_case["tenant_label_name"])
        assert not extproc_deployment_uses_praxis(sibling["processing_deployment"]), (
            f"sibling {sibling['processing_deployment']} must remain legacy Go IPP"
        )
        assert get_json_or_none("deployment", "payload-processing", GATEWAY_NAMESPACE) is not None
