"""Shared helpers for multi-tenant external model E2E.

Builds on ``multitenancy_helpers`` (AITenant bootstrap, tenant gateways,
tenant-scoped maas-api keys) and ``test_helper`` (CR plumbing, waits) to cover
the AGC/Praxis multi-tenant external model contract:

- ExternalModels in user namespaces explicitly attached to tenant gateways
  via ``spec.gatewayRefs``, with per-gateway ``status.gateways`` results
  when the AGC publishes them (see the status fallback below).
- Controller-owned credential copies delivered into the tenant namespace.
- Real inference through a specific tenant gateway against a header-echo
  external endpoint, proving which provider served the request and which
  credential was injected.
- ReferenceGrant-gated cross-namespace providers (P1).

Contracts that live in the AGC repo and are probed defensively here (assert
on observable behavior rather than assumed resource names):

- Attachment readiness is read from the per-gateway ``status.gateways[]``
  entry when the AGC populates it; AGC builds that report aggregate status
  only fall back to the model's top-level conditions, whose reason/message
  carry the first failing (or the aggregate ready) attachment result —
  gateway reasons such as ``NotAllowedByListeners`` and
  ``ModelNameConflict`` flow through verbatim. Condition types are matched
  across the plausible spellings ("Ready"/"Accepted"/"Attached").
- Provider auth must be ``apikey`` with an ``api-key`` Secret key (the only
  strategy the AGC resolver accepts; openai + openai-chat injects the Secret
  value as ``Authorization: Bearer``). Providers in user namespaces stay
  ``Pending``/``TenantNotReady`` by design: the attachment path validates
  them directly and does not gate on their own status.
- Provider DestinationRules are found by content, not hard-coded names: the
  AGC publishes them into the Gateway namespace under hashed names labeled
  with the provider, gateway, and tenant (they carry no ownerReferences).
  Credential copies delivered into the tenant are found by matching
  ``api-key`` Secret data.
- The inference URL is ``{gateway}/{model-namespace}/{modelName}`` plus the
  provider's own path: the AGC's attachment route matches that path prefix
  and rewrites the matched prefix to ``/``, so whatever the client appends
  after the model name reaches the provider verbatim (OpenAI-style clients
  append ``/v1/chat/completions``; the echo endpoint serves
  ``/anything/<tag>``). Models whose client-facing modelName differs from
  their CRD name cannot be called on this stack: the gateway auth resolves
  model identities from MaaSModelRef names and
  ``status.resolvedModelAlias`` (which the controller publishes from the
  ExternalModel CRD name) while the routes match the client-facing name —
  the conflicts module gates its same-named serving proof on that gap. The
  MaaSModelRef ``status.endpoint`` is never used as a request base: it
  only identifies the serving gateway (it may be a cluster-local address
  and carries no model path). The AGC names the provider's ExternalName
  Service in the model namespace ``provider-<ExternalModelProvider
  name>``.

Environment variables:
- E2E_EXTERNAL_ENDPOINT: external echo endpoint FQDN (default: httpbingo.org)
- E2E_TENANT_EXTERNAL_MODEL_READY_TIMEOUT: default wait for model serving (default: 300)
- TENANT_EXTERNAL_MODEL_CROSSNS_E2E: opt-in for P1 cross-namespace provider scenarios
  (requires an AGC build with cross-namespace provider support packaged)
- TENANT_EXTERNAL_MODEL_LEGACY_IPP_E2E: opt-in for the legacy IPP coexistence scenario
  (requires the MaaS IPP image pin containing the legacy-controller exclusion guard)
- TENANT_EXTERNAL_MODEL_WITHDRAWAL_E2E: umbrella opt-in for both provider-withdrawal
  scenarios; per-issue flags TENANT_EXTERNAL_MODEL_LAST_PROVIDER_WITHDRAWAL_E2E (#106)
  and TENANT_EXTERNAL_MODEL_PARTIAL_WITHDRAWAL_E2E (#108) enable them independently
  (requires the praxis release containing the partial/last-provider withdrawal
  fixes for issues #108/#106)
- AGC_DEPLOYMENT_NAME / AGC_DEPLOYMENT_NAMESPACE: overrides for locating the
  ai-gateway-controller Deployment (defaults: "ai-gateway-controller" in
  DEPLOYMENT_NAMESPACE)
"""

from __future__ import annotations

import json
import logging
import os
import re
import time
import uuid
from typing import Any

import pytest

# Tenant-agnostic external model builders, inference and attribution
# assertions live in external_model_helpers; re-exported here so existing
# imports from this module keep working.
from external_model_helpers import (  # noqa: F401 - re-exported contract
    CREDENTIAL_DATA_KEY,
    EXTERNAL_ENDPOINT,
    EXTERNAL_MODEL_CRD,
    EXTERNAL_MODEL_KIND,
    EXTERNAL_PROVIDER_KIND,
    INFERENCE_API_VERSION,
    REFERENCE_GRANT_API_VERSION,
    REFERENCE_GRANT_CRD,
    apply_credential_secret,
    apply_external_model,
    apply_external_provider,
    apply_maas_model_ref_for_external,
    apply_reference_grant,
    assert_returns_usage,
    assert_served_by_provider,
    echo_headers,
    echo_provider_path,
    external_chat_url,
    external_model_binding,
    external_model_route_name,
    infer_external_chat,
    patch_external_model_gateway_refs,
    patch_external_model_provider_refs,
    provider_external_name_service,
)
from multitenancy_helpers import (
    ANNOTATION_PAYLOAD_PROCESSING_TYPE,
    GATEWAY_NAMESPACE,
    TENANT_CONFIG_KIND,
    TENANT_CR_NAME,
    _oc_run,
    apply_gateway_access_label,
    bootstrap_aitenant_tenant,
    create_api_key_at,
    ensure_namespace,
    env_bool,
    extproc_deployment_uses_praxis,
    gateway_route_host,
    get_json_or_none,
    list_json,
    make_tenant_model_accessible,
    per_tenant_ipp_names,
    redact_sensitive,
    wait_for_per_tenant_ipp_ready,
    wait_until,
)
from test_helper import (
    DEPLOYMENT_NAMESPACE,
    _delete_cr,
    _get_cluster_token,
)

log = logging.getLogger(__name__)

# ─── Constants ──────────────────────────────────────────────────────────────

TENANT_EXTERNAL_MODEL_READY_TIMEOUT = int(os.environ.get("E2E_TENANT_EXTERNAL_MODEL_READY_TIMEOUT", "300"))

# Condition types accepted as "this attachment is serving-ready". AGC's exact
# condition naming is not frozen in the shared CRD; match across plausible
# spellings until it is.
_READY_CONDITION_TYPES = ("Ready", "Accepted", "Attached")

