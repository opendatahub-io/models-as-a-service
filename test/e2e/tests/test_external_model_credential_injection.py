"""E2E: Azure api-key credential injection for external models.

With an ``azure`` / ``azure-openai`` ExternalProvider using ``auth.type: apikey`` and
an ExternalModel with ``apiFormat: openai-chat``, the provider qualifies (reports
Ready and gets a route) and the credential is injected into the outbound ``api-key``
header — the header Azure expects, distinct from OpenAI's ``Authorization: Bearer``
and Anthropic's ``x-api-key``.

The model lives in MODEL_NAMESPACE with no gatewayRefs and is served by the default
tenant's Gateway. The provider endpoint is a header-echo service (default
httpbingo.org): under ``/anything/<tag>`` it reflects the forwarded request headers
back, so the injected ``api-key`` header is observed directly on the egress request
rather than inferred from CR state.

Skips unless the default tenant runs praxis-extproc, since credential injection is a
praxis-dataplane capability.

Environment variables:
- E2E_EXTERNAL_ENDPOINT: header-echo endpoint FQDN (default: httpbingo.org)
- E2E_TENANT_EXTERNAL_MODEL_READY_TIMEOUT: reconciliation wait (default: 300)
"""

from __future__ import annotations

import logging
import os
import uuid

import pytest
from external_model_helpers import (
    apply_credential_secret,
    apply_external_model,
    apply_external_provider,
    apply_maas_model_ref_for_external,
    echo_headers,
    infer_external_chat,
)
from multitenancy_helpers import (
    GATEWAY_NAMESPACE,
    extproc_deployment_uses_praxis,
    get_json_or_none,
    redact_sensitive,
    wait_until,
)
from test_helper import (
    MODEL_NAMESPACE,
    _create_api_key,
    _create_test_auth_policy,
    _create_test_subscription,
    _delete_crs_and_wait,
    _get_cluster_token,
    _ns,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
    _wait_for_model_ready,
)

log = logging.getLogger(__name__)

# Serialized with tenant_ipp, which runs legacy IPP tenant stacks: those must not
# run next to this suite's models.
pytestmark = [pytest.mark.xdist_group("tenant_ipp"), pytest.mark.tenant_external_model, pytest.mark.full_suite]

EXTERNAL_PROVIDER_KIND = "externalproviders.inference.opendatahub.io"
EXTERNAL_MODEL_KIND = "externalmodels.inference.opendatahub.io"
READY_TIMEOUT = int(os.environ.get("E2E_TENANT_EXTERNAL_MODEL_READY_TIMEOUT", "300"))

# Provider types that use the apikey credential strategy and deliver the credential
# in the ``api-key`` header (distinct from OpenAI's ``Authorization: Bearer`` and
# Anthropic's ``x-api-key``).
AZURE_PROVIDER_TYPES = ["azure", "azure-openai"]
AZURE_AUTH_HEADER = "api-key"


@pytest.fixture(scope="module", autouse=True)
def _require_praxis_default_tenant():
    if not extproc_deployment_uses_praxis("payload-processing", GATEWAY_NAMESPACE):
        pytest.skip(
            "default tenant payload-processing is not praxis-extproc; credential "
            "injection is a praxis-dataplane capability"
        )


def _wait_provider_ready(provider_name: str, provider_type: str) -> None:
    """Precondition: the apikey ExternalProvider reconciles to Ready."""

    def _check() -> bool:
        provider = get_json_or_none(EXTERNAL_PROVIDER_KIND, provider_name, MODEL_NAMESPACE) or {}
        status = provider.get("status") or {}
        ready = next((c for c in status.get("conditions") or [] if c.get("type") == "Ready"), {})
        assert status.get("phase") == "Ready" and ready.get("status") == "True", (
            f"provider {provider_name} ({provider_type}) is not Ready: {redact_sensitive(status)}"
        )
        return True

    wait_until(_check, READY_TIMEOUT, f"provider {provider_name} Ready")


def _wait_model_route(model_name: str, provider_type: str) -> str:
    """Precondition: the qualified provider yields an HTTPRoute on the model."""

    def _check() -> str:
        external_model = get_json_or_none(EXTERNAL_MODEL_KIND, model_name, MODEL_NAMESPACE) or {}
        route_name = (external_model.get("status") or {}).get("httpRouteName")
        assert route_name, (
            f"model {model_name} ({provider_type}) published no status.httpRouteName — "
            f"the provider was not qualified for a route"
        )
        return route_name

    return wait_until(_check, READY_TIMEOUT, f"httpRouteName for {model_name}")


