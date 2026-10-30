"""Shared helpers for external model E2E tests (tenant-agnostic).

The subset of the external-model contract that does not depend on
multi-tenancy, usable against any gateway serving ``inference.opendatahub.io``
ExternalModels — the default tenant or an explicitly attached tenant gateway:

- Credential Secret, ExternalProvider and ExternalModel builders (echo
  endpoint by default: responses under ``/anything/*`` echo the request
  headers and URL back, proving which provider served a request and which
  credential was injected).
- Provider-set edits (binding builder, wholesale ``spec.externalProviderRefs``
  and ``spec.gatewayRefs`` patches) for switching/withdrawal scenarios.
- ReferenceGrant application (cross-namespace providers).
- Inference through the path convention ``{gateway}/{namespace}/{modelName}``
  plus the provider's own path, with gateway/auth propagation retries.
- Attribution assertions (serving provider, injected credential, usage).

Tenant-specific machinery (tenant bootstrap, per-gateway attachment status,
credential copies into tenant namespaces, MaaS governance per tenant) lives
in ``tenant_external_model_helpers``, which builds on this module.
"""

from __future__ import annotations

import json
import logging
import os
import time
import uuid
from typing import Any

import requests
from multitenancy_helpers import redact_sensitive
from test_helper import _apply_cr, _request_with_gateway_retry, _run_oc

log = logging.getLogger(__name__)

# ─── Constants ──────────────────────────────────────────────────────────────

EXTERNAL_MODEL_CRD = "externalmodels.inference.opendatahub.io"
EXTERNAL_MODEL_KIND = "externalmodels.inference.opendatahub.io"
EXTERNAL_PROVIDER_KIND = "externalproviders.inference.opendatahub.io"
INFERENCE_API_VERSION = "inference.opendatahub.io/v1alpha1"

REFERENCE_GRANT_CRD = "referencegrants.gateway.networking.k8s.io"
REFERENCE_GRANT_API_VERSION = "gateway.networking.k8s.io/v1beta1"

# Header-echo endpoint: responses under /anything/* echo the request headers
# and URL back in the JSON body, proving which provider served the request
# (unique path tag) and which credential was injected (unique marker value).
EXTERNAL_ENDPOINT = os.environ.get("E2E_EXTERNAL_ENDPOINT", os.environ.get("E2E_SIMULATOR_ENDPOINT", "httpbingo.org"))

# Secret data key holding the provider credential (contract in the
# ExternalProvider/ExternalModel CRD descriptions).
CREDENTIAL_DATA_KEY = "api-key"


# ─── Resource builders ──────────────────────────────────────────────────────


def echo_provider_path(path_tag: str) -> str:
    """Provider path of an echo binding: the external endpoint echoes any
    request under ``/anything/<tag>``.

    The AGC passes the client's path after the ``/{namespace}/{modelName}``
    prefix through to the provider verbatim, so requests to echo bindings
    must ask for this path (real OpenAI-style providers instead receive the
    client's natural ``/v1/chat/completions`` suffix).
    """
    return f"/anything/{path_tag}"


def apply_credential_secret(name: str, namespace: str, credential: str) -> dict:
    """Create a provider credential Secret (data key ``api-key``).

    Keeps the legacy ``inference.llm-d.ai/ipp-managed`` label so payload
    processing credential injection finds it on either dataplane.
    """
    secret = {
        "apiVersion": "v1",
        "kind": "Secret",
        "metadata": {
            "name": name,
            "namespace": namespace,
            "labels": {"inference.llm-d.ai/ipp-managed": "true"},
        },
        "type": "Opaque",
        "stringData": {CREDENTIAL_DATA_KEY: credential},
    }
    _apply_cr(secret)
    return secret


def apply_external_provider(
    name: str,
    namespace: str,
    *,
    credential_secret: str,
    endpoint: str = EXTERNAL_ENDPOINT,
    provider: str = "openai",
    config: dict[str, str] | None = None,
) -> dict:
    """Create an ExternalProvider pointing at the header-echo endpoint.

    The echo path tag that identifies this provider's traffic is configured on
    the ExternalModel binding (see ``apply_external_model``). Auth must be
    ``apikey``: the AGC resolver only accepts that strategy (openai +
    openai-chat injects the Secret value as ``Authorization: Bearer``), and
    the credential Secret must carry the ``api-key`` key.

    ``endpoint`` is a bare hostname: the CRD's endpoint pattern rejects
    scheme and port, and the gateway always dials 443 — an in-cluster
    provider Service must therefore listen on 443.

    ``config`` sets provider-specific ``spec.config`` keys — e.g.
    ``{"tls.caCertificates": path}`` so envoy verifies a provider whose cert
    is signed by a CA mounted in the gateway pods (the AGC requires an
    absolute file path and applies it to the provider DestinationRule).
    """
    spec: dict[str, Any] = {
        "provider": provider,
        "endpoint": endpoint,
        "auth": {
            "type": "apikey",
            "secretRef": {"name": credential_secret},
        },
    }
    if config:
        spec["config"] = dict(config)
    external_provider = {
        "apiVersion": INFERENCE_API_VERSION,
        "kind": "ExternalProvider",
        "metadata": {"name": name, "namespace": namespace},
        "spec": spec,
    }
    _apply_cr(external_provider)
    return external_provider


