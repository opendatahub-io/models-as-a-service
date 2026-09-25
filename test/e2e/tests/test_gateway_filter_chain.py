"""
E2E tests for the HTTP filter chain the payload-processing EnvoyFilter renders on MaaS
gateways, and for the InferencePool endpoint picker (EPP) that chain has to reach.

Istio's InferencePool filter (envoy.filters.http.ext_proc) binds the endpoint picker once,
from the route matched in its own decodeHeaders, and KServe pool routes match on
X-Gateway-Model-Name, which ipp-pre produces. The EPP therefore has to run behind ipp-pre,
auth and ipp. A wrong order still answers 200 through plain pool load balancing, so the
chain is read from the live Envoy config of every gateway replica, and traffic through a
pool is checked against the EPP's own log: every authenticated request reaches it, a
rejected one does not.

Environment Variables:
- E2E_EPP_POOL_TIMEOUT: Seconds to wait for the pool backend and for the first response
  picked by its EPP (default: 300)
- E2E_EPP_ROUTE_TIMEOUT: Seconds to wait until every gateway replica binds the pool's
  route to its EPP (default: 120)
"""

from __future__ import annotations

import json
import logging
import os
import uuid
from dataclasses import dataclass

import pytest

from multitenancy_helpers import (
    DEFAULT_GATEWAY_NAME,
    GATEWAY_NAMESPACE,
    _oc_run,
    deployment_log_snapshot,
    extproc_deployment_uses_praxis,
    get_json_or_none,
    list_json,
    response_summary,
    wait_for_llmisvc_backend_ready,
    wait_until,
)
from test_helper import (
    MODEL_NAMESPACE,
    _create_api_key,
    _create_llmis,
    _create_maas_model_ref,
    _create_sa_token,
    _create_test_auth_policy,
    _create_test_subscription,
    _delete_cr,
    _delete_sa,
    _gateway_url,
    _sa_to_user,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
    _wait_for_cr_absent,
    _wait_for_model_ready,
    chat,
)

pytestmark = pytest.mark.xdist_group("readonly")

ENVOY_FILTER_NAME = "payload-processing"
IPP_PRE = "envoy.filters.http.ext_proc.ipp-pre"
IPP = "envoy.filters.http.ext_proc.ipp"
EPP = "envoy.filters.http.ext_proc"
WASM = "envoy.filters.http.wasm"
WASMPLUGIN_PREFIX = "extensions.istio.io/wasmplugin/"
ROUTER = "envoy.filters.http.router"
MODEL_HEADER = "x-gateway-model-name"
# Set by the endpoint picker on responses it processed (Gateway API Inference Extension,
# response header handling; upstream calls it a debugging aid, llm-d's own tests rely on
# it). A 200 without it never reached the picker.
EPP_RESPONSE_HEADER = "x-went-into-resp-headers"
# Istio's per-route override on InferencePool routes (pilot/pkg/networking/core/route/route.go)
# turns on what the listener-level SKIP leaves off: the EPP needs the request's headers and
# body to pick an endpoint.
EPP_ROUTE_PROCESSING = {"request_header_mode": "SEND", "request_body_mode": "FULL_DUPLEX_STREAMED"}
# Logged by the EPP once per request whose headers reach it, at its default verbosity
# (-v 2; llm-d-router pkg/epp/handlers/server.go).
EPP_REQUEST_LOG = "EPP received request"
POOL_READY_TIMEOUT = int(os.environ.get("E2E_EPP_POOL_TIMEOUT", "300"))
ROUTE_BIND_TIMEOUT = int(os.environ.get("E2E_EPP_ROUTE_TIMEOUT", "120"))
PICKER_LOG_TIMEOUT = 60

log = logging.getLogger(__name__)

# Static filter Istio renders for an InferencePool (pilot/pkg/xds/filters/filters.go). The
# EnvoyFilter removes Istio's filter and inserts this copy in front of the router, so this
# pins MaaS's copy; it cannot see a change on Istio's side.
ISTIO_EPP_TYPED_CONFIG = {
    "@type": "type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor",
    "grpc_service": {"envoy_grpc": {"cluster_name": "dummy"}, "timeout": "10s"},
    "failure_mode_allow": True,
    "processing_mode": {"request_header_mode": "SKIP", "response_header_mode": "SKIP"},
    "message_timeout": "1000s",
    "metadata_options": {
        "forwarding_namespaces": {"untyped": ["envoy.lb"]},
        "receiving_namespaces": {"untyped": ["envoy.lb"]},
    },
}


def _gateway_pods(gateway_name: str) -> list[str]:
    pods = [
        p["metadata"]["name"]
        for p in list_json("pods", GATEWAY_NAMESPACE,
                           labels=f"gateway.networking.k8s.io/gateway-name={gateway_name}")
        if p.get("status", {}).get("phase") == "Running"
    ]
    assert pods, f"no Running pod for Gateway {GATEWAY_NAMESPACE}/{gateway_name}"
    return pods