@pytest.fixture(scope="class", params=AZURE_PROVIDER_TYPES)
def azure_model(request):
    """An Azure stack in MODEL_NAMESPACE (credential Secret, apikey ExternalProvider,
    ExternalModel without gatewayRefs, MaaSModelRef) plus default tenant governance
    (MaaSAuthPolicy + MaaSSubscription) and an API key. Parametrized over ``azure``
    and ``azure-openai``."""
    provider_type = request.param
    suffix = uuid.uuid4().hex[:6]
    name = f"e2e-azure-cred-{provider_type}-{suffix}"
    model = {
        "provider_type": provider_type,
        "name": name,
        "provider": f"{name}-provider",
        "secret": f"{name}-cred",
        "credential": f"sk-azure-{suffix}",
        "path_tag": f"{name}-{uuid.uuid4().hex[:6]}",
    }
    tracked: list[tuple[str, str, str]] = []
    try:
        apply_credential_secret(model["secret"], MODEL_NAMESPACE, model["credential"])
        tracked.append(("secret", model["secret"], MODEL_NAMESPACE))

        apply_external_provider(
            model["provider"],
            MODEL_NAMESPACE,
            credential_secret=model["secret"],
            provider=provider_type,
        )
        tracked.append((EXTERNAL_PROVIDER_KIND, model["provider"], MODEL_NAMESPACE))

        apply_external_model(
            model["name"],
            MODEL_NAMESPACE,
            provider_name=model["provider"],
            path_tag=model["path_tag"],
        )
        tracked.append((EXTERNAL_MODEL_KIND, model["name"], MODEL_NAMESPACE))

        apply_maas_model_ref_for_external(model["name"], MODEL_NAMESPACE, provider=provider_type)
        tracked.append(("maasmodelref", model["name"], MODEL_NAMESPACE))

        # Control-plane preconditions: the provider must qualify and a route must be
        # built before the data-plane injection can be exercised.
        _wait_provider_ready(model["provider"], provider_type)
        route_name = _wait_model_route(model["name"], provider_type)
        log.info("provider %s qualified: route %s", provider_type, route_name)

        policy, subscription = f"{model['name']}-access", f"{model['name']}-sub"
        _create_test_auth_policy(policy, model["name"], groups=["system:authenticated"])
        tracked.append(("maasauthpolicy", policy, _ns()))
        _wait_for_maas_auth_policy_phase(policy, timeout=READY_TIMEOUT)

        _create_test_subscription(
            subscription, model["name"], groups=["system:authenticated"], token_limit=10000, window="1h"
        )
        tracked.append(("maassubscription", subscription, _ns()))
        # The MaaSModelRef goes Ready only once the policy and subscription govern it.
        _wait_for_model_ready(model["name"], MODEL_NAMESPACE, timeout=READY_TIMEOUT)
        _wait_for_maas_subscription_phase(subscription, timeout=READY_TIMEOUT)

        api_key = _create_api_key(_get_cluster_token(), f"e2e-azure-{suffix}", subscription=subscription)
        yield {**model, "api_key": api_key}
    finally:
        _delete_crs_and_wait(reversed(tracked))


class TestAzureCredentialInjection:
    """azure / azure-openai apikey providers inject the credential into the outbound
    ``api-key`` header."""

    def test_credential_injected_into_outbound_api_key_header(self, gateway_url, azure_model):
        """Given a valid key, when a chat request is forwarded, then the upstream
        received ``api-key: <credential>`` (the Azure-distinct header) and the
        client's MaaS API key is not leaked to the provider."""
        response = infer_external_chat(
            gateway_url,
            MODEL_NAMESPACE,
            azure_model["name"],
            azure_model["api_key"],
            path_tag=azure_model["path_tag"],
            content="azure credential injection e2e",
            readiness_timeout=READY_TIMEOUT,
        )
        assert response.status_code == 200, (
            f"expected 200 from the echo upstream, got {response.status_code}: {redact_sensitive(response.text)}"
        )

        headers = echo_headers(response)
        injected = headers.get(AZURE_AUTH_HEADER)
        # httpbingo may reflect a header value as a list; normalize.
        if isinstance(injected, list):
            injected = injected[0] if injected else None

        assert injected is not None, (
            f"forwarded request carried no '{AZURE_AUTH_HEADER}' header — the credential was not "
            f"injected. Echoed headers: {sorted(headers)}"
        )
        assert injected == azure_model["credential"], (
            f"forwarded '{AZURE_AUTH_HEADER}' = {injected!r}, expected {azure_model['credential']!r}"
        )
        assert azure_model["api_key"] not in response.text, (
            "the client's MaaS API key was forwarded to the provider"
        )
        log.info(
            "Credential injection confirmed for %s: forwarded %s=%r",
            azure_model["provider_type"],
            AZURE_AUTH_HEADER,
            injected,
        )