AGC_DEPLOYMENT_NAME = os.environ.get("AGC_DEPLOYMENT_NAME", "ai-gateway-controller")
AGC_DEPLOYMENT_NAMESPACE = os.environ.get("AGC_DEPLOYMENT_NAMESPACE", DEPLOYMENT_NAMESPACE)

# AGC renames per-tenant praxis resources to "<resource>-<tenant-name>". The
# longest base name is the per-tenant EnvoyFilter
# ("payload-processing-external-model-filters-"), which leaves 20 characters
# for the tenant name before the Kubernetes 63-character name limit makes AGC
# refuse to render the tenant's whole praxis stack (terminal: it does not
# retry until the tenant name changes).
AGC_LONGEST_RESOURCE_BASE = "payload-processing-external-model-filters-"
MAX_AITENANT_NAME_LENGTH = 63 - len(AGC_LONGEST_RESOURCE_BASE)


# ─── Capability detection ───────────────────────────────────────────────────


def _crd_schema_properties(crd: dict, section: str | None) -> dict:
    """Merge the schema properties of *section* ("spec"/"status") across versions."""
    merged: dict[str, Any] = {}
    for version in (crd.get("spec") or {}).get("versions") or []:
        schema = ((version.get("schema") or {}).get("openAPIV3Schema") or {}).get("properties") or {}
        if section is None:
            merged.update(schema)
        else:
            merged.update(schema.get(section, {}).get("properties") or {})
    return merged


def installed_external_model_crd() -> dict | None:
    return get_json_or_none("crd", EXTERNAL_MODEL_CRD)


def installed_crd_capabilities(crd: dict | None = None) -> dict[str, bool]:
    """Report which additive fields the installed CRD carries."""
    crd = crd if crd is not None else installed_external_model_crd()
    if not crd:
        return {
            "gatewayRefs": False,
            "statusGateways": False,
            "providerNamespaceRef": False,
            "perRefAuthOverride": False,
        }
    spec_props = _crd_schema_properties(crd, "spec")
    status_props = _crd_schema_properties(crd, "status")
    provider_ref_items = ((spec_props.get("externalProviderRefs") or {}).get("items") or {}).get("properties") or {}
    ref_props = (provider_ref_items.get("ref") or {}).get("properties") or {}
    return {
        "gatewayRefs": "gatewayRefs" in spec_props,
        "statusGateways": "gateways" in status_props,
        "providerNamespaceRef": "namespace" in ref_props,
        "perRefAuthOverride": "auth" in provider_ref_items,
    }


def find_agc_deployment() -> dict | None:
    """Locate the ai-gateway-controller Deployment, if installed."""
    deployment = get_json_or_none("deployment", AGC_DEPLOYMENT_NAME, AGC_DEPLOYMENT_NAMESPACE)
    if deployment:
        return deployment
    # Fall back to a name match in case the companion deploy renames it.
    for candidate in list_json("deployment", AGC_DEPLOYMENT_NAMESPACE):
        name = (candidate.get("metadata") or {}).get("name") or ""
        if "ai-gateway-controller" in name or name.startswith("aigc"):
            return candidate
    return None


def agc_controller_image() -> str:
    deployment = find_agc_deployment()
    if not deployment:
        return ""
    containers = ((deployment.get("spec") or {}).get("template") or {}).get("spec", {}).get("containers") or []
    return (containers[0].get("image") if containers else "") or ""


def require_tenant_external_model_support() -> dict[str, bool]:
    """Skip unless the installed cluster can serve multi-tenant external models.

    Requires the additive schema (gatewayRefs + status.gateways)
    in the *installed* ExternalModel CRD and a deployed ai-gateway-controller.
    Keeps default CI green while AGC images catching up; the checks auto-
    activate the suite once the schema and controller ship.
    """
    crd = installed_external_model_crd()
    if crd is None:
        pytest.skip(f"ExternalModel CRD {EXTERNAL_MODEL_CRD} is not installed")
    capabilities = installed_crd_capabilities(crd)
    if not (capabilities["gatewayRefs"] and capabilities["statusGateways"]):
        pytest.skip(
            "installed ExternalModel CRD lacks the gatewayRefs additive schema "
            f"(capabilities: {capabilities}); AGC gatewayRefs support not distributed"
        )
    if find_agc_deployment() is None:
        message = (
            f"ai-gateway-controller deployment not found "
            f"({AGC_DEPLOYMENT_NAME} in {AGC_DEPLOYMENT_NAMESPACE}); "
            "AGC companion not installed"
        )
        # The prow smoke path always deploys AGC (deploy-platform defaults
        # AI_GATEWAY_CONTROLLER_IMAGE): a missing deployment there means the
        # deploy failed, and skipping would report green CI with the whole
        # suite skipped.
        if os.environ.get("AI_GATEWAY_CONTROLLER_IMAGE"):
            pytest.fail(message)
        pytest.skip(message)
    return capabilities


def require_crossns_provider_support() -> None:
    """Gate P1 cross-namespace provider scenarios."""
    if not env_bool("TENANT_EXTERNAL_MODEL_CROSSNS_E2E"):
        pytest.skip(
            "cross-namespace provider E2E is gated until AGC ships "
            "cross-namespace provider support; set TENANT_EXTERNAL_MODEL_CROSSNS_E2E=true to enable"
        )
    result = _oc_run(["get", "crd", REFERENCE_GRANT_CRD])
    if result.returncode != 0:
        pytest.skip(f"ReferenceGrant CRD {REFERENCE_GRANT_CRD} is not installed")


def require_legacy_ipp_coexistence_support() -> None:
    """Gate the legacy IPP coexistence scenario (IPP image pin required)."""
    if not env_bool("TENANT_EXTERNAL_MODEL_LEGACY_IPP_E2E"):
        pytest.skip(
            "legacy IPP coexistence E2E requires the MaaS IPP image pin containing "
            "the legacy-controller exclusion guard; set "
            "TENANT_EXTERNAL_MODEL_LEGACY_IPP_E2E=true to enable"
        )


_WITHDRAWAL_ISSUE_FLAGS = {
    "#106": "TENANT_EXTERNAL_MODEL_LAST_PROVIDER_WITHDRAWAL_E2E",
    "#108": "TENANT_EXTERNAL_MODEL_PARTIAL_WITHDRAWAL_E2E",
}