def _config_dump(pod: str, resource: str) -> dict:
    result = _oc_run([
        "exec", "-n", GATEWAY_NAMESPACE, pod, "-c", "istio-proxy", "--",
        "pilot-agent", "request", "GET", f"config_dump?resource={resource}",
    ])
    assert result.returncode == 0, f"config_dump from {pod} failed: {result.stderr}"
    return json.loads(result.stdout)


def _walk(node, listener, chains, rejected):
    """Collect (listener, http_filters) from active listener state, and rejected listeners."""
    if isinstance(node, dict):
        if "error_state" in node:
            rejected.append((node.get("name", listener), node["error_state"].get("details", "")))
        if "filter_chains" in node and "name" in node:
            listener = node["name"]
        if "http_filters" in node:
            chains.append((listener, node["http_filters"]))
        for key, value in node.items():
            if key not in ("warming_state", "draining_state", "error_state"):
                _walk(value, listener, chains, rejected)
    elif isinstance(node, list):
        for value in node:
            _walk(value, listener, chains, rejected)


def _listener_state(gateway_name: str) -> dict[str, tuple[list, list]]:
    """Per gateway pod: (MaaS chains as (listener, http_filters), rejected listeners)."""
    state = {}
    for pod in _gateway_pods(gateway_name):
        chains, rejected = [], []
        _walk(_config_dump(pod, "dynamic_listeners"), "?", chains, rejected)
        maas_chains = [
            (listener, filters) for listener, filters in chains
            if any(f.get("name") in (IPP_PRE, IPP) for f in filters)
        ]
        assert maas_chains, (
            f"{pod}: no listener carries {IPP_PRE} / {IPP}; EnvoyFilter inserts did not match "
            f"(rejected listeners: {rejected})"
        )
        state[pod] = (maas_chains, rejected)
    return state


def _auth_index(names: list[str]) -> int:
    for i, name in enumerate(names):
        if name == WASM or name.startswith(WASMPLUGIN_PREFIX):
            return i
    return -1


def _assert_chain(pod: str, listener: str, filters: list[dict]):
    names = [f.get("name") for f in filters]
    for name in (IPP_PRE, IPP, EPP, ROUTER):
        assert names.count(name) == 1, f"{pod} {listener}: {name} x{names.count(name)} in {names}"
    order = [names.index(IPP_PRE), names.index(IPP), names.index(EPP), names.index(ROUTER)]
    auth = _auth_index(names)
    if auth >= 0:
        order.insert(1, auth)
    assert order == sorted(order), (
        f"{pod} {listener}: expected ipp-pre -> auth -> ipp -> {EPP} -> router, got {names}"
    )
    epp = next(f for f in filters if f.get("name") == EPP)
    assert epp.get("typed_config") == ISTIO_EPP_TYPED_CONFIG, (
        f"{pod} {listener}: {EPP} config differs from the copy of Istio's static InferencePool filter"
    )


@dataclass(frozen=True)
class PoolRoute:
    """A gateway replica's route to an InferencePool, as bound to its endpoint picker."""

    model: str
    processing_mode: dict


def _pool_routes(gateway_name: str, picker_host: str) -> dict[str, PoolRoute]:
    """Per gateway replica, the route bound to the given endpoint picker, once every replica
    has one; empty until then."""
    routes = {pod: _pod_pool_route(pod, picker_host) for pod in _gateway_pods(gateway_name)}
    return routes if all(routes.values()) else {}


def _pod_pool_route(pod: str, picker_host: str) -> PoolRoute | None:
    stack = [_config_dump(pod, "dynamic_route_configs")]
    while stack:
        node = stack.pop()
        if isinstance(node, list):
            stack.extend(node)
            continue
        if not isinstance(node, dict):
            continue
        overrides = ((node.get("typed_per_filter_config") or {}).get(EPP) or {}).get("overrides") or {}
        cluster = ((overrides.get("grpc_service") or {}).get("envoy_grpc") or {}).get("cluster_name", "")
        if picker_host in cluster:
            for header in (node.get("match") or {}).get("headers") or []:
                exact = (header.get("string_match") or {}).get("exact")
                if header.get("name", "").lower() == MODEL_HEADER and exact:
                    return PoolRoute(exact, overrides.get("processing_mode") or {})
        stack.extend(node.values())
    return None


def _picker_deployment(llmis: str) -> str:
    return f"{llmis}-kserve-router-scheduler"


def _picker_selector(llmis: str) -> str:
    """Label selector of the pool's endpoint picker pods."""
    deployment = get_json_or_none("deployment", _picker_deployment(llmis), MODEL_NAMESPACE) or {}
    labels = ((deployment.get("spec") or {}).get("selector") or {}).get("matchLabels") or {}
    assert labels, f"no endpoint picker deployment for {llmis}"
    return ",".join(f"{k}={v}" for k, v in labels.items())


