"""
Payload fidelity through a pool-backed echo model on a tenant served by the IPP.

maas-controller renders the payload-processing EnvoyFilter for tenants on the IPP
(payload-processing-type: ipp). On Envoy 1.36 (OCP 4.22) a response streamed through both
the endpoint picker (EPP) and ext_proc.ipp can end early and still arrive as a 200
(envoy#47449). With no response plugin in the tenant's profile, the product default,
maas-controller takes ext_proc.ipp off the response path (SKIP/NONE/SKIP).

The module checks that render, then judges by payload the request shapes that fail when
ipp streams the response: non-streaming completions over one EPP chunk (62,000 bytes) and
short streams. Those fail intermittently, so each test sends enough requests to catch them.
The last test turns a response plugin on in the tenant's plugins ConfigMap and checks that
the EnvoyFilter follows.

Environment Variables:
- E2E_EPP_POOL_TIMEOUT: Seconds to wait for the first response picked by the pool's EPP
  (default: 300)
- E2E_EPP_ROUTE_TIMEOUT: Seconds to wait until every gateway replica binds the pool's
  route to its EPP (default: 120)
"""

from __future__ import annotations

import json
import logging
import re
from dataclasses import dataclass

import pytest
from multitenancy_helpers import (
    GATEWAY_NAMESPACE,
    PAYLOAD_PROCESSING_TYPE_IPP,
    _oc_run,
    bootstrap_aitenant_tenant,
    cleanup_discovery_case,
    create_api_key_at,
    delete_maas_auth_policy,
    delete_maas_subscription,
    get_json_or_none,
    make_tenant_model_accessible,
    new_named_tenant_case,
    redact_sensitive,
    require_aitenant_crd,
    require_tenant_namespace_discovery,
    wait_for_llmisvc_backend_ready,
    wait_for_per_tenant_ipp_ready,
    wait_until,
)
from test_gateway_filter_chain import (
    IPP,
    POOL_READY_TIMEOUT,
    ROUTE_BIND_TIMEOUT,
    _log_pool_diagnostics,
    _pool_routes,
)
from test_helper import (
    _create_llmis,
    _create_maas_model_ref,
    _delete_cr,
    _get_cluster_token,
    _wait_for_cr_absent,
)
from test_pool_payload_fidelity import (
    LARGE_PROMPT_BYTES,
    SMALL_PROMPT_BYTES,
    TOKEN_BUDGET,
    PoolEcho,
    _prompt,
    _round_trip,
)

log = logging.getLogger(__name__)

pytestmark = [
    pytest.mark.full_suite,
    pytest.mark.serial,
    pytest.mark.legacy_ipp_pass2b,
    # Same lane as the other modules that create a pool-backed model, so two pools never
    # coexist even with several serial workers.
    pytest.mark.xdist_group("inference_priority"),
]

# On Envoy 1.36 with ipp streaming the response, about 8% of 470 KB completions and 20 to
# 27% of 2 KB streams failed (maas-ipp-response-skip spike). At those rates 60 and 30
# requests miss the fault on main less than 1% of the time.
LARGE_COMPLETIONS = 60
SHORT_STREAMS = 30
IPP_RESPONSE_OFF = {"response_header_mode": "SKIP", "response_body_mode": "NONE", "response_trailer_mode": "SKIP"}
IPP_RESPONSE_ON = {
    "response_header_mode": "SEND",
    "response_body_mode": "FULL_DUPLEX_STREAMED",
    "response_trailer_mode": "SEND",
}
IPP_CONFIG_KEY = "custom-ipp-config.yaml"
# The IPP reads its plugins at startup; the EnvoyFilter follows the ConfigMap on the
# controller's next reconcile.
RENDER_TIMEOUT = 120


@dataclass(frozen=True)
class TenantPool:
    """A tenant on the IPP, its pool-backed echo model and its payload-processing objects."""

    namespace: str
    envoyfilter: str
    plugins_configmap: str
    echo: PoolEcho


def _tenant_gateway_url(gateway_name: str) -> str:
    result = _oc_run(["get", "route", f"{gateway_name}-route", "-n", GATEWAY_NAMESPACE, "-o", "json"])
    if result.returncode != 0:
        raise RuntimeError(f"Failed to get route for gateway {gateway_name}: {result.stderr.strip()}")
    return f"https://{json.loads(result.stdout)['spec']['host']}"


