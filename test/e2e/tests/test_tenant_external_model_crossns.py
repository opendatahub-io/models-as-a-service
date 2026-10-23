"""E2E: ReferenceGrant-gated cross-namespace ExternalProvider sharing (P1).

P1 scenarios:

1. A model referencing an ExternalProvider in another namespace serves
   requests when a matching ReferenceGrant permits it.
2. A missing/non-matching grant rejects the reference descriptively; adding
   permission restores serving.
3. Revoking/narrowing the grant stops access and updates status and runtime
   configuration.
4. A cross-namespace provider uses its provider-local Secret by default and
   the model-local Secret when auth is overridden; a missing model-local
   override must fail without provider-namespace fallback.
5. A model referencing same-named providers from two namespaces serves
   through both.

Gated on TENANT_EXTERNAL_MODEL_CROSSNS_E2E=true until AGC cross-namespace
provider support is packaged
(requires AGC ReferenceGrant validation and its RBAC). This module boots its
own tenant because it runs in a separate xdist group from the P0 modules.
"""

from __future__ import annotations

import logging
import uuid

import pytest
from multitenancy_helpers import (
    cleanup_discovery_case,
    delete_namespace_best_effort,
    new_named_tenant_case,
    redact_sensitive,
    require_aitenant_crd,
    require_tenant_namespace_discovery,
)
from tenant_external_model_helpers import (
    EXTERNAL_MODEL_KIND,
    EXTERNAL_PROVIDER_KIND,
    TenantExternalModelResources,
    apply_credential_secret,
    apply_external_model,
    apply_external_provider,
    apply_maas_model_ref_for_external,
    apply_reference_grant,
    assert_served_by_provider,
    bootstrap_external_model_tenant,
    gateway_ref,
    infer_external_chat,
    new_admitted_user_namespace,
    require_crossns_provider_support,
    require_tenant_external_model_support,
    serve_external_model_in_tenant,
    wait_for_credential_copy,
    wait_for_gateway_attachment_not_ready,
    wait_for_gateway_attachment_ready,
    wait_for_no_credential_copy,
)
from test_helper import _delete_cr, _get_cr

log = logging.getLogger(__name__)

# Serialized with tenant_ipp until the legacy IPP controllers skip
# gatewayRefs ExternalModels (image pin pending): without that guard they
# adopt this suite's models and flap status.httpRouteName.
pytestmark = pytest.mark.xdist_group("tenant_ipp")

# Failure text expected when a cross-namespace reference is not authorized.
GRANT_FAILURE_PATTERN = r"grant|authoriz|not allowed|permission|forbidden|reference"
# Failure text expected when a referenced Secret does not exist.
MISSING_SECRET_PATTERN = r"secret"


@pytest.fixture(scope="module")
def crossns_env():
    require_crossns_provider_support()
    require_tenant_external_model_support()
    require_aitenant_crd()
    require_tenant_namespace_discovery()

    # Short tenant name: AGC per-tenant resource naming caps tenant names at
    # 20 characters (see MAX_AITENANT_NAME_LENGTH).
    case = bootstrap_external_model_tenant(new_named_tenant_case("e2e-extmod-x"))
    model_ns = new_admitted_user_namespace("e2e-extmod-crossns-model", case["gateway_name"])
    provider_ns = new_admitted_user_namespace("e2e-extmod-crossns-prov", case["gateway_name"])
    resources = TenantExternalModelResources()
    try:
        yield {
            "tenant": case,
            "model_ns": model_ns,
            "provider_ns": provider_ns,
            "resources": resources,
        }
    finally:
        resources.cleanup()
        delete_namespace_best_effort(model_ns)
        delete_namespace_best_effort(provider_ns)
        cleanup_discovery_case(case)


def _serve(env: dict, model: dict, **kwargs) -> dict:
    case = env["tenant"]
    access = serve_external_model_in_tenant(
        case,
        model["name"],
        model["namespace"],
        policy_name=f"{model['name']}-access",
        subscription_name=f"{model['name']}-sub",
        **kwargs,
    )
    env["resources"].track("maasauthpolicy", access["policy"], case["tenant_ns"])
    env["resources"].track("maassubscription", access["subscription"], case["tenant_ns"])
    return access