def withdrawal_gate_enabled(issue: str) -> bool:
    """The effective per-issue withdrawal gate state.

    A scenario is enabled by its per-issue flag or by the umbrella
    TENANT_EXTERNAL_MODEL_WITHDRAWAL_E2E flag — the same resolution the
    gate and the evidence artifact must agree on, so it lives here once.
    """
    return env_bool(_WITHDRAWAL_ISSUE_FLAGS[issue]) or env_bool(
        "TENANT_EXTERNAL_MODEL_WITHDRAWAL_E2E"
    )


def require_provider_withdrawal_support(issue: str) -> None:
    """Gate provider-withdrawal scenarios per issue (praxis release pending).

    The flags are per issue so they can be flipped independently as the
    fixes land — and because they assert an expectation, not just an opt-in:
    with a flag set, the corresponding test fails red if the contract does
    not hold, so a post-fix regression cannot hide behind a skip.

    - issue #106 (last-provider withdrawal: an old route may keep serving
      instead of the empty routing revision producing no-route) — verified
      passing on current builds; safe to enable now.
    - issue #108 (partial withdrawal: an old gateway pod may keep selecting
      a removed provider / routing may disrupt instead of failing over) —
      enable only on clusters carrying the AGC seamless-failover fix;
      until then the test fails red on a disruptive withdrawal.

    ``TENANT_EXTERNAL_MODEL_WITHDRAWAL_E2E`` remains as an umbrella alias
    enabling both.
    """
    flag = _WITHDRAWAL_ISSUE_FLAGS[issue]
    if not withdrawal_gate_enabled(issue):
        pytest.skip(
            f"provider withdrawal E2E for {issue} is gated; set {flag}=true to enable "
            f"(with it set, the {issue} contract is expected to hold and a violation "
            "fails red)"
        )


def record_tenant_external_model_evidence() -> dict:
    """Write installed-version/configuration evidence for these suites.

    The acceptance criteria require recording installed AGC/Praxis
    versions and configuration as completion evidence. Best effort: neither
    the cluster lookups nor the artifact write ever fail tests — this runs
    in session-fixture setup, where one throttled or forbidden lookup would
    otherwise error every test in the tenant_external_model group.
    """

    def _safe(fn, default):
        try:
            return fn()
        except Exception as exc:  # noqa: BLE001 - evidence is best effort
            log.warning("evidence lookup failed: %s", exc)
            return default

    evidence = {
        "recordedAt": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "agc": {
            "deploymentName": AGC_DEPLOYMENT_NAME,
            "deploymentNamespace": AGC_DEPLOYMENT_NAMESPACE,
            "image": _safe(agc_controller_image, ""),
        },
        "praxis": {
            "defaultPayloadProcessingImage": _safe(
                lambda: _deployment_image("payload-processing", GATEWAY_NAMESPACE), ""
            ),
        },
        "externalModelCrd": _safe(installed_crd_capabilities, {}),
        "referenceGrantCrdInstalled": _safe(lambda: _crd_exists(REFERENCE_GRANT_CRD), False),
        "gates": {
            "TENANT_EXTERNAL_MODEL_CROSSNS_E2E": env_bool("TENANT_EXTERNAL_MODEL_CROSSNS_E2E"),
            "TENANT_EXTERNAL_MODEL_LEGACY_IPP_E2E": env_bool("TENANT_EXTERNAL_MODEL_LEGACY_IPP_E2E"),
            # Effective gate state (per-issue flag OR umbrella), matching
            # what actually gated the run — the umbrella flag alone enables
            # both scenarios, so recording only the raw per-issue flags
            # would misreport that configuration.
            "TENANT_EXTERNAL_MODEL_LAST_PROVIDER_WITHDRAWAL_E2E": withdrawal_gate_enabled("#106"),
            "TENANT_EXTERNAL_MODEL_PARTIAL_WITHDRAWAL_E2E": withdrawal_gate_enabled("#108"),
            "TENANT_EXTERNAL_MODEL_WITHDRAWAL_E2E": env_bool("TENANT_EXTERNAL_MODEL_WITHDRAWAL_E2E"),
        },
        "externalEndpoint": EXTERNAL_ENDPOINT,
    }
    try:
        artifacts_dir = os.environ.get("ARTIFACTS_DIR") or os.path.join("reports")
        os.makedirs(artifacts_dir, exist_ok=True)
        worker = os.environ.get("PYTEST_XDIST_WORKER", "main")
        path = os.path.join(artifacts_dir, f"tenant-external-model-evidence-{worker}.json")
        with open(path, "w", encoding="utf-8") as handle:
            json.dump(evidence, handle, indent=2, sort_keys=True)
        log.info("tenant external model evidence written to %s", path)
    except OSError as exc:
        log.warning("failed to write tenant external model evidence artifact: %s", exc)
    return evidence


def _deployment_image(name: str, namespace: str) -> str:
    deployment = get_json_or_none("deployment", name, namespace)
    if not deployment:
        return ""
    containers = ((deployment.get("spec") or {}).get("template") or {}).get("spec", {}).get("containers") or []
    return (containers[0].get("image") if containers else "") or ""


def _crd_exists(name: str) -> bool:
    result = _oc_run(["get", "crd", name])
    return result.returncode == 0


def maas_model_ref_alias(namespace: str, name: str) -> str:
    """The MaaSModelRef's ``status.resolvedModelAlias``.

    The controller publishes this as the model identity the gateway auth
    resolves for client-facing names. While it carries the ExternalModel CRD
    name instead of the client-facing ``spec.modelName``, models whose
    client-facing name differs from their CRD name cannot be called
    through the gateway (the routes match the client-facing name).
    """
    ref = get_json_or_none("maasmodelref", name, namespace) or {}
    return ((ref.get("status") or {}).get("resolvedModelAlias")) or ""


# ─── Tenant and namespace fixtures ──────────────────────────────────────────


def tenant_gateway_host(gateway_name: str) -> str:
    """Return the route hostname exposing a tenant gateway (shared helper)."""
    return gateway_route_host(gateway_name, namespace=GATEWAY_NAMESPACE)


def tenant_gateway_url(gateway_name: str) -> str:
    return f"https://{tenant_gateway_host(gateway_name)}"


