"""
E2E tests for the HTTP filter chain on MaaS gateways when the default tenant runs the
AIGC-owned praxis-extproc dataplane (not legacy Go IPP).

Mirrors ``test_gateway_filter_chain.py`` for praxis clusters: the listener order, and
body-routed traffic through an InferencePool reaching its endpoint picker. A bypassed
picker still answers 200 through plain pool load balancing, so only the picker's own
signals (its response header and request log) catch it.
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
    _check_endpoint_picker_traffic,
    _listener_state,
)

def _skip_unless_praxis_payload_processing():
    if get_json_or_none("envoyfilter", ENVOY_FILTER_NAME, GATEWAY_NAMESPACE) is None:
        pytest.skip(
            f"EnvoyFilter {GATEWAY_NAMESPACE}/{ENVOY_FILTER_NAME} is not present "
            "(praxis dataplane not installed)"
        )
    if not extproc_deployment_uses_praxis("payload-processing"):
        pytest.skip("default tenant payload-processing is not praxis-extproc")


@pytest.mark.xdist_group("readonly")
@pytest.mark.smoke
class TestGatewayFilterChainPraxis:
    """Praxis payload-processing EnvoyFilter yields ipp-pre -> auth -> ipp -> EPP -> router."""

    @pytest.fixture(scope="class")
    def gateway_state(self):
        _skip_unless_praxis_payload_processing()
        return _listener_state(DEFAULT_GATEWAY_NAME)

    def test_no_listener_rejected(self, gateway_state):
        for pod, (_, rejected) in gateway_state.items():
            assert not rejected, f"{pod}: Envoy rejected listener config: {rejected}"

    def test_epp_runs_after_payload_processing(self, gateway_state):
        for pod, (chains, _) in gateway_state.items():
            for listener, filters in chains:
                _assert_praxis_chain(pod, listener, filters)


@pytest.mark.serial
@pytest.mark.full_suite
@pytest.mark.xdist_group("praxis_filter_chain")
class TestEndpointPickerTrafficPraxis:
    """Body-routed requests to an InferencePool model reach its endpoint picker through
    praxis-extproc.

    Serial: the pool lives on the shared default gateway, as in the legacy IPP test.
    """

    def test_body_routed_requests_reach_the_endpoint_picker(self):
        _skip_unless_praxis_payload_processing()
        _check_endpoint_picker_traffic(_assert_praxis_chain)
