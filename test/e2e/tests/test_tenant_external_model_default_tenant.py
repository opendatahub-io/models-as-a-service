"""E2E: external models outside tenant namespaces, served by the default tenant.

RHOAIENG-101754 (3.5 → 3.6 compatibility). In 3.5 the default tenant served
ExternalModels in every namespace. With ai-gateway-controller (AGC) on the
Praxis dataplane, an ExternalModel without gatewayRefs in a namespace that has
no MaasTenantConfig and that no AITenant claims is served by the default
tenant (AITenant models-as-a-service) as an implicit attachment to its
Gateway: the same hashed HTTPRoute, DestinationRule and credential copy as an
explicit gatewayRefs attachment, with spec unchanged.

The models live in MODEL_NAMESPACE (llm): the namespace 3.5 demos used, and
one the default Gateway's allowedRoutes already admits (a fresh user
namespace would need the default Gateway patched). The stack is 3.5-shaped:
the provider uses ``auth.type: simple``, which the AGC serves as ``apikey``.
The provider endpoint is a header-echo service (default httpbingo.org): under
``/anything/<tag>`` it echoes the request, so a response proves which binding
served it and which credential was injected.

Scenarios:
- The model and its provider reach Ready and serve inference through the
  default Gateway, with the credential delivered as a copy into the default
  tenant namespace.
- gatewayRefs replace the implicit attachment and remove its objects, without
  falling back to the default Gateway; removing them restores it.
- Deleting the model removes the implicit attachment's objects.

Skips unless the default tenant runs praxis-extproc (installed by the AGC): a
default tenant on IPP serves these models itself and the AGC leaves them alone.

Environment variables:
- E2E_EXTERNAL_ENDPOINT: header-echo endpoint FQDN (default: httpbingo.org)
- E2E_TENANT_EXTERNAL_MODEL_READY_TIMEOUT: wait for AGC reconciliation (default: 300)
"""

from __future__ import annotations

import base64
import json
import logging
import os
import re
import uuid

import pytest
import requests
from multitenancy_helpers import (
    DEFAULT_GATEWAY_NAME,
    GATEWAY_NAMESPACE,
    _oc_run,
    extproc_deployment_uses_praxis,
    get_json_or_none,
    list_json,
    redact_sensitive,
    wait_until,
)
from test_helper import (
    MODEL_NAMESPACE,
    _apply_cr,
    _create_api_key,
    _create_test_auth_policy,
    _create_test_subscription,
    _delete_crs_and_wait,
    _get_cluster_token,
    _get_cr,
    _ns,
    _request_with_gateway_retry,
    _wait_for_httproute_accepted,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
    _wait_for_model_ready,
    _wait_for_subscription_trlp_status,
)

log = logging.getLogger(__name__)

# Serialized with tenant_ipp, which runs legacy IPP tenant stacks: those must
# not run next to this suite's models.
pytestmark = pytest.mark.xdist_group("tenant_ipp")

EXTERNAL_MODEL_KIND = "externalmodels.inference.opendatahub.io"
EXTERNAL_PROVIDER_KIND = "externalproviders.inference.opendatahub.io"
INFERENCE_API_VERSION = "inference.opendatahub.io/v1alpha1"

EXTERNAL_ENDPOINT = os.environ.get("E2E_EXTERNAL_ENDPOINT", "httpbingo.org")
READY_TIMEOUT = int(os.environ.get("E2E_TENANT_EXTERNAL_MODEL_READY_TIMEOUT", "300"))

# A Gateway no AITenant manages (it need not exist): an explicit binding to it
# is refused with GatewayNotManaged.
UNMANAGED_GATEWAY_NAME = "e2e-dft-unmanaged-gateway"

# AGC labels on the Gateway-namespace provider DestinationRules.
LABEL_EXTERNAL_PROVIDER = "inference.opendatahub.io/external-provider"
LABEL_EXTERNAL_GATEWAY = "inference.opendatahub.io/external-gateway"


@pytest.fixture(scope="module", autouse=True)
def _require_praxis_default_tenant():
    if not extproc_deployment_uses_praxis("payload-processing", GATEWAY_NAMESPACE):
        pytest.skip(
            "default tenant payload-processing is not praxis-extproc; its IPP serves "
            "models without gatewayRefs and the AGC leaves them alone"
        )


@pytest.fixture
def resources():
    """(kind, name, namespace) objects deleted in reverse creation order."""
    tracked: list[tuple[str, str, str]] = []
    try:
        yield tracked
    finally:
        _delete_crs_and_wait(reversed(tracked))