def bootstrap_external_model_tenant(case: dict[str, str]) -> dict[str, str]:
    """Bootstrap an AITenant on the praxis dataplane and enrich its case dict.

    Adds ``gateway_host`` / ``gateway_url`` / ``maas_api_url`` for inference
    and API key minting through the tenant's own gateway. Skips when the
    per-tenant payload-processing stack is not praxis (AGC contract inactive).
    """
    tenant_name = case["tenant_label_name"]
    if len(tenant_name) > MAX_AITENANT_NAME_LENGTH:
        pytest.fail(
            f"tenant name {tenant_name!r} is {len(tenant_name)} characters; AGC per-tenant "
            f"praxis resource naming needs it to stay at or under "
            f"{MAX_AITENANT_NAME_LENGTH} (63-char limit minus the longest resource base "
            f"{AGC_LONGEST_RESOURCE_BASE!r}). Longer names make AGC refuse to render the "
            "tenant's praxis stack without retrying.",
            pytrace=False,
        )
    bootstrap_aitenant_tenant(case)
    wait_for_per_tenant_ipp_ready(case)
    names = per_tenant_ipp_names(case["tenant_label_name"])
    if not extproc_deployment_uses_praxis(names["processing_deployment"], GATEWAY_NAMESPACE):
        pytest.skip(
            f"{names['processing_deployment']} is not praxis-extproc; "
            "the AGC/Praxis multi-tenant contract is not active for this tenant"
        )
    case["gateway_host"] = tenant_gateway_host(case["gateway_name"])
    case["gateway_url"] = f"https://{case['gateway_host']}"
    case["maas_api_url"] = f"https://{case['gateway_host']}/maas-api"
    return case


def ensure_user_namespace(name: str, *gateway_names: str) -> str:
    """Create a namespace whose HTTPRoutes the given tenant gateways admit.

    ``apply_gateway_fixture`` ships tenant gateways with an allowedRoutes
    namespace selector keyed on the gateway-access label; labeling the user
    namespace admits routes created in it (the user-namespace serving path).
    """
    ensure_namespace(name)
    for gateway_name in gateway_names:
        apply_gateway_access_label(name, gateway_name)
    return name


def new_user_namespace(prefix: str) -> str:
    return f"{prefix}-{uuid.uuid4().hex[:6]}"


def new_admitted_user_namespace(prefix: str, *gateway_names: str) -> str:
    """Create a user namespace whose routes the given gateways admit."""
    name = new_user_namespace(prefix)
    return ensure_user_namespace(name, *gateway_names)


def new_unadmitted_namespace(prefix: str) -> str:
    """Create a namespace no tenant gateway's allowedRoutes selector admits.

    Default MaaS tenant gateways restrict routes to namespaces labeled with
    the gateway-access label; an unlabeled namespace keeps that gate closed.
    """
    name = new_user_namespace(prefix)
    ensure_namespace(name)
    return name


def gateway_ref(gateway_name: str, gateway_namespace: str = GATEWAY_NAMESPACE) -> dict[str, str]:
    return {"name": gateway_name, "namespace": gateway_namespace}


# ─── Resource builders ──────────────────────────────────────────────────────


def payload_processing_annotation(tenant_namespace: str) -> str | None:
    """Read the tenant config's payload-processing annotation.

    Returns None when the annotation is absent (backend selected by
    cluster default rather than an explicit override).
    """
    tenant_config = get_json_or_none(TENANT_CONFIG_KIND, TENANT_CR_NAME, tenant_namespace) or {}
    annotations = (tenant_config.get("metadata") or {}).get("annotations") or {}
    return annotations.get(ANNOTATION_PAYLOAD_PROCESSING_TYPE)


def restore_payload_processing_annotation(tenant_namespace: str, original: str | None) -> None:
    """Restore the payload-processing annotation to a captured value.

    ``original=None`` removes the annotation — a merge patch with a null
    value deletes the key — restoring cluster-default backend selection.
    """
    patch = json.dumps(
        {"metadata": {"annotations": {ANNOTATION_PAYLOAD_PROCESSING_TYPE: original}}}
    )
    result = _oc_run(
        [
            "patch",
            TENANT_CONFIG_KIND,
            TENANT_CR_NAME,
            "-n",
            tenant_namespace,
            "--type=merge",
            "-p",
            patch,
        ]
    )
    if result.returncode != 0:
        raise RuntimeError(
            f"failed to restore {ANNOTATION_PAYLOAD_PROCESSING_TYPE}={original!r} on "
            f"{TENANT_CONFIG_KIND}/{tenant_namespace}/{TENANT_CR_NAME}: "
            f"{result.stderr.strip() or result.stdout.strip()}"
        )


def provision_external_model_stack(
    resources: TenantExternalModelResources,
    *,
    name: str,
    namespace: str,
    gateway_refs: list[dict[str, str]] | None = None,
    model_name: str | None = None,
    tenant_refs: list[str] | None = None,
    provider_name: str | None = None,
    provider_namespace: str | None = None,
    credential: str | None = None,
    credential_secret_name: str | None = None,
    override_credential: str | None = None,
    override_secret_name: str | None = None,
    endpoint: str = EXTERNAL_ENDPOINT,
    provider_path: str | None = None,
    provider_config: dict[str, str] | None = None,
) -> dict:
    """Create the full serving stack for one external model and track cleanup.

    Credential Secret (in the provider namespace) + ExternalProvider +
    ExternalModel + MaaSModelRef. ``override_credential`` additionally creates
    a model-namespace Secret wired as the per-model auth override.

    ``endpoint``/``provider_path``/``credential`` re-point the provider away
    from the header-echo default — e.g. at an OpenAI-style simulator that
    returns ``usage.total_tokens`` (token accounting needs it; the echo
    endpoint returns none). ``endpoint`` is a bare hostname: the CRD
    rejects scheme/port and the gateway dials 443. ``provider_config`` sets
    the provider's ``spec.config`` — e.g. ``tls.caCertificates`` pointing at
    a CA file mounted in the gateway pods, needed when the provider's cert
    is not publicly trusted (see ``apply_external_provider``).

    ``tenant_refs`` serves a model shared across tenant gateways: instead of
    one tenantRef-less MaaSModelRef named after the model, it creates one
    modelRef per tenant (``<name>-<tenant>``, ``spec.tenantRef`` pinned).
    Per-gateway status makes the controller's tenant selection require
    ``spec.tenantRef`` for models attached to more than one gateway, and the
    per-tenant modelRefs each select their own tenant's attachment.

    Returns identifiers used by the assertions: metadata/client names, the
    provider binding's echo ``path_tag`` (identifies which provider served a
    request) and the credential markers (identify which Secret was injected).
    ``model_refs`` maps each tenant to its modelRef name (empty for the
    default single-modelRef shape).
    """
    suffix = uuid.uuid4().hex[:6]
    credential = credential or f"sk-extmod-{suffix}"
    provider_name = provider_name or f"{name}-provider"
    provider_namespace = provider_namespace or namespace
    secret_name = credential_secret_name or f"{provider_name}-cred"
    path_tag = f"{name}-{suffix}"

    apply_credential_secret(secret_name, provider_namespace, credential)
    resources.track("secret", secret_name, provider_namespace)
    apply_external_provider(
        provider_name,
        provider_namespace,
        credential_secret=secret_name,
        endpoint=endpoint,
        config=provider_config,
    )
    resources.track(EXTERNAL_PROVIDER_KIND, provider_name, provider_namespace)

    override_secret: str | None = None
    if override_credential is not None:
        override_secret = override_secret_name or f"{name}-override-cred"
        apply_credential_secret(override_secret, namespace, override_credential)
        resources.track("secret", override_secret, namespace)

    apply_external_model(
        name,
        namespace,
        provider_name=provider_name,
        provider_namespace=provider_namespace,
        gateway_refs=gateway_refs,
        model_name=model_name,
        path_tag=path_tag,
        provider_path=provider_path,
        credential_secret=override_secret,
    )
    resources.track(EXTERNAL_MODEL_KIND, name, namespace)
    model_refs: dict[str, str] = {}
    if tenant_refs:
        for tenant_ref in tenant_refs:
            ref_name = f"{name}-{tenant_ref}"
            apply_maas_model_ref_for_external(
                name,
                namespace,
                endpoint=endpoint,
                ref_name=ref_name,
                tenant_ref=tenant_ref,
            )
            resources.track("maasmodelref", ref_name, namespace)
            model_refs[tenant_ref] = ref_name
    else:
        apply_maas_model_ref_for_external(name, namespace, endpoint=endpoint)
        resources.track("maasmodelref", name, namespace)
    return {
        "name": name,
        "namespace": namespace,
        "client_name": model_name or name,
        "model_refs": model_refs,
        "provider": provider_name,
        "provider_namespace": provider_namespace,
        "secret_name": secret_name,
        "secret_namespace": provider_namespace,
        "credential": credential,
        "override_credential": override_credential,
        "override_secret": override_secret,
        "path_tag": path_tag,
    }