def _ipp_response_modes(envoyfilter: str) -> list[dict[str, str]]:
    """The response modes of every ext_proc.ipp insert in the tenant's EnvoyFilter."""
    obj = get_json_or_none("envoyfilter", envoyfilter, GATEWAY_NAMESPACE) or {}
    modes = []
    for patch in (obj.get("spec") or {}).get("configPatches") or []:
        value = (patch.get("patch") or {}).get("value") or {}
        if value.get("name") == IPP:
            mode = (value.get("typed_config") or {}).get("processing_mode") or {}
            modes.append({k: mode.get(k) for k in IPP_RESPONSE_OFF})
    return modes


def _renders(envoyfilter: str, want: dict[str, str]) -> bool:
    modes = _ipp_response_modes(envoyfilter)
    return bool(modes) and all(mode == want for mode in modes)


def _set_ipp_config(configmap: str, config: str) -> None:
    patch = json.dumps({"data": {IPP_CONFIG_KEY: config}})
    result = _oc_run(["patch", "configmap", configmap, "-n", GATEWAY_NAMESPACE, "--type=merge", "-p", patch])
    if result.returncode != 0:
        raise RuntimeError(f"Failed to patch ConfigMap {configmap}: {result.stderr.strip()}")


def _failures(tenant: TenantPool, prompts: list[str], stream: bool) -> list[str]:
    """Send every prompt and return why each one that did not come back whole failed."""
    problems = []
    for i, prompt in enumerate(prompts):
        problem = _round_trip(tenant.echo, prompt, stream)
        if problem:
            problems.append(f"request {i}: {tenant.echo.redact(problem)}")
    if problems:
        _log_pool_diagnostics(tenant.echo.llmis, namespace=tenant.namespace)
    return problems


@pytest.fixture(scope="module")
def tenant_pool():
    """A tenant on the IPP with a pool-backed echo model on its own gateway."""
    require_tenant_namespace_discovery()
    require_aitenant_crd()
    case = new_named_tenant_case("e2e-ipp-echo")
    tenant_ns = case["tenant_ns"]
    llmis = f"e2e-ipp-echo-{case['suffix']}"
    served_id = f"test/{llmis}"
    model = f"publishers/{tenant_ns}/models/{served_id}"
    picker_host = f"{llmis}-epp-service.{tenant_ns}."
    policy = f"{llmis}-access"
    subscription = f"{llmis}-sub"

    try:
        bootstrap_aitenant_tenant(case, payload_processing_type=PAYLOAD_PROCESSING_TYPE_IPP)
        ipp = wait_for_per_tenant_ipp_ready(case)

        log.info("Creating pool-backed echo model %s/%s on gateway %s", tenant_ns, llmis, case["gateway_name"])
        _create_llmis(
            llmis, tenant_ns, case["gateway_name"], GATEWAY_NAMESPACE,
            model_name=served_id, scheduler=True, echo=True,
            # KServe gives every service the same route rule names, and istiod before
            # 1.29.9 binds InferencePool pickers by rule name, so another pool could take
            # over this model's routes.
            spec_annotations={"serving.kserve.io/route-rule-naming": "unique"},
        )
        wait_for_llmisvc_backend_ready(llmis, tenant_ns, case["gateway_name"])
        _create_maas_model_ref(llmis, tenant_ns, llmis)
        make_tenant_model_accessible(
            llmis, tenant_ns, policy, subscription,
            token_limit=TOKEN_BUDGET, gateway_name=case["gateway_name"],
        )

        gateway_url = _tenant_gateway_url(case["gateway_name"])
        key = create_api_key_at(f"{gateway_url}/maas-api", _get_cluster_token(), f"e2e-ipp-echo-{case['suffix']}",
                                subscription=subscription)
        assert key.status_code in (200, 201), f"tenant API key: {key.status_code} {redact_sensitive(key.text)}"
        echo = PoolEcho(llmis, model, key.json()["key"], v1_url=f"{gateway_url}/v1")

        # Every replica binds the body-routed rule for this model to its own picker.
        routes = wait_until(lambda: _pool_routes(case["gateway_name"], picker_host), ROUTE_BIND_TIMEOUT,
                            f"not every {case['gateway_name']} replica bound endpoint picker {picker_host}")
        for pod, route in routes.items():
            assert route.model == model, f"{pod}: picker-bound route matches model {route.model!r}, expected {model!r}"

        # Auth needs to propagate and the pool fails open until its picker serves.
        def echoed():
            problem = _round_trip(echo, "tenant pool echo ready")
            assert problem is None, echo.redact(problem)
            return True

        wait_until(echoed, POOL_READY_TIMEOUT, "tenant pool echo model never answered through its endpoint picker")
        yield TenantPool(tenant_ns, ipp["envoyfilter"], ipp["plugins_configmap"], echo)
    except Exception:
        _log_pool_diagnostics(llmis, namespace=tenant_ns)
        raise
    finally:
        delete_maas_subscription(subscription, tenant_ns)
        delete_maas_auth_policy(policy, tenant_ns)
        _delete_cr("maasmodelref", llmis, namespace=tenant_ns)
        _delete_cr("llminferenceservice", llmis, namespace=tenant_ns)
        _wait_for_cr_absent("maasmodelref", llmis, namespace=tenant_ns)
        # LLMIS teardown can lag the default 30s CR-absent wait under parallel load.
        _wait_for_cr_absent("llminferenceservice", llmis, namespace=tenant_ns, timeout=180)
        cleanup_discovery_case(case)