def _model_with_provider(
    env: dict,
    *,
    name: str,
    provider_name: str,
    override_secret: str | None = None,
) -> dict:
    """Create a cross-ns model bound to a caller-managed provider."""
    suffix = uuid.uuid4().hex[:6]
    path_tag = f"{name}-{suffix}"
    apply_external_model(
        name,
        env["model_ns"],
        provider_name=provider_name,
        provider_namespace=env["provider_ns"],
        gateway_refs=[gateway_ref(env["tenant"]["gateway_name"])],
        path_tag=path_tag,
        credential_secret=override_secret,
    )
    env["resources"].track(EXTERNAL_MODEL_KIND, name, env["model_ns"])
    apply_maas_model_ref_for_external(name, env["model_ns"])
    env["resources"].track("maasmodelref", name, env["model_ns"])
    return {
        "name": name,
        "namespace": env["model_ns"],
        "client_name": name,
        "provider": provider_name,
        "provider_namespace": env["provider_ns"],
        "path_tag": path_tag,
    }


def _crossns_model(env: dict, *, name: str, credential: str | None = None, override: str | None = None) -> dict:
    """A model in the model namespace referencing a provider in the provider
    namespace (no ReferenceGrant — callers add grants per scenario)."""
    resources = env["resources"]
    suffix = uuid.uuid4().hex[:6]
    credential = credential or f"sk-extmod-crossns-{suffix}"
    provider_name = f"{name}-provider"
    secret_name = f"{provider_name}-cred"

    apply_credential_secret(secret_name, env["provider_ns"], credential)
    resources.track("secret", secret_name, env["provider_ns"])
    apply_external_provider(provider_name, env["provider_ns"], credential_secret=secret_name)
    resources.track(EXTERNAL_PROVIDER_KIND, provider_name, env["provider_ns"])

    override_secret = None
    if override is not None:
        override_secret = f"{name}-override-cred"
        apply_credential_secret(override_secret, env["model_ns"], override)
        resources.track("secret", override_secret, env["model_ns"])

    model = _model_with_provider(
        env, name=name, provider_name=provider_name, override_secret=override_secret
    )
    return {
        **model,
        "secret_name": secret_name,
        "secret_namespace": env["provider_ns"],
        "credential": credential,
        "override_credential": override,
    }


def _grant_for(env: dict, model: dict, *, name_suffix: str = "", from_namespace: str | None = None) -> str:
    grant_name = f"{model['name']}-grant{name_suffix}"
    apply_reference_grant(
        grant_name,
        model["provider_namespace"],
        from_namespace=from_namespace or model["namespace"],
        to_name=model["provider"],
    )
    env["resources"].track("referencegrant", grant_name, model["provider_namespace"])
    return grant_name


def _revoke_grant(env: dict, grant_name: str, provider_namespace: str) -> None:
    _delete_cr("referencegrant", grant_name, provider_namespace)


class TestCrossNsProviderGrant:
    """A ReferenceGrant in the provider namespace permits serving."""

    def test_grant_permits_cross_namespace_provider(self, crossns_env):
        env = crossns_env
        model = _crossns_model(env, name=f"e2e-crossns-grant-{uuid.uuid4().hex[:6]}")
        _grant_for(env, model)
        access = _serve(env, model)

        wait_for_gateway_attachment_ready(
            model["name"], model["namespace"], env["tenant"]["gateway_name"]
        )
        # The provider-local (provider-namespace) credential is delivered as a
        # controller-owned copy into the tenant namespace and used upstream.
        wait_for_credential_copy(
            env["tenant"]["tenant_ns"], model["secret_name"], model["secret_namespace"]
        )
        response = infer_external_chat(
            env["tenant"]["gateway_url"],
            model["namespace"],
            model["client_name"],
            access["api_key"],
            path_tag=model["path_tag"],
        )
        assert_served_by_provider(response, model["path_tag"], credential=model["credential"])