# ─── ExternalModel per-gateway status ───────────────────────────────────────


def gateway_status_entry(
    model_name: str,
    model_namespace: str,
    gateway_name: str,
    gateway_namespace: str = GATEWAY_NAMESPACE,
) -> dict | None:
    """Return the status.gateways entry for one gateway attachment."""
    external_model = get_json_or_none(EXTERNAL_MODEL_KIND, model_name, model_namespace)
    if not external_model:
        return None
    for entry in (external_model.get("status") or {}).get("gateways") or []:
        if entry.get("name") == gateway_name and entry.get("namespace") == gateway_namespace:
            return entry
    return None


def _attachment_conditions(
    external_model: dict,
    gateway_name: str,
    gateway_namespace: str,
) -> tuple[list[dict], str]:
    """Conditions describing one gateway attachment.

    Prefers the per-gateway ``status.gateways[]`` entry when the AGC
    populates it. AGC builds that report aggregate status only fall back to
    the model's top-level conditions: the aggregate carries the first
    failing (or the overall ready) attachment result, and gateway reasons
    (``NotAllowedByListeners``, ``ModelNameConflict``, ...) flow through
    verbatim. Returns ``(conditions, where)`` with ``where`` describing the
    source for error messages.
    """
    entries = (external_model.get("status") or {}).get("gateways") or []
    entry = next(
        (
            item
            for item in entries
            if item.get("name") == gateway_name and item.get("namespace") == gateway_namespace
        ),
        None,
    )
    if entry is not None and entry.get("conditions"):
        return entry["conditions"], f"attachment to {gateway_namespace}/{gateway_name}"
    status = external_model.get("status") or {}
    return (status.get("conditions") or []), "aggregate model status"


def _wait_for_gateway_attachment(
    model_name: str,
    model_namespace: str,
    gateway_name: str,
    *,
    gateway_namespace: str = GATEWAY_NAMESPACE,
    matcher,
    what: str,
    timeout: int | None = None,
):
    def _check():
        external_model = get_json_or_none(EXTERNAL_MODEL_KIND, model_name, model_namespace)
        assert external_model is not None, f"{EXTERNAL_MODEL_KIND}/{model_namespace}/{model_name} not found"
        conditions, where = _attachment_conditions(external_model, gateway_name, gateway_namespace)
        assert conditions, (
            f"no attachment conditions for {gateway_namespace}/{gateway_name} on "
            f"{model_namespace}/{model_name}; status: {redact_sensitive(external_model.get('status'))}"
        )
        return matcher(conditions, where)

    return wait_until(
        _check,
        timeout or TENANT_EXTERNAL_MODEL_READY_TIMEOUT,
        f"{model_namespace}/{model_name} attachment to {gateway_namespace}/{gateway_name}: {what}",
    )


def wait_for_gateway_attachment_ready(
    model_name: str,
    model_namespace: str,
    gateway_name: str,
    *,
    gateway_namespace: str = GATEWAY_NAMESPACE,
    timeout: int | None = None,
) -> dict:
    """Wait until one gateway attachment reports serving readiness."""

    def _matcher(conditions: list[dict], where: str) -> dict:
        by_type = {c.get("type"): c for c in conditions}
        for condition_type in _READY_CONDITION_TYPES:
            condition = by_type.get(condition_type)
            if condition and condition.get("status") == "True":
                return condition
        raise AssertionError(f"attachment not ready on {where}; conditions: {conditions}")

    return _wait_for_gateway_attachment(
        model_name,
        model_namespace,
        gateway_name,
        gateway_namespace=gateway_namespace,
        matcher=_matcher,
        what="report Ready",
        timeout=timeout,
    )


def wait_for_gateway_attachment_reason(
    model_name: str,
    model_namespace: str,
    gateway_name: str,
    reason: str,
    *,
    gateway_namespace: str = GATEWAY_NAMESPACE,
    timeout: int | None = None,
) -> dict:
    """Wait until one gateway attachment reports a condition with *reason*.

    Used for NotAllowedByListeners (gateway-attachment authorization) and
    ModelNameConflict (client-facing name conflicts). The reason is the
    contract: it is matched on the per-gateway ``status.gateways`` entry
    when present, otherwise on the model's aggregate conditions, into which
    the AGC flows the gateway's reason verbatim.
    """

    def _matcher(conditions: list[dict], where: str) -> dict:
        matches = [c for c in conditions if c.get("reason") == reason]
        assert matches, f"no condition with reason {reason!r} on {where}; conditions: {conditions}"
        return matches[0]

    return _wait_for_gateway_attachment(
        model_name,
        model_namespace,
        gateway_name,
        gateway_namespace=gateway_namespace,
        matcher=_matcher,
        what=f"report reason {reason}",
        timeout=timeout,
    )