def _provision_model(tracked: list, prefix: str) -> dict:
    """A 3.5-shaped stack in MODEL_NAMESPACE: credential Secret, provider with
    ``auth.type: simple``, ExternalModel without gatewayRefs and its MaaSModelRef."""
    suffix = uuid.uuid4().hex[:6]
    name = f"{prefix}-{suffix}"
    model = {
        "name": name,
        "namespace": MODEL_NAMESPACE,
        "provider": f"{name}-provider",
        "secret_name": f"{name}-cred",
        "credential": f"sk-extmod-{suffix}",
        "path_tag": f"{name}-{uuid.uuid4().hex[:6]}",
    }
    _apply_cr(
        {
            "apiVersion": "v1",
            "kind": "Secret",
            "metadata": {"name": model["secret_name"], "namespace": MODEL_NAMESPACE},
            "type": "Opaque",
            "stringData": {"api-key": model["credential"]},
        }
    )
    tracked.append(("secret", model["secret_name"], MODEL_NAMESPACE))
    _apply_cr(
        {
            "apiVersion": INFERENCE_API_VERSION,
            "kind": "ExternalProvider",
            "metadata": {"name": model["provider"], "namespace": MODEL_NAMESPACE},
            "spec": {
                "provider": "openai",
                "endpoint": EXTERNAL_ENDPOINT,
                "auth": {"type": "simple", "secretRef": {"name": model["secret_name"]}},
            },
        }
    )
    tracked.append((EXTERNAL_PROVIDER_KIND, model["provider"], MODEL_NAMESPACE))
    _apply_cr(
        {
            "apiVersion": INFERENCE_API_VERSION,
            "kind": "ExternalModel",
            "metadata": {"name": name, "namespace": MODEL_NAMESPACE},
            "spec": {
                "externalProviderRefs": [
                    {
                        "ref": {"name": model["provider"]},
                        "targetModel": "gpt-3.5-turbo",
                        "apiFormat": "openai-chat",
                        "path": f"/anything/{model['path_tag']}",
                    }
                ],
            },
        }
    )
    tracked.append((EXTERNAL_MODEL_KIND, name, MODEL_NAMESPACE))
    _apply_cr(
        {
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSModelRef",
            "metadata": {"name": name, "namespace": MODEL_NAMESPACE},
            "spec": {"modelRef": {"kind": "ExternalModel", "name": name}},
        }
    )
    tracked.append(("maasmodelref", name, MODEL_NAMESPACE))
    return model


def _patch_gateway_refs(model: dict, gateway_refs: list[dict] | None) -> None:
    result = _oc_run(
        [
            "patch",
            EXTERNAL_MODEL_KIND,
            model["name"],
            "-n",
            model["namespace"],
            "--type=merge",
            "-p",
            json.dumps({"spec": {"gatewayRefs": gateway_refs}}),
        ]
    )
    if result.returncode != 0:
        raise RuntimeError(f"failed to patch gatewayRefs on {model['name']}: {result.stderr.strip()}")


# ─── Attachment state ───────────────────────────────────────────────────────


def _gateway_result(model: dict, gateway_name: str) -> dict | None:
    """The model's status.gateways result for one Gateway."""
    external_model = get_json_or_none(EXTERNAL_MODEL_KIND, model["name"], model["namespace"]) or {}
    for entry in (external_model.get("status") or {}).get("gateways") or []:
        if entry.get("name") == gateway_name and entry.get("namespace") == GATEWAY_NAMESPACE:
            return entry
    return None


def _wait_gateway_result(model: dict, gateway_name: str, *, reason: str | None = None) -> dict:
    """Wait for a Ready result for one Gateway, or a not-Ready one with *reason*."""

    def _check() -> dict:
        entry = _gateway_result(model, gateway_name)
        ready = next((c for c in (entry or {}).get("conditions") or [] if c.get("type") == "Ready"), {})
        if reason is None:
            assert ready.get("status") == "True", f"no Ready result for {gateway_name}: {redact_sensitive(entry)}"
        else:
            assert ready.get("status") == "False" and ready.get("reason") == reason, (
                f"no {reason} result for {gateway_name}: {redact_sensitive(entry)}"
            )
        return entry

    return wait_until(_check, READY_TIMEOUT, f"{model['name']} result for {gateway_name} ({reason or 'Ready'})")


def _credential_copies(model: dict) -> list[dict]:
    """Secrets in the default tenant namespace carrying the model's credential."""
    encoded = base64.b64encode(model["credential"].encode()).decode()
    return [s for s in list_json("secret", _ns()) if (s.get("data") or {}).get("api-key") == encoded]