def external_model_binding(
    provider_name: str,
    *,
    provider_namespace: str | None = None,
    path_tag: str | None = None,
    provider_path: str | None = None,
    credential_secret: str | None = None,
    target_model: str = "gpt-3.5-turbo",
) -> dict:
    """One ``spec.externalProviderRefs[]`` entry, in the same shape
    ``apply_external_model`` builds (see that builder for the field
    contracts). Used by tests that later re-patch a model's provider set
    (provider switching, withdrawal)."""
    ref: dict[str, str] = {"name": provider_name}
    if provider_namespace:
        ref["namespace"] = provider_namespace
    binding: dict[str, Any] = {
        "ref": ref,
        "targetModel": target_model,
        "apiFormat": "openai-chat",
        "path": provider_path or echo_provider_path(path_tag or uuid.uuid4().hex[:10]),
    }
    if credential_secret:
        binding["auth"] = {
            "type": "apikey",
            "secretRef": {"name": credential_secret},
        }
    return binding


def apply_external_model(
    name: str,
    namespace: str,
    *,
    provider_name: str,
    provider_namespace: str | None = None,
    gateway_refs: list[dict[str, str]] | None = None,
    model_name: str | None = None,
    path_tag: str | None = None,
    provider_path: str | None = None,
    credential_secret: str | None = None,
    target_model: str = "gpt-3.5-turbo",
    extra_provider_refs: list[dict] | None = None,
) -> dict:
    """Create an ExternalModel, optionally attached to explicit gateways.

    Args:
        provider_name/provider_namespace: the ExternalProvider to bind. A
            provider_namespace different from the model namespace creates the
            P1 cross-namespace reference (requires a ReferenceGrant).
        gateway_refs: explicit tenant gateway attachments (spec.gatewayRefs).
            None keeps the legacy shape (tenant-local resolution).
        model_name: client-facing modelName (defaults to metadata.name).
        path_tag: unique echo-path tag; defaults to a fresh uuid so responses
            identify this binding's provider.
        provider_path: the path the provider serves, for bindings whose
            provider is not the header-echo endpoint (e.g. an OpenAI-style
            simulator behind another gateway). Overrides the echo path.
        credential_secret: per-model auth override Secret (in the model
            namespace). None uses the provider-local Secret.
        extra_provider_refs: additional provider bindings (same shape as the
            primary binding) for multi-provider models.
    """
    path_tag = path_tag or uuid.uuid4().hex[:10]
    binding = external_model_binding(
        provider_name,
        provider_namespace=provider_namespace,
        path_tag=path_tag,
        provider_path=provider_path,
        credential_secret=credential_secret,
        target_model=target_model,
    )
    bindings = [binding]
    if extra_provider_refs:
        bindings.extend(extra_provider_refs)
    spec: dict[str, Any] = {"externalProviderRefs": bindings}
    if gateway_refs is not None:
        spec["gatewayRefs"] = gateway_refs
    if model_name:
        spec["modelName"] = model_name
    external_model = {
        "apiVersion": INFERENCE_API_VERSION,
        "kind": "ExternalModel",
        "metadata": {"name": name, "namespace": namespace},
        "spec": spec,
    }
    _apply_cr(external_model)
    return external_model