class TestTenantIPPPayloadFidelity:
    """Responses from a pool-backed model reach the client whole on a tenant served by the IPP."""

    def test_ipp_is_off_the_response_path(self, tenant_pool):
        """The product default profile has no response plugin, so ipp skips the response."""
        assert _renders(tenant_pool.envoyfilter, IPP_RESPONSE_OFF), (
            f"{tenant_pool.envoyfilter}: ipp response modes {_ipp_response_modes(tenant_pool.envoyfilter)}, "
            f"expected {IPP_RESPONSE_OFF}"
        )

    def test_large_completions_round_trip(self, tenant_pool):
        """470 KB non-streaming completions, several EPP chunks each, come back byte for byte."""
        prompts = [_prompt(LARGE_PROMPT_BYTES, seed) for seed in range(LARGE_COMPLETIONS)]
        problems = _failures(tenant_pool, prompts, stream=False)
        assert not problems, f"{len(problems)} of {LARGE_COMPLETIONS} failed: {problems[:3]}"

    def test_short_streams_round_trip(self, tenant_pool):
        """2 KB streams come back byte for byte, ending with [DONE]."""
        prompts = [_prompt(SMALL_PROMPT_BYTES, seed) for seed in range(SHORT_STREAMS)]
        problems = _failures(tenant_pool, prompts, stream=True)
        assert not problems, f"{len(problems)} of {SHORT_STREAMS} failed: {problems[:3]}"

    def test_response_plugin_turns_ipp_response_on(self, tenant_pool):
        """A response plugin in the profile puts ipp back on the response path, and removing it
        takes ipp off again. Runs last: each change rebuilds the gateway listeners."""
        configmap = get_json_or_none("configmap", tenant_pool.plugins_configmap, GATEWAY_NAMESPACE)
        assert configmap, f"ConfigMap {tenant_pool.plugins_configmap} not found"
        original = configmap["data"][IPP_CONFIG_KEY]
        with_response, edits = re.subn(r"^(\s*)response: \[\]$", r"\1response:\n\1- pluginRef: api-translation",
                                       original, flags=re.MULTILINE)
        assert edits == 1, f"expected one empty response list in {tenant_pool.plugins_configmap}:\n{original}"

        _set_ipp_config(tenant_pool.plugins_configmap, with_response)
        try:
            wait_until(lambda: _renders(tenant_pool.envoyfilter, IPP_RESPONSE_ON), RENDER_TIMEOUT,
                       f"{tenant_pool.envoyfilter} kept ipp off the response path with a response plugin")
        finally:
            _set_ipp_config(tenant_pool.plugins_configmap, original)
        wait_until(lambda: _renders(tenant_pool.envoyfilter, IPP_RESPONSE_OFF), RENDER_TIMEOUT,
                   f"{tenant_pool.envoyfilter} kept ipp on the response path after the plugin was removed")