def _destination_rules(model: dict) -> list[dict]:
    """The provider's DestinationRules for the default Gateway."""
    return list_json(
        "destinationrule",
        GATEWAY_NAMESPACE,
        labels=f"{LABEL_EXTERNAL_PROVIDER}={model['provider']},{LABEL_EXTERNAL_GATEWAY}={DEFAULT_GATEWAY_NAME}",
    )


def _wait_implicitly_attached(model: dict) -> dict:
    """Wait for the implicit attachment's result, accepted route, credential
    copy and DestinationRule; returns the result and route."""
    result = _wait_gateway_result(model, DEFAULT_GATEWAY_NAME)
    route_name = (result.get("httpRouteRef") or {}).get("name")
    assert route_name, f"Ready result names no route: {redact_sensitive(result)}"
    route = _wait_for_httproute_accepted(route_name, model["namespace"], timeout=READY_TIMEOUT)

    # Published only after the Gateway accepts the route.
    def _published() -> bool:
        assert _credential_copies(model), f"no credential copy for {model['name']} in {_ns()}"
        assert _destination_rules(model), f"no DestinationRule for {model['provider']} in {GATEWAY_NAMESPACE}"
        return True

    wait_until(_published, READY_TIMEOUT, f"credential copy and DestinationRule for {model['name']}")
    return {"result": result, "route": route}


def _wait_not_implicitly_attached(model: dict, route_name: str) -> None:
    """Wait until the implicit attachment and every object it published are gone."""

    def _gone() -> bool:
        assert _gateway_result(model, DEFAULT_GATEWAY_NAME) is None, f"{model['name']} still has a default result"
        assert _get_cr("httproute", route_name, model["namespace"]) is None, f"route {route_name} still exists"
        assert not _credential_copies(model), f"credential copy for {model['name']} still in {_ns()}"
        assert not _destination_rules(model), f"DestinationRule for {model['provider']} still present"
        return True

    wait_until(_gone, READY_TIMEOUT, f"removal of {model['name']}'s implicit attachment")


# ─── Serving ────────────────────────────────────────────────────────────────


def _serve_in_default_tenant(tracked: list, model: dict) -> str:
    """MaaSAuthPolicy + MaaSSubscription in the default tenant; returns an API key."""
    policy, subscription = f"{model['name']}-access", f"{model['name']}-sub"
    _create_test_auth_policy(policy, model["name"], groups=["system:authenticated"])
    tracked.append(("maasauthpolicy", policy, _ns()))
    _wait_for_maas_auth_policy_phase(policy, timeout=READY_TIMEOUT)
    _create_test_subscription(subscription, model["name"], groups=["system:authenticated"])
    tracked.append(("maassubscription", subscription, _ns()))
    # The MaaSModelRef is Ready only once the policy and subscription govern it.
    _wait_for_model_ready(model["name"], model["namespace"], timeout=READY_TIMEOUT)
    _wait_for_maas_subscription_phase(subscription, timeout=READY_TIMEOUT)
    _wait_for_subscription_trlp_status(subscription, expected_ready=True, namespace=_ns(), timeout=READY_TIMEOUT)
    return _create_api_key(_get_cluster_token(), f"e2e-dft-{uuid.uuid4().hex[:8]}", subscription=subscription)


def _infer(gateway_url: str, model: dict, api_key: str, **retry) -> requests.Response:
    """POST a chat completion to the model's echo path through a Gateway."""
    return _request_with_gateway_retry(
        requests.post,
        f"{gateway_url}/{model['namespace']}/{model['name']}/anything/{model['path_tag']}",
        headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
        json={"model": model["name"], "messages": [{"role": "user", "content": "default tenant e2e"}], "max_tokens": 8},
        **retry,
    )


# ─── Serving through the default Gateway ────────────────────────────────────