def wait_for_gateway_attachment_not_ready(
    model_name: str,
    model_namespace: str,
    gateway_name: str,
    *,
    gateway_namespace: str = GATEWAY_NAMESPACE,
    message_pattern: str | None = None,
    timeout: int | None = None,
):
    """Wait until an attachment reports a failure (not serving).

    Matches either a False condition on the gateway's status.gateways entry
    or — when AGC has not published an entry — the model's aggregate Failed
    phase / top-level False conditions. ``message_pattern`` (case-insensitive
    regex) additionally requires the failure to mention e.g. the missing
    ReferenceGrant authorization or Secret.
    """
    pattern = re.compile(message_pattern, re.IGNORECASE) if message_pattern else None

    def _match_condition(conditions: list[dict], where: str):
        failures = [c for c in conditions if c.get("status") == "False"]
        assert failures, f"no False condition on {where}; conditions: {conditions}"
        if pattern is None:
            return failures[0]
        matched = [
            c
            for c in failures
            if pattern.search(c.get("message") or "") or pattern.search(c.get("reason") or "")
        ]
        assert matched, (
            f"no failure on {where} matching {message_pattern!r}; conditions: {conditions}"
        )
        return matched[0]

    def _check():
        external_model = get_json_or_none(EXTERNAL_MODEL_KIND, model_name, model_namespace)
        assert external_model is not None, f"{EXTERNAL_MODEL_KIND}/{model_namespace}/{model_name} not found"
        conditions, where = _attachment_conditions(external_model, gateway_name, gateway_namespace)
        assert conditions, (
            f"no attachment conditions for {gateway_namespace}/{gateway_name} on "
            f"{model_namespace}/{model_name}; status: {redact_sensitive(external_model.get('status'))}"
        )
        return _match_condition(conditions, where)

    return wait_until(
        _check,
        timeout or TENANT_EXTERNAL_MODEL_READY_TIMEOUT,
        f"{model_namespace}/{model_name} attachment to {gateway_namespace}/{gateway_name} "
        f"report failure ({message_pattern or 'any reason'})",
    )


def wait_for_gateway_attachment_absent(
    model_name: str,
    model_namespace: str,
    gateway_name: str,
    *,
    gateway_namespace: str = GATEWAY_NAMESPACE,
    timeout: int | None = None,
) -> None:
    """Wait until the status.gateways entry for a detached gateway is gone."""

    def _check():
        entry = gateway_status_entry(model_name, model_namespace, gateway_name, gateway_namespace)
        assert entry is None, f"status.gateways entry still present: {redact_sensitive(entry)}"
        return True

    return wait_until(
        _check,
        timeout or TENANT_EXTERNAL_MODEL_READY_TIMEOUT,
        f"{model_namespace}/{model_name} detachment from {gateway_namespace}/{gateway_name}",
    )


def external_model_http_route_name(model_name: str, model_namespace: str) -> str:
    """Legacy single-attachment route name (status.httpRouteName or model name)."""
    external_model = get_json_or_none(EXTERNAL_MODEL_KIND, model_name, model_namespace)
    if external_model:
        return (external_model.get("status") or {}).get("httpRouteName") or model_name
    return model_name


def external_model_reports_maas_route(model_name: str, model_namespace: str) -> bool:
    """Whether the MaaS stack can resolve this ExternalModel's serving route.

    maas-controller resolves an attached model's HTTPRoute from the AGC's
    per-gateway ``status.gateways[]`` results (entries with an
    ``httpRouteRef`` and a Ready condition) or, on aggregate-status AGC
    builds, from ``status.httpRouteName`` — which those builds write only
    for single-attachment models. A model attached to several gateways on an
    aggregate-status build reports no MaaS-resolvable route even while every
    attachment serves: its MaaSModelRef stays without a route and the
    subscription TRLP backend is not ready, so auth keys and quotas cannot
    be wired for it.
    """
    external_model = get_json_or_none(EXTERNAL_MODEL_KIND, model_name, model_namespace)
    if not external_model:
        return False
    status = external_model.get("status") or {}
    if status.get("httpRouteName"):
        return True
    for entry in status.get("gateways") or []:
        if not entry.get("httpRouteRef"):
            continue
        if any(
            c.get("type") in _READY_CONDITION_TYPES and c.get("status") == "True"
            for c in entry.get("conditions") or []
        ):
            return True
    return False


# ─── HTTPRoute discovery per attachment ─────────────────────────────────────

# The AGC's attachment routes append shortHash — exactly 12 lowercase hex
# characters (sha256 over ns/name/gateway-namespace/gateway-name) — after
# the model name; see attachmentRouteName in the AGC controller.
ATTACHMENT_ROUTE_HASH_RE = re.compile(r"[0-9a-f]{12}")


def route_is_for_model(route: dict, model_name: str) -> bool:
    """Whether an AGC-managed HTTPRoute belongs to the given ExternalModel.

    Route names carry the model: ``external-model-<name>`` for tenant-local
    models and ``external-model-<name>-<hash>`` for cross-namespace
    attachments, where the hash is the controller's shortHash — exactly 12
    lowercase hex characters. Requiring that exact shape keeps models whose
    names share a prefix distinct: model ``demo`` must not match
    ``external-model-demo-blue``, model ``demo-blue``'s home route. (Names
    long enough to overflow the 253-character route-name limit degrade to
    ``external-model--<hash>``, which carries no name to match; the names
    used here stay far below that limit.) The model-name route label is not
    present on every AGC build, so the name is the portable contract.
    """
    route_name = (route.get("metadata") or {}).get("name") or ""
    base = external_model_route_name(model_name)
    if route_name == base:
        return True
    prefix = f"{base}-"
    if not route_name.startswith(prefix):
        return False
    return ATTACHMENT_ROUTE_HASH_RE.fullmatch(route_name[len(prefix):]) is not None


def attachment_httproutes(
    route_namespace: str,
    gateway_name: str,
    gateway_namespace: str = GATEWAY_NAMESPACE,
) -> list[dict]:
    """HTTPRoutes in *route_namespace* whose parentRefs select the gateway."""
    routes: list[dict] = []
    for route in list_json("httproute", route_namespace):
        for parent in (route.get("spec") or {}).get("parentRefs") or []:
            if (
                parent.get("name") == gateway_name
                and (parent.get("namespace") or gateway_namespace) == gateway_namespace
            ):
                routes.append(route)
                break
    return routes


def httproute_parent_conditions(
    route: dict,
    gateway_name: str,
    gateway_namespace: str = GATEWAY_NAMESPACE,
) -> list[dict]:
    for parent in (route.get("status") or {}).get("parents") or []:
        ref = parent.get("parentRef") or {}
        if ref.get("name") == gateway_name and (ref.get("namespace") or gateway_namespace) == gateway_namespace:
            return parent.get("conditions") or []
    return []