class TestMissingGrant:
    """A missing or non-matching grant rejects the reference descriptively;
    adding permission restores serving."""

    def test_missing_grant_rejects_then_restores(self, crossns_env):
        env = crossns_env
        model = _crossns_model(env, name=f"e2e-crossns-missing-{uuid.uuid4().hex[:6]}")
        gateway = env["tenant"]["gateway_name"]

        # No grant: the reference is rejected descriptively (not silently
        # resolved or served with a fallback provider).
        wait_for_gateway_attachment_not_ready(
            model["name"], model["namespace"], gateway, message_pattern=GRANT_FAILURE_PATTERN
        )

        # Adding permission restores serving.
        _grant_for(env, model)
        access = _serve(env, model)
        wait_for_gateway_attachment_ready(model["name"], model["namespace"], gateway)
        response = infer_external_chat(
            env["tenant"]["gateway_url"],
            model["namespace"],
            model["client_name"],
            access["api_key"],
            path_tag=model["path_tag"],
        )
        assert_served_by_provider(response, model["path_tag"], credential=model["credential"])

    def test_non_matching_grant_rejects(self, crossns_env):
        env = crossns_env
        model = _crossns_model(env, name=f"e2e-crossns-wronggr-{uuid.uuid4().hex[:6]}")
        gateway = env["tenant"]["gateway_name"]
        # A grant that authorizes a different source namespace must not open
        # the model's reference.
        _grant_for(env, model, name_suffix="-wrong", from_namespace=f"{model['namespace']}-other")

        wait_for_gateway_attachment_not_ready(
            model["name"], model["namespace"], gateway, message_pattern=GRANT_FAILURE_PATTERN
        )


class TestGrantRevocation:
    """Revoking or narrowing the grant stops access and updates status and
    runtime configuration."""

    def test_revoke_stops_access(self, crossns_env):
        env = crossns_env
        model = _crossns_model(env, name=f"e2e-crossns-revoke-{uuid.uuid4().hex[:6]}")
        gateway = env["tenant"]["gateway_name"]
        grant_name = _grant_for(env, model)
        access = _serve(env, model)
        wait_for_gateway_attachment_ready(model["name"], model["namespace"], gateway)

        response = infer_external_chat(
            env["tenant"]["gateway_url"],
            model["namespace"],
            model["client_name"],
            access["api_key"],
            path_tag=model["path_tag"],
        )
        assert_served_by_provider(response, model["path_tag"], credential=model["credential"])

        # Revoke: status flips to a failure mentioning authorization and the
        # runtime configuration (credential copy) is withdrawn.
        _revoke_grant(env, grant_name, model["provider_namespace"])
        wait_for_gateway_attachment_not_ready(
            model["name"], model["namespace"], gateway, message_pattern=GRANT_FAILURE_PATTERN
        )
        wait_for_no_credential_copy(
            env["tenant"]["tenant_ns"], model["secret_name"], model["secret_namespace"]
        )
        denied = infer_external_chat(
            env["tenant"]["gateway_url"],
            model["namespace"],
            model["client_name"],
            access["api_key"],
            path_tag=model["path_tag"],
            retries=1,
        )
        assert denied.status_code != 200, (
            f"inference must stop after the grant is revoked, got "
            f"{denied.status_code}: {redact_sensitive(denied.text)}"
        )

    def test_narrowed_grant_stops_access(self, crossns_env):
        env = crossns_env
        model = _crossns_model(env, name=f"e2e-crossns-narrow-{uuid.uuid4().hex[:6]}")
        gateway = env["tenant"]["gateway_name"]
        grant_name = _grant_for(env, model)
        access = _serve(env, model)
        wait_for_gateway_attachment_ready(model["name"], model["namespace"], gateway)

        # Narrow the grant to a different source namespace: the model's
        # reference is no longer covered. The grant name comes from
        # _grant_for itself — reconstructing it here would silently no-op
        # the revocation if that naming ever changed.
        _revoke_grant(env, grant_name, model["provider_namespace"])
        _grant_for(env, model, name_suffix="-narrowed", from_namespace=f"{model['namespace']}-other")
        wait_for_gateway_attachment_not_ready(
            model["name"], model["namespace"], gateway, message_pattern=GRANT_FAILURE_PATTERN
        )
        denied = infer_external_chat(
            env["tenant"]["gateway_url"],
            model["namespace"],
            model["client_name"],
            access["api_key"],
            path_tag=model["path_tag"],
            retries=1,
        )
        assert denied.status_code != 200, (
            f"inference must stop after the grant is narrowed, got {denied.status_code}"
        )