def apply_maas_model_ref_for_external(
    name: str,
    namespace: str,
    *,
    endpoint: str = EXTERNAL_ENDPOINT,
    provider: str = "openai",
    ref_name: str | None = None,
    tenant_ref: str | None = None,
) -> dict:
    """Create the MaaSModelRef pointing at an ExternalModel (same name/ns).

    ``ref_name`` names the MaaSModelRef differently from the ExternalModel:
    per-tenant modelRefs for a shared model must have distinct names, since
    ``spec.modelRef`` resolves within the modelRef's own namespace.
    ``tenant_ref`` pins the modelRef's tenant selection (``spec.tenantRef``)
    so a model attached to several gateways resolves one attachment per
    modelRef instead of an ambiguous multi-gateway selection.
    """
    model_ref = {
        "apiVersion": "maas.opendatahub.io/v1alpha1",
        "kind": "MaaSModelRef",
        "metadata": {
            "name": ref_name or name,
            "namespace": namespace,
            "annotations": {
                "maas.opendatahub.io/endpoint": endpoint,
                "maas.opendatahub.io/provider": provider,
            },
        },
        "spec": {"modelRef": {"kind": "ExternalModel", "name": name}},
    }
    if tenant_ref:
        model_ref["spec"]["tenantRef"] = tenant_ref
    _apply_cr(model_ref)
    return model_ref


def _patch_external_model(name: str, namespace: str, patch: dict, what: str) -> None:
    """Merge-patch an ExternalModel; both list-valued spec fields
    (gatewayRefs, externalProviderRefs) are replaced wholesale by a merge
    patch, so callers pass the complete new list."""
    result = _run_oc(
        ["oc", "patch", EXTERNAL_MODEL_KIND, name, "-n", namespace, "--type=merge", "-p", json.dumps(patch)]
    )
    if result.returncode != 0:
        raise RuntimeError(
            f"failed to patch {what} on {EXTERNAL_MODEL_KIND}/{namespace}/{name}: "
            f"{result.stderr.strip() or result.stdout.strip()}"
        )


def patch_external_model_provider_refs(
    name: str,
    namespace: str,
    bindings: list[dict],
) -> None:
    """Replace ``spec.externalProviderRefs`` (provider switching edits)."""
    _patch_external_model(
        name, namespace, {"spec": {"externalProviderRefs": bindings}}, "externalProviderRefs"
    )


def patch_external_model_gateway_refs(
    name: str,
    namespace: str,
    gateway_refs: list[dict[str, str]] | None,
) -> None:
    """Set (or clear) ``spec.gatewayRefs``."""
    patch = {"spec": {"gatewayRefs": gateway_refs}}
    _patch_external_model(name, namespace, patch, "gatewayRefs")


def apply_reference_grant(
    name: str,
    provider_namespace: str,
    *,
    from_namespace: str,
    to_name: str | None = None,
) -> dict:
    """Authorize ExternalModels in *from_namespace* to reference providers in
    the grant's namespace (P1 cross-namespace contract).

    Grants authorize at namespace granularity only: ReferenceGrant v1beta1's
    ``from`` has no ``name`` field, and the API server prunes unknown fields,
    so every ExternalModel in *from_namespace* is covered.
    """
    grant_from: dict[str, str] = {
        "group": "inference.opendatahub.io",
        "kind": "ExternalModel",
        "namespace": from_namespace,
    }
    grant_to: dict[str, str] = {
        "group": "inference.opendatahub.io",
        "kind": "ExternalProvider",
    }
    if to_name:
        grant_to["name"] = to_name
    grant = {
        "apiVersion": REFERENCE_GRANT_API_VERSION,
        "kind": "ReferenceGrant",
        "metadata": {"name": name, "namespace": provider_namespace},
        "spec": {"from": [grant_from], "to": [grant_to]},
    }
    _apply_cr(grant)
    return grant


# ─── AGC naming contracts ───────────────────────────────────────────────────


def provider_external_name_service(provider_name: str) -> str:
    """The AGC's ExternalName Service name for a provider.

    The attachment HTTPRoute's backendRefs reference this Service (created in
    the model namespace, resolving to the provider endpoint), alongside the
    provider-selection sink Service.
    """
    return f"provider-{provider_name}"


def external_model_route_name(model_name: str) -> str:
    """The AGC's HTTPRoute name for a tenant-local (legacy-shaped) model.

    The AGC renders ``external-model-<name>`` regardless of the gatewayRefs
    shape — tenant-local resolution included; cross-namespace attachments
    append a gateway hash. ``status.httpRouteName`` carries this value and
    the MaaS stack consumes it for TokenRateLimitPolicy creation.
    """
    return f"external-model-{model_name}"


# ─── Inference through a specific gateway ───────────────────────────────────