def wait_for_attachment_httproute(
    route_namespace: str,
    gateway_name: str,
    *,
    model_name: str | None = None,
    gateway_namespace: str = GATEWAY_NAMESPACE,
    accepted: bool = True,
    reason: str | None = None,
    timeout: int | None = None,
) -> list[dict]:
    """Wait for a gateway-attached HTTPRoute in the route namespace.

    ``accepted=False`` waits for the gateway to refuse the route (e.g.
    NotAllowedByListeners when allowedRoutes does not admit the namespace).

    model_name scopes the wait to one ExternalModel's route (routes are
    named after the model — see route_is_for_model): a namespace can host
    other models attached to the same gateway, and an unscoped wait could
    pass on their routes.
    """
    expected_reason = reason
    scope = f" for model {model_name}" if model_name else ""

    def _check() -> list[dict]:
        routes = attachment_httproutes(route_namespace, gateway_name, gateway_namespace)
        if model_name is not None:
            routes = [r for r in routes if route_is_for_model(r, model_name)]
        assert routes, (
            f"no HTTPRoute in {route_namespace} attached to "
            f"{gateway_namespace}/{gateway_name}{scope}"
        )
        for route in routes:
            conditions = {
                c.get("type"): c for c in httproute_parent_conditions(route, gateway_name, gateway_namespace)
            }
            accepted_condition = conditions.get("Accepted")
            if accepted:
                if accepted_condition and accepted_condition.get("status") == "True":
                    return routes
            elif (
                accepted_condition
                and accepted_condition.get("status") == "False"
                and (expected_reason is None or accepted_condition.get("reason") == expected_reason)
            ):
                return routes
        raise AssertionError(
            f"HTTPRoute(s) in {route_namespace} not in expected state "
            f"(accepted={accepted}, reason={expected_reason!r}): "
            f"{[r.get('status') for r in routes]}"
        )

    return wait_until(
        _check,
        timeout or TENANT_EXTERNAL_MODEL_READY_TIMEOUT,
        f"HTTPRoute in {route_namespace} attached to {gateway_namespace}/{gateway_name}{scope} "
        f"(accepted={accepted}, reason={expected_reason!r})",
    )


def wait_for_no_attachment_httproute(
    route_namespace: str,
    gateway_name: str,
    *,
    model_name: str | None = None,
    gateway_namespace: str = GATEWAY_NAMESPACE,
    timeout: int | None = None,
) -> None:
    """Wait until no HTTPRoute remains attached to the gateway.

    model_name scopes the wait to one ExternalModel's route: a namespace
    can legitimately host other models attached to the same gateway (the
    lifecycle tests share user namespaces and gateways), so an unscoped
    absence check would fail on their routes.
    """
    scope = f" for model {model_name}" if model_name else ""

    def _check() -> bool:
        routes = attachment_httproutes(route_namespace, gateway_name, gateway_namespace)
        if model_name is not None:
            routes = [r for r in routes if route_is_for_model(r, model_name)]
        assert not routes, f"stale HTTPRoute(s) still attached: {[r['metadata']['name'] for r in routes]}"
        return True

    return wait_until(
        _check,
        timeout or TENANT_EXTERNAL_MODEL_READY_TIMEOUT,
        f"no HTTPRoute in {route_namespace} attached to {gateway_namespace}/{gateway_name}{scope}",
    )


# ─── Tenant-published resources (credential copies, DestinationRules) ───────


def _find_credential_copy(tenant_namespace: str, expected: str, *, exclude_uid: str | None) -> dict | None:
    """Find a Secret in *tenant_namespace* carrying *expected*, excluding the
    source object itself (matched by uid, for sources living in the tenant
    namespace)."""
    for secret in list_json("secret", tenant_namespace):
        metadata = secret.get("metadata") or {}
        if metadata.get("uid") == exclude_uid:
            continue
        if (secret.get("data") or {}).get(CREDENTIAL_DATA_KEY) == expected:
            return secret
    return None


def credential_copy_in(tenant_namespace: str, source_secret_name: str, source_namespace: str) -> dict | None:
    """Find the controller-owned copy of a credential Secret in a tenant namespace.

    AGC delivers credentials from outside the tenant namespace as
    controller-owned copies. The copy is identified by content (matching
    ``api-key`` data) rather than name: any Secret in the tenant namespace
    holding the same credential that is not the user-created source object.
    """
    source = get_json_or_none("secret", source_secret_name, source_namespace)
    if not source:
        return None
    expected = (source.get("data") or {}).get(CREDENTIAL_DATA_KEY)
    if not expected:
        return None
    return _find_credential_copy(
        tenant_namespace, expected, exclude_uid=(source.get("metadata") or {}).get("uid")
    )


def wait_for_credential_copy(
    tenant_namespace: str,
    source_secret_name: str,
    source_namespace: str,
    *,
    timeout: int | None = None,
) -> dict:
    def _check() -> dict:
        copy = credential_copy_in(tenant_namespace, source_secret_name, source_namespace)
        assert copy is not None, (
            f"no controller-owned credential copy of {source_namespace}/{source_secret_name} "
            f"found in tenant namespace {tenant_namespace}"
        )
        return copy

    return wait_until(
        _check,
        timeout or TENANT_EXTERNAL_MODEL_READY_TIMEOUT,
        f"credential copy of {source_namespace}/{source_secret_name} in {tenant_namespace}",
    )


def wait_for_no_credential_copy(
    tenant_namespace: str,
    source_secret_name: str,
    source_namespace: str,
    *,
    timeout: int | None = None,
) -> None:
    """Wait until no controller-owned copy of the credential remains.

    The expected ``api-key`` is captured from the source Secret up front:
    an unreadable source fails the wait rather than passing vacuously.
    "No copy left" and "cannot know what to look for" are different
    outcomes — a stale controller-owned copy would otherwise go undetected
    once the source is cleaned up.
    """
    source = get_json_or_none("secret", source_secret_name, source_namespace)
    if not source:
        pytest.fail(
            f"source secret {source_namespace}/{source_secret_name} is unreadable; "
            "the absence of a credential copy cannot be proven without it"
        )
    expected = (source.get("data") or {}).get(CREDENTIAL_DATA_KEY)
    if not expected:
        pytest.fail(
            f"source secret {source_namespace}/{source_secret_name} carries no "
            f"{CREDENTIAL_DATA_KEY} data; the absence of a credential copy "
            "cannot be proven without it"
        )
    source_uid = (source.get("metadata") or {}).get("uid")

    def _check() -> bool:
        copy = _find_credential_copy(tenant_namespace, expected, exclude_uid=source_uid)
        assert copy is None, (
            f"stale credential copy {copy.get('metadata', {}).get('name')} still present "
            f"in {tenant_namespace}"
        )
        return True

    return wait_until(
        _check,
        timeout or TENANT_EXTERNAL_MODEL_READY_TIMEOUT,
        f"no credential copy of {source_namespace}/{source_secret_name} in {tenant_namespace}",
    )