class TestCredentialSourceSelection:
    """Provider-local Secret by default, model-local Secret on override, and
    no provider-namespace fallback for a missing override."""

    def test_provider_local_credential_used_by_default(self, crossns_env):
        env = crossns_env
        # Same-named Secrets in both namespaces with different values; the
        # provider-local one must be selected without an override.
        shared_secret = "shared-cred"
        provider_credential = f"sk-extmod-prov-{uuid.uuid4().hex[:8]}"
        apply_credential_secret(shared_secret, env["provider_ns"], provider_credential)
        env["resources"].track("secret", shared_secret, env["provider_ns"])
        model_credential = f"sk-extmod-model-{uuid.uuid4().hex[:8]}"
        apply_credential_secret(shared_secret, env["model_ns"], model_credential)
        env["resources"].track("secret", shared_secret, env["model_ns"])

        provider_name = f"e2e-crossns-credsel-{uuid.uuid4().hex[:6]}-provider"
        apply_external_provider(provider_name, env["provider_ns"], credential_secret=shared_secret)
        env["resources"].track(
            EXTERNAL_PROVIDER_KIND, provider_name, env["provider_ns"]
        )

        # Default: provider-local Secret.
        default_model = _model_with_provider(env, name=f"e2e-crossns-creddef-{uuid.uuid4().hex[:6]}", provider_name=provider_name)
        _grant_for(env, default_model)
        access = _serve(env, default_model)
        response = infer_external_chat(
            env["tenant"]["gateway_url"],
            default_model["namespace"],
            default_model["client_name"],
            access["api_key"],
            path_tag=default_model["path_tag"],
        )
        assert_served_by_provider(response, default_model["path_tag"], credential=provider_credential)
        assert model_credential not in response.text, (
            "model-local Secret must not be used without an auth override"
        )

        # Override: model-local Secret wins.
        override_model = _model_with_provider(
            env,
            name=f"e2e-crossns-credovr-{uuid.uuid4().hex[:6]}",
            provider_name=provider_name,
            override_secret=shared_secret,
        )
        _grant_for(env, override_model)
        access = _serve(env, override_model)
        response = infer_external_chat(
            env["tenant"]["gateway_url"],
            override_model["namespace"],
            override_model["client_name"],
            access["api_key"],
            path_tag=override_model["path_tag"],
        )
        assert_served_by_provider(response, override_model["path_tag"], credential=model_credential)
        assert provider_credential not in response.text, (
            "provider-local Secret must not be used when auth is overridden"
        )

    def test_missing_model_local_override_fails_without_fallback(self, crossns_env):
        env = crossns_env
        provider_name = f"e2e-crossns-missing-{uuid.uuid4().hex[:6]}-provider"
        provider_credential = f"sk-extmod-prov-{uuid.uuid4().hex[:8]}"
        secret_name = f"{provider_name}-cred"
        apply_credential_secret(secret_name, env["provider_ns"], provider_credential)
        env["resources"].track("secret", secret_name, env["provider_ns"])
        apply_external_provider(provider_name, env["provider_ns"], credential_secret=secret_name)
        env["resources"].track(
            EXTERNAL_PROVIDER_KIND, provider_name, env["provider_ns"]
        )

        # Override points at a Secret that does not exist in the model
        # namespace; serving must fail rather than fall back to the
        # provider-namespace Secret.
        model = _model_with_provider(
            env,
            name=f"e2e-crossns-nofb-{uuid.uuid4().hex[:6]}",
            provider_name=provider_name,
            override_secret="does-not-exist-cred",
        )
        _grant_for(env, model)
        gateway = env["tenant"]["gateway_name"]
        wait_for_gateway_attachment_not_ready(
            model["name"], model["namespace"], gateway, message_pattern=MISSING_SECRET_PATTERN
        )
        external_model = _get_cr(EXTERNAL_MODEL_KIND, model["name"], model["namespace"]) or {}
        phase = (external_model.get("status") or {}).get("phase")
        assert phase and phase != "Ready", (
            f"model must report a present, non-Ready phase after the missing-secret "
            f"failure, got {phase!r}"
        )


