"""
E2E tests for the HTTP filter chain on MaaS gateways when the default tenant runs the
AIGC-owned praxis-extproc dataplane (not legacy Go IPP).

Mirrors ``test_gateway_filter_chain.py`` listener-order checks for praxis clusters.
``TestEndpointPickerTraffic`` is intentionally omitted: it provisions a dedicated
InferencePool on the shared default gateway and asserts EPP request logs — the same
dataplane ordering is already covered by ``test_tenant_model_inference`` and the legacy
IPP serial test; duplicating it here would add long-running cluster mutation with little
extra signal for praxis.
"""

from __future__ import annotations

import pytest

from multitenancy_helpers import (
    DEFAULT_GATEWAY_NAME,
    GATEWAY_NAMESPACE,
    extproc_deployment_uses_praxis,
    get_json_or_none,
)
from test_gateway_filter_chain import (
    ENVOY_FILTER_NAME,
    _assert_praxis_chain,
    _listener_state,
)

pytestmark = pytest.mark.xdist_group("readonly")


def _skip_unless_praxis_payload_processing():
    if get_json_or_none("envoyfilter", ENVOY_FILTER_NAME, GATEWAY_NAMESPACE) is None:
        pytest.skip(
            f"EnvoyFilter {GATEWAY_NAMESPACE}/{ENVOY_FILTER_NAME} is not present "
            "(praxis dataplane not installed)"
        )
    if not extproc_deployment_uses_praxis("payload-processing"):
        pytest.skip("default tenant payload-processing is not praxis-extproc")


class TestGatewayFilterChainPraxis:
    """Praxis payload-processing EnvoyFilter yields ipp-pre -> auth -> ipp -> router."""

    @pytest.fixture(scope="class")
    def gateway_state(self):
        _skip_unless_praxis_payload_processing()
        return _listener_state(DEFAULT_GATEWAY_NAME)

    def test_no_listener_rejected(self, gateway_state):
        for pod, (_, rejected) in gateway_state.items():
            assert not rejected, f"{pod}: Envoy rejected listener config: {rejected}"

    def test_payload_processing_precedes_router(self, gateway_state):
        for pod, (chains, _) in gateway_state.items():
            for listener, filters in chains:
                _assert_praxis_chain(pod, listener, filters)