class TestDefaultTenantServesModelWithoutGatewayRefs:
    """A 3.5-shaped model in a namespace of no tenant is served through the
    default tenant's Gateway."""

    @pytest.fixture(scope="class")
    def implicit_model(self):
        tracked: list[tuple[str, str, str]] = []
        try:
            model = _provision_model(tracked, "e2e-dft")
            attachment = _wait_implicitly_attached(model)
            api_key = _serve_in_default_tenant(tracked, model)
            yield {"model": model, "api_key": api_key, **attachment}
        finally:
            _delete_crs_and_wait(reversed(tracked))

    def test_attached_to_default_gateway(self, implicit_model):
        """The single result is for the default Gateway and names an attachment
        route on it, and the spec still names no Gateway."""
        model = implicit_model["model"]
        route = implicit_model["route"]
        route_name = route["metadata"]["name"]
        assert re.fullmatch(rf"external-model-{re.escape(model['name'])}-[0-9a-f]{{12}}", route_name), (
            f"{route_name} is not an attachment route name (external-model-<model>-<hash>)"
        )
        parents = [(p.get("namespace"), p.get("name")) for p in (route.get("spec") or {}).get("parentRefs") or []]
        assert parents == [(GATEWAY_NAMESPACE, DEFAULT_GATEWAY_NAME)], f"unexpected route parentRefs: {parents}"
        external_model = get_json_or_none(EXTERNAL_MODEL_KIND, model["name"], model["namespace"]) or {}
        spec = external_model.get("spec") or {}
        assert not spec.get("gatewayRefs"), f"the implicit attachment must not mutate spec: {spec}"
        status = external_model.get("status") or {}
        results = [(entry.get("namespace"), entry.get("name")) for entry in status.get("gateways") or []]
        assert results == [(GATEWAY_NAMESPACE, DEFAULT_GATEWAY_NAME)], f"unexpected status.gateways: {results}"
        assert status.get("httpRouteName") == route_name, (
            f"status.httpRouteName {status.get('httpRouteName')!r} must name the single attachment "
            f"route {route_name} (maas-controller resolves the model's route from it)"
        )

    def test_provider_reports_ready(self, implicit_model):
        """The provider reports its own validated configuration instead of
        Pending on the namespace's missing MaasTenantConfig."""
        model = implicit_model["model"]

        def _check() -> bool:
            provider = get_json_or_none(EXTERNAL_PROVIDER_KIND, model["provider"], model["namespace"]) or {}
            status = provider.get("status") or {}
            ready = next((c for c in status.get("conditions") or [] if c.get("type") == "Ready"), {})
            assert status.get("phase") == "Ready" and ready.get("status") == "True", (
                f"provider {model['provider']} is not Ready: {redact_sensitive(status)}"
            )
            return True

        wait_until(_check, READY_TIMEOUT, f"provider {model['provider']} Ready")

    def test_served_through_default_gateway(self, gateway_url, implicit_model):
        """Inference through the default Gateway reaches the provider with the
        model's credential injected (``simple`` auth served as ``apikey``) and
        without the client's MaaS API key."""
        model = implicit_model["model"]
        response = _infer(gateway_url, model, implicit_model["api_key"])
        assert response.status_code == 200, (
            f"expected 200 through {DEFAULT_GATEWAY_NAME}, got {response.status_code}: "
            f"{redact_sensitive(response.text)}"
        )
        assert model["path_tag"] in response.text, (
            f"response did not come from the model's provider: {redact_sensitive(response.text)}"
        )
        assert model["credential"] in response.text, "the model's credential did not reach the provider"
        assert implicit_model["api_key"] not in response.text, "the client's MaaS API key was forwarded to the provider"

    def test_unknown_key_rejected(self, gateway_url, implicit_model):
        response = _infer(gateway_url, implicit_model["model"], f"sk-extmod-invalid-{uuid.uuid4().hex[:8]}", retries=1)
        assert response.status_code in (401, 403), (
            f"unknown API key must be rejected, got {response.status_code}: {redact_sensitive(response.text)}"
        )


# ─── Implicit attachment lifecycle ──────────────────────────────────────────


class TestImplicitAttachmentLifecycle:
    """gatewayRefs replace the implicit attachment and removing them restores
    it; deleting the model removes its objects."""

    def test_gateway_refs_replace_implicit_attachment(self, resources):
        """An explicit binding replaces the implicit one and never falls back
        to the default Gateway, even when no tenant can serve it."""
        model = _provision_model(resources, "e2e-dft-refs")
        route_name = _wait_implicitly_attached(model)["route"]["metadata"]["name"]

        _patch_gateway_refs(model, [{"name": UNMANAGED_GATEWAY_NAME, "namespace": GATEWAY_NAMESPACE}])
        _wait_gateway_result(model, UNMANAGED_GATEWAY_NAME, reason="GatewayNotManaged")
        _wait_not_implicitly_attached(model, route_name)

        _patch_gateway_refs(model, None)
        _wait_implicitly_attached(model)
        assert _gateway_result(model, UNMANAGED_GATEWAY_NAME) is None, (
            f"removing gatewayRefs must drop the {UNMANAGED_GATEWAY_NAME} result"
        )

    def test_model_delete_removes_implicit_attachment(self, resources):
        model = _provision_model(resources, "e2e-dft-del")
        route_name = _wait_implicitly_attached(model)["route"]["metadata"]["name"]

        _delete_crs_and_wait(
            [
                ("maasmodelref", model["name"], model["namespace"]),
                (EXTERNAL_MODEL_KIND, model["name"], model["namespace"]),
            ],
            timeout=READY_TIMEOUT,
        )
        _wait_not_implicitly_attached(model, route_name)