class TestSameNamedProvidersTwoNamespaces:
    """A model referencing same-named providers from two namespaces serves
    through both."""

    def test_same_named_providers_both_serve(self, crossns_env):
        env = crossns_env
        case = env["tenant"]
        shared_provider = f"e2e-crossns-twin-{uuid.uuid4().hex[:6]}"
        credential_1 = f"sk-extmod-t1-{uuid.uuid4().hex[:8]}"
        credential_2 = f"sk-extmod-t2-{uuid.uuid4().hex[:8]}"
        shared_secret = f"{shared_provider}-cred"
        tag_1 = f"{shared_provider}-ns1"
        tag_2 = f"{shared_provider}-ns2"
        # The request path is the client's choice and is passed through to
        # whichever provider the gateway selects, so a single client tag is
        # used and the injected credentials distinguish the two providers.
        client_tag = f"{shared_provider}-client"
        provider_ns_1: str | None = None
        provider_ns_2: str | None = None

        try:
            # Created inside the try: if the second namespace creation
            # fails, the first is still cleaned up below.
            provider_ns_1 = new_admitted_user_namespace("e2e-extmod-crossns-p1", case["gateway_name"])
            provider_ns_2 = new_admitted_user_namespace("e2e-extmod-crossns-p2", case["gateway_name"])
            for provider_ns, credential in ((provider_ns_1, credential_1), (provider_ns_2, credential_2)):
                apply_credential_secret(shared_secret, provider_ns, credential)
                env["resources"].track("secret", shared_secret, provider_ns)
                apply_external_provider(shared_provider, provider_ns, credential_secret=shared_secret)
                env["resources"].track(
                    EXTERNAL_PROVIDER_KIND, shared_provider, provider_ns
                )
                apply_reference_grant(
                    f"{shared_provider}-grant",
                    provider_ns,
                    from_namespace=env["model_ns"],
                    to_name=shared_provider,
                )
                env["resources"].track("referencegrant", f"{shared_provider}-grant", provider_ns)

            name = f"e2e-crossns-twinmodel-{uuid.uuid4().hex[:6]}"
            apply_external_model(
                name,
                env["model_ns"],
                provider_name=shared_provider,
                provider_namespace=provider_ns_1,
                gateway_refs=[gateway_ref(case["gateway_name"])],
                path_tag=tag_1,
                extra_provider_refs=[
                    {
                        "ref": {"name": shared_provider, "namespace": provider_ns_2},
                        "targetModel": "gpt-3.5-turbo",
                        "apiFormat": "openai-chat",
                        "path": f"/anything/{tag_2}",
                    }
                ],
            )
            env["resources"].track(EXTERNAL_MODEL_KIND, name, env["model_ns"])
            apply_maas_model_ref_for_external(name, env["model_ns"])
            env["resources"].track("maasmodelref", name, env["model_ns"])
            model = {
                "name": name,
                "namespace": env["model_ns"],
                "client_name": name,
            }

            access = _serve(env, model)
            wait_for_gateway_attachment_ready(name, env["model_ns"], case["gateway_name"])

            # Weighted selection across the two bindings: both providers must
            # receive traffic across a dozen requests. The injected
            # credential — not the echoed path — identifies which same-named
            # provider served each request.
            seen = set()
            for _ in range(12):
                response = infer_external_chat(
                    case["gateway_url"],
                    model["namespace"],
                    model["client_name"],
                    access["api_key"],
                    path_tag=client_tag,
                )
                assert response.status_code == 200, (
                    f"inference failed on multi-provider model: {redact_sensitive(response.text)}"
                )
                if credential_1 in response.text:
                    seen.add("provider-ns-1")
                if credential_2 in response.text:
                    seen.add("provider-ns-2")
            assert seen == {"provider-ns-1", "provider-ns-2"}, (
                f"expected both same-named providers to serve, saw {seen}"
            )
        finally:
            if provider_ns_1:
                delete_namespace_best_effort(provider_ns_1)
            if provider_ns_2:
                delete_namespace_best_effort(provider_ns_2)