def destination_rules_referencing(
    model_name: str,
    *namespaces: str,
    also_matching: tuple[str, ...] = (),
) -> list[dict]:
    """DestinationRules that reference *model_name* (by name or labels) in
    the given namespaces, additionally requiring every
    string in *also_matching* to appear in the same metadata — e.g. a
    gateway name, to scope to one attachment's rules while another gateway
    still serves the same model. (The match blob also includes
    ownerReferences, but the AGC sets none on these rules.)

    AGC creates the provider DestinationRules in the Gateway namespace
    (``GATEWAY_NAMESPACE``), not the tenant's, under hashed names that are
    not part of the shared contract — so discovery runs against the Gateway
    namespace and matches on references to the model (the test provider
    names embed the model name, and the rules carry them in their names and
    ``external-provider`` labels).
    """
    matches: list[dict] = []
    needles = [model_name, *also_matching]
    for namespace in namespaces:
        for rule in list_json("destinationrule", namespace):
            metadata = rule.get("metadata") or {}
            owners = [
                (owner or {}).get("name") or ""
                for owner in metadata.get("ownerReferences") or []
            ]
            blob = json.dumps(
                {
                    "name": metadata.get("name") or "",
                    "labels": metadata.get("labels") or {},
                    "ownerReferences": owners,
                }
            )
            if all(needle in blob for needle in needles):
                matches.append(rule)
    return matches


def wait_for_no_destination_rules_referencing(
    model_name: str,
    *namespaces: str,
    also_matching: tuple[str, ...] = (),
    timeout: int | None = None,
) -> None:
    def _check() -> bool:
        rules = destination_rules_referencing(model_name, *namespaces, also_matching=also_matching)
        assert not rules, (
            f"stale DestinationRules referencing {model_name} (also matching {also_matching}): "
            f"{[r['metadata']['name'] for r in rules]}"
        )
        return True

    return wait_until(
        _check,
        timeout or TENANT_EXTERNAL_MODEL_READY_TIMEOUT,
        f"no DestinationRules referencing {model_name} in {namespaces}",
    )


# ─── Inference through a specific gateway ───────────────────────────────────


# ─── MaaS governance + API keys per tenant ──────────────────────────────────


def serve_external_model_in_tenant(
    case: dict[str, str],
    model_name: str,
    model_namespace: str,
    *,
    policy_name: str | None = None,
    subscription_name: str | None = None,
    token_limit: int = 100,
    window: str = "1m",
    key_name: str | None = None,
    wait_model_ready: bool = True,
    timeout: int | None = None,
) -> dict[str, str]:
    """Apply the existing authorization/subscription semantics for one tenant.

    Creates MaaSAuthPolicy + MaaSSubscription in the tenant namespace
    referencing a model that may live in a user namespace, waits for
    reconciliation (including the mirrored TRLP) and mints a tenant API key
    for the subscription. Resources are tracked before they are applied, so
    a failure partway through reconciliation still cleans up whatever was
    already created.
    """
    timeout = timeout or TENANT_EXTERNAL_MODEL_READY_TIMEOUT
    tenant_namespace = case["tenant_ns"]
    policy_name = policy_name or f"{model_name}-access"
    subscription_name = subscription_name or f"{model_name}-sub"
    created = TenantExternalModelResources()
    try:
        # Tracked before applying: cleanup must cover a reconciliation that
        # fails after the policy/subscription were already created.
        created.track("maasauthpolicy", policy_name, tenant_namespace)
        created.track("maassubscription", subscription_name, tenant_namespace)
        # Shared policy/subscription/reconciliation flow (make_tenant_model_
        # accessible), pointed at the user-namespace model via model_namespace.
        make_tenant_model_accessible(
            model_name,
            tenant_namespace,
            policy_name,
            subscription_name,
            token_limit=token_limit,
            window=window,
            trlp_timeout=timeout,
            gateway_name=case["gateway_name"],
            model_namespace=model_namespace,
            require_model_ready=wait_model_ready,
            model_ready_timeout=timeout,
        )
        api_key = mint_tenant_api_key(
            case,
            key_name or f"e2e-extmod-{uuid.uuid4().hex[:8]}",
            subscription_name,
        )
        return {"policy": policy_name, "subscription": subscription_name, "api_key": api_key}
    except Exception:
        created.cleanup()
        raise


def mint_tenant_api_key(case: dict[str, str], key_name: str, subscription_name: str) -> str:
    response = create_api_key_at(case["maas_api_url"], _get_cluster_token(), key_name, subscription=subscription_name)
    assert response.status_code in (200, 201), (
        f"failed to mint API key on tenant {case['tenant_label_name']} maas-api: "
        f"{response.status_code} {redact_sensitive(response.text)}"
    )
    api_key = response.json().get("key")
    assert api_key, f"API key missing in response: {redact_sensitive(response.json())}"
    return api_key


# ─── Tracked fixture cleanup ────────────────────────────────────────────────


class TenantExternalModelResources:
    """Track created objects and delete them in reverse creation order.

    MaaSModelRef finalizers are cleared before deletion (same pattern as
    test_external_models.py) so cleanup never hangs on the cleanup handshake.
    """

    def __init__(self):
        self._items: list[tuple[str, str, str | None]] = []

    def track(self, kind: str, name: str, namespace: str | None = None) -> None:
        self._items.append((kind, name, namespace))

    def cleanup(self) -> None:
        for kind, name, namespace in reversed(self._items):
            try:
                if kind == "maasmodelref":
                    _clear_finalizers_best_effort(kind, name, namespace)
                _delete_cr(kind, name, namespace)
            except Exception as exc:  # noqa: BLE001 - one failed item must not abort the rest
                log.warning("cleanup failed for %s/%s in %s: %s", kind, name, namespace, exc)

    def cleanup_namespace(self, namespace: str) -> None:
        from multitenancy_helpers import delete_namespace_best_effort

        self.cleanup()
        delete_namespace_best_effort(namespace)


def _clear_finalizers_best_effort(kind: str, name: str, namespace: str | None) -> None:
    namespace = namespace or ""
    result = _oc_run(
        [
            "patch",
            kind,
            name,
            "-n",
            namespace,
            "--type=merge",
            "-p",
            json.dumps({"metadata": {"finalizers": []}}),
        ]
    )
    if result.returncode != 0:
        log.warning("failed to clear finalizers on %s/%s/%s: %s", kind, namespace, name, result.stderr.strip())