def _picker_request_count(selector: str) -> int:
    """Requests the endpoint picker pods have taken in. A selector only returns the last
    10 lines per pod unless told otherwise."""
    result = _oc_run(["logs", "-n", MODEL_NAMESPACE, "-l", selector, "--tail=-1"], timeout=120)
    assert result.returncode == 0, f"endpoint picker logs: {result.stderr}"
    return result.stdout.count(EPP_REQUEST_LOG)


def _skip_unless_maas_payload_processing():
    if get_json_or_none("envoyfilter", ENVOY_FILTER_NAME, GATEWAY_NAMESPACE) is None:
        pytest.skip(f"EnvoyFilter {GATEWAY_NAMESPACE}/{ENVOY_FILTER_NAME} is not rendered by maas-controller here")
    if extproc_deployment_uses_praxis("payload-processing"):
        pytest.skip("default tenant runs praxis-extproc; ai-gateway-controller owns its EnvoyFilter")


def _log_pool_diagnostics(llmis: str):
    """Log the pool's state before cleanup deletes it."""
    obj = get_json_or_none("llminferenceservice", llmis, MODEL_NAMESPACE) or {}
    log.error("LLMInferenceService %s conditions: %s", llmis, (obj.get("status") or {}).get("conditions"))
    for kind in ("inferencepool", "httproute"):
        for item in list_json(kind, MODEL_NAMESPACE):
            if item["metadata"]["name"].startswith(llmis):
                log.error("%s %s status: %s", kind, item["metadata"]["name"], item.get("status"))
    log.error("scheduler logs:\n%s", deployment_log_snapshot(
        _picker_deployment(llmis), namespace=MODEL_NAMESPACE, since="10m"))


def _chat(api_key, model_name, stream=False):
    """Body-routed chat completion: the model is only named in the body."""
    headers = {"Content-Type": "application/json"}
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"
    return chat("Hello", f"{_gateway_url()}/v1", headers, model_name, stream=stream, max_tokens=5)


class TestGatewayFilterChain:
    """The payload-processing EnvoyFilter yields ipp-pre -> auth -> ipp -> EPP -> router."""

    @pytest.fixture(scope="class")
    def gateway_state(self):
        _skip_unless_maas_payload_processing()
        return _listener_state(DEFAULT_GATEWAY_NAME)

    def test_no_listener_rejected(self, gateway_state):
        for pod, (_, rejected) in gateway_state.items():
            assert not rejected, f"{pod}: Envoy rejected listener config: {rejected}"

    def test_epp_runs_after_payload_processing(self, gateway_state):
        for pod, (chains, _) in gateway_state.items():
            for listener, filters in chains:
                _assert_chain(pod, listener, filters)