def external_chat_url(
    gateway_url: str,
    model_namespace: str,
    client_model_name: str,
    *,
    provider_path: str = "/v1/chat/completions",
) -> str:
    """Chat-completions URL for an external model through a given gateway.

    The AGC's attachment HTTPRoute matches the ``/{namespace}/{modelName}``
    path prefix (namespace = the model's namespace, modelName = the
    client-facing name) and rewrites the matched prefix to ``/``, so
    whatever the client appends after the model name reaches the provider
    verbatim: OpenAI-style clients append ``/v1/chat/completions`` (the
    default) and the echo endpoint serves ``/anything/<tag>``. The
    MaaSModelRef ``status.endpoint`` is NOT used: it identifies the serving
    gateway (its value may even be the cluster-local Gateway address) and
    carries no model path.
    """
    base = gateway_url.rstrip("/")
    return f"{base}/{model_namespace}/{client_model_name}{provider_path}"


def infer_external_chat(
    gateway_url: str,
    model_namespace: str,
    client_model_name: str,
    api_key: str,
    *,
    path_tag: str | None = None,
    provider_path: str | None = None,
    content: str = "multi-tenant external model e2e",
    retries: int = 6,
    delay: int = 5,
) -> requests.Response:
    """POST a chat completion through a specific gateway, retrying route
    propagation (404) and transient gateway/auth responses.

    ``path_tag`` requests the echo binding's provider path — see
    external_chat_url for why the provider's own path must follow the
    model-name prefix. ``provider_path`` overrides it for bindings whose
    provider serves a different path (e.g. an OpenAI-style simulator behind
    another gateway, whose model path must reach it verbatim).
    """
    if provider_path is None:
        provider_path = echo_provider_path(path_tag) if path_tag else "/v1/chat/completions"
    url = external_chat_url(
        gateway_url, model_namespace, client_model_name, provider_path=provider_path
    )
    headers = {"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"}
    body = {
        "model": client_model_name,
        "messages": [{"role": "user", "content": content}],
        "max_tokens": 8,
    }
    response = None
    for attempt in range(1, retries + 1):
        # The caller's budget bounds transient gateway/auth retries too —
        # negative tests pass retries=1 to get the refusal verdict from a
        # single attempt instead of riding out propagation windows.
        response = _request_with_gateway_retry(
            requests.post, url, headers=headers, json=body, retries=retries, delay=delay
        )
        if response.status_code == 200:
            return response
        if response.status_code == 404 and attempt < retries:
            log.info(
                "%s got 404 (attempt %d/%d), retrying in %ds (route propagation)",
                url,
                attempt,
                retries,
                delay,
            )
            time.sleep(delay)
            continue
        return response
    return response


# ─── Attribution assertions ─────────────────────────────────────────────────


def assert_served_by_provider(response: requests.Response, path_tag: str, *, credential: str | None = None) -> None:
    """Assert a request reached the expected provider (and credential).

    The echo endpoint reflects the outgoing request URL and headers in the
    response body: the path tag confirms the path the client requested is
    the path the provider received (the gateway rewrites only the
    ``/{namespace}/{modelName}`` prefix), and the credential marker
    identifies the injected Secret — i.e. which provider binding served.
    """
    assert response.status_code == 200, (
        f"expected 200 from external model inference, got {response.status_code}: "
        f"{redact_sensitive(response.text)}"
    )
    assert path_tag in response.text, (
        f"response did not come from provider binding {path_tag}: {redact_sensitive(response.text)}"
    )
    if credential is not None:
        assert credential in response.text, (
            f"expected credential marker to reach the provider, response: "
            f"{redact_sensitive(response.text)}"
        )


def assert_returns_usage(response: requests.Response) -> None:
    """Assert an inference response carries OpenAI-style usage tokens.

    Token quotas are accounted from ``usage.total_tokens`` in the response,
    so a provider that returns no usage (e.g. the header-echo endpoint)
    can never exhaust a budget — quota assertions are only meaningful
    against responses that pass this check.
    """
    assert response.status_code == 200, (
        f"expected 200 from external model inference, got {response.status_code}: "
        f"{redact_sensitive(response.text)}"
    )
    try:
        usage = (response.json() or {}).get("usage") or {}
    except ValueError:
        usage = {}
    total_tokens = usage.get("total_tokens")
    assert isinstance(total_tokens, int) and total_tokens > 0, (
        "response carries no usage.total_tokens for token accounting: "
        f"{redact_sensitive(response.text)}"
    )


def echo_headers(response: requests.Response) -> dict[str, str]:
    """Headers the provider saw, as echoed by the external endpoint."""
    try:
        headers = response.json().get("headers") or {}
        return {str(k).lower(): v for k, v in headers.items()}
    except ValueError:
        return {}