@pytest.mark.serial
class TestEndpointPickerTraffic:
    """Body-routed requests to an InferencePool model reach its endpoint picker.

    Serial: the pool lives on the shared default gateway. Access is granted to a
    dedicated service account only, so no other caller sees the new subscription.
    """

    def test_body_routed_requests_reach_the_endpoint_picker(self):
        _skip_unless_maas_payload_processing()
        suffix = uuid.uuid4().hex[:8]
        llmis = f"e2e-epp-pool-{suffix}"
        policy = f"e2e-epp-pool-access-{suffix}"
        subscription = f"e2e-epp-pool-sub-{suffix}"
        sa_name = f"e2e-epp-pool-{suffix}"
        served_id = f"test/e2e-epp-pool-{suffix}"
        model = f"publishers/{MODEL_NAMESPACE}/models/{served_id}"
        picker_host = f"{llmis}-epp-service.{MODEL_NAMESPACE}."

        try:
            log.info("Creating InferencePool model %s/%s", MODEL_NAMESPACE, llmis)
            _create_llmis(llmis, MODEL_NAMESPACE, DEFAULT_GATEWAY_NAME, GATEWAY_NAMESPACE,
                          model_name=served_id, scheduler=True)
            wait_for_llmisvc_backend_ready(llmis, MODEL_NAMESPACE, DEFAULT_GATEWAY_NAME,
                                           timeout=POOL_READY_TIMEOUT)
            _create_maas_model_ref(llmis, MODEL_NAMESPACE, llmis)

            # A MaaSModelRef turns Ready only once an auth policy and subscription govern it.
            log.info("Granting service account %s access to %s", sa_name, llmis)
            sa_token = _create_sa_token(sa_name, duration="30m")
            sa_user = _sa_to_user(sa_name)
            _create_test_auth_policy(policy, llmis, users=[sa_user])
            _create_test_subscription(subscription, llmis, users=[sa_user], token_limit=100000)
            _wait_for_maas_auth_policy_phase(policy)
            _wait_for_maas_subscription_phase(subscription)
            _wait_for_model_ready(llmis, namespace=MODEL_NAMESPACE, timeout=120)
            api_key = _create_api_key(sa_token, name=f"e2e-epp-pool-{suffix}", subscription=subscription)

            # Config: every replica binds the model's route to its picker and sends the picker
            # the request, and the chain reaches it.
            log.info("Waiting for every %s replica to bind %s", DEFAULT_GATEWAY_NAME, picker_host)
            routes = wait_until(lambda: _pool_routes(DEFAULT_GATEWAY_NAME, picker_host), ROUTE_BIND_TIMEOUT,
                                f"not every {DEFAULT_GATEWAY_NAME} replica bound endpoint picker {picker_host}")
            for pod, route in routes.items():
                assert route.model == model, f"{pod}: picker-bound route matches model {route.model!r}, expected {model!r}"
                sent = {mode: route.processing_mode.get(mode) for mode in EPP_ROUTE_PROCESSING}
                assert sent == EPP_ROUTE_PROCESSING, f"{pod}: pool route does not send requests to the picker"
            for pod, (chains, rejected) in _listener_state(DEFAULT_GATEWAY_NAME).items():
                assert not rejected, f"{pod}: Envoy rejected listener config: {rejected}"
                for listener, filters in chains:
                    _assert_chain(pod, listener, filters)

            # Traffic: auth needs to propagate and the pool fails open until its picker serves,
            # so wait for the first picked response. With the EPP ahead of ipp-pre no response
            # ever carries the header and this times out.
            def picked():
                response = _chat(api_key, model)
                assert response.status_code == 200 and response.headers.get(EPP_RESPONSE_HEADER) == "true", (
                    f"{response_summary(response)} headers={dict(response.headers)}"
                )
                return response

            log.info("Waiting for the first body-routed response picked by the EPP")
            wait_until(picked, POOL_READY_TIMEOUT, "endpoint picker never answered")

            # Auth rejects ahead of the picker. The picker's response header proves nothing
            # here: Envoy sends local replies through every encoder filter, so the picker sees
            # the 401's headers without ever seeing the request. Its request log does: the
            # picker logs a request before the response goes out, so by the time the two
            # requests below have been answered and logged, a line for the rejected one sent
            # first would be in the log too.
            picker = _picker_selector(llmis)
            baseline = _picker_request_count(picker)
            rejected = _chat(None, model)
            assert rejected.status_code == 401, f"no credentials: {response_summary(rejected)}"

            response = _chat(api_key, model)
            assert response.status_code == 200, f"completion: {response_summary(response)}"
            assert response.json().get("choices"), f"completion without choices: {response_summary(response)}"
            assert response.headers.get(EPP_RESPONSE_HEADER) == "true", (
                f"completion skipped the endpoint picker: {dict(response.headers)}"
            )

            with _chat(api_key, model, stream=True) as streamed:
                assert streamed.status_code == 200, f"stream: {response_summary(streamed)}"
                assert streamed.headers.get(EPP_RESPONSE_HEADER) == "true", (
                    f"stream skipped the endpoint picker: {dict(streamed.headers)}"
                )
                events = [line for line in streamed.iter_lines(decode_unicode=True) if line]
            assert events and events[-1] == "data: [DONE]", f"stream did not finish: {events[-3:]}"
            assert any(e.startswith("data: {") for e in events[:-1]), f"stream carried no chunks: {events}"

            authenticated = 2  # the plain and the streamed completion

            def logged_authenticated():
                seen = _picker_request_count(picker) - baseline
                assert seen >= authenticated, f"endpoint picker logged {seen} of {authenticated} authenticated requests"
                return seen

            seen = wait_until(logged_authenticated, PICKER_LOG_TIMEOUT,
                              "endpoint picker never logged the authenticated requests")
            assert seen == authenticated, (
                f"endpoint picker logged {seen} requests for {authenticated} authenticated and 1 rejected"
            )
        except Exception:
            _log_pool_diagnostics(llmis)
            raise
        finally:
            _delete_cr("maassubscription", subscription)
            _delete_cr("maasauthpolicy", policy)
            _delete_cr("maasmodelref", llmis, namespace=MODEL_NAMESPACE)
            _delete_cr("llminferenceservice", llmis, namespace=MODEL_NAMESPACE)
            _delete_sa(sa_name)
            _wait_for_cr_absent("maassubscription", subscription)
            _wait_for_cr_absent("maasauthpolicy", policy)
            _wait_for_cr_absent("maasmodelref", llmis, namespace=MODEL_NAMESPACE)
            _wait_for_cr_absent("llminferenceservice", llmis, namespace=MODEL_NAMESPACE, timeout=180)
